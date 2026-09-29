// Package forge reads what a Forgejo repo has released: its newest tag,
// and the binaries a tag's release carries (goreleaser's, run by the
// repo's tp release). The API and downloads are public for a public
// repo, so no token is needed.
package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// Repo is a repository on a Forgejo, by its web URL
// (https://git.bytestone.uk/hum3/gobank).
type Repo struct {
	URL     string
	Timeout time.Duration // defaults to 5s
}

// api is the URL of an API path for the repo: /api/v1/repos/<owner>/<repo><rest>.
func (r *Repo) api(rest string) (string, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s/api/v1/repos/%s%s", u.Scheme, u.Host, strings.Trim(u.Path, "/"), rest), nil
}

// get fetches a URL within timeout; the caller closes the body.
func (r *Repo) get(ctx context.Context, url string, timeout time.Duration) (*http.Response, error) {
	if r.Timeout != 0 {
		timeout = r.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &bodyWithCancel{resp.Body, cancel}
	return resp, nil
}

type bodyWithCancel struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *bodyWithCancel) Close() error { b.cancel(); return b.ReadCloser.Close() }

// LatestTag is the repo's newest tag, or "" when it has none.
func (r *Repo) LatestTag(ctx context.Context) (string, error) {
	api, err := r.api("/tags?limit=1")
	if err != nil {
		return "", err
	}
	resp, err := r.get(ctx, api, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("tags of %s: %w", r.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tags of %s: %s", r.URL, resp.Status)
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", fmt.Errorf("tags of %s: %w", r.URL, err)
	}
	if len(tags) == 0 {
		return "", nil
	}
	return tags[0].Name, nil
}

// Download implements deploy.Repo: the demo-linux-<goarch> asset of tag's
// release, written executable to dst. A tag with no release, or a release
// without that asset, is deploy.ErrNoRelease. Nothing is left at dst on
// failure.
func (r *Repo) Download(ctx context.Context, tag, goarch, dst string) error {
	api, err := r.api("/releases/tags/" + url.PathEscape(tag))
	if err != nil {
		return err
	}
	resp, err := r.get(ctx, api, 5*time.Second)
	if err != nil {
		return fmt.Errorf("release %s of %s: %w", tag, r.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release %s of %s: %w", tag, r.URL, deploy.ErrNoRelease)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release %s of %s: %s", tag, r.URL, resp.Status)
	}
	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("release %s of %s: %w", tag, r.URL, err)
	}
	want := "demo-linux-" + goarch
	var from string
	for _, a := range release.Assets {
		if a.Name == want {
			from = a.URL
		}
	}
	if from == "" {
		return fmt.Errorf("release %s of %s has no %s: %w", tag, r.URL, want, deploy.ErrNoRelease)
	}
	return r.download(ctx, from, dst)
}

// downloadTimeout bounds one binary: tens of MB over a home connection.
const downloadTimeout = 5 * time.Minute

func (r *Repo) download(ctx context.Context, from, dst string) error {
	resp, err := r.get(ctx, from, downloadTimeout)
	if err != nil {
		return fmt.Errorf("download %s: %w", from, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", from, resp.Status)
	}
	f, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(dst + ".tmp")
		return fmt.Errorf("download %s: %w", from, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(dst + ".tmp")
		return err
	}
	return os.Rename(dst+".tmp", dst)
}
