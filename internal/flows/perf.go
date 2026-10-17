package flows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
)

// WorkflowPerf is gobank's performance run on a fresh environment: create
// it at a scale, set the demo flat out, add customers until the books
// hold the target, run days until the bank is the years on or the ceiling
// is reached, read the two rates off about.json, remove the server. The
// instance key is <env> <scale> <date>, so a run that failed resumes the
// same day on the same box rather than paying for another.
const WorkflowPerf wf.WorkflowType = "perf"

const perfSrc = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/perf.go"

// PerfDefinition describes the workflow for the registry and the pages.
var PerfDefinition = wf.Definition{
	Type:        WorkflowPerf,
	Description: "gobank's performance run: an environment created at a scale, the demo set flat out, customers added until the books hold the target, days run until the bank is the years on or the ceiling is reached, the two rates — customers added per second and account days per 12h — read off the demo's about.json with what they were made of and how long each stage took, the server removed. The figures go in gobank's benchmark.md.",
	InstanceKey: "environment scale date",
	Steps: []wf.Step{
		{Name: "create", Description: "up -create at the scale with an expiry of twice the ceiling and an hour (skipped when the server exists); record where the run is: server type, RAM, gobank version. Undone by remove when a later stage fails, so the next attempt starts here", Source: perfSrc},
		{Name: "add", Description: "flat out with room for a million customers; batches of customers until the books hold the target (10,000) or the ceiling (6h) is reached, each at the rate the demo reports", Source: perfSrc},
		{Name: "days", Description: "Run until the bank's day is the years on (7), or the days asked for, or the ceiling (6h) is reached, then Stop; a demo found stopped is pressed Run again at most twice in a row, then the stage fails; read account days per 12h and the last day it is made of", Source: perfSrc},
		{Name: "routes", Description: "the last reading before the box goes: every route the demo served over the run, its count and p95 (gobank v0.51.0 onwards; none before), and every table's rows and size on disk with the database's whole size (gobank with about.json's tables; none before), read off about.json", Source: perfSrc},
		{Name: "remove", Description: "down: delete the server, firewall and hostname", Source: perfSrc},
	},
}

func init() { wf.Definitions = append(wf.Definitions, PerfDefinition) }

// PerfConsole is the demo's console as the run drives it: read at
// about.json, and the forms an operator would press.
type PerfConsole interface {
	Read(ctx context.Context, url string) (drills.Reading, error)
	// The posts are made as the operator, with the environment's API
	// token (gobank story 9.4).
	SetSettings(ctx context.Context, url, token string, dayLength time.Duration, maxCustomers int) error
	AddCustomers(ctx context.Context, url, token string, n int) error
	Start(ctx context.Context, url, token string) error
	Stop(ctx context.Context, url, token string) error
}

// Perf runs the performance workflow.
type Perf struct {
	Ops     Ops         // Status, Up (create) and Down
	Console PerfConsole // the demo
	Store   wf.Store    // the workflow runs
	Perfs   *perf.Store // the runs and their figures

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Poll  time.Duration // how often a stage looks at the demo; default 10s

	Customers    int           // the add stage runs the books up to this many; default 10,000
	Years        int           // the days stage runs the bank this many years on, on its calendar; default 7
	Days         int           // when set, the days stage runs this many days instead of the years: a short run
	Ceiling      time.Duration // the most wall clock either stage runs for; default 6h
	Batch        int           // customers per add; default 1000
	MaxCustomers int           // the room set on the demo; default a million
}

func (p *Perf) customers() int {
	if p.Customers > 0 {
		return p.Customers
	}
	return 10_000
}

func (p *Perf) years() int {
	if p.Years > 0 {
		return p.Years
	}
	return 7
}

func (p *Perf) ceiling() time.Duration {
	return or(p.Ceiling, 6*time.Hour)
}

// expiry is how long the box lives from create before the page's tick
// reaps it: both stages at the ceiling, and an hour for create, remove
// and slack. A run that hangs or an engine that dies still stops billing.
func (p *Perf) expiry() time.Duration {
	return 2*p.ceiling() + time.Hour
}

func (p *Perf) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Perf) sleep(ctx context.Context, t time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, t)
	}
	select {
	case <-time.After(t):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Perf) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return 1000
}

func (p *Perf) maxCustomers() int {
	if p.MaxCustomers > 0 {
		return p.MaxCustomers
	}
	return 1_000_000
}

// Run measures env at scale, logging to out. A run for a key that failed
// before resumes from the failed stage.
func (p *Perf) Run(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	if scale == "" {
		scale = "small"
	}
	return p.run(ctx, env, scale, p.key(env, scale), out)
}

// run is Run for a key already fixed: the date in the key is the day the
// run started, however many hours it then takes.
func (p *Perf) run(ctx context.Context, env deploy.Environment, scale, key string, out io.Writer) error {
	run := &perfRun{p: p, env: env, scale: scale, key: key, out: out}
	remove := func(ctx context.Context) error {
		fmt.Fprintln(out, "== remove")
		return p.Ops.Down(ctx, env, out)
	}
	// A box bills while a failed run sits, so create is undone by the
	// same remove: a failure deletes the box and the next attempt starts
	// from create.
	stages := []wf.Stage{
		{Name: "create", Run: run.create, Undo: remove},
		{Name: "add", Run: run.add},
		{Name: "days", Run: run.days},
		{Name: "routes", Run: run.routes},
		{Name: "remove", Run: remove},
	}
	rec, err := wf.NewRunner(p.Store, nil).RunPipeline(ctx, WorkflowPerf, key, wf.BusinessDateFrom(p.now()), stages)
	if err != nil {
		return err
	}
	if rec == nil {
		fmt.Fprintf(out, "%s already completed\n", key)
	}
	return nil
}

// RunAll measures every scale at once: an environment per scale, named
// <env>-<scale>, each run in parallel on its own server with its log
// lines prefixed by the scale. The keys are fixed before the runs start,
// so a run that crosses midnight is still found. The records come back
// in the order asked for; the error is every run's, joined.
func (p *Perf) RunAll(ctx context.Context, env deploy.Environment, scales []string, out io.Writer) ([]perf.Run, error) {
	errs := make([]error, len(scales))
	keys := make([]string, len(scales))
	var wg sync.WaitGroup
	var mu sync.Mutex // one writer to out at a time
	for i, scale := range scales {
		scaled := deploy.Environment{Name: env.Name + "-" + scale}
		keys[i] = p.key(scaled, scale)
		wg.Go(func() {
			w := &prefixWriter{prefix: "[" + scale + "] ", out: out, mu: &mu}
			errs[i] = p.run(ctx, scaled, scale, keys[i], w)
		})
	}
	wg.Wait()
	var records []perf.Run
	for _, key := range keys {
		run, err := p.Store.FindRunByKey(ctx, WorkflowPerf, key)
		if err != nil || run == nil {
			continue
		}
		if rec, err := p.Perfs.Get(ctx, run.ID); err == nil && rec != nil {
			records = append(records, *rec)
		}
	}
	return records, errors.Join(errs...)
}

// key is the instance key of env's run at scale today.
func (p *Perf) key(env deploy.Environment, scale string) string {
	return fmt.Sprintf("%s %s %s", env.Name, scale, p.now().Format("2006-01-02"))
}

// prefixWriter starts every line it writes with a prefix, so the logs of
// runs made at once can be told apart.
type prefixWriter struct {
	prefix  string
	out     io.Writer
	mu      *sync.Mutex
	midLine bool
}

func (w *prefixWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(b)
	for len(b) > 0 {
		if !w.midLine {
			if _, err := io.WriteString(w.out, w.prefix); err != nil {
				return 0, err
			}
			w.midLine = true
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if _, err := w.out.Write(b); err != nil {
				return 0, err
			}
			return n, nil
		}
		if _, err := w.out.Write(b[:i+1]); err != nil {
			return 0, err
		}
		w.midLine = false
		b = b[i+1:]
	}
	return n, nil
}

// perfRun is one run: the key's instance and its record.
type perfRun struct {
	p     *Perf
	env   deploy.Environment
	scale string
	key   string
	out   io.Writer
	runID string
}

// resolve is the id of the workflow run the pipeline made for the key.
func (r *perfRun) resolve(ctx context.Context) (string, error) {
	if r.runID == "" {
		run, err := r.p.Store.FindRunByKey(ctx, WorkflowPerf, r.key)
		if err != nil || run == nil {
			return "", fmt.Errorf("finding the run for %s: %v", r.key, err)
		}
		r.runID = run.ID
	}
	return r.runID, nil
}

// record is the run's row; nil until create has recorded it.
func (r *perfRun) record(ctx context.Context) (*perf.Run, error) {
	id, err := r.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return r.p.Perfs.Get(ctx, id)
}

// read is the demo as it is now.
func (r *perfRun) read(ctx context.Context) (deploy.Status, drills.Reading, error) {
	return readDemo(ctx, r.p.Ops, r.p.Console, r.env)
}

// readDemo is the demo as it is now: the status's URL and version, and
// the console's reading.
func readDemo(ctx context.Context, ops Ops, console PerfConsole, env deploy.Environment) (deploy.Status, drills.Reading, error) {
	st, err := ops.Status(ctx, env)
	if err != nil {
		return st, drills.Reading{}, err
	}
	if st.Server == nil || !st.Serving || st.Version == "" {
		return st, drills.Reading{}, fmt.Errorf("%s is not serving a known version", env.Name)
	}
	rd, err := console.Read(ctx, st.URL)
	if err != nil {
		return st, rd, fmt.Errorf("reading the demo: %w", err)
	}
	if rd.Version == "" {
		rd.Version = st.Version
	}
	return st, rd, nil
}

// create makes the environment at the scale unless its server exists,
// and records where the run is.
func (r *perfRun) create(ctx context.Context) error {
	fmt.Fprintf(r.out, "== create %s at %s\n", r.env.Name, r.scale)
	st, err := r.p.Ops.Status(ctx, r.env)
	if err != nil {
		return err
	}
	if st.Server != nil {
		fmt.Fprintf(r.out, "server %s exists, keeping it\n", st.Server.Name)
	} else if err := r.p.Ops.Up(ctx, deploy.UpOptions{Env: r.env, Scale: r.scale, Create: true, Expires: r.p.now().Add(r.p.expiry())}, r.out); err != nil {
		return err
	}
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	rec, err := r.record(ctx)
	if err != nil {
		return err
	}
	if rec == nil {
		if err := r.p.Perfs.Create(ctx, perf.Run{RunID: r.runID, Environment: r.env.Name, Scale: r.scale,
			ServerType: st.Server.Type, MemoryGB: st.Server.MemoryGB, Version: rd.Version, CreatedAt: r.p.now()}); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "%s serving %s on %s (%g GB)\n", r.env.Name, rd.Version, st.Server.Type, st.Server.MemoryGB)
	return nil
}

// add sets the demo flat out with room for every customer, then adds
// customers until the books hold the target or the ceiling is reached,
// and records the span and the rate.
func (r *perfRun) add(ctx context.Context) error {
	fmt.Fprintln(r.out, "== add")
	rec, err := r.record(ctx)
	if err != nil {
		return err
	}
	if rec == nil {
		return errors.New("the run was not recorded at create")
	}
	f, err := r.p.fill(ctx, r.env, r.out)
	if err != nil {
		return err
	}
	rec.AddSpan, rec.Customers, rec.CustomersPerSec = f.took, f.customers, f.rate
	return r.p.Perfs.Update(ctx, *rec)
}

// filled is what an add came to: the books before and after, how long it
// took, and the demo's own rate.
type filled struct {
	from, customers int
	took            time.Duration
	rate            float64 // customers added a second
}

// fill stops the demo if it is running, sets it flat out with room for
// every customer, then adds customers a batch at a time until the books
// hold the target or the ceiling is reached, the last batch cut to size.
// The rate is the demo's own, batch by batch: customers over the time the
// demo took to add them, without this program's polling in between.
func (p *Perf) fill(ctx context.Context, env deploy.Environment, out io.Writer) (filled, error) {
	var f filled
	st, rd, err := readDemo(ctx, p.Ops, p.Console, env)
	if err != nil {
		return f, err
	}
	if rd.Running {
		if err := p.Console.Stop(ctx, st.URL, st.APIToken); err != nil {
			return f, fmt.Errorf("stopping the run: %w", err)
		}
	}
	if err := p.Console.SetSettings(ctx, st.URL, st.APIToken, 0, p.maxCustomers()); err != nil {
		return f, fmt.Errorf("setting flat out: %w", err)
	}
	target, ceiling := p.customers(), p.ceiling()
	start := p.now()
	f.from = rd.Position.Customers
	var adding time.Duration
	for rd.Position.Customers < target && p.now().Sub(start) < ceiling {
		n := min(p.batch(), target-rd.Position.Customers)
		if err := p.Console.AddCustomers(ctx, st.URL, st.APIToken, n); err != nil {
			return f, fmt.Errorf("adding customers: %w", err)
		}
		for {
			if err := p.sleep(ctx, or(p.Poll, 10*time.Second)); err != nil {
				return f, err
			}
			if _, rd, err = readDemo(ctx, p.Ops, p.Console, env); err != nil {
				return f, err
			}
			if !rd.Adding {
				break
			}
		}
		if rd.CustomersPerSec > 0 {
			adding += time.Duration(float64(n) / rd.CustomersPerSec * float64(time.Second))
		}
	}
	f.took = p.now().Sub(start)
	f.customers = rd.Position.Customers
	added := f.customers - f.from
	if adding > 0 {
		f.rate = float64(added) / adding.Seconds()
	} else if f.took > 0 {
		f.rate = float64(added) / f.took.Seconds()
	}
	fmt.Fprintf(out, "%d customers added in %s: %.1f customers/s; %d on the books%s\n", added, f.took, f.rate, f.customers, stoppedBy(f.customers < target, target, "customers"))
	return f, nil
}

// days runs the simulation flat out until the bank's day is the years on
// from where it started or the ceiling is reached, give or take a poll,
// reads the pass's rate and stops it.
func (r *perfRun) days(ctx context.Context) error {
	fmt.Fprintln(r.out, "== days")
	rec, err := r.record(ctx)
	if err != nil {
		return err
	}
	if rec == nil {
		return errors.New("the run was not recorded at create")
	}
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	if rd.Position == nil {
		return errors.New("the demo reports no position")
	}
	day, err := time.Parse("2006-01-02", rd.Position.Day)
	if err != nil {
		return fmt.Errorf("the demo's day %q: %w", rd.Position.Day, err)
	}
	until := day.AddDate(r.p.years(), 0, 0)
	if r.p.Days > 0 {
		until = day.AddDate(0, 0, r.p.Days)
	}
	target, ceiling := until.Format("2006-01-02"), r.p.ceiling()
	from, fromDay := rd.Position.DayCount, rd.Position.Day
	press := func() error {
		if err := r.p.Console.Start(ctx, st.URL, st.APIToken); err != nil {
			return fmt.Errorf("starting the run: %w", err)
		}
		return nil
	}
	if !rd.Running {
		if err := press(); err != nil {
			return err
		}
	}
	// A demo seen stopped short of the target is pressed again, at most
	// runRetries times in a row; still stopped, the stage fails rather
	// than polling to the ceiling with the box billing.
	retries := 0
	start := r.p.now()
	for rd.Position.Day < target && r.p.now().Sub(start) < ceiling {
		if err := r.p.sleep(ctx, min(or(r.p.Poll, 10*time.Second), ceiling-r.p.now().Sub(start))); err != nil {
			return err
		}
		if _, rd, err = r.read(ctx); err != nil {
			return err
		}
		if rd.Running || rd.Position.Day >= target {
			retries = 0
			continue
		}
		if retries == runRetries {
			return fmt.Errorf("the demo is not running after Run was pressed %d times (day %s)", retries+1, rd.Position.Day)
		}
		retries++
		fmt.Fprintf(r.out, "not running: pressing Run again (%d of %d)\n", retries, runRetries)
		if err := press(); err != nil {
			return err
		}
	}
	took := r.p.now().Sub(start)
	if err := r.p.Console.Stop(ctx, st.URL, st.APIToken); err != nil {
		return fmt.Errorf("stopping the run: %w", err)
	}
	rec.DaysSpan = took
	rec.Days = rd.Position.DayCount - from
	rec.AccountDaysPer12h = rd.AccountDaysPer12h
	rec.LastDay, rec.LastDayAccounts = rd.LastDayDuration, rd.LastDayAccounts
	fmt.Fprintf(r.out, "%d days from %s to %s in %s: %d account days per 12h; last day %s over %d accounts%s\n",
		rec.Days, fromDay, rd.Position.Day, took, rec.AccountDaysPer12h, rec.LastDay.Round(time.Second), rec.LastDayAccounts, stoppedBy(rd.Position.Day < target, target, ""))
	return r.p.Perfs.Update(ctx, *rec)
}

// routes records what the demo measured of every route it served over
// the run, and its tables' sizes: the last reading before the server
// goes, and with it the figures, which live only in the demo's process
// and on its disk.
func (r *perfRun) routes(ctx context.Context) error {
	fmt.Fprintln(r.out, "== routes")
	rec, err := r.record(ctx)
	if err != nil {
		return err
	}
	if rec == nil {
		return errors.New("the run was not recorded at create")
	}
	_, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	rec.Routes = rd.Routes
	rec.Tables, rec.DatabaseBytes = rd.Tables, rd.DatabaseBytes
	fmt.Fprintf(r.out, "%d routes measured; %d tables sized\n", len(rec.Routes), len(rec.Tables))
	return r.p.Perfs.Update(ctx, *rec)
}

// runRetries is how many times in a row the days stage presses Run again
// on a demo it finds stopped before it gives up.
const runRetries = 2

// stoppedBy is the log's note when a stage stopped short of its target at
// the ceiling; empty when the target was reached.
func stoppedBy(short bool, target any, unit string) string {
	if !short {
		return ""
	}
	if unit != "" {
		unit = " " + unit
	}
	return fmt.Sprintf(" (stopped at the ceiling, short of %v%s)", target, unit)
}
