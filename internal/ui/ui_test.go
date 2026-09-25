package ui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// fakeOperator reports canned statuses and records actions. Actions block
// until released so tests can observe the running state.
type fakeOperator struct {
	mu       sync.Mutex
	statuses map[string]deploy.Status
	ups      []deploy.UpOptions
	downs    []deploy.Environment
	release  chan struct{}
	ctxErr   error
}

func newFakeOperator() *fakeOperator {
	return &fakeOperator{statuses: map[string]deploy.Status{}, release: make(chan struct{})}
}

func (f *fakeOperator) Status(_ context.Context, env deploy.Environment) (deploy.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[env.Name], nil
}

func (f *fakeOperator) Up(ctx context.Context, o deploy.UpOptions, out io.Writer) error {
	f.mu.Lock()
	f.ups = append(f.ups, o)
	f.mu.Unlock()
	fmt.Fprintf(out, "== deploying %s\n", o.Env.Name)
	select {
	case <-f.release:
	case <-ctx.Done():
		f.mu.Lock()
		f.ctxErr = ctx.Err()
		f.mu.Unlock()
		return ctx.Err()
	}
	fmt.Fprintln(out, "deployed")
	return nil
}

func (f *fakeOperator) Down(_ context.Context, env deploy.Environment, out io.Writer) error {
	f.mu.Lock()
	f.downs = append(f.downs, env)
	f.mu.Unlock()
	fmt.Fprintln(out, "done — nothing left billing")
	return nil
}

var envs = []deploy.Environment{{Name: "prod"}, {Name: "preprod"}}

func newTestServer(t *testing.T) (*httptest.Server, *fakeOperator) {
	t.Helper()
	op := newFakeOperator()
	op.statuses["prod"] = deploy.Status{
		Server:  &deploy.Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33", Status: "running", Location: "fsn1"},
		URL:     "http://10.0.0.9:1347/",
		Serving: true,
	}
	s, err := New(op, envs)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return ts, op
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func post(t *testing.T, ts *httptest.Server, path string, form url.Values) int {
	t.Helper()
	resp, err := ts.Client().PostForm(ts.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestPageShowsEachEnvironmentState(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := get(t, ts, "/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"prod", "preprod", "Serving", "Not provisioned", "cx33", "10.0.0.9", "http://10.0.0.9:1347/"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestPageOffersTheRightControls(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := get(t, ts, "/")
	// Provisioned: redeploy and down. Not provisioned: create with a scale.
	for _, want := range []string{`action="/env/prod/redeploy"`, `action="/env/prod/down"`, `action="/env/preprod/create"`, `name="scale"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, unwanted := range []string{`action="/env/prod/create"`, `action="/env/preprod/down"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page should not offer %q", unwanted)
		}
	}
}

func TestCreateStartsUpAndShowsProgressThenResult(t *testing.T) {
	ts, op := newTestServer(t)
	code := post(t, ts, "/env/preprod/create", url.Values{"scale": {"medium"}})
	if code != http.StatusSeeOther {
		t.Fatalf("status %d, want redirect", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.ups) == 1 })
	if o := op.ups[0]; !o.Create || o.Scale != "medium" || o.Env.Name != "preprod" {
		t.Errorf("up options = %+v", o)
	}

	_, body := get(t, ts, "/")
	if !strings.Contains(body, "Working") || !strings.Contains(body, "== deploying preprod") {
		t.Errorf("page should show the job running with its log:\n%s", body)
	}
	if !strings.Contains(body, `action="/env/preprod/cancel"`) {
		t.Error("a running job offers cancel")
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("page should poll while a job runs")
	}

	close(op.release)
	var body2 string
	waitFor(t, func() bool { _, body2 = get(t, ts, "/"); return strings.Contains(body2, "Finished") })
	body = body2
	if !strings.Contains(body, "deployed") {
		t.Errorf("finished log missing:\n%s", body)
	}
}

func TestOneJobPerEnvironment(t *testing.T) {
	ts, op := newTestServer(t)
	post(t, ts, "/env/prod/redeploy", nil)
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.ups) == 1 })

	if code := post(t, ts, "/env/prod/redeploy", nil); code != http.StatusConflict {
		t.Errorf("second job on the same env: status %d, want 409", code)
	}
	// Another environment is independent.
	if code := post(t, ts, "/env/preprod/create", url.Values{"scale": {"small"}}); code != http.StatusSeeOther {
		t.Errorf("job on another env: status %d", code)
	}
	close(op.release)
}

func TestDownRequiresConfirmation(t *testing.T) {
	ts, op := newTestServer(t)
	if code := post(t, ts, "/env/prod/down", nil); code != http.StatusBadRequest {
		t.Errorf("unconfirmed down: status %d, want 400", code)
	}
	if len(op.downs) != 0 {
		t.Fatal("down must not run unconfirmed")
	}
	if code := post(t, ts, "/env/prod/down", url.Values{"confirm": {"on"}}); code != http.StatusSeeOther {
		t.Errorf("confirmed down: status %d", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.downs) == 1 })
}

func TestCancelStopsARunningJob(t *testing.T) {
	ts, op := newTestServer(t)
	post(t, ts, "/env/prod/redeploy", nil)
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.ups) == 1 })
	if code := post(t, ts, "/env/prod/cancel", nil); code != http.StatusSeeOther {
		t.Errorf("cancel: status %d", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return op.ctxErr != nil })
	_, body := get(t, ts, "/")
	if !strings.Contains(body, "Failed") {
		t.Errorf("cancelled job should show as failed:\n%s", body)
	}
}

func TestUnknownEnvironmentIs404(t *testing.T) {
	ts, _ := newTestServer(t)
	if code := post(t, ts, "/env/staging/redeploy", nil); code != http.StatusNotFound {
		t.Errorf("status %d", code)
	}
}
