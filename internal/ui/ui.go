// Package ui is a small polling web page showing each environment's state
// with controls to create, redeploy and delete it. One job runs per
// environment at a time; its log is shown on the page.
package ui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"
	"git.bytestone.uk/hum3/gobank-workflow/diagram"
	"git.bytestone.uk/hum3/lofigui"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
)

//go:embed templates
var templateFS embed.FS

// assetFS holds the component diagrams, rendered by task docs:d2 and
// committed so the module builds anywhere.
//
//go:embed assets
var assetFS embed.FS

// Operator is what the page drives: the environment list and each status
// are read on every render, up and down run as background jobs writing
// their progress to out.
type Operator interface {
	Environments(ctx context.Context) ([]deploy.Environment, error)
	Status(ctx context.Context, env deploy.Environment) (deploy.Status, error)
	Up(ctx context.Context, o deploy.UpOptions, out io.Writer) error
	Down(ctx context.Context, env deploy.Environment, out io.Writer) error
	// Demo runs the temporary-environment workflow for env until env.Expires.
	Demo(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error
	// Runs lists recent workflow runs, most recent first.
	Runs(ctx context.Context, limit int) ([]wf.RunRecord, error)
	// Counts is the number of runs of each workflow type in every state.
	Counts(ctx context.Context) (map[wf.WorkflowType]wf.RunCounts, error)
	// Releases is the newest tag on the repo against what is deployable here.
	Releases(ctx context.Context) (deploy.Releases, error)
	// Fetch puts a release into the store and makes it the next deploy,
	// returning its tag: the repo's newest when tag is "", else that tag
	// (a rollback when it is older).
	Fetch(ctx context.Context, tag string) (string, error)
	// Drill runs gobank's upgrade drill on env: up to the newest release,
	// back, forward, observing the demo at each hop.
	Drill(ctx context.Context, env deploy.Environment, out io.Writer) error
	// Drills lists recent drills with their observations, most recent first.
	Drills(ctx context.Context, limit int) ([]drills.Drill, error)
	// Run is one workflow run with its steps; nil when there is none.
	Run(ctx context.Context, id string) (*wf.RunRecord, []wf.StepResult, error)
	// Perf runs gobank's performance run: a new environment at scale,
	// measured and removed.
	Perf(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error
	// Perfs lists recent performance runs, most recent first.
	Perfs(ctx context.Context, limit int) ([]perf.Run, error)
}

// Ops is the production Operator: a Deployer per job, and the workflows.
type Ops struct {
	DeployerFactory
	Flows  *flows.Demo
	Drill_ *flows.Drill
	Perf_  *flows.Perf
}

func (o Ops) Perf(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	return o.Perf_.Run(ctx, env, scale, out)
}

func (o Ops) Perfs(ctx context.Context, limit int) ([]perf.Run, error) {
	return o.Perf_.Perfs.List(ctx, limit)
}

func (o Ops) Demo(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	return o.Flows.Run(ctx, env, scale, out)
}

func (o Ops) Runs(ctx context.Context, limit int) ([]wf.RunRecord, error) {
	return o.Flows.Store.ListRuns(ctx, limit)
}

func (o Ops) Counts(ctx context.Context) (map[wf.WorkflowType]wf.RunCounts, error) {
	return o.Flows.Store.CountRuns(ctx)
}

func (o Ops) Drill(ctx context.Context, env deploy.Environment, out io.Writer) error {
	return o.Drill_.Run(ctx, env, out)
}

func (o Ops) Drills(ctx context.Context, limit int) ([]drills.Drill, error) {
	return o.Drill_.Drills.List(ctx, limit)
}

func (o Ops) Run(ctx context.Context, id string) (*wf.RunRecord, []wf.StepResult, error) {
	run, err := o.Flows.Store.GetRun(ctx, id)
	if err != nil || run == nil {
		return nil, nil, err
	}
	steps, err := o.Flows.Store.ListSteps(ctx, id)
	return run, steps, err
}

// DeployerFactory adapts deploy.Deployer to Operator: each job gets a
// Deployer whose Out is the job's log.
type DeployerFactory func(out io.Writer) *deploy.Deployer

func (f DeployerFactory) Environments(ctx context.Context) ([]deploy.Environment, error) {
	return f(io.Discard).Environments(ctx)
}

func (f DeployerFactory) Releases(ctx context.Context) (deploy.Releases, error) {
	return f(io.Discard).Releases(ctx)
}

func (f DeployerFactory) Fetch(ctx context.Context, tag string) (string, error) {
	return f(io.Discard).Fetch(ctx, tag)
}

func (f DeployerFactory) Status(ctx context.Context, env deploy.Environment) (deploy.Status, error) {
	return f(io.Discard).Status(ctx, env)
}

func (f DeployerFactory) Up(ctx context.Context, o deploy.UpOptions, out io.Writer) error {
	_, err := f(out).Up(ctx, o)
	return err
}

func (f DeployerFactory) Down(ctx context.Context, env deploy.Environment, out io.Writer) error {
	return f(out).Down(ctx, env)
}

const (
	refreshIdle    = 15 // seconds between polls with nothing running
	refreshWorking = 3
)

// clockHands is where an analogue clock's hands point at t, in degrees
// clockwise from twelve. The page redraws the clock on every poll, so the
// second hand jumping round shows the page is live and how often it polls.
type clockHands struct{ Hour, Minute, Second float64 }

func clockHandsAt(t time.Time) clockHands {
	h, m, sec := t.Clock()
	return clockHands{
		Hour:   float64(h%12)*30 + float64(m)*0.5 + float64(sec)/120,
		Minute: float64(m)*6 + float64(sec)*0.1,
		Second: float64(sec) * 6,
	}
}

type job struct {
	Action  string
	Started time.Time
	cancel  context.CancelFunc

	mu      sync.Mutex
	log     bytes.Buffer
	running bool
	err     error
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.log.Write(p)
}

func (j *job) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running, j.err = false, err
}

// jobView is the job as the template sees it.
type jobView struct {
	Action  string
	Started string
	Running bool
	Err     error
	Log     string
}

func (j *job) view() *jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return &jobView{Action: j.Action, Started: j.Started.Format("15:04:05"), Running: j.running, Err: j.err, Log: j.log.String()}
}

type envView struct {
	Name string
	deploy.Status
	Error   error
	Job     *jobView
	Removed string // when a temporary environment goes, local time; "" for a standing one
	DrillTo string // the release a drill would upgrade to; "" when none is offered
}

// drillTarget is the release gobank's upgrade drill would take st to: the
// repo's newest (or the store's latest), when it differs from what is
// serving and this host deploys from a store it can roll back from.
func drillTarget(st deploy.Status, rel deploy.Releases) string {
	if st.Server == nil || !st.Serving || st.Version == "" || !rel.Fetches {
		return ""
	}
	to := rel.Repo
	if to == "" {
		to = rel.Available
	}
	if to == "" || to == st.Version {
		return ""
	}
	return to
}

// runView is a workflow run as the template sees it.
type runView struct {
	ID      string
	Type    string
	Key     string
	Status  string
	Started string
	Took    string // "" until the run completed or failed
	Error   string
}

func viewRun(rec wf.RunRecord) runView {
	v := runView{ID: rec.ID, Type: string(rec.WorkflowType), Key: rec.Key, Status: string(rec.Status), Started: rec.StartedAt.Local().Format("15:04 Mon 2 Jan"), Error: rec.Error}
	if !rec.CompletedAt.IsZero() {
		v.Took = rec.CompletedAt.Sub(rec.StartedAt).Round(time.Second).String()
	}
	return v
}

// definitionView is a workflow as the Workflows page shows it: what the
// definition says, its steps, and the instances recorded against it.
type definitionView struct {
	Type        string
	Description string
	InstanceKey string
	Steps       []definitionStepView
	Counts      []countView
	Total       int
	Runs        []runView
}

// definitionStepView is one step of a definition; on an instance page
// also the state the instance left it in.
type definitionStepView struct {
	Number      int
	Name        string
	Description string
	Source      string
	Status      string // "not run" when the instance has no record of it
	Duration    string
	Error       string
}

type countView struct {
	Status string
	N      int
}

// viewDefinition lays the runs of d's type over its definition.
func viewDefinition(d wf.Definition, counts wf.RunCounts, runs []wf.RunRecord) definitionView {
	v := definitionView{Type: string(d.Type), Description: d.Description, InstanceKey: d.InstanceKey, Steps: viewDefinitionSteps(d, nil), Total: counts.Total()}
	for _, st := range []wf.RunStatus{wf.StatusPending, wf.StatusRunning, wf.StatusCompleted, wf.StatusFailed} {
		v.Counts = append(v.Counts, countView{Status: string(st), N: counts[st]})
	}
	for _, rec := range runs {
		if rec.WorkflowType == d.Type {
			v.Runs = append(v.Runs, viewRun(rec))
		}
	}
	return v
}

// viewDefinitionSteps is every step of d in order, in the state steps
// (an instance's records, nil for the bare definition) left it.
func viewDefinitionSteps(d wf.Definition, steps []wf.StepResult) []definitionStepView {
	byName := map[string]wf.StepResult{}
	for _, s := range steps {
		byName[s.StepName] = s
	}
	var out []definitionStepView
	for i, st := range d.Steps {
		v := definitionStepView{Number: i + 1, Name: st.Name, Description: st.Description, Source: st.Source}
		if steps != nil {
			v.Status = "not run"
			if s, ok := byName[st.Name]; ok {
				v.Status, v.Duration, v.Error = string(s.Status), time.Duration(s.DurationNs).Round(time.Second).String(), s.Error
			}
		}
		out = append(out, v)
	}
	return out
}

// stepView is a recorded step as the template sees it.
type stepView struct {
	Name     string
	Status   string
	Duration string
	Error    string
}

// drillView is a drill as the template sees it: one row per drill with
// its downtime per hop and the line for the record.
type drillView struct {
	drills.Drill
	Date         string
	Run          *runView   // the workflow run the drill is; nil when the store has lost it
	Steps        []stepView // the run's recorded steps
	Observations []observationView
	Summary      string // the line gobank's drill asks to be recorded
}

type observationView struct {
	Moment   string
	Version  string
	Position *drills.Position
	Previous string // "" when there is no restart row
	Downtime string // "", "unknown" or a duration
	Intact   string // "", "intact" or "NOT intact"
}

func viewDrill(d drills.Drill) drillView {
	v := drillView{Drill: d, Date: d.CreatedAt.Local().Format("Mon 2 Jan 2006 15:04")}
	downtimes := map[drills.Moment]string{}
	lost := false
	for _, o := range d.Observations {
		ov := observationView{Moment: strings.ReplaceAll(string(o.Moment), "_", " "), Version: o.Version, Position: o.Position}
		if rs := o.Restart; rs != nil {
			ov.Previous = rs.PreviousVersion
			if ov.Previous == "" {
				ov.Previous = "unrecorded"
			}
			ov.Downtime = "unknown"
			if rs.DowntimeKnown {
				ov.Downtime = rs.Downtime.Round(time.Second).String()
			}
			if rs.Intact != nil {
				ov.Intact = "intact"
				if !*rs.Intact {
					ov.Intact, lost = "NOT intact", true
				}
			}
			if o.Moment != drills.Before {
				downtimes[o.Moment] = ov.Downtime
			}
		}
		v.Observations = append(v.Observations, ov)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %s → %s", d.CreatedAt.Local().Format("2006-01-02"), d.Environment, d.From, d.To)
	for _, m := range []drills.Moment{drills.Upgraded, drills.RolledBack, drills.Forward} {
		if dt, ok := downtimes[m]; ok {
			fmt.Fprintf(&b, ", %s %s", strings.ReplaceAll(string(m), "_", " "), dt)
		}
	}
	if _, ok := downtimes[drills.RolledBack]; ok {
		b.WriteString(", rollback done")
	}
	if lost {
		b.WriteString(", handover NOT intact")
	} else if len(downtimes) == 3 {
		b.WriteString(", nothing lost")
	}
	v.Summary = b.String()
	return v
}

// Server is the http.Handler for the page.
type Server struct {
	op      Operator
	envs    []deploy.Environment // configured: always listed, provisioned or not
	page    *template.Template   // index.html, with the envs fragment and the clock
	mux     *http.ServeMux
	Version string
	// UpUnavailable, when set and returning a reason, is why this host
	// cannot run up right now (no Go toolchain or gobank checkout, an empty
	// release store): the page then offers status and down only, and
	// create / redeploy requests are refused. Asked on every request, so a
	// store that fills up later is noticed without a restart.
	UpUnavailable func() string
	// Now is the time the page shows on its clock; nil means time.Now.
	Now func() time.Time
	// Explorer renders the database explorer for a request URI under
	// /internal/explorer (go-dbexplorer's Render); nil means no explorer.
	Explorer func(ctx context.Context, rawURL string) string

	mu   sync.Mutex
	jobs map[string]*job // latest job per environment
}

// New builds the page. envs are always listed; environments that exist in
// the cloud project, and ones being created from the page, join them.
func New(op Operator, envs []deploy.Environment) (*Server, error) {
	page, err := template.ParseFS(templateFS, "templates/index.html", "templates/envs.html", "templates/page.html", "templates/about.html", "templates/drills.html", "templates/perf.html", "templates/workflows.html", "templates/run.html", "templates/explorer.html")
	if err != nil {
		return nil, err
	}
	s := &Server{op: op, envs: envs, page: page, jobs: map[string]*job{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /fragment", s.fragment)
	s.mux.HandleFunc("GET /about", func(w http.ResponseWriter, r *http.Request) { s.render(w, "about", map[string]any{}) })
	s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(must(fs.Sub(assetFS, "assets")))))
	s.mux.HandleFunc("POST /env", s.create)
	s.mux.HandleFunc("POST /env/{env}/create", s.action("create"))
	s.mux.HandleFunc("POST /env/{env}/redeploy", s.action("redeploy"))
	s.mux.HandleFunc("POST /env/{env}/down", s.action("down"))
	s.mux.HandleFunc("POST /env/{env}/drill", s.action("drill"))
	s.mux.HandleFunc("POST /env/{env}/cancel", s.cancel)
	s.mux.HandleFunc("POST /fetch", s.fetch)
	s.mux.HandleFunc("GET /drills", s.drills)
	s.mux.HandleFunc("POST /perf", s.perf)
	s.mux.HandleFunc("GET /perf", s.perfs)
	s.mux.HandleFunc("GET /workflows", s.workflows)
	s.mux.HandleFunc("GET /workflows/{id}", s.run)
	s.mux.HandleFunc("GET /workflows/{id}/diagram.d2", s.runDiagram)
	s.mux.HandleFunc("GET /internal/explorer", s.explorer)
	s.mux.HandleFunc("GET /internal/explorer/", s.explorer)
	s.mux.HandleFunc("GET /favicon.ico", lofigui.ServeFavicon)
	s.mux.HandleFunc("GET /assets/bulma.min.css", lofigui.ServeBulma)
	return s, nil
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// environments lists the configured environments first, in their order,
// then any others that exist in the cloud project or have a job here,
// sorted. The error is the list read failing; the rest is still returned.
func (s *Server) environments(ctx context.Context) ([]deploy.Environment, error) {
	envs := slices.Clone(s.envs)
	seen := map[string]bool{}
	for _, e := range envs {
		seen[e.Name] = true
	}
	var extra []deploy.Environment
	add := func(e deploy.Environment) {
		if !seen[e.Name] {
			seen[e.Name] = true
			extra = append(extra, e)
		}
	}
	existing, err := s.op.Environments(ctx)
	for _, e := range existing {
		add(e)
	}
	s.mu.Lock()
	for name := range s.jobs {
		add(deploy.Environment{Name: name})
	}
	s.mu.Unlock()
	slices.SortFunc(extra, func(a, b deploy.Environment) int { return strings.Compare(a.Name, b.Name) })
	return append(envs, extra...), err
}

func (s *Server) environment(ctx context.Context, name string) (deploy.Environment, bool) {
	envs, _ := s.environments(ctx)
	for _, e := range envs {
		if e.Name == name {
			return e, true
		}
	}
	return deploy.Environment{}, false
}

// pageData is what the templates see: every environment's state, the
// recent runs, and how often to poll (fast while a job runs).
func (s *Server) pageData(ctx context.Context) map[string]any {
	refresh := refreshIdle
	var views []envView
	envs, listErr := s.environments(ctx)
	for _, e := range envs {
		v := envView{Name: e.Name}
		if !e.Expires.IsZero() {
			v.Removed = e.Expires.Local().Format("15:04 Mon 2 Jan")
		}
		v.Status, v.Error = s.op.Status(ctx, e)
		s.mu.Lock()
		j := s.jobs[e.Name]
		s.mu.Unlock()
		if j != nil {
			v.Job = j.view()
			if v.Job.Running {
				refresh = refreshWorking
			}
		}
		views = append(views, v)
	}
	var runs []runView
	if recs, err := s.op.Runs(ctx, 20); err == nil {
		for _, rec := range recs {
			runs = append(runs, viewRun(rec))
		}
	}
	releases, relErr := s.op.Releases(ctx)
	for i := range views {
		views[i].DrillTo = drillTarget(views[i].Status, releases)
	}
	return map[string]any{
		"page":          "envs",
		"explorer":      s.Explorer != nil,
		"envs":          views,
		"runs":          runs,
		"releases":      releases,
		"releasesError": relErr,
		"listError":     listErr,
		"refresh":       refresh,
		"clock":         clockHandsAt(s.now()),
		"version":       s.Version,
		"upUnavailable": s.upUnavailable(),
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.page.ExecuteTemplate(w, "index.html", s.pageData(r.Context())); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fragment is what the page polls, and what an htmx action gets back:
// the environment table and the clock, which swaps out of band.
func (s *Server) fragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.writeFragment(w, r, "")
}

func (s *Server) writeFragment(w http.ResponseWriter, r *http.Request, flash string) {
	data := s.pageData(r.Context())
	data["flash"] = flash
	if err := s.page.ExecuteTemplate(w, "envs", data); err != nil {
		return
	}
	data["oob"] = true
	s.page.ExecuteTemplate(w, "clock", data)
}

// fromHtmx is a request made by the page's script, which wants the
// fragment back; a plain form post wants the redirect or error page.
func fromHtmx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// done is how an action ends well: the fragment for htmx, else back to the page.
func (s *Server) done(w http.ResponseWriter, r *http.Request) {
	if fromHtmx(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.writeFragment(w, r, "")
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// fail is how an action is refused: for htmx the fragment with the
// message flashed at the top, under the same status, so it shows in place.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if fromHtmx(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		s.writeFragment(w, r, msg)
		return
	}
	http.Error(w, msg, code)
}

func (s *Server) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Server) upUnavailable() string {
	if s.UpUnavailable == nil {
		return ""
	}
	return s.UpUnavailable()
}

var errBusy = errors.New("a job is already running for this environment")

// envName is what an environment may be called: a DNS label, since it
// becomes the server's hostname.
var envName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// create starts a new environment named on the form; it is listed from
// then on because it has a job, and after that because it has a server.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	if why := s.upUnavailable(); why != "" {
		s.fail(w, r, http.StatusForbidden, "create not available here: "+why)
		return
	}
	name := r.FormValue("name")
	if !envName.MatchString(name) {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf("environment name %q: use lower-case letters, digits and hyphens", name))
		return
	}
	if _, exists := s.environment(r.Context(), name); exists {
		s.fail(w, r, http.StatusConflict, name+": already an environment")
		return
	}
	s.startCreate(w, r, deploy.Environment{Name: name})
}

// perf starts a performance run: a new environment named on the form,
// created at its scale, measured and removed by the perf workflow. Like
// create, it is listed while it has a job and then while it has a server.
func (s *Server) perf(w http.ResponseWriter, r *http.Request) {
	if why := s.upUnavailable(); why != "" {
		s.fail(w, r, http.StatusForbidden, "performance run not available here: "+why)
		return
	}
	name := r.FormValue("name")
	if !envName.MatchString(name) {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf("environment name %q: use lower-case letters, digits and hyphens", name))
		return
	}
	if _, exists := s.environment(r.Context(), name); exists {
		s.fail(w, r, http.StatusConflict, name+": already an environment; a performance run wants a fresh one")
		return
	}
	scale := r.FormValue("scale")
	if scale == "" {
		scale = "small"
	}
	env := deploy.Environment{Name: name}
	run := func(ctx context.Context, out io.Writer) error { return s.op.Perf(ctx, env, scale, out) }
	if err := s.start(env, "perf", run); err != nil {
		s.fail(w, r, http.StatusConflict, fmt.Sprintf("%s: %v", env.Name, err))
		return
	}
	s.done(w, r)
}

// removeAfter is the form's "remove" choice as a duration; zero is keep.
func removeAfter(v string) (time.Duration, error) {
	if v == "" || v == "keep" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("remove after %q: want a duration such as 4h", v)
	}
	return d, nil
}

// startCreate runs up -create for env at the form's scale, or the demo
// workflow when the form asks for removal after a while, and redirects.
func (s *Server) startCreate(w http.ResponseWriter, r *http.Request, env deploy.Environment) {
	scale := r.FormValue("scale")
	if scale == "" {
		scale = "small"
	}
	ttl, err := removeAfter(r.FormValue("remove"))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	action := "create"
	run := func(ctx context.Context, out io.Writer) error {
		return s.op.Up(ctx, deploy.UpOptions{Env: env, Scale: scale, Create: true}, out)
	}
	if ttl > 0 {
		env.Expires = time.Now().Add(ttl).UTC().Truncate(time.Second)
		action = "demo"
		run = func(ctx context.Context, out io.Writer) error { return s.op.Demo(ctx, env, scale, out) }
	}
	if err := s.start(env, action, run); err != nil {
		s.fail(w, r, http.StatusConflict, fmt.Sprintf("%s: %v", env.Name, err))
		return
	}
	s.done(w, r)
}

// start registers and runs a job for env unless one is already running.
func (s *Server) start(env deploy.Environment, action string, run func(ctx context.Context, out io.Writer) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobs[env.Name]; j != nil {
		j.mu.Lock()
		running := j.running
		j.mu.Unlock()
		if running {
			return errBusy
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{Action: action, Started: time.Now(), cancel: cancel, running: true}
	s.jobs[env.Name] = j
	go func() {
		defer cancel()
		j.finish(run(ctx, j))
	}()
	return nil
}

func (s *Server) action(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env, ok := s.environment(r.Context(), r.PathValue("env"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		if why := s.upUnavailable(); why != "" && name != "down" {
			s.fail(w, r, http.StatusForbidden, fmt.Sprintf("%s: %s not available here: %s", env.Name, name, why))
			return
		}
		var run func(ctx context.Context, out io.Writer) error
		switch name {
		case "create":
			s.startCreate(w, r, env)
			return
		case "redeploy":
			run = func(ctx context.Context, out io.Writer) error {
				return s.op.Up(ctx, deploy.UpOptions{Env: env}, out)
			}
		case "down":
			if r.FormValue("confirm") == "" {
				s.fail(w, r, http.StatusBadRequest, "tick the confirmation to delete the server and its database")
				return
			}
			run = func(ctx context.Context, out io.Writer) error {
				return s.op.Down(ctx, env, out)
			}
		case "drill":
			if r.FormValue("confirm") == "" {
				s.fail(w, r, http.StatusBadRequest, "tick the confirmation: the drill restarts the environment three times")
				return
			}
			run = func(ctx context.Context, out io.Writer) error {
				return s.op.Drill(ctx, env, out)
			}
		}
		if err := s.start(env, name, run); err != nil {
			s.fail(w, r, http.StatusConflict, fmt.Sprintf("%s: %v", env.Name, err))
			return
		}
		s.done(w, r)
	}
}

// Reconcile starts the demo workflow for every temporary environment in
// the project that has no job looking after it: after a restart, or when
// a job was cancelled. The workflow resumes from where the server is.
func (s *Server) Reconcile(ctx context.Context) {
	envs, err := s.op.Environments(ctx)
	if err != nil {
		return
	}
	for _, env := range envs {
		if env.Expires.IsZero() {
			continue
		}
		env := env
		_ = s.start(env, "demo", func(ctx context.Context, out io.Writer) error { return s.op.Demo(ctx, env, "", out) })
	}
}

// Run reconciles now and then every interval until ctx ends.
func (s *Server) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	env, ok := s.environment(r.Context(), r.PathValue("env"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	j := s.jobs[env.Name]
	s.mu.Unlock()
	if j != nil {
		j.cancel()
	}
	s.done(w, r)
}

// fetch is gobank's release telling the page its release is built: the
// release is fetched into the store now rather than at the next deploy.
// With ?tag=vX it is an operator choosing the next deploy: that release,
// fetched if the store lacks it — the rollback step of gobank's upgrade
// drill. A failure is an error status, so the release's post_release
// step warns.
func (s *Server) fetch(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("tag")
	tag, err := s.op.Fetch(r.Context(), want)
	if err != nil {
		http.Error(w, "fetch: "+err.Error(), http.StatusBadGateway)
		return
	}
	if want != "" {
		fmt.Fprintf(w, "%s is the next deploy\n", tag)
		return
	}
	fmt.Fprintf(w, "%s is in the store\n", tag)
}

// render writes one of the secondary pages (drills, a run, the explorer)
// in the page layout.
func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	data["version"] = s.Version
	data["explorer"] = s.Explorer != nil
	data["page"] = name
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.page.ExecuteTemplate(w, name+".html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// drills is the history: every drill with its run's state and steps and
// its observations, newest first.
func (s *Server) drills(w http.ResponseWriter, r *http.Request) {
	list, err := s.op.Drills(r.Context(), 50)
	var views []drillView
	for _, d := range list {
		v := viewDrill(d)
		if rec, steps, err := s.op.Run(r.Context(), d.RunID); err == nil && rec != nil {
			rv := viewRun(*rec)
			v.Run, v.Steps = &rv, viewSteps(steps)
		}
		views = append(views, v)
	}
	s.render(w, "drills", map[string]any{"drills": views, "error": err})
}

// perfView is a performance run as the template sees it: the row, with
// the figures formatted, and the line for gobank's benchmark.md.
type perfView struct {
	perf.Run
	Date      string
	Run_      *runView // the workflow run; nil when the store has lost it
	Steps     []stepView
	Memory    string // "32 GB"
	Customers string // grouped
	Rate      string // customers/s to one place; "" before the add span ran
	Accounts  string // account days per 12h, grouped; "" before the days span ran
	LastDay   string // "1m25s over 96,000 accounts"
	Row       string // the markdown row for benchmark.md
}

func viewPerf(r perf.Run) perfView {
	v := perfView{Run: r, Date: r.CreatedAt.Local().Format("Mon 2 Jan 2006 15:04"), Memory: fmt.Sprintf("%g GB", r.MemoryGB), Customers: groupInt(int64(r.Customers))}
	if r.AddSpan > 0 {
		v.Rate = fmt.Sprintf("%.1f", r.CustomersPerSec)
	}
	if r.DaysSpan > 0 {
		v.Accounts = groupInt(r.AccountDaysPer12h)
		v.LastDay = fmt.Sprintf("%s over %s accounts", r.LastDay.Round(time.Second), groupInt(int64(r.LastDayAccounts)))
	}
	v.Row = fmt.Sprintf("| %s | %s | %s | %s: %s | %s | %s | %d | %s | %s |",
		r.CreatedAt.Format("2006-01-02"), r.Version, r.Scale, r.ServerType, v.Memory, v.Customers, v.Rate, r.Days, v.Accounts, v.LastDay)
	return v
}

// BenchmarkRow is the run's row for gobank's benchmark.md, as the
// Performance page shows it and the command line prints it.
func BenchmarkRow(r perf.Run) string { return viewPerf(r).Row }

// groupInt writes n with thousands separators.
func groupInt(n int64) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// perfs is the history of performance runs, newest first, each with its
// run's state and the line for gobank's benchmark.md.
func (s *Server) perfs(w http.ResponseWriter, r *http.Request) {
	list, err := s.op.Perfs(r.Context(), 50)
	var views []perfView
	for _, p := range list {
		v := viewPerf(p)
		if rec, steps, err := s.op.Run(r.Context(), p.RunID); err == nil && rec != nil {
			rv := viewRun(*rec)
			v.Run_, v.Steps = &rv, viewSteps(steps)
		}
		views = append(views, v)
	}
	s.render(w, "perf", map[string]any{"perfs": views, "error": err})
}

func viewSteps(steps []wf.StepResult) []stepView {
	var out []stepView
	for _, st := range steps {
		out = append(out, stepView{Name: st.StepName, Status: string(st.Status), Duration: time.Duration(st.DurationNs).Round(time.Second).String(), Error: st.Error})
	}
	return out
}

// workflows is the engine's view of this program: each workflow it runs
// described from its definition, with the count of its instances in every
// state and the instances themselves, newest first.
func (s *Server) workflows(w http.ResponseWriter, r *http.Request) {
	counts, err := s.op.Counts(r.Context())
	runs, runsErr := s.op.Runs(r.Context(), 200)
	var views []definitionView
	for _, d := range flows.Definitions {
		views = append(views, viewDefinition(d, counts[d.Type], runs))
	}
	s.render(w, "workflows", map[string]any{"definitions": views, "error": errors.Join(err, runsErr)})
}

// instance finds the run id names, with its steps; it answers the request
// itself (404, 500) and returns nil when there is nothing to show.
func (s *Server) instance(w http.ResponseWriter, r *http.Request) (*wf.RunRecord, []wf.StepResult) {
	rec, steps, err := s.op.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil
	}
	if rec == nil {
		http.NotFound(w, r)
		return nil, nil
	}
	return rec, steps
}

// runDiagram is the instance as d2 source, its steps coloured by state,
// for rendering by hand.
func (s *Server) runDiagram(w http.ResponseWriter, r *http.Request) {
	rec, steps := s.instance(w, r)
	if rec == nil {
		return
	}
	def := flows.DefinitionOf(rec.WorkflowType)
	if def == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, diagram.Instance(*def, *rec, steps))
}

// run is one workflow instance: its recorded steps laid over its
// definition, and when it is a drill or a perf run, that record too.
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	rec, steps := s.instance(w, r)
	if rec == nil {
		return
	}
	data := map[string]any{"run": viewRun(*rec), "steps": viewSteps(steps)}
	if def := flows.DefinitionOf(rec.WorkflowType); def != nil {
		data["definition"] = map[string]any{"Steps": viewDefinitionSteps(*def, steps)}
	}
	if s.Explorer != nil {
		data["explorerSteps"] = "/internal/explorer/workflow_steps?filter=run_id&value=" + rec.ID
	}
	if rec.WorkflowType == flows.WorkflowDrill {
		if list, err := s.op.Drills(r.Context(), 200); err == nil {
			for _, d := range list {
				if d.RunID == rec.ID {
					data["drill"] = viewDrill(d)
				}
			}
		}
	}
	if rec.WorkflowType == flows.WorkflowPerf {
		if list, err := s.op.Perfs(r.Context(), 200); err == nil {
			for _, p := range list {
				if p.RunID == rec.ID {
					data["perf"] = viewPerf(p)
				}
			}
		}
	}
	s.render(w, "run", data)
}

// explorer is go-dbexplorer over this program's database, in the page
// layout; the request URI carries the explorer's own navigation.
func (s *Server) explorer(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "explorer", map[string]any{"html": template.HTML(s.Explorer(r.Context(), r.URL.RequestURI()))})
}
