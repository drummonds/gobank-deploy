package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
