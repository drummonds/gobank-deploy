package flows

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
)

// WorkflowLoad is gobank's load test: a target at a scale with its books
// filled, a loader beside it on the private network, the customer journey
// walked up the ladder of concurrent users until a screen's p95 is over
// the budget, both boxes removed. The instance key is <env> <scale>
// <date>, so a run that failed resumes the same day.
const WorkflowLoad wf.WorkflowType = "load"

const loadSrc = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/load.go"

// LoadDefinition describes the workflow for the registry and the pages.
var LoadDefinition = wf.Definition{
	Type:        WorkflowLoad,
	Description: "gobank's load test: a target environment created at a scale and its books filled, a loader box made beside it on the private network gobank, the customer journey — login, the accounts screen, one account's screen — walked from the loader by each rung's users ten times each, the customers in sequence through the books so nothing comes from a cache, up the ladder until a screen's p95 is over 2s or a journey fails; the capacity is the last rung that held. Both boxes removed.",
	InstanceKey: "environment scale date",
	Steps: []wf.Step{
		{Name: "create", Description: "up -create the target at the scale with an expiry of twice the ceiling and an hour; record where the test is: server type, RAM, gobank version. Undone by remove when a later stage fails", Source: loadSrc},
		{Name: "add", Description: "the demo stopped and set flat out; batches of customers until the books hold the target (the top rung's users, 500) or the ceiling (6h) is reached — the journeys' customers", Source: loadSrc},
		{Name: "loader", Description: "a box <env>-loader in the target's location (another only when that has no capacity, and the log says so), on the private network: ssh from home only, this program copied onto it, the same expiry", Source: loadSrc},
		{Name: "load", Description: "from the loader, against the target's private address: the ladder of users (1, 2, 5, 10, 20, 50, 100, 200, 500), each walking the journey 10 times, stopped at the first rung whose p95 on a screen is over 2s or where a journey failed; a first rung with no journey completed fails the test", Source: loadSrc},
		{Name: "remove", Description: "down the loader and the target: servers, firewalls and hostname", Source: loadSrc},
	},
}

func init() { wf.Definitions = append(wf.Definitions, LoadDefinition) }

// LoadOps is the deployer as the load test drives it: the target's
// status, up and down, and the loader.
type LoadOps interface {
	Ops
	// Loader makes env's loader in location, the target's.
	Loader(ctx context.Context, env deploy.Environment, location string, expires time.Time, out io.Writer) error
	Journey(ctx context.Context, loader deploy.Environment, target *deploy.Server, o deploy.JourneyOptions, out io.Writer) (load.Result, error)
}

// Load runs the load test workflow.
type Load struct {
	Ops     LoadOps
	Console PerfConsole // the demo, for filling its books
	Store   wf.Store
	Loads   *load.Store

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Poll  time.Duration

	Customers int           // the books' target; default the top rung's users
	Ladder    []int         // default load.Ladder
	Reps      int           // journeys per user per rung; default 10
	Budget    time.Duration // p95 per screen; default 2s
	Ceiling   time.Duration // the most the add or the load runs for; default 6h
}

func (l *Load) ladder() []int {
	if len(l.Ladder) == 0 {
		return load.Ladder
	}
	return l.Ladder
}

// customers is the books' target: enough for every user of the top rung
// to log in as a customer of their own, unless told otherwise.
func (l *Load) customers() int {
	if l.Customers > 0 {
		return l.Customers
	}
	return slices.Max(l.ladder())
}

// filler is the perf workflow's add, for the books.
func (l *Load) filler() *Perf {
	return &Perf{Ops: l.Ops, Console: l.Console, Now: l.Now, Sleep: l.Sleep, Poll: l.Poll, Customers: l.customers(), Ceiling: l.Ceiling}
}

func (l *Load) options(customers int) deploy.JourneyOptions {
	o := deploy.JourneyOptions{Customers: customers, Ladder: l.ladder(), Reps: l.Reps, Budget: l.Budget}
	if o.Reps <= 0 {
		o.Reps = 10
	}
	if o.Budget <= 0 {
		o.Budget = 2 * time.Second
	}
	return o
}

// Run load tests env at scale, logging to out.
func (l *Load) Run(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	if scale == "" {
		scale = "small"
	}
	p := l.filler()
	key := fmt.Sprintf("%s %s %s", env.Name, scale, p.now().Format("2006-01-02"))
	run := &loadRun{l: l, p: p, env: env, loader: deploy.Environment{Name: env.Name + "-loader"}, scale: scale, key: key, out: out}
	removeAll := func(ctx context.Context) error {
		fmt.Fprintln(out, "== remove")
		return errors.Join(l.Ops.Down(ctx, run.loader, out), l.Ops.Down(ctx, env, out))
	}
	stages := []wf.Stage{
		{Name: "create", Run: run.create, Undo: removeAll},
		{Name: "add", Run: run.add},
		{Name: "loader", Run: run.makeLoader},
		{Name: "load", Run: run.load},
		{Name: "remove", Run: removeAll},
	}
	rec, err := wf.NewRunner(l.Store, nil).RunPipeline(ctx, WorkflowLoad, key, wf.BusinessDateFrom(p.now()), stages)
	if err != nil {
		return err
	}
	if rec == nil {
		fmt.Fprintf(out, "%s already completed\n", key)
	}
	return nil
}

// loadRun is one load test: the key's instance and its record.
type loadRun struct {
	l           *Load
	p           *Perf
	env, loader deploy.Environment
	scale, key  string
	out         io.Writer
	runID       string
}

func (r *loadRun) record(ctx context.Context) (*load.Run, error) {
	if r.runID == "" {
		run, err := r.l.Store.FindRunByKey(ctx, WorkflowLoad, r.key)
		if err != nil || run == nil {
			return nil, fmt.Errorf("finding the run for %s: %v", r.key, err)
		}
		r.runID = run.ID
	}
	return r.l.Loads.Get(ctx, r.runID)
}

// recorded is the run's record, an error when create has not made it.
func (r *loadRun) recorded(ctx context.Context) (*load.Run, error) {
	rec, err := r.record(ctx)
	if err == nil && rec == nil {
		err = errors.New("the run was not recorded at create")
	}
	return rec, err
}

func (r *loadRun) create(ctx context.Context) error {
	fmt.Fprintf(r.out, "== create %s at %s\n", r.env.Name, r.scale)
	st, err := r.l.Ops.Status(ctx, r.env)
	if err != nil {
		return err
	}
	if st.Server != nil {
		fmt.Fprintf(r.out, "server %s exists, keeping it\n", st.Server.Name)
	} else if err := r.l.Ops.Up(ctx, deploy.UpOptions{Env: r.env, Scale: r.scale, Create: true, Expires: r.p.now().Add(r.p.expiry())}, r.out); err != nil {
		return err
	}
	st, rd, err := readDemo(ctx, r.l.Ops, r.l.Console, r.env)
	if err != nil {
		return err
	}
	rec, err := r.record(ctx)
	if err != nil {
		return err
	}
	if rec == nil {
		return r.l.Loads.Create(ctx, load.Run{RunID: r.runID, Environment: r.env.Name, Scale: r.scale,
			ServerType: st.Server.Type, MemoryGB: st.Server.MemoryGB, Version: rd.Version, CreatedAt: r.p.now()})
	}
	return nil
}

func (r *loadRun) add(ctx context.Context) error {
	fmt.Fprintln(r.out, "== add")
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	f, err := r.p.fill(ctx, r.env, r.out)
	if err != nil {
		return err
	}
	rec.Customers = f.customers
	return r.l.Loads.Update(ctx, *rec)
}

// makeLoader makes the loader in the target's location, so the load
// crosses only a data centre's network, and with the target's expiry, so
// the two are reaped together.
func (r *loadRun) makeLoader(ctx context.Context) error {
	fmt.Fprintf(r.out, "== loader %s\n", r.loader.Name)
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	st, err := r.l.Ops.Status(ctx, r.env)
	if err != nil {
		return err
	}
	if st.Server == nil {
		return fmt.Errorf("%s has no server", r.env.Name)
	}
	return r.l.Ops.Loader(ctx, r.loader, st.Server.Location, rec.CreatedAt.Add(r.p.expiry()), r.out)
}

func (r *loadRun) load(ctx context.Context) error {
	fmt.Fprintln(r.out, "== load")
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	st, err := r.l.Ops.Status(ctx, r.env)
	if err != nil {
		return err
	}
	if st.Server == nil {
		return fmt.Errorf("%s has no server", r.env.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, r.p.ceiling())
	defer cancel()
	res, err := r.l.Ops.Journey(ctx, r.loader, st.Server, r.l.options(rec.Customers), r.out)
	if err != nil {
		return err
	}
	if len(res.Rungs) == 0 || res.Rungs[0].Journeys == 0 {
		return fmt.Errorf("nothing measured: %s", res.Breach)
	}
	rec.Result = res
	why := res.Breach
	if why == "" {
		why = "the top rung held"
	}
	fmt.Fprintf(r.out, "capacity %d users at p95 %s a screen; stopped: %s\n", res.Capacity, res.Budget, why)
	return r.l.Loads.Update(ctx, *rec)
}
