// Package forge reads what a Forgejo repo has released: its newest tag.
// The tags API is public for a public repo, so no token is needed.
package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo is a repository on a Forgejo, by its web URL
// (https://git.bytestone.uk/hum3/gobank).
type Repo struct {
	URL     string
	Timeout time.Duration // defaults to 5s
}

// LatestTag is the repo's newest tag, or "" when it has none.
func (r *Repo) LatestTag(ctx context.Context) (string, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return "", err
	}
	path := strings.Trim(u.Path, "/")
	api := fmt.Sprintf("%s://%s/api/v1/repos/%s/tags?limit=1", u.Scheme, u.Host, path)
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
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
