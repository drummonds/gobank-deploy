package flows

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
)

// loadOps is the deployer as the load test sees it: perfOps for the
// target, and loaders made and walked.
type loadOps struct {
	*perfOps
	loaders  []string // env@expiry
	journeys []string // loader -> target
	opts     []deploy.JourneyOptions
	result   load.Result
	fail     error
}

func (o *loadOps) Loader(_ context.Context, env deploy.Environment, expires time.Time, _ io.Writer) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loaders = append(o.loaders, env.Name+"@"+expires.Format(time.RFC3339))
	o.servers[env.Name] = &deploy.Server{Name: "gobank-" + env.Name, IP: "10.0.0.10", PrivateIP: "10.128.1.3", Type: "cx23"}
	return nil
}

func (o *loadOps) Journey(_ context.Context, loader deploy.Environment, target *deploy.Server, opts deploy.JourneyOptions, _ io.Writer) (load.Result, error) {
	o.journeys = append(o.journeys, loader.Name+" -> "+target.Name)
	o.opts = append(o.opts, opts)
	return o.result, o.fail
}

type loadHarness struct {
	*perfHarness
	ops   *loadOps
	loads *load.Store
	flow  *Load
}

func newLoadHarness(t *testing.T) *loadHarness {
	t.Helper()
	ph := newPerfHarness(t)
	d, err := sql.Open("pglike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	loads, err := load.NewStore(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	ops := &loadOps{perfOps: ph.ops, result: load.Result{Budget: 2 * time.Second, Capacity: 50, Breach: "100 users: login p95 2.4s over 2s", Rungs: []load.Rung{{Users: 50}, {Users: 100}}}}
	h := &loadHarness{perfHarness: ph, ops: ops, loads: loads}
	h.flow = &Load{Ops: ops, Console: ph.con, Store: ph.runs, Loads: loads, Now: ph.flow.Now, Sleep: ph.flow.Sleep, Poll: 10 * time.Second, Customers: 2000}
	return h
}

// A load test creates the target at the scale, fills its books, makes a
// loader beside it on the private network, walks the customer journey up
// the ladder from the loader against the target, records the climb with
// where it ran, and removes both boxes. Both boxes carry an expiry, so a
// run that dies does not leave them billing.
func TestLoadCreatesFillsLoadsRecordsAndRemoves(t *testing.T) {
	h := newLoadHarness(t)
	if err := h.flow.Run(context.Background(), deploy.Environment{Name: "load1"}, "large", &h.out); err != nil {
		t.Fatal(err)
	}
	expiry := start.Add(2*6*time.Hour + time.Hour)
	if len(h.ops.ups) != 1 || !h.ops.ups[0].Create || h.ops.ups[0].Scale != "large" || !h.ops.ups[0].Expires.Equal(expiry) {
		t.Errorf("ups = %+v; want the target created at large with an expiry", h.ops.ups)
	}
	if !slices.Equal(h.ops.loaders, []string{"load1-loader@" + expiry.Format(time.RFC3339)}) {
		t.Errorf("loaders = %v", h.ops.loaders)
	}
	if !slices.Equal(h.ops.journeys, []string{"load1-loader -> gobank-load1"}) {
		t.Errorf("journeys = %v", h.ops.journeys)
	}
	if o := h.ops.opts[0]; o.Customers != 2000 || !slices.Equal(o.Ladder, load.Ladder) || o.Reps != 10 || o.Budget != 2*time.Second {
		t.Errorf("journey options = %+v; want the 2000 customers on the books, the default ladder, 10 reps, 2s", o)
	}
	if h.ops.downs != 2 || len(h.ops.servers) != 0 {
		t.Errorf("downs %d servers %v; want the loader and the target removed", h.ops.downs, h.ops.servers)
	}
	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if len(runs) != 1 || runs[0].WorkflowType != WorkflowLoad || runs[0].Key != "load1 large 2026-09-27" || runs[0].Status != wf.StatusCompleted {
		t.Fatalf("runs = %+v", runs)
	}
	steps, _ := h.runs.ListSteps(context.Background(), runs[0].ID)
	var names []string
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	if !slices.Equal(names, []string{"create", "add", "loader", "load", "remove"}) {
		t.Errorf("steps = %v", names)
	}
	rec, err := h.loads.Get(context.Background(), runs[0].ID)
	if err != nil || rec == nil {
		t.Fatalf("record = %v %v", rec, err)
	}
	if rec.Environment != "load1" || rec.Scale != "large" || rec.ServerType != "ccx33" || rec.Version != "v0.12.0" || rec.Customers != 2000 || rec.Capacity != 50 || len(rec.Rungs) != 2 {
		t.Errorf("record = %+v", rec)
	}
	for _, want := range []string{"capacity 50 users", "100 users: login p95 2.4s over 2s"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, h.out.String())
		}
	}
}

// A load test that fails removes both boxes, so nothing bills while it
// sits; the next attempt starts from create.
func TestLoadFailureRemovesBothBoxes(t *testing.T) {
	h := newLoadHarness(t)
	h.ops.fail = errors.New("ssh: connection lost")
	err := h.flow.Run(context.Background(), deploy.Environment{Name: "load1"}, "small", &h.out)
	if err == nil || !strings.Contains(err.Error(), "connection lost") {
		t.Fatalf("err = %v", err)
	}
	if len(h.ops.servers) != 0 {
		t.Errorf("servers left %v; want none", h.ops.servers)
	}
	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if len(runs) != 1 || runs[0].Status != wf.StatusFailed {
		t.Errorf("runs = %+v", runs)
	}
}
