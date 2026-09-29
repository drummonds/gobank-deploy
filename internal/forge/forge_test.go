package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

func TestLatestTagIsTheFirstTagTheAPILists(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		w.Write([]byte(`[{"name":"v0.3.48","commit":{"sha":"abc"}},{"name":"v0.3.47"}]`))
	}))
	defer srv.Close()
	repo := &Repo{URL: srv.URL + "/hum3/gobank"}
	tag, err := repo.LatestTag(context.Background())
	if err != nil || tag != "v0.3.48" {
		t.Fatalf("tag = %q, err = %v", tag, err)
	}
	if path != "/api/v1/repos/hum3/gobank/tags?limit=1" {
		t.Errorf("asked %s", path)
	}
}

func TestNoTagsIsAnEmptyTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) }))
	defer srv.Close()
	tag, err := (&Repo{URL: srv.URL + "/o/r"}).LatestTag(context.Background())
	if err != nil || tag != "" {
		t.Errorf("tag = %q, err = %v", tag, err)
	}
}

func TestAnUnreachableForgeIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := (&Repo{URL: srv.URL + "/o/r"}).LatestTag(context.Background()); err == nil {
		t.Error("want an error")
	}
}

// forgeWithRelease serves one release, tagged v0.3.48, whose assets are
// the demo binaries for linux amd64 and arm64.
func forgeWithRelease(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/v1/repos/hum3/gobank/releases/tags/v0.3.48":
			fmt.Fprintf(w, `{"tag_name":"v0.3.48","assets":[
			  {"name":"demo-linux-amd64","browser_download_url":"%[1]s/hum3/gobank/releases/download/v0.3.48/demo-linux-amd64"},
			  {"name":"demo-linux-arm64","browser_download_url":"%[1]s/hum3/gobank/releases/download/v0.3.48/demo-linux-arm64"},
			  {"name":"checksums.txt","browser_download_url":"%[1]s/hum3/gobank/releases/download/v0.3.48/checksums.txt"}]}`, srv.URL)
		case "/api/v1/repos/hum3/gobank/releases/tags/v0.3.47":
			fmt.Fprint(w, `{"tag_name":"v0.3.47","assets":[]}`)
		case "/hum3/gobank/releases/download/v0.3.48/demo-linux-arm64":
			fmt.Fprint(w, "arm64 bits")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func TestDownloadTakesTheDemoBinaryFromTheTagsRelease(t *testing.T) {
	srv, asked := forgeWithRelease(t)
	repo := &Repo{URL: srv.URL + "/hum3/gobank"}
	dst := filepath.Join(t.TempDir(), "demo")
	if err := repo.Download(context.Background(), "v0.3.48", "arm64", dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "arm64 bits" {
		t.Errorf("downloaded %q, %v", got, err)
	}
	if st, _ := os.Stat(dst); st.Mode()&0o100 == 0 {
		t.Error("the binary should be executable")
	}
	if (*asked)[0] != "/api/v1/repos/hum3/gobank/releases/tags/v0.3.48" {
		t.Errorf("asked %v", *asked)
	}
}

func TestATagWithoutAReleaseOrWithoutTheBinaryIsErrNoRelease(t *testing.T) {
	srv, _ := forgeWithRelease(t)
	repo := &Repo{URL: srv.URL + "/hum3/gobank"}
	for _, tag := range []string{"v0.3.47", "v0.3.40"} { // release with no assets; no release
		err := repo.Download(context.Background(), tag, "amd64", filepath.Join(t.TempDir(), "demo"))
		if !errors.Is(err, deploy.ErrNoRelease) {
			t.Errorf("%s: err = %v, want ErrNoRelease", tag, err)
		}
	}
}

func TestAFailedDownloadLeavesNoFile(t *testing.T) {
	srv, _ := forgeWithRelease(t)
	repo := &Repo{URL: srv.URL + "/hum3/gobank"}
	dst := filepath.Join(t.TempDir(), "demo")
	err := repo.Download(context.Background(), "v0.3.48", "amd64", dst) // asset listed but 404 on download
	if err == nil || errors.Is(err, deploy.ErrNoRelease) {
		t.Fatalf("err = %v, want a download error", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a partial file was left behind")
	}
}
