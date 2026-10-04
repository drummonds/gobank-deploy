package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
)

// fakeOperator reports canned statuses and records actions. Actions block
// until released so tests can observe the running state.
type fakeOperator struct {
	mu        sync.Mutex
	statuses  map[string]deploy.Status // provisioned environments
	envsErr   error
	ups       []deploy.UpOptions
	downs     []deploy.Environment
	demos     []demoCall
	runs      []wf.RunRecord
	releases  deploy.Releases
	relErr    error
	fetched   string   // what Fetch reports fetching
	fetchTags []string // the tags Fetch was asked for ("" is the newest)
	fetchErr  error
	fetches   int
	release   chan struct{}
	ctxErr    error
	drilled   []deploy.Environment
	drills    []drills.Drill
	steps     map[string][]wf.StepResult
}

func (f *fakeOperator) Drill(ctx context.Context, env deploy.Environment, out io.Writer) error {
	f.mu.Lock()
	f.drilled = append(f.drilled, env)
	f.mu.Unlock()
	fmt.Fprintf(out, "== prepare %s\n", env.Name)
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (f *fakeOperator) Drills(context.Context, int) ([]drills.Drill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drills, nil
}

func (f *fakeOperator) Run(_ context.Context, id string) (*wf.RunRecord, []wf.StepResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.ID == id {
			return &r, f.steps[id], nil
		}
	}
	return nil, nil, nil
}

func newFakeOperator() *fakeOperator {
	return &fakeOperator{statuses: map[string]deploy.Status{}, release: make(chan struct{})}
}

type demoCall struct {
	Env   deploy.Environment
	Scale string
}

func (f *fakeOperator) Demo(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	f.mu.Lock()
	f.demos = append(f.demos, demoCall{env, scale})
	f.mu.Unlock()
	fmt.Fprintf(out, "== create %s until %s\n", env.Name, env.Expires.Format(time.RFC3339))
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (f *fakeOperator) Releases(context.Context) (deploy.Releases, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releases, f.relErr
}

func (f *fakeOperator) Fetch(_ context.Context, tag string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	f.fetchTags = append(f.fetchTags, tag)
	if tag != "" && f.fetchErr == nil {
		return tag, nil
	}
	return f.fetched, f.fetchErr
}

func (f *fakeOperator) Runs(context.Context, int) ([]wf.RunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs, nil
}

func (f *fakeOperator) Environments(context.Context) ([]deploy.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envsErr != nil {
		return nil, f.envsErr
	}
	var envs []deploy.Environment
	for name, st := range f.statuses {
		if st.Server != nil {
			envs = append(envs, deploy.Environment{Name: name, Expires: st.Server.Expires()})
		}
	}
	slices.SortFunc(envs, func(a, b deploy.Environment) int { return strings.Compare(a.Name, b.Name) })
	return envs, nil
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
	return newTestServerWith(t, func(*Server) {})
}

// newTestServerWith lets a test configure the Server before it serves.
func newTestServerWith(t *testing.T, configure func(*Server)) (*httptest.Server, *fakeOperator) {
	t.Helper()
	op := newFakeOperator()
	op.statuses["prod"] = deploy.Status{
		Server:    &deploy.Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33", Status: "running", Location: "fsn1"},
		Host:      "prod.gobank.test",
		URL:       "http://prod.gobank.test:1347/",
		Serving:   true,
		Version:   "v0.3.46",
		Available: "v0.3.47",
	}
	s, err := New(op, envs)
	if err != nil {
		t.Fatal(err)
	}
	configure(s)
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

// hxPost posts as htmx does, asking for the fragment back.
func hxPost(t *testing.T, ts *httptest.Server, path string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
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
	for _, want := range []string{"prod", "preprod", "Serving", "Not provisioned", "cx33", "10.0.0.9", "prod.gobank.test", `href="http://prod.gobank.test:1347/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestPageComparesRunningAndAvailableVersions(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/")
	for _, want := range []string{"v0.3.46", "v0.3.47 available"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	op.mu.Lock()
	st := op.statuses["prod"]
	st.Version = "v0.3.47"
	op.statuses["prod"] = st
	op.mu.Unlock()
	_, body = get(t, ts, "/")
	if strings.Contains(body, "available") || !strings.Contains(body, "current") {
		t.Errorf("an up-to-date environment is current:\n%s", body)
	}
}

func TestPageShowsTheReposLatestTagAgainstWhatIsDeployableHere(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.releases = deploy.Releases{Repo: "v0.3.48", Available: "v0.3.44"}
	op.mu.Unlock()
	_, body := get(t, ts, "/")
	for _, want := range []string{"v0.3.48", "newest tag", "v0.3.44", "behind the repo"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if !strings.Contains(body, "push a newer release") {
		t.Error("a host that cannot fetch is told to push or pull")
	}
	op.mu.Lock()
	op.releases = deploy.Releases{Repo: "v0.3.48", Available: "v0.3.44", Fetches: true}
	op.mu.Unlock()
	_, body = get(t, ts, "/")
	if !strings.Contains(body, "behind the repo") || !strings.Contains(body, "fetched by the next deploy") || strings.Contains(body, "push a newer release") {
		t.Error("a store is told the next deploy fetches the release")
	}
	op.mu.Lock()
	op.releases = deploy.Releases{Repo: "v0.3.48", Available: "v0.3.48"}
	op.mu.Unlock()
	_, body = get(t, ts, "/")
	if strings.Contains(body, "behind the repo") {
		t.Error("up to date is not behind")
	}
	op.mu.Lock()
	op.relErr = errors.New("forge down")
	op.mu.Unlock()
	code, body := get(t, ts, "/")
	if code != 200 || !strings.Contains(body, "forge down") {
		t.Errorf("a forge outage is shown, not fatal (status %d)", code)
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
	if !strings.Contains(body, `hx-trigger="every 3s`) {
		t.Error("page should poll fast while a job runs")
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

// The list is the configured environments (always shown, provisioned or
// not) plus whatever gobank servers exist in the project.
func TestPageListsConfiguredAndExistingEnvironments(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.statuses["demo"] = deploy.Status{Server: &deploy.Server{Name: "gobank-demo", IP: "10.0.0.3", Type: "cx23", Status: "running", Location: "fsn1"}}
	op.mu.Unlock()
	_, body := get(t, ts, "/")
	for _, want := range []string{"prod", "preprod", "demo", `action="/env/demo/down"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if i, j := strings.Index(body, "preprod</td>"), strings.Index(body, "demo</td>"); i > j {
		t.Error("configured environments come first, in the configured order")
	}
	if code := post(t, ts, "/env/demo/redeploy", nil); code != http.StatusSeeOther {
		t.Errorf("redeploy of a discovered environment: status %d", code)
	}
	close(op.release)
}

func TestPageSaysWhenTheListCannotBeRead(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.envsErr = errors.New("hcloud: 401")
	op.mu.Unlock()
	code, body := get(t, ts, "/")
	if code != 200 || !strings.Contains(body, "hcloud: 401") || !strings.Contains(body, "prod") {
		t.Errorf("page should still show configured environments and the error (status %d):\n%s", code, body)
	}
}

func TestNewEnvironmentIsCreatedByNameAndListed(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/")
	if !strings.Contains(body, `action="/env"`) || !strings.Contains(body, `name="name"`) {
		t.Fatalf("page should offer a new environment form:\n%s", body)
	}
	code := post(t, ts, "/env", url.Values{"name": {"demo"}, "scale": {"large"}})
	if code != http.StatusSeeOther {
		t.Fatalf("status %d, want redirect", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.ups) == 1 })
	if o := op.ups[0]; !o.Create || o.Scale != "large" || o.Env.Name != "demo" {
		t.Errorf("up options = %+v", o)
	}
	// Listed while creating, before the cloud knows about it.
	_, body = get(t, ts, "/")
	if !strings.Contains(body, "demo</td>") || !strings.Contains(body, `action="/env/demo/cancel"`) {
		t.Errorf("new environment should be a row with its job:\n%s", body)
	}
	close(op.release)
}

// --- About ----------------------------------------------------------------------

func TestAboutPageDescribesTheStructureWithDiagrams(t *testing.T) {
	ts, _ := newTestServer(t)
	_, index := get(t, ts, "/")
	if !strings.Contains(index, `href="/about"`) {
		t.Error("the page should link to About")
	}
	code, body := get(t, ts, "/about")
	if code != 200 {
		t.Fatalf("about: status %d", code)
	}
	for _, want := range []string{"Forgejo", "Hetzner", "Route 53", "hydrogen", `src="/assets/components.svg"`, `src="/assets/release-path.svg"`, `src="/assets/deploy-sequence.svg"`, `src="/assets/demo-workflow.svg"`, `href="/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("about missing %q", want)
		}
	}
	resp, err := ts.Client().Get(ts.URL + "/assets/components.svg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("diagram: status %d, content type %q", resp.StatusCode, ct)
	}
}

// --- Temporary environments: the demo workflow --------------------------------

func TestNewEnvironmentWithARemovalTimeRunsTheDemoWorkflow(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/")
	if !strings.Contains(body, `name="remove"`) {
		t.Fatalf("create forms should offer a removal time:\n%s", body)
	}
	before := time.Now()
	if code := post(t, ts, "/env", url.Values{"name": {"demo"}, "scale": {"medium"}, "remove": {"4h"}}); code != http.StatusSeeOther {
		t.Fatalf("status %d", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.demos) == 1 })
	call := op.demos[0]
	if call.Env.Name != "demo" || call.Scale != "medium" || len(op.ups) != 0 {
		t.Errorf("demo call = %+v, ups = %v", call, op.ups)
	}
	if got := call.Env.Expires.Sub(before); got < 4*time.Hour-time.Second || got > 4*time.Hour+time.Minute {
		t.Errorf("expires in %v, want 4h", got)
	}
	_, body = get(t, ts, "/")
	if !strings.Contains(body, "Working: demo") || !strings.Contains(body, "== create demo until") {
		t.Errorf("page should show the demo job running:\n%s", body)
	}
	close(op.release)
}

// A configured row's Create form can make it temporary too.
func TestCreateWithARemovalTimeRunsTheDemoWorkflow(t *testing.T) {
	ts, op := newTestServer(t)
	post(t, ts, "/env/preprod/create", url.Values{"scale": {"small"}, "remove": {"1h"}})
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.demos) == 1 })
	if op.demos[0].Env.Name != "preprod" || len(op.ups) != 0 {
		t.Errorf("demos = %+v ups = %+v", op.demos, op.ups)
	}
	close(op.release)
}

func TestKeepIsAnOrdinaryCreate(t *testing.T) {
	ts, op := newTestServer(t)
	post(t, ts, "/env/preprod/create", url.Values{"scale": {"small"}, "remove": {"keep"}})
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.ups) == 1 })
	if len(op.demos) != 0 || !op.ups[0].Expires.IsZero() {
		t.Errorf("demos = %+v ups = %+v", op.demos, op.ups)
	}
	close(op.release)
}

// A temporary server found in the project with nothing looking after it
// (the page restarted) gets its workflow resumed, and shows its expiry.
func TestReconcileResumesTemporaryEnvironmentsWithoutAJob(t *testing.T) {
	ts, op := newTestServer(t)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	op.mu.Lock()
	op.statuses["demo"] = deploy.Status{Server: &deploy.Server{Name: "gobank-demo", IP: "10.0.0.3", Labels: map[string]string{"expires": expires.Format("20060102T150405Z0700")}}}
	op.mu.Unlock()
	srv := ts.Config.Handler.(*Server)
	srv.Reconcile(context.Background())
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.demos) == 1 })
	if !op.demos[0].Env.Expires.Equal(expires) {
		t.Errorf("resumed with expiry %v, want %v", op.demos[0].Env.Expires, expires)
	}
	srv.Reconcile(context.Background()) // already looked after
	_, body := get(t, ts, "/")
	if !strings.Contains(body, "removed at "+expires.Local().Format("15:04")) {
		t.Errorf("page should show when the demo goes:\n%s", body)
	}
	if len(op.demos) != 1 {
		t.Errorf("a running job is not started again: %d", len(op.demos))
	}
	close(op.release)
}

func TestPageListsWorkflowRuns(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.runs = []wf.RunRecord{{WorkflowType: "demo", Key: "demo@2026-09-27T14:00:00Z", Status: wf.StatusFailed, Error: "create: out of stock", StartedAt: time.Now()}}
	op.mu.Unlock()
	_, body := get(t, ts, "/")
	for _, want := range []string{"demo@2026-09-27T14:00:00Z", "failed", "create: out of stock"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestNewEnvironmentNameMustBeAHostnameLabel(t *testing.T) {
	ts, op := newTestServer(t)
	for _, name := range []string{"", "Prod", "pre prod", "a/b", "-x", strings.Repeat("a", 40)} {
		if code := post(t, ts, "/env", url.Values{"name": {name}, "scale": {"small"}}); code != http.StatusBadRequest {
			t.Errorf("name %q: status %d, want 400", name, code)
		}
	}
	if code := post(t, ts, "/env", url.Values{"name": {"prod"}, "scale": {"small"}}); code != http.StatusConflict {
		t.Errorf("an existing environment: status %d, want 409", code)
	}
	if len(op.ups) != 0 {
		t.Fatal("up must not run for a bad name")
	}
}

func TestNewEnvironmentNeedsABuilder(t *testing.T) {
	ts, op := newTestServerWith(t, func(s *Server) { s.UpUnavailable = func() string { return "no store" } })
	_, body := get(t, ts, "/")
	if strings.Contains(body, `action="/env"`) {
		t.Error("page should not offer a new environment without a builder")
	}
	if code := post(t, ts, "/env", url.Values{"name": {"demo"}, "scale": {"small"}}); code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}
	if len(op.ups) != 0 {
		t.Fatal("up must not run without a builder")
	}
}

func TestUnknownEnvironmentIs404(t *testing.T) {
	ts, _ := newTestServer(t)
	if code := post(t, ts, "/env/staging/redeploy", nil); code != http.StatusNotFound {
		t.Errorf("status %d", code)
	}
}

// A host that cannot build the demo (no Go toolchain, no gobank checkout —
// a gokrazy appliance, say) still shows every environment and can turn one
// off, but offers nothing that would need a release.
func TestWithoutABuilderOnlyStatusAndDownAreOffered(t *testing.T) {
	const reason = "no Go toolchain on this host"
	ts, op := newTestServerWith(t, func(s *Server) { s.UpUnavailable = func() string { return reason } })
	_, body := get(t, ts, "/")
	for _, want := range []string{reason, "Serving", "Not provisioned", `action="/env/prod/down"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, unwanted := range []string{`action="/env/prod/redeploy"`, `action="/env/preprod/create"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page should not offer %q", unwanted)
		}
	}
	if code := post(t, ts, "/env/preprod/create", url.Values{"scale": {"small"}}); code != http.StatusForbidden {
		t.Errorf("create without a builder: status %d, want 403", code)
	}
	if code := post(t, ts, "/env/prod/redeploy", nil); code != http.StatusForbidden {
		t.Errorf("redeploy without a builder: status %d, want 403", code)
	}
	if len(op.ups) != 0 {
		t.Fatal("up must not run without a builder")
	}
	if code := post(t, ts, "/env/prod/down", url.Values{"confirm": {"on"}}); code != http.StatusSeeOther {
		t.Errorf("down is still allowed: status %d", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.downs) == 1 })
}

// POST /fetch is the release's post_release step: it fetches the repo's
// newest release into the store and says which, or fails visibly so the
// release prints a warning.
func TestFetchEndpointFetchesTheNewestReleaseIntoTheStore(t *testing.T) {
	ts, op := newTestServer(t)
	op.fetched = "v0.3.52"

	resp, err := http.Post(ts.URL+"/fetch", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "v0.3.52") {
		t.Errorf("status %d, body %q", resp.StatusCode, body)
	}
	if op.fetches != 1 {
		t.Errorf("fetches = %d", op.fetches)
	}
}

// POST /fetch?tag=vX asks for that release: fetched if the store lacks
// it, and the next deploy either way, so a rollback is a fetch of the
// previous tag and a redeploy.
func TestFetchEndpointFetchesANamedTag(t *testing.T) {
	ts, op := newTestServer(t)

	resp, err := http.Post(ts.URL+"/fetch?tag=v0.3.44", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "v0.3.44") {
		t.Errorf("status %d, body %q", resp.StatusCode, body)
	}
	if !slices.Equal(op.fetchTags, []string{"v0.3.44"}) {
		t.Errorf("fetchTags = %v, want [v0.3.44]", op.fetchTags)
	}
}

func TestFetchEndpointReportsAFailedFetch(t *testing.T) {
	ts, op := newTestServer(t)
	op.fetchErr = errors.New("v0.3.52 has no built demo")

	resp, err := http.Post(ts.URL+"/fetch", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "no built demo") {
		t.Errorf("status %d, body %q", resp.StatusCode, body)
	}
}

func TestClockHandsPointAtTheTime(t *testing.T) {
	at := time.Date(2026, 10, 2, 10, 8, 30, 0, time.UTC)
	h := clockHandsAt(at)
	if h.Hour != 304.25 || h.Minute != 51 || h.Second != 180 {
		t.Errorf("hands at 10:08:30 = %+v, want hour 304.25 minute 51 second 180", h)
	}
}

func TestPageShowsAClockSetToTheRenderTime(t *testing.T) {
	at := time.Date(2026, 10, 2, 10, 8, 30, 0, time.UTC)
	ts, _ := newTestServerWith(t, func(s *Server) { s.Now = func() time.Time { return at } })
	_, body := get(t, ts, "/")
	if !strings.Contains(body, `class="clock"`) {
		t.Fatal("page should show a clock")
	}
	for _, want := range []string{`rotate(304.25 `, `rotate(51 `, `rotate(180 `} {
		if !strings.Contains(body, want) {
			t.Errorf("clock hands missing %q", want)
		}
	}
}

func TestPageIsAShellAroundAPolledFragment(t *testing.T) {
	ts, _ := newTestServer(t)
	_, page := get(t, ts, "/")
	for _, want := range []string{`src="/assets/htmx.min.js"`, `id="envs"`, `hx-get="/fragment"`, `hx-trigger="every 15s`, `name="name"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("the page must not reload itself")
	}
	code, frag := get(t, ts, "/fragment")
	if code != 200 {
		t.Fatalf("fragment status %d", code)
	}
	for _, want := range []string{`id="envs"`, "prod", "Serving", `hx-trigger="every 15s`, `hx-swap-oob`, `class="clock"`} {
		if !strings.Contains(frag, want) {
			t.Errorf("fragment missing %q", want)
		}
	}
	for _, unwanted := range []string{"<html", `name="name"`} {
		if strings.Contains(frag, unwanted) {
			t.Errorf("fragment should not contain %q", unwanted)
		}
	}
}

func TestPollingPausesWhileAFormHasFocus(t *testing.T) {
	ts, _ := newTestServer(t)
	_, frag := get(t, ts, "/fragment")
	if !strings.Contains(frag, `hx-trigger="every 15s [!document.activeElement.closest('form')]"`) {
		t.Errorf("fragment should not poll while a form is being filled in:\n%s", frag)
	}
}

func TestHtmxActionReturnsTheFragmentInsteadOfRedirecting(t *testing.T) {
	ts, op := newTestServer(t)
	code, body := hxPost(t, ts, "/env/preprod/create", url.Values{"scale": {"medium"}})
	if code != 200 {
		t.Fatalf("status %d, want the fragment", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.ups) == 1 })
	for _, want := range []string{`id="envs"`, "Working", `hx-trigger="every 3s`} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment after create missing %q", want)
		}
	}
	if strings.Contains(body, "<html") {
		t.Error("an htmx post gets the fragment, not the page")
	}
}

func TestHtmxActionErrorShowsInTheFragment(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := hxPost(t, ts, "/env/prod/down", url.Values{})
	if code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`id="envs"`, "tick the confirmation", "prod"} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment after a refused action missing %q", want)
		}
	}
	_, after := get(t, ts, "/fragment")
	if strings.Contains(after, "tick the confirmation") {
		t.Error("the error belongs to the refused request, not to later polls")
	}
}

func TestHtmxIsEmbedded(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := get(t, ts, "/assets/htmx.min.js")
	if code != 200 || !strings.HasPrefix(body, "var htmx=") {
		t.Errorf("htmx should be served from the binary: status %d, body %.40q", code, body)
	}
}

// The drill is offered where the manual one runs: a serving environment,
// a newer release on the repo, and a store to roll back from.
func TestDrillIsOfferedWhenANewerReleaseCanBeFetched(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/")
	if strings.Contains(body, "/env/prod/drill") {
		t.Error("without a store there is nothing to roll back from: no drill")
	}
	op.mu.Lock()
	op.releases = deploy.Releases{Repo: "v0.3.48", Available: "v0.3.47", Fetches: true}
	op.mu.Unlock()
	_, body = get(t, ts, "/")
	if !strings.Contains(body, `action="/env/prod/drill"`) || !strings.Contains(body, "Drill to v0.3.48") || strings.Contains(body, `action="/env/preprod/drill"`) {
		t.Errorf("drill offered on prod to the repo's newest only:\n%s", body)
	}
	op.mu.Lock()
	op.releases = deploy.Releases{Repo: "v0.3.46", Available: "v0.3.46", Fetches: true}
	op.mu.Unlock()
	_, body = get(t, ts, "/")
	if strings.Contains(body, "/env/prod/drill") {
		t.Error("serving the newest release: nothing to drill")
	}
}

func TestDrillNeedsConfirmationThenRunsAsAJob(t *testing.T) {
	ts, op := newTestServer(t)
	if code := post(t, ts, "/env/prod/drill", nil); code != http.StatusBadRequest {
		t.Errorf("unconfirmed drill: status %d", code)
	}
	if code := post(t, ts, "/env/prod/drill", url.Values{"confirm": {"on"}}); code != http.StatusSeeOther {
		t.Errorf("drill: status %d", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.drilled) == 1 })
	_, body := get(t, ts, "/")
	if !strings.Contains(body, "Working: drill") || !strings.Contains(body, "== prepare prod") {
		t.Errorf("page should show the drill running with its log:\n%s", body)
	}
	close(op.release)
}

func sampleDrill() drills.Drill {
	yes := true
	at := time.Date(2026, 10, 4, 10, 0, 0, 0, time.Local)
	pos := &drills.Position{Day: "2026-03-01", DayCount: 59, Customers: 1200, Savings: "£1,000.00", Lending: "£500.00"}
	return drills.Drill{RunID: "run-7", Environment: "prod", From: "v0.7.0", To: "v0.8.0", CreatedAt: at, Observations: []drills.Observation{
		{Moment: drills.Before, Version: "v0.7.0", ObservedAt: at},
		{Moment: drills.Upgraded, Version: "v0.8.0", Position: pos, Restart: &drills.Restart{PreviousVersion: "", Downtime: 42 * time.Second, DowntimeKnown: true}},
		{Moment: drills.RolledBack, Version: "v0.7.0", ObservedAt: at},
		{Moment: drills.Forward, Version: "v0.8.0", Position: pos, Restart: &drills.Restart{PreviousVersion: "", Downtime: 39 * time.Second, DowntimeKnown: true, Intact: &yes}},
	}}
}

func TestDrillsPageIsTheHistoryWithTheLineForTheRecord(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/drills")
	if !strings.Contains(body, "No drills yet") {
		t.Errorf("empty history:\n%s", body)
	}
	op.mu.Lock()
	op.drills = []drills.Drill{sampleDrill()}
	op.mu.Unlock()
	code, body := get(t, ts, "/drills")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"v0.7.0 → v0.8.0", "42s", "39s", "unrecorded", "no about.json", "intact", `href="/workflows/run-7"`, "2026-10-04 prod: v0.7.0 → v0.8.0, upgraded 42s, forward 39s"} {
		if !strings.Contains(body, want) {
			t.Errorf("drills page missing %q:\n%s", want, body)
		}
	}
}

func TestRunPageShowsTheStepsAndADrillsObservations(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.runs = []wf.RunRecord{{ID: "run-7", WorkflowType: "drill", Key: "prod v0.7.0→v0.8.0 2026-10-04", Status: wf.StatusFailed, Error: "upgrade: serving v0.7.0, expected v0.8.0", StartedAt: time.Now()}}
	op.steps = map[string][]wf.StepResult{"run-7": {{StepName: "prepare", Status: wf.StatusCompleted, DurationNs: int64(3 * time.Second)}, {StepName: "upgrade", Status: wf.StatusFailed, Error: "serving v0.7.0, expected v0.8.0"}}}
	op.drills = []drills.Drill{sampleDrill()}
	op.mu.Unlock()
	code, body := get(t, ts, "/workflows/run-7")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"prod v0.7.0→v0.8.0 2026-10-04", "failed", "prepare", "3s", "serving v0.7.0, expected v0.8.0", "Observations", "42s"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
	if code, _ := get(t, ts, "/workflows/nope"); code != 404 {
		t.Errorf("unknown run: status %d", code)
	}
	_, body = get(t, ts, "/")
	if !strings.Contains(body, `href="/workflows/run-7"`) {
		t.Error("the runs table links each instance")
	}
}

func TestExplorerIsServedInThePageLayoutWhenConfigured(t *testing.T) {
	ts, _ := newTestServer(t)
	if code, body := get(t, ts, "/internal/explorer"); code != 404 || strings.Contains(body, "DB Explorer") {
		t.Errorf("no explorer configured: status %d", code)
	}
	var asked string
	ts, _ = newTestServerWith(t, func(s *Server) {
		s.Explorer = func(_ context.Context, rawURL string) string { asked = rawURL; return "<p>EXPLORER</p>" }
	})
	code, body := get(t, ts, "/internal/explorer/drills?page=2")
	if code != 200 || !strings.Contains(body, "<p>EXPLORER</p>") || !strings.Contains(body, "navbar") || asked != "/internal/explorer/drills?page=2" {
		t.Errorf("status %d asked %q:\n%s", code, asked, body)
	}
	_, body = get(t, ts, "/")
	if !strings.Contains(body, `href="/internal/explorer"`) {
		t.Error("the navbar links the explorer when there is one")
	}
}
