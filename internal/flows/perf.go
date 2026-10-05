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
// it at a scale, set the demo flat out, add customers for a span, let days
// run for another, read the two rates off about.json, remove the server.
// The instance key is <env> <scale> <date>, so a run that failed resumes
// the same day on the same box rather than paying for another.
const WorkflowPerf wf.WorkflowType = "perf"

const perfSrc = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/perf.go"

// PerfDefinition describes the workflow for the registry and the pages.
var PerfDefinition = wf.Definition{
	Type:        WorkflowPerf,
	Description: "gobank's performance run: an environment created at a scale, the demo set flat out, customers added for a fixed span and days run for another, the two rates — customers added per second and account days per 12h — read off the demo's about.json with what they were made of, the server removed. The figures go in gobank's benchmark.md.",
	InstanceKey: "environment scale date",
	Steps: []wf.Step{
		{Name: "create", Description: "up -create at the scale (skipped when the server exists); record where the run is: server type, RAM, gobank version", Source: perfSrc},
		{Name: "add", Description: "flat out with room for a million customers; batches of customers for the add span, each at the rate the demo reports", Source: perfSrc},
		{Name: "days", Description: "Run for the days span, then Stop; read account days per 12h and the last day it is made of", Source: perfSrc},
		{Name: "remove", Description: "down: delete the server, firewall and hostname", Source: perfSrc},
	},
}

func init() { wf.Definitions = append(wf.Definitions, PerfDefinition) }

// PerfConsole is the demo's console as the run drives it: read at
// about.json, and the forms an operator would press.
type PerfConsole interface {
	Read(ctx context.Context, url string) (drills.Reading, error)
	SetSettings(ctx context.Context, url string, dayLength time.Duration, maxCustomers int) error
	AddCustomers(ctx context.Context, url string, n int) error
	Start(ctx context.Context, url string) error
	Stop(ctx context.Context, url string) error
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

	AddSpan      time.Duration // how long customers are added for; default 10m
	DaysSpan     time.Duration // how long days run for; default 10m
	Batch        int           // customers per add; default 1000
	MaxCustomers int           // the ceiling set on the demo; default a million
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
	key := p.key(env, scale)
	run := &perfRun{p: p, env: env, scale: scale, key: key, out: out}
	stages := []wf.Stage{
		{Name: "create", Run: run.create},
		{Name: "add", Run: run.add},
		{Name: "days", Run: run.days},
		{Name: "remove", Run: func(ctx context.Context) error {
			fmt.Fprintln(out, "== remove")
			return p.Ops.Down(ctx, env, out)
		}},
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
// lines prefixed by the scale. The records come back in the order asked
// for; the error is every run's, joined.
func (p *Perf) RunAll(ctx context.Context, env deploy.Environment, scales []string, out io.Writer) ([]perf.Run, error) {
	errs := make([]error, len(scales))
	var wg sync.WaitGroup
	var mu sync.Mutex // one writer to out at a time
	for i, scale := range scales {
		wg.Go(func() {
			w := &prefixWriter{prefix: "[" + scale + "] ", out: out, mu: &mu}
			errs[i] = p.Run(ctx, deploy.Environment{Name: env.Name + "-" + scale}, scale, w)
		})
	}
	wg.Wait()
	var records []perf.Run
	for _, scale := range scales {
		run, err := p.Store.FindRunByKey(ctx, WorkflowPerf, p.key(deploy.Environment{Name: env.Name + "-" + scale}, scale))
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
	for len(b) > 0 {
		if !w.midLine {
			if _, err := io.WriteString(w.out, w.prefix); err != nil {
				return 0, err
			}
			w.midLine = true
		}
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			_, err := w.out.Write(b)
			return len(b), err
		}
		if _, err := w.out.Write(b[:i+1]); err != nil {
			return 0, err
		}
		w.midLine = false
		b = b[i+1:]
	}
	return 0, nil
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

// read is the demo as it is now: the status's URL and version, and the
// console's reading.
func (r *perfRun) read(ctx context.Context) (deploy.Status, drills.Reading, error) {
	st, err := r.p.Ops.Status(ctx, r.env)
	if err != nil {
		return st, drills.Reading{}, err
	}
	if st.Server == nil || !st.Serving || st.Version == "" {
		return st, drills.Reading{}, fmt.Errorf("%s is not serving a known version", r.env.Name)
	}
	rd, err := r.p.Console.Read(ctx, st.URL)
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
	} else if err := r.p.Ops.Up(ctx, deploy.UpOptions{Env: r.env, Scale: r.scale, Create: true}, r.out); err != nil {
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
// customers a batch at a time for the add span. The rate is the demo's
// own, batch by batch: customers over the time the demo took to add them,
// without this program's polling in between.
func (r *perfRun) add(ctx context.Context) error {
	fmt.Fprintln(r.out, "== add")
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
	if rd.Running {
		if err := r.p.Console.Stop(ctx, st.URL); err != nil {
			return fmt.Errorf("stopping the run: %w", err)
		}
	}
	if err := r.p.Console.SetSettings(ctx, st.URL, 0, r.p.maxCustomers()); err != nil {
		return fmt.Errorf("setting flat out: %w", err)
	}
	span := or(r.p.AddSpan, 10*time.Minute)
	start, from := r.p.now(), rd.Position.Customers
	var adding time.Duration
	for r.p.now().Sub(start) < span {
		if err := r.p.Console.AddCustomers(ctx, st.URL, r.p.batch()); err != nil {
			return fmt.Errorf("adding customers: %w", err)
		}
		for {
			if err := r.p.sleep(ctx, or(r.p.Poll, 10*time.Second)); err != nil {
				return err
			}
			if _, rd, err = r.read(ctx); err != nil {
				return err
			}
			if !rd.Adding {
				break
			}
		}
		if rd.CustomersPerSec > 0 {
			adding += time.Duration(float64(r.p.batch()) / rd.CustomersPerSec * float64(time.Second))
		}
	}
	rec.AddSpan = span
	rec.Customers = rd.Position.Customers
	added := rec.Customers - from
	if adding > 0 {
		rec.CustomersPerSec = float64(added) / adding.Seconds()
	} else if elapsed := r.p.now().Sub(start); elapsed > 0 {
		rec.CustomersPerSec = float64(added) / elapsed.Seconds()
	}
	fmt.Fprintf(r.out, "%d customers added in %s: %.1f customers/s; %d on the books\n", added, span, rec.CustomersPerSec, rec.Customers)
	return r.p.Perfs.Update(ctx, *rec)
}

// days runs the simulation flat out for the days span, reads the pass's
// rate and stops it.
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
	from := rd.Position.DayCount
	if !rd.Running {
		if err := r.p.Console.Start(ctx, st.URL); err != nil {
			return fmt.Errorf("starting the run: %w", err)
		}
	}
	span := or(r.p.DaysSpan, 10*time.Minute)
	start := r.p.now()
	for r.p.now().Sub(start) < span {
		if err := r.p.sleep(ctx, min(or(r.p.Poll, 10*time.Second), span-r.p.now().Sub(start))); err != nil {
			return err
		}
	}
	if _, rd, err = r.read(ctx); err != nil {
		return err
	}
	if err := r.p.Console.Stop(ctx, st.URL); err != nil {
		return fmt.Errorf("stopping the run: %w", err)
	}
	rec.DaysSpan = span
	rec.Days = rd.Position.DayCount - from
	rec.AccountDaysPer12h = rd.AccountDaysPer12h
	rec.LastDay, rec.LastDayAccounts = rd.LastDayDuration, rd.LastDayAccounts
	fmt.Fprintf(r.out, "%d days in %s: %d account days per 12h; last day %s over %d accounts\n",
		rec.Days, span, rec.AccountDaysPer12h, rec.LastDay.Round(time.Second), rec.LastDayAccounts)
	return r.p.Perfs.Update(ctx, *rec)
}
