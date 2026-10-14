package flows

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/probe"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
)

// WorkflowScenario is the scenario workflow's type.
const WorkflowScenario wf.WorkflowType = "scenario"

const scenarioSrc = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/scenario.go"

// ScenarioDefinition describes the workflow for the registry and the pages.
var ScenarioDefinition = wf.Definition{
	Type:        WorkflowScenario,
	Description: "A scenario: a named configuration driven through timed steps on a fresh environment — the customers added flat out, years run flat out, the day length set to wall clock, the staff pages probed from outside — each step recorded with its figures as a page of the journal. The first scenario, seven-years, is the instrument for gobank milestone 1.",
	InstanceKey: "environment scale date",
	Steps: []wf.Step{
		{Name: "create", Description: "up -create at the scale (skipped when the server exists); record where the run is and what was asked", Source: scenarioSrc},
		{Name: "add", Description: "flat out with room for the customers asked for; add them in batches: added, on the books, customers/s over the step's wall clock", Source: scenarioSrc},
		{Name: "years", Description: "Run flat out until the bank's day is the years on from where it started: bank days run from and to, bank days and account days per wall-clock hour, the last day", Source: scenarioSrc},
		{Name: "wallclock", Description: "set the day length to the wall-clock value and wait for the bank to follow the clock; the last day's pass", Source: scenarioSrc},
		{Name: "probe", Description: "the dashboard, the customer search by name and a customer's page, from outside at concurrency 1 and 4: p50 and p95 each; then Stop", Source: scenarioSrc},
		{Name: "remove", Description: "down: delete the server, firewall and hostname (skipped with --keep)", Source: scenarioSrc},
	},
}

func init() { wf.Definitions = append(wf.Definitions, ScenarioDefinition) }

// ScenarioConsole is the demo's console as a scenario drives it: the
// perf run's forms and the day length on its own.
type ScenarioConsole interface {
	PerfConsole
	SetDayLength(ctx context.Context, url, token string, d time.Duration) error
}

// Prober fetches a page as a browser would, as the operator, and says how
// long it took.
type Prober interface {
	Fetch(ctx context.Context, url, token string) (body string, took time.Duration, err error)
	// Post submits a form and returns the page it answers with, the way
	// Fetch does: the customer search takes its name this way, never in
	// a URL.
	Post(ctx context.Context, url, token string, form url.Values) (body string, took time.Duration, err error)
	// Login signs a customer in to the demo at base and returns the
	// session token the customer routes take as a bearer.
	Login(ctx context.Context, base, customerID, password string) (token string, err error)
}

// fetcher fetches one URL and says how long it took: a GET as the
// operator, a POST of the search, a GET as a customer.
type fetcher func(ctx context.Context, url string) (string, time.Duration, error)

// Scenario runs the scenario workflow.
type Scenario struct {
	Ops       Ops             // Status, Up (create) and Down
	Console   ScenarioConsole // the demo
	Prober    Prober          // the staff pages from outside
	Store     wf.Store        // the workflow runs
	Scenarios *scenario.Store // the runs and their figures

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Poll  time.Duration // how often a step looks at the demo; default 10s

	Batch   int   // customers per add; default 1000
	Samples int   // calls per route per rung; default 20
	Ladder  []int // the concurrencies each route is probed at; default 1, 4, 16
}

func (s *Scenario) ladder() []int {
	if len(s.Ladder) > 0 {
		return s.Ladder
	}
	return []int{1, 4, 16}
}

func (s *Scenario) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scenario) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (s *Scenario) poll() time.Duration { return or(s.Poll, 10*time.Second) }

func (s *Scenario) batch() int {
	if s.Batch > 0 {
		return s.Batch
	}
	return 1000
}

func (s *Scenario) samples() int {
	if s.Samples > 0 {
		return s.Samples
	}
	return 20
}

// Run drives one scenario on env at scale and returns its record. A
// server that exists is kept; the run is one workflow instance keyed by
// environment, scale and date, so a run that failed resumes at the step
// it failed on. With keep the server is left up at the end.
func (s *Scenario) Run(ctx context.Context, env deploy.Environment, scale string, cfg scenario.Config, keep bool, out io.Writer) (*scenario.Run, error) {
	if scale == "" {
		scale = "small"
	}
	if cfg.Search == "" {
		cfg.Search = "Jones"
	}
	cfg.Ladder, cfg.Samples = s.ladder(), s.samples()
	key := fmt.Sprintf("%s %s %s", env.Name, scale, s.now().Format("2006-01-02"))
	run := &scenarioRun{s: s, env: env, scale: scale, cfg: cfg, key: key, out: out, wfType: WorkflowScenario}
	stages := []wf.Stage{
		{Name: "create", Run: run.create},
		{Name: "add", Run: run.add},
		{Name: "years", Run: run.years},
		{Name: "wallclock", Run: run.wallclock},
		{Name: "probe", Run: run.probe},
	}
	if !keep {
		stages = append(stages, wf.Stage{Name: "remove", Run: func(ctx context.Context) error {
			fmt.Fprintln(out, "== remove")
			return s.Ops.Down(ctx, env, out)
		}})
	}
	if _, err := wf.NewRunner(s.Store, nil).RunPipeline(ctx, WorkflowScenario, key, wf.BusinessDateFrom(s.now()), stages); err != nil {
		return nil, err
	}
	rec, err := run.record(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%s completed but left no record", key)
	}
	return rec, nil
}

type scenarioRun struct {
	s      *Scenario
	env    deploy.Environment
	scale  string
	cfg    scenario.Config
	key    string
	out    io.Writer
	wfType wf.WorkflowType // the scenario workflow, or the scan
	runID  string
}

// resolve is the id of the workflow run the pipeline made for the key.
func (r *scenarioRun) resolve(ctx context.Context) (string, error) {
	if r.runID == "" {
		run, err := r.s.Store.FindRunByKey(ctx, r.wfType, r.key)
		if err != nil || run == nil {
			return "", fmt.Errorf("finding the run for %s: %v", r.key, err)
		}
		r.runID = run.ID
	}
	return r.runID, nil
}

// record is the run's row; nil until create has recorded it.
func (r *scenarioRun) record(ctx context.Context) (*scenario.Run, error) {
	id, err := r.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return r.s.Scenarios.Get(ctx, id)
}

// recorded is the run's row, which must exist once create has run.
func (r *scenarioRun) recorded(ctx context.Context) (*scenario.Run, error) {
	rec, err := r.record(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errors.New("the run was not recorded at create")
	}
	return rec, nil
}

// read is the demo as it is now: the status's URL, token and version,
// and the console's reading.
func (r *scenarioRun) read(ctx context.Context) (deploy.Status, drills.Reading, error) {
	st, err := r.s.Ops.Status(ctx, r.env)
	if err != nil {
		return st, drills.Reading{}, err
	}
	if st.Server == nil || !st.Serving || st.Version == "" {
		return st, drills.Reading{}, fmt.Errorf("%s is not serving a known version", r.env.Name)
	}
	rd, err := r.s.Console.Read(ctx, st.URL)
	if err != nil {
		return st, rd, fmt.Errorf("reading the demo: %w", err)
	}
	if rd.Version == "" {
		rd.Version = st.Version
	}
	return st, rd, nil
}

// step records a completed step on the run, replacing an earlier record
// of the same step (a step re-run after a failure).
func (r *scenarioRun) step(ctx context.Context, rec *scenario.Run, st scenario.Step) error {
	for i, old := range rec.Steps {
		if old.Name == st.Name {
			rec.Steps[i] = st
			return r.s.Scenarios.Update(ctx, *rec)
		}
	}
	rec.Steps = append(rec.Steps, st)
	return r.s.Scenarios.Update(ctx, *rec)
}

// create makes the environment at the scale unless its server exists,
// and records where the run is and what was asked.
func (r *scenarioRun) create(ctx context.Context) error {
	fmt.Fprintf(r.out, "== create %s at %s\n", r.env.Name, r.scale)
	st, err := r.s.Ops.Status(ctx, r.env)
	if err != nil {
		return err
	}
	if st.Server != nil {
		fmt.Fprintf(r.out, "server %s exists, keeping it\n", st.Server.Name)
	} else if err := r.s.Ops.Up(ctx, deploy.UpOptions{Env: r.env, Scale: r.scale, Create: true}, r.out); err != nil {
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
		if err := r.s.Scenarios.Create(ctx, scenario.Run{RunID: r.runID, Environment: r.env.Name, Scale: r.scale,
			ServerType: st.Server.Type, MemoryGB: st.Server.MemoryGB, Version: rd.Version, CreatedAt: r.s.now(), Config: r.cfg}); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "%s serving %s on %s (%g GB)\n", r.env.Name, rd.Version, st.Server.Type, st.Server.MemoryGB)
	return nil
}

// add sets the demo flat out with room for the customers asked for, then
// adds them a batch at a time. The rate is the demo's own, batch by
// batch, without this program's polling in between.
func (r *scenarioRun) add(ctx context.Context) error {
	fmt.Fprintln(r.out, "== add")
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	if rd.Running {
		if err := r.s.Console.Stop(ctx, st.URL, st.APIToken); err != nil {
			return fmt.Errorf("stopping the run: %w", err)
		}
	}
	if err := r.s.Console.SetSettings(ctx, st.URL, st.APIToken, 0, r.cfg.Customers); err != nil {
		return fmt.Errorf("setting flat out: %w", err)
	}
	started, from := r.s.now(), rd.Position.Customers
	for rd.Position.Customers < from+r.cfg.Customers {
		n := min(r.s.batch(), from+r.cfg.Customers-rd.Position.Customers)
		if err := r.s.Console.AddCustomers(ctx, st.URL, st.APIToken, n); err != nil {
			return fmt.Errorf("adding customers: %w", err)
		}
		for {
			if err := r.s.sleep(ctx, r.s.poll()); err != nil {
				return err
			}
			if _, rd, err = r.read(ctx); err != nil {
				return err
			}
			if !rd.Adding {
				break
			}
		}
	}
	// The rate is the step's own: what it added over its wall clock,
	// polling and all, so the page's figures multiply out. The demo's
	// in-batch rate is the perf run's figure, not this one.
	added, took := rd.Position.Customers-from, r.s.now().Sub(started)
	rate := 0.0
	if took > 0 {
		rate = float64(added) / took.Seconds()
	}
	fmt.Fprintf(r.out, "%d customers added in %s: %.1f customers/s; %d on the books\n", added, scenario.Dur(took), rate, rd.Position.Customers)
	return r.step(ctx, rec, scenario.Step{Name: "add", Started: started, Duration: took, Figures: []scenario.Figure{
		{Label: "added", Value: scenario.Comma(int64(added))},
		{Label: "on the books", Value: scenario.Comma(int64(rd.Position.Customers))},
		{Label: "customers/s", Value: fmt.Sprintf("%.1f", rate)},
	}})
}

// years runs the bank flat out until its day is the configured years on
// from where it started, give or take a poll.
func (r *scenarioRun) years(ctx context.Context) error {
	fmt.Fprintf(r.out, "== years: %d\n", r.cfg.Years)
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
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
	target := day.AddDate(r.cfg.Years, 0, 0).Format("2006-01-02")
	from, fromDay := rd.Position.DayCount, rd.Position.Day
	if !rd.Running {
		if err := r.s.Console.Start(ctx, st.URL, st.APIToken); err != nil {
			return fmt.Errorf("starting the run: %w", err)
		}
	}
	started := r.s.now()
	for rd.Position.Day < target {
		if err := r.s.sleep(ctx, r.s.poll()); err != nil {
			return err
		}
		if _, rd, err = r.read(ctx); err != nil {
			return err
		}
	}
	// Two clocks: bank days run, from and to on the bank's calendar; the
	// rates per hour of the step's wall clock, so days times accounts over
	// the duration is the account days figure.
	days, took := rd.Position.DayCount-from, r.s.now().Sub(started)
	daysPerHour, accountDaysPerHour := 0.0, 0.0
	if hours := took.Hours(); hours > 0 {
		daysPerHour = float64(days) / hours
		accountDaysPerHour = float64(days) * float64(rd.LastDayAccounts) / hours
	}
	fmt.Fprintf(r.out, "%d bank days run from %s to %s in %s: %.0f bank days/h, %s account days/h; last day %s over %d accounts\n",
		days, fromDay, rd.Position.Day, scenario.Dur(took), daysPerHour, scenario.Comma(int64(accountDaysPerHour+0.5)), scenario.Dur(rd.LastDayDuration), rd.LastDayAccounts)
	return r.step(ctx, rec, scenario.Step{Name: "years", Started: started, Duration: took, Figures: []scenario.Figure{
		{Label: "bank days", Value: scenario.Comma(int64(days))},
		{Label: "from", Value: fromDay},
		{Label: "to", Value: rd.Position.Day},
		{Label: "bank days/h", Value: fmt.Sprintf("%.0f", daysPerHour)},
		{Label: "account days/h", Value: scenario.Comma(int64(accountDaysPerHour + 0.5))},
		{Label: "last day", Value: scenario.Dur(rd.LastDayDuration)},
		{Label: "accounts", Value: scenario.Comma(int64(rd.LastDayAccounts))},
	}})
}

// wallclock sets the day length to the configured value and waits for
// the bank to follow the clock: the day in progress finished, the next
// waiting on it.
func (r *scenarioRun) wallclock(ctx context.Context) error {
	fmt.Fprintf(r.out, "== wallclock: day length %s\n", r.cfg.DayLength)
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	st, rd, err := r.read(ctx)
	if err != nil {
		return err
	}
	if err := r.s.Console.SetDayLength(ctx, st.URL, st.APIToken, r.cfg.DayLength); err != nil {
		return fmt.Errorf("setting the day length: %w", err)
	}
	started := r.s.now()
	for {
		if _, rd, err = r.read(ctx); err != nil {
			return err
		}
		if rd.Running && rd.DayEndsIn > 0 {
			break
		}
		if err := r.s.sleep(ctx, r.s.poll()); err != nil {
			return err
		}
	}
	fmt.Fprintf(r.out, "the bank follows the clock after %s; last day %s\n", scenario.Dur(r.s.now().Sub(started)), scenario.Dur(rd.LastDayDuration))
	return r.step(ctx, rec, scenario.Step{Name: "wallclock", Started: started, Duration: r.s.now().Sub(started), Figures: []scenario.Figure{
		{Label: "day length", Value: scenario.Dur(r.cfg.DayLength)},
		{Label: "last day", Value: scenario.Dur(rd.LastDayDuration)},
	}})
}

var customerLink = regexp.MustCompile(`href="(/customers/[^"/?#]+)"`)

// asOperator fetches as the operator, on the environment's API token.
func (r *scenarioRun) asOperator(st deploy.Status) fetcher {
	return func(ctx context.Context, url string) (string, time.Duration, error) {
		return r.s.Prober.Fetch(ctx, url, st.APIToken)
	}
}

// searching posts the customer search with the run's name, as the
// operator: the name travels in the form, never in a URL.
func (r *scenarioRun) searching(st deploy.Status) fetcher {
	form := url.Values{"name": {r.cfg.Search}}
	return func(ctx context.Context, url string) (string, time.Duration, error) {
		return r.s.Prober.Post(ctx, url, st.APIToken, form)
	}
}

// rungs probes one route at every rung of the run's ladder, Samples
// calls each, and returns the probes with the first body seen (the
// search's, to find a customer in).
func (r *scenarioRun) rungs(ctx context.Context, base, route, method, path string, fetch fetcher) ([]scenario.Probe, string, error) {
	var mu sync.Mutex
	var body string
	var probes []scenario.Probe
	for _, c := range r.cfg.Ladder {
		sample, err := probe.Measure(ctx, r.cfg.Samples, c, func(ctx context.Context) (time.Duration, error) {
			b, took, err := fetch(ctx, base+path)
			mu.Lock()
			if body == "" {
				body = b
			}
			mu.Unlock()
			return took, err
		})
		if err != nil {
			return nil, "", fmt.Errorf("%s at concurrency %d: %w", route, c, err)
		}
		probes = append(probes, scenario.Probe{Route: route, Method: method, Path: path, Concurrency: c, N: sample.N, P50: sample.P50, P95: sample.P95, PerSecond: sample.PerSecond})
		fmt.Fprintf(r.out, "%s %s %s at %d: p50 %s p95 %s %.1f/s over %d\n", route, method, path, c, scenario.Dur(sample.P50), scenario.Dur(sample.P95), sample.PerSecond, sample.N)
	}
	return probes, body, nil
}

// probe measures the staff pages from outside at each rung of the
// ladder: the dashboard, the customer search by name, and the first
// customer the search lists. Then the run is stopped.
func (r *scenarioRun) probe(ctx context.Context) error {
	fmt.Fprintln(r.out, "== probe")
	rec, err := r.recorded(ctx)
	if err != nil {
		return err
	}
	st, _, err := r.read(ctx)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(st.URL, "/")
	started := r.s.now()
	var probes []scenario.Probe
	dash, _, err := r.rungs(ctx, base, "dashboard", "GET", "/", r.asOperator(st))
	if err != nil {
		return err
	}
	probes = append(probes, dash...)
	found, body, err := r.rungs(ctx, base, "customer search by name", "POST", "/customers/search", r.searching(st))
	if err != nil {
		return err
	}
	probes = append(probes, found...)
	if m := customerLink.FindStringSubmatch(body); m != nil {
		page, _, err := r.rungs(ctx, base, "customer page", "GET", m[1], r.asOperator(st))
		if err != nil {
			return err
		}
		probes = append(probes, page...)
	} else {
		fmt.Fprintln(r.out, "the search listed no customer: customer page not probed")
	}
	rec.Probes = probes
	if err := r.step(ctx, rec, scenario.Step{Name: "probe", Started: started, Duration: r.s.now().Sub(started), Figures: []scenario.Figure{
		{Label: "routes", Value: fmt.Sprint(len(probes) / len(r.cfg.Ladder))},
		{Label: "ladder", Value: fmt.Sprint(r.cfg.Ladder)},
		{Label: "samples", Value: fmt.Sprint(r.cfg.Samples)},
	}}); err != nil {
		return err
	}
	if err := r.s.Console.Stop(ctx, st.URL, st.APIToken); err != nil {
		return fmt.Errorf("stopping the run: %w", err)
	}
	return nil
}
