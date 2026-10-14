package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The browser fetches a staff page as the operator — the bearer token on
// the request, redirects not followed — and reports the body and how
// long the page took.
func TestBrowserFetchesAsTheOperatorAndTimesIt(t *testing.T) {
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.RequestURI() {
		case "/customers?q=Smith":
			time.Sleep(20 * time.Millisecond)
			w.Write([]byte(`<a href="/customers/abc">Ann Smith</a>`))
		case "/login":
			http.Redirect(w, r, "/login?next=/", http.StatusSeeOther)
		default:
			http.Error(w, "boom", http.StatusBadGateway)
		}
	}))
	defer ts.Close()
	p := &Browser{}
	body, took, err := p.Fetch(context.Background(), ts.URL+"/customers?q=Smith", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || !strings.Contains(body, "Ann Smith") || took < 20*time.Millisecond {
		t.Errorf("auth %q body %q took %s", auth, body, took)
	}
	if _, _, err := p.Fetch(context.Background(), ts.URL+"/broken", "tok"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("a failing page should be an error naming the status: %v", err)
	}
	if _, _, err := p.Fetch(context.Background(), ts.URL+"/login", "tok"); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("being sent to the login is a refusal, not a page: %v", err)
	}
}
