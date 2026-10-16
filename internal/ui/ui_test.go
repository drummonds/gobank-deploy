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
	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
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
	perfs     []demoCall
	perfRuns  []perf.Run
	loads     []demoCall
	loadRuns  []load.Run
	scenarios []scenario.Run
}

func (f *fakeOperator) Load(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	f.mu.Lock()
	f.loads = append(f.loads, demoCall{env, scale})
	f.mu.Unlock()
	fmt.Fprintf(out, "== create %s at %s\n", env.Name, scale)
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (f *fakeOperator) Loads(context.Context, int) ([]load.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadRuns, nil
}

func (f *fakeOperator) Perf(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	f.mu.Lock()
	f.perfs = append(f.perfs, demoCall{env, scale})
	f.mu.Unlock()
	fmt.Fprintf(out, "== create %s at %s\n", env.Name, scale)
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (f *fakeOperator) Perfs(context.Context, int) ([]perf.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.perfRuns, nil
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

func (f *fakeOperator) Counts(context.Context) (map[wf.WorkflowType]wf.RunCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := map[wf.WorkflowType]wf.RunCounts{}
	for _, r := range f.runs {
		if counts[r.WorkflowType] == nil {
			counts[r.WorkflowType] = wf.RunCounts{}
		}
		counts[r.WorkflowType][r.Status]++
	}
	return counts, nil
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
	for _, want := range []string{"prod", "preprod", "Serving", "Not provisioned", "cx33", "10.0.0.9", "prod.gobank.test", `href="http://prod.gobank.test:1347/" target="_blank" rel="noopener"`} {
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

// A finished run's row is forgotten on request once its server has gone:
// not while its job runs, not while it has a server, never a configured
// environment, which is always listed.
func TestForgetDropsAFinishedEnvironmentFromThePage(t *testing.T) {
	ts, op := newTestServer(t)
	op.statuses["kept"] = deploy.Status{Server: &deploy.Server{Name: "gobank-kept", Status: "running"}}
	post(t, ts, "/perf", url.Values{"name": {"perf1"}, "scale": {"small"}})
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.perfs) == 1 })
	_, body := get(t, ts, "/")
	if strings.Contains(body, `action="/env/perf1/forget"`) {
		t.Error("a running job's row should not offer Forget")
	}
	if code := post(t, ts, "/env/perf1/forget", nil); code != http.StatusConflict {
		t.Errorf("forget while running: status %d, want 409", code)
	}
	close(op.release)
	waitFor(t, func() bool { _, body := get(t, ts, "/"); return strings.Contains(body, "Finished: perf") })

	_, body = get(t, ts, "/")
	if !strings.Contains(body, `action="/env/perf1/forget"`) {
		t.Errorf("a finished run with no server should offer Forget:\n%s", body)
	}
	for _, name := range []string{"preprod", "kept"} {
		if strings.Contains(body, `action="/env/`+name+`/forget"`) {
			t.Errorf("%s should not offer Forget", name)
		}
		if code := post(t, ts, "/env/"+name+"/forget", nil); code != http.StatusConflict {
			t.Errorf("forget %s: status %d, want 409", name, code)
		}
	}
	if code := post(t, ts, "/env/nosuch/forget", nil); code != http.StatusNotFound {
		t.Errorf("forget an unknown environment: status %d, want 404", code)
	}

	if code := post(t, ts, "/env/perf1/forget", nil); code != http.StatusSeeOther {
		t.Errorf("forget: status %d, want 303", code)
	}
	_, body = get(t, ts, "/")
	if strings.Contains(body, ">perf1<") {
		t.Errorf("a forgotten environment should be gone from the page:\n%s", body)
	}
	if !strings.Contains(body, ">preprod<") || !strings.Contains(body, ">kept<") {
		t.Error("the other rows stay")
	}
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
	for _, want := range []string{"Sun 4 Oct 2026 10:00 · prod · v0.7.0 → v0.8.0", "42s", "39s", "unrecorded", "no about.json", "intact", `href="/workflows/run-7">details</a>`, "2026-10-04 prod: v0.7.0 → v0.8.0, upgraded 42s, forward 39s"} {
		if !strings.Contains(body, want) {
			t.Errorf("drills page missing %q:\n%s", want, body)
		}
	}
}

func TestDrillsPageShowsEachDrillsRunStateAndSteps(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.runs = []wf.RunRecord{{ID: "run-7", WorkflowType: "drill", Key: "prod v0.7.0→v0.8.0 2026-10-04", Status: wf.StatusFailed, Error: "upgrade: service not answering", StartedAt: time.Now()}}
	op.steps = map[string][]wf.StepResult{"run-7": {{StepName: "prepare", Status: wf.StatusCompleted, DurationNs: int64(3 * time.Second)}, {StepName: "upgrade", Status: wf.StatusFailed, DurationNs: int64(80 * time.Second), Error: "service not answering"}}}
	op.drills = []drills.Drill{sampleDrill()}
	op.mu.Unlock()
	code, body := get(t, ts, "/drills")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"<strong>failed</strong>", "upgrade: service not answering", "<td>prepare</td><td>completed</td><td>3s</td>", "<td>upgrade</td><td>failed</td><td>1m20s</td>"} {
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

// A performance run is started from its own form: a new environment at a
// scale, measured and removed by the perf workflow, shown as a job while
// it runs.
func TestPerfRunStartsTheWorkflowAsAJob(t *testing.T) {
	ts, op := newTestServer(t)
	_, page := get(t, ts, "/")
	if !strings.Contains(page, `action="/perf"`) || !strings.Contains(page, `href="/perf"`) {
		t.Errorf("page should offer a performance run and link the history:\n%s", page)
	}
	if code := post(t, ts, "/perf", url.Values{"name": {"prod"}, "scale": {"large"}}); code != http.StatusConflict {
		t.Errorf("an existing environment: status %d, want 409", code)
	}
	if code := post(t, ts, "/perf", url.Values{"name": {"perf1"}, "scale": {"large"}}); code != http.StatusSeeOther {
		t.Errorf("status %d, want 303", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.perfs) == 1 })
	if op.perfs[0].Env.Name != "perf1" || op.perfs[0].Scale != "large" {
		t.Errorf("perf run = %+v", op.perfs[0])
	}
	_, body := get(t, ts, "/")
	if !strings.Contains(body, "Working: perf") || !strings.Contains(body, "== create perf1 at large") {
		t.Errorf("page should show the run with its log:\n%s", body)
	}
	close(op.release)
}

func TestPerfRunNeedsABuilder(t *testing.T) {
	ts, op := newTestServerWith(t, func(s *Server) { s.UpUnavailable = func() string { return "no store" } })
	_, body := get(t, ts, "/")
	if strings.Contains(body, `action="/perf"`) {
		t.Error("page should not offer a performance run without a builder")
	}
	if code := post(t, ts, "/perf", url.Values{"name": {"perf1"}, "scale": {"small"}}); code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}
	if len(op.perfs) != 0 {
		t.Fatal("the run must not start without a builder")
	}
}

func TestPerfPageIsTheHistoryWithARowPerRun(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/perf")
	if !strings.Contains(body, "No performance runs yet") {
		t.Errorf("empty history:\n%s", body)
	}
	op.mu.Lock()
	op.perfRuns = []perf.Run{{RunID: "run-9", Environment: "perf1", Scale: "large", ServerType: "cx53", MemoryGB: 32, Version: "v0.12.0",
		CreatedAt: time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local), AddSpan: 10 * time.Minute, Customers: 48000, CustomersPerSec: 80.5,
		DaysSpan: 10 * time.Minute, Days: 7, AccountDaysPer12h: 9_000_000, LastDay: 85 * time.Second, LastDayAccounts: 96000,
		Tables: []perf.Table{{Name: "movements", Rows: 44_000_000, Bytes: 6 << 30}, {Name: "customers", Rows: 10_000, Bytes: 3 << 20}}, DatabaseBytes: 8 << 30}}
	op.runs = []wf.RunRecord{{ID: "run-9", WorkflowType: "perf", Key: "perf1 large 2026-10-06", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	code, body := get(t, ts, "/perf")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"Tue 6 Oct 2026 09:00", "perf1", "large", "cx53", "32 GB", "v0.12.0", "48,000", "80.5", "9,000,000", "1m25s", "96,000", `href="/workflows/run-9"`,
		"| 2026-10-06 | v0.12.0 | large | cx53: 32 GB | 48,000 | 80.5 | 7 | 9,000,000 | 1m25s over 96,000 accounts |"} {
		if !strings.Contains(body, want) {
			t.Errorf("perf page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<code>movements</code>") {
		t.Error("the history keeps to the figures: the tables are on the run's page")
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

// --- The Workflows tab ----------------------------------------------------------

// The Workflows page is the engine's view of this program: each workflow
// it runs described from its definition (steps, code, diagram), with the
// count of its instances in every state and the instances themselves.
func TestWorkflowsPageDescribesEachDefinitionWithItsInstances(t *testing.T) {
	ts, op := newTestServer(t)
	for _, path := range []string{"/", "/drills", "/about", "/perf"} {
		if _, body := get(t, ts, path); !strings.Contains(body, `href="/workflows"`) {
			t.Errorf("%s has no Workflows tab", path)
		}
	}
	op.mu.Lock()
	op.runs = []wf.RunRecord{
		{ID: "run-7", WorkflowType: "drill", Key: "prod v0.7.0→v0.8.0 2026-10-04", Status: wf.StatusFailed, Error: "upgrade: service not answering", StartedAt: time.Now()},
		{ID: "run-3", WorkflowType: "demo", Key: "demo@2026-09-27T14:00:00Z", Status: wf.StatusCompleted, StartedAt: time.Now().Add(-time.Hour), CompletedAt: time.Now()},
		{ID: "run-2", WorkflowType: "demo", Key: "demo@2026-09-26T14:00:00Z", Status: wf.StatusFailed, Error: "create: out of stock", StartedAt: time.Now().Add(-25 * time.Hour)},
	}
	op.mu.Unlock()
	code, body := get(t, ts, "/workflows")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		`id="demo"`, `id="drill"`, `id="perf"`,
		"A temporary environment", "one instance per environment@expiry",
		`src="/assets/drill-workflow.svg"`, `src="/assets/perf-workflow.svg"`,
		"up -create with the expiry label", `href="https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/drill.go"`,
		`href="/workflows/run-7"`, `href="/workflows/run-3"`, `href="/workflows/run-2"`,
		"service not answering", "out of stock",
		"No instances yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workflows page missing %q:\n%s", want, body)
		}
	}
	// The demo section counts its two instances; the perf section has none.
	demo := body[strings.Index(body, `id="demo"`):strings.Index(body, `id="drill"`)]
	for _, want := range []string{"completed <strong>1</strong>", "failed <strong>1</strong>", "instances <strong>2</strong>"} {
		if !strings.Contains(demo, want) {
			t.Errorf("demo section missing %q:\n%s", want, demo)
		}
	}
	if strings.Contains(demo, "No instances yet") {
		t.Error("the demo section has instances")
	}
	// The engine's own definitions, which this program never runs, are not shown.
	if strings.Contains(body, "daily_update") || strings.Contains(body, `id="release"`) {
		t.Error("only this program's workflows belong on the page")
	}
}

// An instance page lays the recorded steps over its definition: every
// step in order, the ones not reached marked so, each with what it does;
// and the instance is downloadable as d2 with the steps coloured by state.
func TestRunPageShowsEveryDefinitionStepWithItsState(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.runs = []wf.RunRecord{{ID: "run-7", WorkflowType: "drill", Key: "prod v0.7.0→v0.8.0 2026-10-04", Status: wf.StatusFailed, Error: "upgrade: serving v0.7.0, expected v0.8.0", StartedAt: time.Now()}}
	op.steps = map[string][]wf.StepResult{"run-7": {
		{StepName: "prepare", Status: wf.StatusCompleted, DurationNs: int64(3 * time.Second)},
		{StepName: "before", Status: wf.StatusCompleted, DurationNs: int64(time.Second)},
		{StepName: "upgrade", Status: wf.StatusFailed, DurationNs: int64(80 * time.Second), Error: "serving v0.7.0, expected v0.8.0"},
	}}
	op.mu.Unlock()
	code, body := get(t, ts, "/workflows/run-7")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		`href="/workflows#drill"`,
		"<td>1</td><td><strong>prepare</strong>", "a day long enough to upgrade inside", "<td>completed</td><td>3s</td>",
		"<td>3</td><td><strong>upgrade</strong>", "<td>failed</td><td>1m20s</td>",
		"<td>4</td><td><strong>rollback</strong>", "<td>5</td><td><strong>forward</strong>", "not run",
		`href="https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/drill.go"`,
		`href="/workflows/run-7/diagram.d2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "not run"); n != 2 {
		t.Errorf("rollback and forward were not run; %d steps marked", n)
	}
	resp, err := ts.Client().Get(ts.URL + "/workflows/run-7/diagram.d2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	d2, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("diagram: status %d, content type %q", resp.StatusCode, ct)
	}
	for _, want := range []string{"prepare -> before -> upgrade -> rollback -> forward", "upgrade.class: failed", "rollback.class: not_run", "prepare.class: completed"} {
		if !strings.Contains(string(d2), want) {
			t.Errorf("instance d2 missing %q:\n%s", want, d2)
		}
	}
	if code, _ := get(t, ts, "/workflows/nope/diagram.d2"); code != 404 {
		t.Errorf("unknown run's diagram: status %d", code)
	}
}

// With the explorer configured, an instance links to its rows in the
// workflow_steps table.
func TestRunPageLinksItsStepsInTheExplorerWhenConfigured(t *testing.T) {
	ts, op := newTestServerWith(t, func(s *Server) {
		s.Explorer = func(_ context.Context, rawURL string) string { return "<p>explorer " + rawURL + "</p>" }
	})
	op.mu.Lock()
	op.runs = []wf.RunRecord{{ID: "run-7", WorkflowType: "drill", Key: "prod", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	_, body := get(t, ts, "/workflows/run-7")
	if !strings.Contains(body, `href="/internal/explorer/workflow_steps?filter=run_id&amp;value=run-7"`) {
		t.Errorf("run page does not link the explorer:\n%s", body)
	}
	ts2, op2 := newTestServer(t)
	op2.mu.Lock()
	op2.runs = op.runs
	op2.mu.Unlock()
	if _, body := get(t, ts2, "/workflows/run-7"); strings.Contains(body, "/internal/explorer") {
		t.Error("no explorer, no link")
	}
}

// scanRun is a scan of prod with two routes driven up a ladder of 1 and
// 4 and one not scanned, its p95 at the top rung as given.
func scanRun(id string, day int, version string, dashP95, searchP95 time.Duration, skipped string) scenario.Run {
	r := scenario.Run{RunID: id, Environment: "prod", ServerType: "cx33", MemoryGB: 8, Version: version, CreatedAt: time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC),
		Config: scenario.Config{Name: "scan", Ladder: []int{1, 4}, Samples: 10, Customers: 3000}}
	for _, c := range []int{1, 4} {
		r.Probes = append(r.Probes,
			scenario.Probe{Route: "GET /", Method: "GET", Path: "/", Concurrency: c, N: 10, P50: dashP95 / 2, P95: dashP95 * time.Duration(c) / 4, PerSecond: float64(100 / c)},
			scenario.Probe{Route: "POST /customers/search", Method: "POST", Path: "/customers/search", Concurrency: c, N: 10, P50: searchP95 / 2, P95: searchP95 * time.Duration(c) / 4, PerSecond: float64(50 / c)})
	}
	if skipped != "" {
		r.Probes = append(r.Probes, scenario.Probe{Route: "GET /about", Method: "GET", Path: "/about", Skipped: skipped})
	}
	return r
}

func TestScansPageIsTheScansWithTheWorstTheCriticalAndTheTrend(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/perf/scans")
	if !strings.Contains(body, "No scans yet") {
		t.Errorf("empty:\n%s", body)
	}
	op.mu.Lock()
	op.scenarios = []scenario.Run{
		scanRun("scan-2", 2, "v0.54.0", 100*time.Millisecond, 600*time.Millisecond, "failed: 500 Internal Server Error"),
		scanRun("scan-1", 1, "v0.53.0", 100*time.Millisecond, 400*time.Millisecond, ""),
		{RunID: "seven", Environment: "x", Config: scenario.Config{Name: "seven-years"}, CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}
	op.mu.Unlock()
	code, body := get(t, ts, "/perf/scans")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{
		`href="/perf/scans/scan-2"`, `href="/perf/scans/scan-1"`, "2026-10-02", "v0.54.0", "cx33", "3,000",
		// the ten worst, from the latest scan, with the move since the previous
		"600ms", "worse", "&#43;50%",
		// the critical routes
		"over the 500ms budget", "failed: 500 Internal Server Error",
		// the trend: a column per scan, a row per route
		"400ms", "v0.53.0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scans page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "seven-years") {
		t.Errorf("a scenario is not a scan:\n%s", body)
	}
}

func TestScanPageIsOneScanWithEveryRouteWorstFirst(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.scenarios = []scenario.Run{scanRun("scan-2", 2, "v0.54.0", 100*time.Millisecond, 600*time.Millisecond, "needs a path parameter")}
	op.runs = []wf.RunRecord{{ID: "scan-2", WorkflowType: "scan", Key: "prod 2026-10-02", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	code, body := get(t, ts, "/perf/scans/scan-2")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{"prod", "cx33", "8 GB", "v0.54.0", "3,000", "1, 4", `href="/workflows/scan-2"`,
		"/customers/search", "600ms", "300ms", "12.0", "150ms", "needs a path parameter"} {
		if !strings.Contains(body, want) {
			t.Errorf("scan page missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "/customers/search") > strings.Index(body, "GET /</") {
		t.Errorf("worst first:\n%s", body)
	}
	if code, _ := get(t, ts, "/perf/scans/nope"); code != 404 {
		t.Errorf("unknown scan: %d", code)
	}
}

func TestRunAndJournalPagesLinkToTheScan(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.scenarios = []scenario.Run{scanRun("scan-2", 2, "v0.54.0", 100*time.Millisecond, 600*time.Millisecond, "")}
	op.runs = []wf.RunRecord{{ID: "scan-2", WorkflowType: "scan", Key: "prod 2026-10-02", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	for _, path := range []string{"/workflows/scan-2", "/about/journal", "/perf"} {
		if _, body := get(t, ts, path); !strings.Contains(body, `href="/perf/scans/scan-2"`) && !strings.Contains(body, `href="/perf/scans"`) {
			t.Errorf("%s does not reach the scan:\n%s", path, body)
		}
	}
}

// routesRuns are two scales at two versions, as gobank measured its
// routes over each run.
func routesRuns() []perf.Run {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.Local)
	routes := func(health, customers int) []perf.Route {
		return []perf.Route{{Pattern: "GET /v1/health", Count: 1200, P95: time.Duration(health) * time.Millisecond}, {Pattern: "/customers/", Count: 30, P95: time.Duration(customers) * time.Millisecond}}
	}
	return []perf.Run{
		{RunID: "run-4", Environment: "perf-large", Scale: "large", ServerType: "ccx33", MemoryGB: 32, Version: "v0.52.0", CreatedAt: t0.Add(3 * time.Hour), Routes: routes(3, 41)},
		{RunID: "run-3", Environment: "perf-small", Scale: "small", ServerType: "cx23", MemoryGB: 4, Version: "v0.52.0", CreatedAt: t0.Add(2 * time.Hour), Routes: routes(9, 77)},
		{RunID: "run-2", Environment: "perf-large", Scale: "large", ServerType: "ccx33", MemoryGB: 32, Version: "v0.51.0", CreatedAt: t0.Add(time.Hour), Routes: routes(4, 40)},
		{RunID: "run-1", Environment: "perf-small", Scale: "small", ServerType: "cx23", MemoryGB: 4, Version: "v0.51.0", CreatedAt: t0, Routes: routes(8, 70)},
	}
}

// A performance run's page shows what the demo measured of every route
// it served over the run: the count and the p95.
func TestRunPageShowsAPerfRunsRoutes(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.perfRuns = routesRuns()
	op.runs = []wf.RunRecord{{ID: "run-3", WorkflowType: "perf", Key: "perf-small small 2026-10-06", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	_, body := get(t, ts, "/workflows/run-3")
	for _, want := range []string{"<code>GET /v1/health</code></a></td><td>1,200</td><td>9ms</td>", "<code>/customers/</code></a></td><td>30</td><td>77ms</td>", `href="/perf/routes?route=GET%20%2fv1%2fhealth"`} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
}

// A performance run's page shows the data's budget: the database's size,
// then each table biggest first with its rows, its size and its share of
// the whole. A run from before gobank reported sizes shows no table.
func TestRunPageShowsAPerfRunsTableSizes(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.perfRuns = []perf.Run{{RunID: "run-3", Environment: "perf-small", Scale: "small", Version: "v0.56.0", CreatedAt: time.Now(),
		Tables: []perf.Table{{Name: "customers", Rows: 10_000, Bytes: 3 << 20}, {Name: "movements", Rows: 44_000_000, Bytes: 6 << 30}}, DatabaseBytes: 8 << 30}}
	op.runs = []wf.RunRecord{{ID: "run-3", WorkflowType: "perf", Key: "perf-small small 2026-10-06", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	_, body := get(t, ts, "/workflows/run-3")
	for _, want := range []string{"8.0 GB on disk", "<td><code>movements</code></td><td>44,000,000</td><td>6.0 GB</td><td>75%</td>", "<td><code>customers</code></td><td>10,000</td><td>3.0 MB</td><td>0%</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "<code>movements</code>") > strings.Index(body, "<code>customers</code>") {
		t.Error("tables not biggest first")
	}

	op.mu.Lock()
	op.perfRuns[0].Tables, op.perfRuns[0].DatabaseBytes = nil, 0
	op.mu.Unlock()
	if _, body := get(t, ts, "/workflows/run-3"); strings.Contains(body, "Share of database") {
		t.Error("a run without sizes shows no tables")
	}
}

// The routes page follows one route across gobank's versions: a chart of
// its p95 with a line per scale, the version on the x axis, and the same
// figures as a table. The busiest route is shown unless another is
// chosen; every measured route can be chosen.
func TestRoutesPageChartsARoutesP95ByVersionPerScale(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/perf/routes")
	if !strings.Contains(body, "No performance run has measured its routes yet") {
		t.Errorf("empty routes page:\n%s", body)
	}
	op.mu.Lock()
	op.perfRuns = routesRuns()
	op.mu.Unlock()
	code, body := get(t, ts, "/perf/routes")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`<option value="GET /v1/health" selected>`, `<option value="/customers/">`, "<svg", "v0.51.0", "v0.52.0", "small (cx23, 4 GB)", "large (ccx33, 32 GB)",
		`<td>8ms · 1,200</td><td>4ms · 1,200</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("routes page missing %q:\n%s", want, body)
		}
	}
	_, body = get(t, ts, "/perf/routes?route=%2Fcustomers%2F")
	for _, want := range []string{`<option value="/customers/" selected>`, `<td>77ms · 30</td><td>41ms · 30</td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("routes page for /customers/ missing %q:\n%s", want, body)
		}
	}
	_, body = get(t, ts, "/perf")
	if !strings.Contains(body, `href="/perf/routes"`) {
		t.Error("the performance page links the routes over versions")
	}
}

// A load test is started from its own form: a new environment at a
// scale, loaded from a loader beside it and both removed by the load
// workflow, shown as a job while it runs.
func TestLoadTestStartsTheWorkflowAsAJob(t *testing.T) {
	ts, op := newTestServer(t)
	_, page := get(t, ts, "/")
	if !strings.Contains(page, `action="/load"`) {
		t.Errorf("page should offer a load test:\n%s", page)
	}
	if code := post(t, ts, "/load", url.Values{"name": {"prod"}, "scale": {"large"}}); code != http.StatusConflict {
		t.Errorf("an existing environment: status %d, want 409", code)
	}
	if code := post(t, ts, "/load", url.Values{"name": {"load1"}, "scale": {"large"}}); code != http.StatusSeeOther {
		t.Errorf("status %d, want 303", code)
	}
	waitFor(t, func() bool { op.mu.Lock(); defer op.mu.Unlock(); return len(op.loads) == 1 })
	if op.loads[0].Env.Name != "load1" || op.loads[0].Scale != "large" {
		t.Errorf("load test = %+v", op.loads[0])
	}
	_, body := get(t, ts, "/")
	if !strings.Contains(body, "Working: load") {
		t.Errorf("page should show the test with its log:\n%s", body)
	}
	close(op.release)
}

func loadRuns() []load.Run {
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.Local)
	rung := func(users int, login time.Duration) load.Rung {
		return load.Rung{Users: users, Journeys: users * 10, PerSecond: 12.5,
			Screens: []load.Screen{{Name: "login", P95: login}, {Name: "accounts", P95: 300 * time.Millisecond}, {Name: "account", P95: 400 * time.Millisecond}},
			Journey: load.Screen{Name: "journey", P95: login + 700*time.Millisecond}}
	}
	return []load.Run{
		{RunID: "load-3", Environment: "load3", Scale: "large", ServerType: "ccx33", MemoryGB: 32, Version: "v0.57.0", CreatedAt: t0.Add(2 * time.Hour), Customers: 10_000,
			Result: load.Result{Budget: 2 * time.Second, Capacity: 100, Breach: "200 users: login p95 2.6s over 2s", Rungs: []load.Rung{rung(100, 1800*time.Millisecond), rung(200, 2600*time.Millisecond)}}},
		{RunID: "load-2", Environment: "load2", Scale: "small", ServerType: "cx23", MemoryGB: 4, Version: "v0.57.0", CreatedAt: t0.Add(time.Hour), Customers: 10_000,
			Result: load.Result{Budget: 2 * time.Second, Capacity: 20, Breach: "50 users: 3 journeys failed (first: login: 502 Bad Gateway)"}},
		{RunID: "load-1", Environment: "load1", Scale: "small", ServerType: "cx23", MemoryGB: 4, Version: "v0.56.0", CreatedAt: t0, Customers: 10_000,
			Result: load.Result{Budget: 2 * time.Second, Capacity: 10, Breach: "20 users: login p95 2.2s over 2s"}},
	}
}

// The load tests page lists every load test with its capacity and why the
// climb stopped, and charts the capacity against gobank's version, a line
// per scale, with the same as a table.
func TestLoadPageListsTheTestsAndChartsCapacityByVersion(t *testing.T) {
	ts, op := newTestServer(t)
	_, body := get(t, ts, "/perf/load")
	if !strings.Contains(body, "No load tests yet") {
		t.Errorf("empty page:\n%s", body)
	}
	op.mu.Lock()
	op.loadRuns = loadRuns()
	op.mu.Unlock()
	code, body := get(t, ts, "/perf/load")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`href="/workflows/load-3"`, "load3", "ccx33, 32 GB", "v0.57.0", "10,000", "200 users: login p95 2.6s over 2s",
		"<svg", "small (cx23, 4 GB)", "large (ccx33, 32 GB)", "<tr><td>v0.56.0</td><td>10</td><td></td></tr>", "<tr><td>v0.57.0</td><td>20</td><td>100</td></tr>"} {
		if !strings.Contains(body, want) {
			t.Errorf("load page missing %q:\n%s", want, body)
		}
	}
	_, body = get(t, ts, "/perf")
	if !strings.Contains(body, `href="/perf/load"`) {
		t.Error("the performance page links the load tests")
	}
}

// A load test's run page shows the climb: each rung's users, journeys,
// failures, every screen's p95 and the journey's, with the capacity and
// why it stopped.
func TestRunPageShowsALoadTestsRungs(t *testing.T) {
	ts, op := newTestServer(t)
	op.mu.Lock()
	op.loadRuns = loadRuns()
	op.runs = []wf.RunRecord{{ID: "load-3", WorkflowType: "load", Key: "load3 large 2026-10-16", Status: wf.StatusCompleted, StartedAt: time.Now()}}
	op.mu.Unlock()
	_, body := get(t, ts, "/workflows/load-3")
	for _, want := range []string{"Capacity <strong>100 users</strong>", "200 users: login p95 2.6s over 2s", "<th>login p95</th><th>accounts p95</th><th>account p95</th>",
		"<td>100</td><td>1,000</td><td>0</td><td>1.8s</td><td>300ms</td><td>400ms</td><td>2.5s</td><td>12.5</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
}
