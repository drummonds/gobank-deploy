package flows

import (
	"context"
	"database/sql"
	"fmt"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

// scenarioConsole is the perf fake with a day length: set, the demo
// follows the clock (a day takes the day length and the reading says
// when it ends) instead of going flat out.
type scenarioConsole struct {
	*perfConsole
	dayLengths []time.Duration
}

func (c *scenarioConsole) Read(ctx context.Context, url string) (drills.Reading, error) {
	rd, err := c.perfConsole.Read(ctx, url)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.dayLengths); n > 0 && c.dayLengths[n-1] > 0 {
		rd.DayLength = c.dayLengths[n-1]
		if rd.Running {
			rd.DayEndsIn = c.dayLengths[n-1]
		}
	}
	return rd, err
}

func (c *scenarioConsole) SetDayLength(_ context.Context, _, _ string, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dayLengths = append(c.dayLengths, d)
	if d > 0 {
		c.dayTime = d
	}
	return nil
}

// fakeProber answers every page in a fixed time; the customer search
// lists one customer, so the customer page can be found from it.
type fakeProber struct {
	mu   sync.Mutex
	urls []string
}

func (p *fakeProber) Fetch(_ context.Context, url, _ string) (string, time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, url)
	return "<html>page</html>", 80 * time.Millisecond, nil
}

func (p *fakeProber) Post(_ context.Context, url, _ string, form neturl.Values) (string, time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, url)
	if strings.HasSuffix(url, "/customers/search") && form.Get("name") == "Smith" {
		return `<ul><li><a href="/customers/abc-123">Ann Smith</a></li></ul>`, 120 * time.Millisecond, nil
	}
	return "", 0, fmt.Errorf("unexpected post %s %v", url, form)
}

func (p *fakeProber) count(suffix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, u := range p.urls {
		if strings.HasSuffix(u, suffix) {
			n++
		}
	}
	return n
}

type scenarioHarness struct {
	ops   *perfOps
	con   *scenarioConsole
	prob  *fakeProber
	runs  *wf.MemStore
	store *scenario.Store
	clock sync.Mutex
	now   time.Time
	out   strings.Builder
	flow  *Scenario
}

func newScenarioHarness(t *testing.T) *scenarioHarness {
	t.Helper()
	d, err := sql.Open("pglike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store, err := scenario.New(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	h := &scenarioHarness{ops: &perfOps{version: "v0.49.0", servers: map[string]*deploy.Server{}}, runs: wf.NewMemStore(), store: store, now: start, prob: &fakeProber{}}
	clock := func() time.Time { h.clock.Lock(); defer h.clock.Unlock(); return h.now }
	// Flat out a simulated day passes every ten seconds, one per poll.
	h.con = &scenarioConsole{perfConsole: &perfConsole{ops: h.ops, now: clock, batchTime: time.Minute, dayTime: 10 * time.Second}}
	h.flow = &Scenario{Ops: h.ops, Console: h.con, Prober: h.prob, Store: h.runs, Scenarios: store,
		Now: clock,
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.clock.Lock()
			h.now = h.now.Add(d)
			h.clock.Unlock()
			return ctx.Err()
		},
		Poll: 10 * time.Second, Batch: 1000, Samples: 20,
	}
	return h
}

var sevenYears = scenario.Config{Name: "seven-years", Customers: 1000, Years: 7, DayLength: 24 * time.Hour, Search: "Smith"}

func (h *scenarioHarness) run(cfg scenario.Config, keep bool) (*scenario.Run, error) {
	return h.flow.Run(context.Background(), deploy.Environment{Name: "seven-years"}, "large", cfg, keep, &h.out)
}

func figure(s scenario.Step, label string) string {
	for _, f := range s.Figures {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

// The seven-years scenario creates the environment at the scale, adds
// the customers flat out, runs until the bank's day is seven years on,
// sets the day length to the wall-clock value and waits for the bank to
// follow the clock, probes the staff pages at concurrency 1 and 4, records
// every step with its figures, and removes the server.
func TestSevenYearsScenario(t *testing.T) {
	h := newScenarioHarness(t)
	rec, err := h.run(sevenYears, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 1 || !h.ops.ups[0].Create || h.ops.ups[0].Scale != "large" || h.ops.downs != 1 {
		t.Errorf("ups %+v downs %d; want one create at large and one down", h.ops.ups, h.ops.downs)
	}
	if strings.Join(h.con.settings, ",") != "0s 1000" {
		t.Errorf("settings set %v; want flat out with room for the customers", h.con.settings)
	}
	demo := h.con.demo("http://seven-years/")
	if len(h.con.addCalls) != 1 || demo.customers != 1000 {
		t.Errorf("add calls %v, customers %d; want one batch of 1000", h.con.addCalls, demo.customers)
	}
	if h.con.starts != 1 || h.con.stop != 1 {
		t.Errorf("starts %d stops %d; want the run started for the years and stopped at the end", h.con.starts, h.con.stop)
	}
	if demo.dayCount < 2555 || demo.dayCount > 2558 {
		t.Errorf("days run = %d; want seven years, give or take a poll", demo.dayCount)
	}
	if fmt.Sprint(h.con.dayLengths) != "[24h0m0s]" {
		t.Errorf("day lengths set %v; want 24h once, after the years", h.con.dayLengths)
	}

	// Three routes at two concurrencies, Samples each; the customer page
	// found from the search.
	for _, suffix := range []string{"/", "/customers/search", "/customers/abc-123"} {
		if n := h.prob.count(suffix); n != 40 {
			t.Errorf("%s fetched %d times; want 20 at each of concurrency 1 and 4", suffix, n)
		}
	}

	if rec == nil {
		t.Fatal("no record")
	}
	if rec.Environment != "seven-years" || rec.Scale != "large" || rec.ServerType != "ccx33" || rec.MemoryGB != 32 || rec.Version != "v0.49.0" || rec.Config != sevenYears {
		t.Errorf("where it ran and what was asked = %+v", rec)
	}
	var names []string
	for _, s := range rec.Steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, " ") != "add years wallclock probe" {
		t.Errorf("steps recorded = %v", names)
	}
	add, years, wall := rec.Steps[0], rec.Steps[1], rec.Steps[2]
	if add.Duration != time.Minute || figure(add, "customers") != "1,000" || figure(add, "customers/s") != "16.7" {
		t.Errorf("add = %+v; want a minute, 1,000 customers at 16.7/s", add)
	}
	if years.Duration < 2555*10*time.Second || years.Duration > 2558*10*time.Second || figure(years, "days") == "" || figure(years, "account days / 12h") != "9,000,000" || figure(years, "last day") != "1m25s" || figure(years, "accounts") != "2,000" {
		t.Errorf("years = %+v", years)
	}
	if figure(wall, "last day") != "1m25s" {
		t.Errorf("wallclock = %+v; want the pass at wall clock", wall)
	}
	if len(rec.Probes) != 6 {
		t.Fatalf("probes = %+v; want three routes at two concurrencies", rec.Probes)
	}
	for _, p := range rec.Probes {
		if p.N != 20 || (p.Concurrency != 1 && p.Concurrency != 4) || p.P50 == 0 || p.P95 == 0 || p.Route == "" || !strings.HasPrefix(p.Path, "/") {
			t.Errorf("probe = %+v", p)
		}
	}

	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if len(runs) != 1 || runs[0].WorkflowType != WorkflowScenario || runs[0].Key != "seven-years large 2026-09-27" || runs[0].Status != wf.StatusCompleted {
		t.Fatalf("run = %+v", runs)
	}
	steps, _ := h.runs.ListSteps(context.Background(), runs[0].ID)
	done := map[string]wf.RunStatus{}
	for _, s := range steps {
		done[s.StepName] = s.Status
	}
	for _, step := range []string{"create", "add", "years", "wallclock", "probe", "remove"} {
		if done[step] != wf.StatusCompleted {
			t.Errorf("step %s = %v", step, done[step])
		}
	}
	if rec.RunID != runs[0].ID {
		t.Errorf("record run id %s; want the workflow run's %s", rec.RunID, runs[0].ID)
	}
	stored, err := h.store.Get(context.Background(), rec.RunID)
	if err != nil || stored == nil || len(stored.Steps) != 4 || len(stored.Probes) != 6 {
		t.Errorf("stored = %+v, %v; want the run as returned", stored, err)
	}
	if out := h.out.String(); !strings.Contains(out, "days") || !strings.Contains(out, "p95") {
		t.Errorf("log should state the days run and the probes' p95:\n%s", out)
	}
}

// --keep leaves the server up for the operator to look at: no remove step.
func TestScenarioKeepLeavesTheServer(t *testing.T) {
	h := newScenarioHarness(t)
	if _, err := h.run(sevenYears, true); err != nil {
		t.Fatal(err)
	}
	if h.ops.downs != 0 || len(h.ops.servers) != 1 {
		t.Errorf("downs %d servers %d; want the server kept", h.ops.downs, len(h.ops.servers))
	}
	runs, _ := h.runs.ListRuns(context.Background(), 1)
	steps, _ := h.runs.ListSteps(context.Background(), runs[0].ID)
	for _, s := range steps {
		if s.StepName == "remove" {
			t.Errorf("remove ran: %+v", s)
		}
	}
}

// Without a serving demo after create there is nothing to run: the
// scenario fails at create.
func TestScenarioFailsWhenTheDemoDoesNotAnswer(t *testing.T) {
	h := newScenarioHarness(t)
	h.ops.version = ""
	if _, err := h.run(sevenYears, false); err == nil || !strings.Contains(err.Error(), "not serving") {
		t.Fatalf("err = %v, want not serving", err)
	}
}

// A scenario that fails after its box was created removes the box, as
// the perf run does; with keep the box is left, failed or not, since
// keep is for looking at it.
func TestScenarioFailureRemovesTheBoxUnlessKept(t *testing.T) {
	h := newScenarioHarness(t)
	h.con.failAt = 1000
	_, err := h.run(sevenYears, false)
	if err == nil || !strings.Contains(err.Error(), "undone: create") {
		t.Fatalf("err = %v; want create undone", err)
	}
	if h.ops.downs != 1 || len(h.ops.servers) != 0 {
		t.Errorf("downs %d servers %d; want the box removed on failure", h.ops.downs, len(h.ops.servers))
	}

	kept := newScenarioHarness(t)
	kept.con.failAt = 1000
	_, err = kept.run(sevenYears, true)
	if err == nil || strings.Contains(err.Error(), "undone") {
		t.Fatalf("err = %v; want the failure with nothing undone", err)
	}
	if kept.ops.downs != 0 || len(kept.ops.servers) != 1 {
		t.Errorf("downs %d servers %d; want the box kept", kept.ops.downs, len(kept.ops.servers))
	}
}
