package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
)

func TestProbeReadsTheVersionFromThePageFooter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("<title>Model Bank</title><body>...<footer>\n      <p>Model Bank v0.3.47-2-gabc1234</p>\n</footer>"))
	}))
	defer srv.Close()
	version, serving := (&Prober{}).Probe(context.Background(), srv.URL+"/")
	if !serving || version != "v0.3.47-2-gabc1234" {
		t.Errorf("probe = %q, %v", version, serving)
	}
}

func TestProbeOfAnOlderDemoIsServingWithNoVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<h1>Model Bank</h1><p>Model Bank</p>"))
	}))
	defer srv.Close()
	version, serving := (&Prober{}).Probe(context.Background(), srv.URL+"/")
	if !serving || version != "" {
		t.Errorf("probe = %q, %v", version, serving)
	}
}

func TestProbeOfNothingIsNotServing(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, serving := (&Prober{}).Probe(context.Background(), srv.URL+"/"); serving {
		t.Error("a closed server is not serving")
	}
}

// The version a laptop build would carry is the checkout's git describe.
func TestBuilderAvailableIsTheCheckoutsDescribedVersion(t *testing.T) {
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "one"}, {"tag", "v0.9.0"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if got := (&Builder{Src: src}).Available(); got != "v0.9.0" {
		t.Errorf("available = %q", got)
	}
	if got := (&Builder{Src: t.TempDir()}).Available(); got != "" {
		t.Errorf("no checkout: available = %q, want empty", got)
	}
}
