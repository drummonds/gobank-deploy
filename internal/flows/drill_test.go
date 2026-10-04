package flows

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	_ "git.bytestone.uk/hum3/go-postgres"
	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
)

// drillOps is the deployer as the drill sees it: a server serving one
// version, a store whose latest is what the next up deploys, and the
// repo's newest tag.
type drillOps struct {
	serving  string // version answering; "" is not answering
	latest   string // the store's latest
	repo     string
	fetches  bool
	fetched  []string
	ups      int
	upErr    error  // returned by the next Up, once
	upServes string // what Up leaves serving instead of latest, once
}

func (o *drillOps) Status(context.Context, deploy.Environment) (deploy.Status, error) {
	return deploy.Status{Server: &deploy.Server{Name: "gobank-prod", IP: "10.0.0.7"}, URL: "http://10.0.0.7:1347/", Serving: o.serving != "", Version: o.serving, Available: o.latest}, nil
}

func (o *drillOps) Releases(context.Context) (deploy.Releases, error) {
	return deploy.Releases{Repo: o.repo, Available: o.latest, Fetches: o.fetches}, nil
}

func (o *drillOps) Fetch(_ context.Context, tag string) (string, error) {
	if tag == "" {
		tag = o.repo
	}
	o.fetched = append(o.fetched, tag)
	o.latest = tag
	return tag, nil
}

func (o *drillOps) Up(_ context.Context, _ deploy.UpOptions, out io.Writer) error {
	o.ups++
	if o.upErr != nil {
		err := o.upErr
		o.upErr = nil
		return err
	}
	io.WriteString(out, "systemctl restart\n")
	o.serving = o.latest
	if o.upServes != "" {
		o.serving, o.upServes = o.upServes, ""
	}
	return nil
}

// console is the demo as the drill reads and sets it. Each Up is a
// restart: the reading after it names the version before as previous.
type console struct {
	ops        *drillOps
	noAbout    map[string]bool // releases without about.json
	dayLength  time.Duration
	dayEnd     time.Time // when the day in progress ends
	running    bool
	day        string
	dayCount   int
	unclean    bool // the next restart row has no downtime
	turnOnUp   int  // the day turns once this many ups have happened; 0 never
	previous   string
	lastServed string
	seenUps    int
	reads      int
	setDay     []time.Duration
	now        func() time.Time
}

func (c *console) Read(ctx context.Context, url string) (drills.Reading, error) {
	c.reads++
	v := c.ops.serving
	if c.ops.ups > c.seenUps {
		c.previous = c.lastServed
		c.seenUps = c.ops.ups
	}
	c.lastServed = v
	if c.noAbout[v] {
		return drills.Reading{}, ErrNoAbout
	}
	if c.turnOnUp > 0 && c.ops.ups >= c.turnOnUp {
		c.day, c.dayCount = "2026-03-02", 60
	}
	rd := drills.Reading{Version: v, About: true, Running: c.running, DayLength: c.dayLength,
		Position: &drills.Position{Day: c.day, DayCount: c.dayCount, Customers: 1200, Savings: "£1.00", Lending: "£0.50"}}
	if c.running && c.dayLength > 0 {
		for !c.dayEnd.After(c.now()) { // the day turned: the next one begins
			c.dayEnd = c.dayEnd.Add(c.dayLength)
			c.dayCount++
			c.day = "2026-03-02"
		}
		rd.DayEndsIn = c.dayEnd.Sub(c.now())
	}
	if c.ops.ups > 0 {
		intact := true
		rd.Restart = &drills.Restart{PreviousVersion: c.previous, Downtime: 40 * time.Second, DowntimeKnown: !c.unclean, Intact: &intact}
		if c.noAbout[c.previous] {
			rd.Restart.PreviousVersion, rd.Restart.Intact = "", nil
		}
	}
	return rd, nil
}

func (c *console) SetDayLength(_ context.Context, _ string, d time.Duration) error {
	c.setDay = append(c.setDay, d)
	c.dayLength = d
	c.dayEnd = c.now().Add(d)
	return nil
}

type drillHarness struct {
	ops    *drillOps
	con    *console
	runs   *wf.MemStore
	drills *drills.Store
	now    time.Time
	out    strings.Builder
	drill  *Drill
	env    deploy.Environment
	slept  time.Duration
}

func newDrillHarness(t *testing.T) *drillHarness {
	t.Helper()
	d, err := sql.Open("pglike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	store, err := drills.New(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	h := &drillHarness{ops: &drillOps{serving: "v1", latest: "v1", repo: "v2", fetches: true}, runs: wf.NewMemStore(), drills: store, now: start}
	h.con = &console{ops: h.ops, dayLength: 2 * time.Hour, dayEnd: start.Add(time.Hour), running: true, day: "2026-03-01", dayCount: 59, noAbout: map[string]bool{}, now: func() time.Time { return h.now }}
	h.drill = &Drill{Ops: h.ops, Console: h.con, Store: h.runs, Drills: store,
		Now:   func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error { h.now = h.now.Add(d); h.slept += d; return ctx.Err() },
	}
	h.env = deploy.Environment{Name: "prod"}
	return h
}

func (h *drillHarness) run() error { return h.drill.Run(context.Background(), h.env, &h.out) }

func (h *drillHarness) steps(t *testing.T) map[string]wf.RunStatus {
	t.Helper()
	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	steps, _ := h.runs.ListSteps(context.Background(), runs[0].ID)
	out := map[string]wf.RunStatus{}
	for _, s := range steps {
		out[s.StepName] = s.Status
	}
	return out
}

func (h *drillHarness) drillRecord(t *testing.T) drills.Drill {
	t.Helper()
	list, err := h.drills.List(context.Background(), 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("drills = %v, %v", list, err)
	}
	return list[0]
}

func TestDrillUpgradesRollsBackAndGoesForwardRecordingEachHop(t *testing.T) {
	h := newDrillHarness(t)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if got := h.ops.fetched; strings.Join(got, " ") != "v2 v1 v2" || h.ops.ups != 3 || h.ops.serving != "v2" {
		t.Errorf("fetched %v ups %d serving %s", got, h.ops.ups, h.ops.serving)
	}
	runs, _ := h.runs.ListRuns(context.Background(), 1)
	if runs[0].WorkflowType != WorkflowDrill || runs[0].Key != "prod v1→v2 2026-09-27" || runs[0].Status != wf.StatusCompleted {
		t.Errorf("run = %+v", runs[0])
	}
	for _, step := range []string{"prepare", "before", "upgrade", "rollback", "forward"} {
		if h.steps(t)[step] != wf.StatusCompleted {
			t.Errorf("step %s = %v", step, h.steps(t)[step])
		}
	}
	rec := h.drillRecord(t)
	if rec.RunID != runs[0].ID || rec.Environment != "prod" || rec.From != "v1" || rec.To != "v2" {
		t.Errorf("drill = %+v", rec)
	}
	if len(rec.Observations) != 4 {
		t.Fatalf("observations = %+v", rec.Observations)
	}
	want := []struct {
		moment   drills.Moment
		version  string
		previous string
	}{{drills.Before, "v1", ""}, {drills.Upgraded, "v2", "v1"}, {drills.RolledBack, "v1", "v2"}, {drills.Forward, "v2", "v1"}}
	for i, w := range want {
		o := rec.Observations[i]
		if o.Moment != w.moment || o.Version != w.version || o.Position == nil || o.Position.DayCount != 59 {
			t.Errorf("observation %d = %+v", i, o)
		}
		if i > 0 && (o.Restart == nil || o.Restart.PreviousVersion != w.previous || !o.Restart.DowntimeKnown) {
			t.Errorf("observation %d restart = %+v", i, o.Restart)
		}
	}
	for _, want := range []string{"== prepare", "== before", "== upgrade to v2", "systemctl restart", "downtime 40s", "== rollback to v1", "== forward to v2"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("log missing %q:\n%s", want, h.out.String())
		}
	}
	if len(h.con.setDay) != 0 {
		t.Errorf("a 2h day is left alone; set %v", h.con.setDay)
	}
}

func TestDrillNeedsANewerReleaseAndAStore(t *testing.T) {
	h := newDrillHarness(t)
	h.ops.repo = "v1"
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "nothing to drill") {
		t.Errorf("err = %v", err)
	}
	h = newDrillHarness(t)
	h.ops.fetches = false
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "release store") {
		t.Errorf("err = %v", err)
	}
	if runs, _ := h.runs.ListRuns(context.Background(), 5); len(runs) != 0 {
		t.Errorf("a refused drill leaves no run: %+v", runs)
	}
}

// Step 1 of the drill: a day long enough for an upgrade to land inside it.
// A short day is set to two hours; then the drill waits for a day with
// enough left.
func TestDrillPreparesTheDayLengthAndWaitsForRoomInTheDay(t *testing.T) {
	h := newDrillHarness(t)
	h.con.dayLength = 5 * time.Minute
	h.con.dayEnd = start.Add(3 * time.Minute)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if len(h.con.setDay) != 1 || h.con.setDay[0] != 2*time.Hour {
		t.Errorf("day length set %v, want 2h once", h.con.setDay)
	}
	// Then: a 2h day with 3 minutes left is not room enough.
	h = newDrillHarness(t)
	h.con.dayEnd = start.Add(3 * time.Minute)
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if h.slept < 3*time.Minute {
		t.Errorf("slept %v; should have waited for the next day", h.slept)
	}
	if !strings.Contains(h.out.String(), "waiting") {
		t.Errorf("log should say it waits:\n%s", h.out.String())
	}
	// A stopped run has nothing to land inside: no wait.
	h = newDrillHarness(t)
	h.con.running = false
	h.con.dayEnd = start.Add(3 * time.Minute)
	if err := h.run(); err != nil || h.slept != 0 {
		t.Errorf("err %v slept %v", err, h.slept)
	}
}

func TestDrillFailsAHopThatServesTheWrongVersionAndResumesThere(t *testing.T) {
	h := newDrillHarness(t)
	h.ops.upServes = "v1" // the upgrade leaves v1 answering
	err := h.run()
	if err == nil || !strings.Contains(err.Error(), "serving v1") {
		t.Fatalf("err = %v", err)
	}
	steps := h.steps(t)
	if steps["before"] != wf.StatusCompleted || steps["upgrade"] != wf.StatusFailed {
		t.Errorf("steps = %v", steps)
	}
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	if h.steps(t)["forward"] != wf.StatusCompleted || h.ops.ups != 4 {
		t.Errorf("resume: steps %v ups %d", h.steps(t), h.ops.ups)
	}
	if rec := h.drillRecord(t); len(rec.Observations) != 4 || rec.Observations[0].ObservedAt.After(start.Add(time.Minute)) {
		t.Errorf("before is not observed again on resume: %+v", rec.Observations)
	}
}

func TestDrillFailsOnAnUncleanStop(t *testing.T) {
	h := newDrillHarness(t)
	h.con.unclean = true
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("err = %v", err)
	}
}

func TestDrillFailsWhenTheHopLandsOnADayBoundary(t *testing.T) {
	h := newDrillHarness(t)
	h.con.turnOnUp = 1 // the day turns during the upgrade
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "day") {
		t.Errorf("err = %v", err)
	}
	if h.steps(t)["upgrade"] != wf.StatusFailed {
		t.Errorf("steps = %v", h.steps(t))
	}
}

// Rolling back to a release without about.json: the version is all that
// can be checked, and the record that follows it names no previous.
func TestDrillOfAReleaseWithoutAboutJSONChecksTheVersionOnly(t *testing.T) {
	h := newDrillHarness(t)
	h.con.noAbout["v1"] = true
	if err := h.run(); err != nil {
		t.Fatal(err)
	}
	rec := h.drillRecord(t)
	if b := rec.Observations[0]; b.Version != "v1" || b.Position != nil || b.Restart != nil {
		t.Errorf("before = %+v", b)
	}
	if u := rec.Observations[1]; u.Restart == nil || u.Restart.PreviousVersion != "" {
		t.Errorf("upgraded = %+v restart %+v", u, u.Restart)
	}
	if !strings.Contains(h.out.String(), "no about.json") {
		t.Errorf("log should say what could not be checked:\n%s", h.out.String())
	}
}

func TestDrillFailsWhenUpFails(t *testing.T) {
	h := newDrillHarness(t)
	h.ops.upErr = errors.New("ssh: connection refused")
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v", err)
	}
	if h.steps(t)["upgrade"] != wf.StatusFailed {
		t.Errorf("steps = %v", h.steps(t))
	}
}

func TestDrillDefinitionIsRegistered(t *testing.T) {
	for _, d := range wf.Definitions {
		if d.Type == WorkflowDrill && len(d.Steps) == 5 {
			return
		}
	}
	t.Error("drill definition not in the registry")
}
