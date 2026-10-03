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
	"git.bytestone.uk/hum3/lofigui"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
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
	// Releases is the newest tag on the repo against what is deployable here.
	Releases(ctx context.Context) (deploy.Releases, error)
	// Fetch puts the repo's newest release into the store, returning its tag.
	Fetch(ctx context.Context) (string, error)
}

// Ops is the production Operator: a Deployer per job, and the workflows.
type Ops struct {
	DeployerFactory
	Flows *flows.Demo
}

func (o Ops) Demo(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	return o.Flows.Run(ctx, env, scale, out)
}

func (o Ops) Runs(ctx context.Context, limit int) ([]wf.RunRecord, error) {
	return o.Flows.Store.ListRuns(ctx, limit)
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

func (f DeployerFactory) Fetch(ctx context.Context) (string, error) {
	return f(io.Discard).Fetch(ctx)
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
}

// runView is a workflow run as the template sees it.
type runView struct {
	Type    string
	Key     string
	Status  string
	Started string
	Error   string
}

// Server is the http.Handler for the page.
type Server struct {
	op      Operator
	envs    []deploy.Environment // configured: always listed, provisioned or not
	page    *template.Template   // index.html, with the envs fragment and the clock
	about   *lofigui.Controller
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

	mu   sync.Mutex
	jobs map[string]*job // latest job per environment
}

// New builds the page. envs are always listed; environments that exist in
// the cloud project, and ones being created from the page, join them.
func New(op Operator, envs []deploy.Environment) (*Server, error) {
	page, err := template.ParseFS(templateFS, "templates/index.html", "templates/envs.html")
	if err != nil {
		return nil, err
	}
	about, err := lofigui.NewControllerFromFS(templateFS, "templates", "about.html")
	if err != nil {
		return nil, err
	}
	s := &Server{op: op, envs: envs, page: page, about: about, jobs: map[string]*job{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /fragment", s.fragment)
	s.mux.HandleFunc("GET /about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.about.RenderTemplate(w, lofigui.TemplateContext{"version": s.Version})
	})
	s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(must(fs.Sub(assetFS, "assets")))))
	s.mux.HandleFunc("POST /env", s.create)
	s.mux.HandleFunc("POST /env/{env}/create", s.action("create"))
	s.mux.HandleFunc("POST /env/{env}/redeploy", s.action("redeploy"))
	s.mux.HandleFunc("POST /env/{env}/down", s.action("down"))
	s.mux.HandleFunc("POST /env/{env}/cancel", s.cancel)
	s.mux.HandleFunc("POST /fetch", s.fetch)
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
			runs = append(runs, runView{Type: string(rec.WorkflowType), Key: rec.Key, Status: string(rec.Status), Started: rec.StartedAt.Local().Format("15:04 Mon 2 Jan"), Error: rec.Error})
		}
	}
	releases, relErr := s.op.Releases(ctx)
	return map[string]any{
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
// A failure is an error status, so the release's post_release step warns.
func (s *Server) fetch(w http.ResponseWriter, r *http.Request) {
	tag, err := s.op.Fetch(r.Context())
	if err != nil {
		http.Error(w, "fetch: "+err.Error(), http.StatusBadGateway)
		return
	}
	fmt.Fprintf(w, "%s is in the store\n", tag)
}
