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
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"git.bytestone.uk/hum3/lofigui"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

//go:embed templates
var templateFS embed.FS

// Operator is what the page drives: the environment list and each status
// are read on every render, up and down run as background jobs writing
// their progress to out.
type Operator interface {
	Environments(ctx context.Context) ([]deploy.Environment, error)
	Status(ctx context.Context, env deploy.Environment) (deploy.Status, error)
	Up(ctx context.Context, o deploy.UpOptions, out io.Writer) error
	Down(ctx context.Context, env deploy.Environment, out io.Writer) error
}

// DeployerFactory adapts deploy.Deployer to Operator: each job gets a
// Deployer whose Out is the job's log.
type DeployerFactory func(out io.Writer) *deploy.Deployer

func (f DeployerFactory) Environments(ctx context.Context) ([]deploy.Environment, error) {
	return f(io.Discard).Environments(ctx)
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
	Error error
	Job   *jobView
}

// Server is the http.Handler for the page.
type Server struct {
	op      Operator
	envs    []deploy.Environment // configured: always listed, provisioned or not
	ctrl    *lofigui.Controller
	mux     *http.ServeMux
	Version string
	// UpUnavailable, when set and returning a reason, is why this host
	// cannot run up right now (no Go toolchain or gobank checkout, an empty
	// release store): the page then offers status and down only, and
	// create / redeploy requests are refused. Asked on every request, so a
	// store that fills up later is noticed without a restart.
	UpUnavailable func() string

	mu   sync.Mutex
	jobs map[string]*job // latest job per environment
}

// New builds the page. envs are always listed; environments that exist in
// the cloud project, and ones being created from the page, join them.
func New(op Operator, envs []deploy.Environment) (*Server, error) {
	ctrl, err := lofigui.NewControllerFromFS(templateFS, "templates", "index.html")
	if err != nil {
		return nil, err
	}
	s := &Server{op: op, envs: envs, ctrl: ctrl, jobs: map[string]*job{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("POST /env", s.create)
	s.mux.HandleFunc("POST /env/{env}/create", s.action("create"))
	s.mux.HandleFunc("POST /env/{env}/redeploy", s.action("redeploy"))
	s.mux.HandleFunc("POST /env/{env}/down", s.action("down"))
	s.mux.HandleFunc("POST /env/{env}/cancel", s.cancel)
	s.mux.HandleFunc("GET /favicon.ico", lofigui.ServeFavicon)
	s.mux.HandleFunc("GET /assets/bulma.min.css", lofigui.ServeBulma)
	return s, nil
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

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	refresh := refreshIdle
	var views []envView
	envs, listErr := s.environments(r.Context())
	for _, e := range envs {
		v := envView{Name: e.Name}
		v.Status, v.Error = s.op.Status(r.Context(), e)
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.ctrl.RenderTemplate(w, lofigui.TemplateContext{
		"envs":          views,
		"listError":     listErr,
		"refresh":       refresh,
		"version":       s.Version,
		"upUnavailable": s.upUnavailable(),
	})
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
		http.Error(w, "create not available here: "+why, http.StatusForbidden)
		return
	}
	name := r.FormValue("name")
	if !envName.MatchString(name) {
		http.Error(w, fmt.Sprintf("environment name %q: use lower-case letters, digits and hyphens", name), http.StatusBadRequest)
		return
	}
	if _, exists := s.environment(r.Context(), name); exists {
		http.Error(w, name+": already an environment", http.StatusConflict)
		return
	}
	s.startCreate(w, r, deploy.Environment{Name: name})
}

// startCreate runs up -create for env at the form's scale and redirects to the page.
func (s *Server) startCreate(w http.ResponseWriter, r *http.Request, env deploy.Environment) {
	scale := r.FormValue("scale")
	if scale == "" {
		scale = "small"
	}
	run := func(ctx context.Context, out io.Writer) error {
		return s.op.Up(ctx, deploy.UpOptions{Env: env, Scale: scale, Create: true}, out)
	}
	if err := s.start(env, "create", run); err != nil {
		http.Error(w, fmt.Sprintf("%s: %v", env.Name, err), http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
			http.Error(w, fmt.Sprintf("%s: %s not available here: %s", env.Name, name, why), http.StatusForbidden)
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
				http.Error(w, "tick the confirmation to delete the server and its database", http.StatusBadRequest)
				return
			}
			run = func(ctx context.Context, out io.Writer) error {
				return s.op.Down(ctx, env, out)
			}
		}
		if err := s.start(env, name, run); err != nil {
			http.Error(w, fmt.Sprintf("%s: %v", env.Name, err), http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
