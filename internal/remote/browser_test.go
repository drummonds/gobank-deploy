package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The browser fetches a staff page as the operator — the bearer token on
// the request, redirects not followed — and reports the body and how
// long the page took; a form is posted the same way, the customer
// search's name in the body and never in the URL.
func TestBrowserFetchesAsTheOperatorAndTimesIt(t *testing.T) {
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /customers?page=2":
			time.Sleep(20 * time.Millisecond)
			w.Write([]byte(`<a href="/customers/abc">Ann Smith</a>`))
		case "POST /customers/search":
			if r.FormValue("name") != "Jones" {
				http.Error(w, "no name", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`<a href="/customers/def">Bob Jones</a>`))
		case "GET /login":
			http.Redirect(w, r, "/login?next=/", http.StatusSeeOther)
		default:
			http.Error(w, "boom", http.StatusBadGateway)
		}
	}))
	defer ts.Close()
	p := &Browser{}
	body, took, err := p.Fetch(context.Background(), ts.URL+"/customers?page=2", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || !strings.Contains(body, "Ann Smith") || took < 20*time.Millisecond {
		t.Errorf("auth %q body %q took %s", auth, body, took)
	}
	body, _, err = p.Post(context.Background(), ts.URL+"/customers/search", "tok", url.Values{"name": {"Jones"}})
	if err != nil || auth != "Bearer tok" || !strings.Contains(body, "Bob Jones") {
		t.Errorf("post: %v, auth %q, body %q", err, auth, body)
	}
	if _, _, err := p.Fetch(context.Background(), ts.URL+"/broken", "tok"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("a failing page should be an error naming the status: %v", err)
	}
	if _, _, err := p.Fetch(context.Background(), ts.URL+"/login", "tok"); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("being sent to the login is a refusal, not a page: %v", err)
	}
}
