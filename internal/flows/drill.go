package flows

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
)

// WorkflowDrill is gobank's upgrade drill (ADR-0003) run on an environment:
// upgrade the running release N to N+1, roll back to N, forward to N+1,
// observing the demo before and after each hop. The instance key is
// <env> N→N+1 <date>, so one drill of a pair per day resumes if it failed.
const WorkflowDrill wf.WorkflowType = "drill"

const drillSrc = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/drill.go"

// DrillDefinition describes the workflow for the registry and the pages.
var DrillDefinition = wf.Definition{
	Type:        WorkflowDrill,
	Description: "gobank's upgrade drill on an environment: upgrade to the newest release, roll back, forward again; the demo is observed before and after every hop and each hop is gated on the version serving, a clean previous stop with a known downtime, an intact handover and the same day as before.",
	InstanceKey: "environment N→N+1 date",
	Steps: []wf.Step{
		{Name: "prepare", Description: "a day long enough to upgrade inside (2h when shorter than 30m), then wait for a day with 10m left", Source: drillSrc},
		{Name: "before", Description: "observe: version, position, newest restart row", Source: drillSrc},
		{Name: "upgrade", Description: "fetch N+1, redeploy, observe and gate", Source: drillSrc},
		{Name: "rollback", Description: "fetch N, redeploy, observe and gate", Source: drillSrc},
		{Name: "forward", Description: "fetch N+1, redeploy, observe and gate", Source: drillSrc},
	},
}

func init() { wf.Definitions = append(wf.Definitions, DrillDefinition) }

// DrillOps is the deployer as the drill drives it: a redeploy from a
// release store whose latest is chosen by Fetch.
type DrillOps interface {
	Status(ctx context.Context, env deploy.Environment) (deploy.Status, error)
	Up(ctx context.Context, o deploy.UpOptions, out io.Writer) error
	Fetch(ctx context.Context, tag string) (string, error)
	Releases(ctx context.Context) (deploy.Releases, error)
}

// Console is the demo's simulation console as the drill uses it: read at
// about.json, and the day length set on the settings page.
type Console interface {
	// Read is the demo at url now; ErrNoAbout when the release has no about.json.
	Read(ctx context.Context, url string) (drills.Reading, error)
	SetDayLength(ctx context.Context, url string, d time.Duration) error
}

// ErrNoAbout: the release serving offers no about.json (gobank before
// it had one), so only its version can be checked.
var ErrNoAbout = errors.New("no about.json on this release")

// Drill runs the drill workflow.
type Drill struct {
	Ops     DrillOps
	Console Console
	Store   wf.Store      // the workflow runs
	Drills  *drills.Store // the drills and their observations

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Poll  time.Duration // how often prepare looks at the day; defaults to a minute

	MinDayLength time.Duration // a day shorter than this is set to DayLength; default 30m
	DayLength    time.Duration // default 2h
	Budget       time.Duration // the day must have this long left before a hop; default 10m
}

func (d *Drill) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Drill) sleep(ctx context.Context, t time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, t)
	}
	select {
	case <-time.After(t):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func or(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Run drills env from the release it serves to the repo's newest, logging
// to out. It refuses when there is nothing newer or no store to roll back
// from; a run for a key that failed before resumes from the failed stage.
func (d *Drill) Run(ctx context.Context, env deploy.Environment, out io.Writer) error {
	st, err := d.Ops.Status(ctx, env)
	if err != nil {
		return err
	}
	if st.Server == nil || !st.Serving || st.Version == "" {
		return fmt.Errorf("%s is not serving a known version", env.Name)
	}
	rel, err := d.Ops.Releases(ctx)
	if err != nil {
		return fmt.Errorf("releases: %w", err)
	}
	if !rel.Fetches {
		return errors.New("the drill needs a release store to roll back from; this host builds from its checkout")
	}
	from, to := st.Version, rel.Repo
	if to == "" {
		to = rel.Available
	}
	if to == "" || to == from {
		return fmt.Errorf("nothing to drill: %s serves %s and nothing newer is released", env.Name, from)
	}
	key := fmt.Sprintf("%s %s→%s %s", env.Name, from, to, d.now().Format("2006-01-02"))
	run := &drillRun{d: d, env: env, from: from, to: to, key: key, out: out}
	stages := []wf.Stage{
		{Name: "prepare", Run: run.prepare},
		{Name: "before", Run: run.before},
		{Name: "upgrade", Run: func(ctx context.Context) error { return run.hop(ctx, "upgrade", to, from, drills.Upgraded) }},
		{Name: "rollback", Run: func(ctx context.Context) error { return run.hop(ctx, "rollback", from, to, drills.RolledBack) }},
		{Name: "forward", Run: func(ctx context.Context) error { return run.hop(ctx, "forward", to, from, drills.Forward) }},
	}
	rec, err := wf.NewRunner(d.Store, nil).RunPipeline(ctx, WorkflowDrill, key, wf.BusinessDateFrom(d.now()), stages)
	if err != nil {
		return err
	}
	if rec == nil {
		fmt.Fprintf(out, "%s already completed\n", key)
	}
	return nil
}

// drillRun is one run of the drill: the key's instance and its record.
type drillRun struct {
	d        *Drill
	env      deploy.Environment
	from, to string
	key      string
	out      io.Writer
	runID    string
}

// record is the drill's row, created on the first stage that needs it,
// against the workflow run the pipeline made for the key.
func (r *drillRun) record(ctx context.Context) (string, error) {
	if r.runID != "" {
		return r.runID, nil
	}
	run, err := r.d.Store.FindRunByKey(ctx, WorkflowDrill, r.key)
	if err != nil || run == nil {
		return "", fmt.Errorf("finding the run for %s: %v", r.key, err)
	}
	existing, err := r.d.Drills.Get(ctx, run.ID)
	if err != nil {
		return "", err
	}
	if existing == nil {
		if err := r.d.Drills.Create(ctx, drills.Drill{RunID: run.ID, Environment: r.env.Name, From: r.from, To: r.to, CreatedAt: r.d.now()}); err != nil {
			return "", err
		}
	}
	r.runID = run.ID
	return run.ID, nil
}

// read is the demo as it is now, by the version Status reports and the
// console's about.json when the release has one.
func (r *drillRun) read(ctx context.Context) (deploy.Status, drills.Reading, error) {
	st, err := r.d.Ops.Status(ctx, r.env)
	if err != nil {
		return st, drills.Reading{}, err
	}
	if !st.Serving {
		return st, drills.Reading{}, fmt.Errorf("%s is not answering", r.env.Name)
	}
	rd, err := r.d.Console.Read(ctx, st.URL)
	switch {
	case errors.Is(err, ErrNoAbout):
		fmt.Fprintf(r.out, "%s has no about.json: only its version can be checked\n", st.Version)
		rd = drills.Reading{Version: st.Version}
	case err != nil:
		return st, rd, fmt.Errorf("reading the demo: %w", err)
	}
	if rd.Version == "" {
		rd.Version = st.Version
	}
	return st, rd, nil
}

// prepare is step 1 of the drill: a day long enough for an upgrade to land
// inside it, and enough of the day left for this hop.
func (r *drillRun) prepare(ctx context.Context) error {
	fmt.Fprintf(r.out, "== prepare %s: %s → %s\n", r.env.Name, r.from, r.to)
	if _, err := r.record(ctx); err != nil {
		return err
	}
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	if !rd.About {
		return nil
	}
	if err := r.ensureDayLength(ctx, st.URL, rd); err != nil {
		return err
	}
	budget := or(r.d.Budget, 10*time.Minute)
	for waits := 0; ; waits++ {
		_, rd, err := r.read(ctx)
		if err != nil {
			return err
		}
		if !rd.Running || rd.DayLength == 0 || rd.DayEndsIn >= budget {
			return nil
		}
		if waits == 0 {
			fmt.Fprintf(r.out, "day ends in %s, under the %s budget: waiting for the next day\n", rd.DayEndsIn.Round(time.Second), budget)
		}
		if err := r.d.sleep(ctx, min(rd.DayEndsIn+time.Second, or(r.d.Poll, time.Minute))); err != nil {
			return err
		}
	}
}

// ensureDayLength sets the drill's day length when the demo's is under
// the minimum. Prepare does it so the upgrade lands mid-day; every hop
// does it again, since a release before gobank v0.10.3 forgets the
// setting on restart and the days would race before the next hop.
func (r *drillRun) ensureDayLength(ctx context.Context, url string, rd drills.Reading) error {
	minDay, dayLength := or(r.d.MinDayLength, 30*time.Minute), or(r.d.DayLength, 2*time.Hour)
	if rd.DayLength >= minDay {
		return nil
	}
	fmt.Fprintf(r.out, "day length %s is under %s: setting %s so the upgrade lands mid-day\n", rd.DayLength, minDay, dayLength)
	if err := r.d.Console.SetDayLength(ctx, url, dayLength); err != nil {
		return fmt.Errorf("setting the day length: %w", err)
	}
	return nil
}

func (r *drillRun) before(ctx context.Context) error {
	fmt.Fprintln(r.out, "== before")
	runID, err := r.record(ctx)
	if err != nil {
		return err
	}
	_, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	if rd.Version != r.from {
		return fmt.Errorf("serving %s, expected %s", rd.Version, r.from)
	}
	r.describe(rd)
	return r.d.Drills.Observe(ctx, runID, rd.Observation(drills.Before, r.d.now()))
}

// hop makes version the store's latest and redeploys, then observes the
// demo and gates: serving version, a restart row following previous (or an
// unrecorded process) with a known downtime and an intact handover, and
// the run no further on than a restart takes it: the stop finishes the
// day in progress and the start begins a new one, so one day at each.
func (r *drillRun) hop(ctx context.Context, name, version, previous string, moment drills.Moment) error {
	fmt.Fprintf(r.out, "== %s to %s\n", name, version)
	runID, err := r.record(ctx)
	if err != nil {
		return err
	}
	if _, err := r.d.Ops.Fetch(ctx, version); err != nil {
		return fmt.Errorf("fetch %s: %w", version, err)
	}
	if err := r.d.Ops.Up(ctx, deploy.UpOptions{Env: r.env}, r.out); err != nil {
		return err
	}
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	if rd.About {
		if err := r.ensureDayLength(ctx, st.URL, rd); err != nil {
			return err
		}
	}
	if err := r.d.Drills.Observe(ctx, runID, rd.Observation(moment, r.d.now())); err != nil {
		return err
	}
	r.describe(rd)
	if rd.Version != version {
		return fmt.Errorf("serving %s, expected %s", rd.Version, version)
	}
	if !rd.About {
		return nil
	}
	if rd.Restart == nil {
		return errors.New("no restart row: the demo kept no record of this start")
	}
	rs := rd.Restart
	if rs.PreviousVersion != "" && rs.PreviousVersion != previous {
		return fmt.Errorf("restart row follows %s, expected %s", rs.PreviousVersion, previous)
	}
	if !rs.DowntimeKnown {
		return fmt.Errorf("downtime unknown: %s did not stop cleanly", previous)
	}
	if rs.Intact != nil && !*rs.Intact {
		return errors.New("handover not intact: day or customers differ across the stop")
	}
	rec, err := r.d.Drills.Get(ctx, runID)
	if err != nil {
		return err
	}
	if prev := lastPositionBefore(rec.Observations, moment); prev != nil && rd.Position != nil {
		if n := rs.DayCount - prev.DayCount; n < 0 || n > 1 {
			return fmt.Errorf("the run went on before the stop: day %d when last observed, day %d at the stop; a clean stop finishes only the day in progress", prev.DayCount, rs.DayCount)
		}
		if n := rd.Position.DayCount - rs.DayCount; n < 0 || n > 1 {
			return fmt.Errorf("the run went on after the start: day %d at the start, day %d now; a restart begins only one day", rs.DayCount, rd.Position.DayCount)
		}
	}
	return nil
}

// lastPositionBefore is the position observed at the latest moment before
// m, nil when none was (a release without about.json).
func lastPositionBefore(obs []drills.Observation, m drills.Moment) *drills.Position {
	var pos *drills.Position
	for _, o := range obs { // in moment order
		if o.Moment == m {
			break
		}
		if o.Position != nil {
			pos = o.Position
		}
	}
	return pos
}

func (r *drillRun) describe(rd drills.Reading) {
	fmt.Fprintf(r.out, "serving %s", rd.Version)
	if p := rd.Position; p != nil {
		fmt.Fprintf(r.out, "; day %d (%s), %d customers, savings %s, lending %s", p.DayCount, p.Day, p.Customers, p.Savings, p.Lending)
	}
	if rs := rd.Restart; rs != nil {
		prev := rs.PreviousVersion
		if prev == "" {
			prev = "an unrecorded process"
		}
		fmt.Fprintf(r.out, "; after %s", prev)
		if rs.DowntimeKnown {
			fmt.Fprintf(r.out, ", downtime %s", rs.Downtime.Round(time.Second))
		} else {
			fmt.Fprint(r.out, ", downtime unknown")
		}
		if rs.Intact != nil {
			if *rs.Intact {
				fmt.Fprint(r.out, ", handover intact")
			} else {
				fmt.Fprint(r.out, ", handover NOT intact")
			}
		}
	}
	fmt.Fprintln(r.out)
}
