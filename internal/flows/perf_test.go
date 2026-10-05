package flows

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
)

// perfOps is the deployer as the perf run sees it: no server until Up
// creates one, which then serves a version; Down removes it.
type perfOps struct {
	server  *deploy.Server
	version string
	ups     []deploy.UpOptions
	downs   int
}

func (o *perfOps) Status(context.Context, deploy.Environment) (deploy.Status, error) {
	st := deploy.Status{Server: o.server, URL: "http://10.0.0.9:1347/"}
	if o.server != nil {
		st.Serving, st.Version = true, o.version
	}
	return st, nil
}

func (o *perfOps) Up(_ context.Context, opts deploy.UpOptions, out io.Writer) error {
	o.ups = append(o.ups, opts)
	typ, _ := deploy.ServerType(opts.Scale)
	o.server = &deploy.Server{Name: "gobank-" + opts.Env.Name, IP: "10.0.0.9", Type: typ, MemoryGB: 32}
	io.WriteString(out, "deployed\n")
	return nil
}

func (o *perfOps) Down(_ context.Context, _ deploy.Environment, out io.Writer) error {
	o.downs++
	o.server = nil
	io.WriteString(out, "done\n")
	return nil
}

// perfConsole is the demo as the perf run drives it: a batch add takes
// batchTime of (fake) wall clock and reports its rate; while running, a
// simulated day passes every dayTime; the pass's rate is what the demo
// would quote.
type perfConsole struct {
	ops       *perfOps
	now       func() time.Time
	batchTime time.Duration
	dayTime   time.Duration

	customers    int
	addUntil     time.Time
	lastBatch    int
	running      bool
	runFrom      time.Time
	dayCount     int
	settings     []string
	starts, stop int
	addCalls     []int
}

func (c *perfConsole) Read(context.Context, string) (drills.Reading, error) {
	now := c.now()
	if c.running {
		for !c.runFrom.Add(c.dayTime).After(now) {
			c.runFrom = c.runFrom.Add(c.dayTime)
			c.dayCount++
		}
	}
	rd := drills.Reading{Version: c.ops.version, About: true, Running: c.running,
		Position:          &drills.Position{Day: dayOf(c.dayCount), DayCount: c.dayCount, Customers: c.customers, Savings: "£1.00", Lending: "£0.50"},
		AccountDaysPer12h: 9_000_000, LastDayDuration: 85 * time.Second, LastDayAccounts: 2 * c.customers}
	if now.Before(c.addUntil) {
		rd.Adding = true
	} else if c.lastBatch > 0 {
		rd.CustomersPerSec = float64(c.lastBatch) / c.batchTime.Seconds()
	}
	return rd, nil
}

func (c *perfConsole) SetSettings(_ context.Context, _ string, dayLength time.Duration, maxCustomers int) error {
	c.settings = append(c.settings, fmt.Sprintf("%s %d", dayLength, maxCustomers))
	return nil
}

func (c *perfConsole) AddCustomers(_ context.Context, _ string, n int) error {
	c.addCalls = append(c.addCalls, n)
	c.customers += n
	c.lastBatch = n
	c.addUntil = c.now().Add(c.batchTime)
	return nil
}

func (c *perfConsole) Start(context.Context, string) error {
	c.starts++
	c.running, c.runFrom = true, c.now()
	return nil
}

func (c *perfConsole) Stop(context.Context, string) error {
	c.stop++
	c.running = false
	return nil
}

type perfHarness struct {
	ops   *perfOps
	con   *perfConsole
	runs  *wf.MemStore
	perfs *perf.Store
	now   time.Time
	out   strings.Builder
	flow  *Perf
}

func newPerfHarness(t *testing.T) *perfHarness {
	t.Helper()
	d, err := sql.Open("pglike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store, err := perf.New(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	h := &perfHarness{ops: &perfOps{version: "v0.12.0"}, runs: wf.NewMemStore(), perfs: store, now: start}
	h.con = &perfConsole{ops: h.ops, now: func() time.Time { return h.now }, batchTime: time.Minute, dayTime: 90 * time.Second}
	h.flow = &Perf{Ops: h.ops, Console: h.con, Store: h.runs, Perfs: store,
		Now:   func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error { h.now = h.now.Add(d); return ctx.Err() },
		Poll:  10 * time.Second, AddSpan: 10 * time.Minute, DaysSpan: 10 * time.Minute, Batch: 1000,
	}
	return h
}

func (h *perfHarness) run(scale string) error {
	return h.flow.Run(context.Background(), deploy.Environment{Name: "perf"}, scale, &h.out)
}

// A performance run creates the environment at the scale asked for, sets
// the demo flat out with room for every customer, adds customers for the
// add span and lets days run for the days span, records both rates with
// what they were made of, and removes the server.
func TestPerfCreatesMeasuresRecordsAndRemoves(t *testing.T) {
	h := newPerfHarness(t)
	if err := h.run("large"); err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 1 || !h.ops.ups[0].Create || h.ops.ups[0].Scale != "large" || h.ops.downs != 1 {
		t.Errorf("ups %+v downs %d; want one create at large and one down", h.ops.ups, h.ops.downs)
	}
	if strings.Join(h.con.settings, ",") != "0s 1000000" {
		t.Errorf("settings set %v; want flat out with a million customers' room", h.con.settings)
	}
	// Ten minutes of one-minute batches: ten batches, measured at the
	// demo's own rate for each.
	if len(h.con.addCalls) != 10 || h.con.customers != 10_000 {
		t.Errorf("add calls %v, customers %d; want ten batches of 1000", h.con.addCalls, h.con.customers)
	}
	if h.con.starts != 1 || h.con.stop != 1 {
		t.Errorf("starts %d stops %d; want the run started for the days span and stopped after", h.con.starts, h.con.stop)
	}

	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if len(runs) != 1 || runs[0].WorkflowType != WorkflowPerf || runs[0].Key != "perf large 2026-09-27" || runs[0].Status != wf.StatusCompleted {
		t.Fatalf("run = %+v", runs)
	}
	steps, _ := h.runs.ListSteps(context.Background(), runs[0].ID)
	done := map[string]wf.RunStatus{}
	for _, s := range steps {
		done[s.StepName] = s.Status
	}
	for _, step := range []string{"create", "add", "days", "remove"} {
		if done[step] != wf.StatusCompleted {
			t.Errorf("step %s = %v", step, done[step])
		}
	}

	list, err := h.perfs.List(context.Background(), 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("records = %v, %v", list, err)
	}
	rec := list[0]
	if rec.RunID != runs[0].ID || rec.Environment != "perf" || rec.Scale != "large" || rec.ServerType != "cx53" || rec.MemoryGB != 32 || rec.Version != "v0.12.0" {
		t.Errorf("where it ran = %+v", rec)
	}
	if rec.AddSpan != 10*time.Minute || rec.Customers != 10_000 || rec.CustomersPerSec < 16 || rec.CustomersPerSec > 17 {
		t.Errorf("add figures = span %s, %d customers at %.1f/s; want 10m, 10000 at 16.7/s (1000 a minute)", rec.AddSpan, rec.Customers, rec.CustomersPerSec)
	}
	if rec.DaysSpan != 10*time.Minute || rec.Days < 6 || rec.Days > 7 || rec.AccountDaysPer12h != 9_000_000 || rec.LastDay != 85*time.Second || rec.LastDayAccounts != 20_000 {
		t.Errorf("days figures = %+v; want 10m, 6 or 7 days at 9,000,000 account days per 12h, last day 85s over 20000 accounts", rec)
	}
	if !strings.Contains(h.out.String(), "customers/s") || !strings.Contains(h.out.String(), "account days") {
		t.Errorf("log should state both rates:\n%s", h.out.String())
	}
}

// A server that already exists is used as it is, so a run that failed
// mid-way resumes on the same box rather than paying for another.
func TestPerfKeepsAnExistingServer(t *testing.T) {
	h := newPerfHarness(t)
	h.ops.server = &deploy.Server{Name: "gobank-perf", IP: "10.0.0.9", Type: "cx23", MemoryGB: 4}
	if err := h.run("small"); err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 0 || h.ops.downs != 1 {
		t.Errorf("ups %d downs %d; want none and one", len(h.ops.ups), h.ops.downs)
	}
	list, _ := h.perfs.List(context.Background(), 1)
	if len(list) != 1 || list[0].ServerType != "cx23" || list[0].MemoryGB != 4 {
		t.Errorf("record should describe the server found: %+v", list)
	}
}

// Without a server and without a serving demo after create there is
// nothing to measure: the run fails at create and leaves nothing behind
// to bill for beyond what create made.
func TestPerfFailsWhenTheDemoDoesNotAnswer(t *testing.T) {
	h := newPerfHarness(t)
	h.ops.version = ""
	if err := h.run("small"); err == nil || !strings.Contains(err.Error(), "not serving") {
		t.Fatalf("err = %v, want not serving", err)
	}
}
