package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Browser fetches a staff page of the demo as a browser would, admitted
// as the operator by the environment's API token, and says how long the
// page took to arrive whole. A redirect to the login is a refusal, as it
// is for the console's forms.
type Browser struct {
	Timeout time.Duration // per request; defaults to 30s
}

// Fetch GETs url with the token and returns the body and the time from
// the request to the last byte.
func (p *Browser) Fetch(ctx context.Context, url, token string) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	return p.do(req, token)
}

// Post submits form to url with the token, as a browser submits a form,
// and returns the page it answers with the way Fetch does. The customer
// search takes its name this way: personal data never goes in a URL.
func (p *Browser) Post(ctx context.Context, url, token string, form url.Values) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return p.do(req, token)
}

func (p *Browser) do(req *http.Request, token string) (string, time.Duration, error) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	took := time.Since(started)
	if err != nil {
		return "", took, err
	}
	if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 && strings.Contains(loc, "login") {
		return "", took, fmt.Errorf("%s: %w", req.URL, ErrLoginWanted)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", took, fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	return string(body), took, nil
}
