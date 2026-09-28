// Package deploy orchestrates a gobank environment on a single cloud server:
// create it, put a release on it, report on it, and delete it. The cloud
// provider, the ssh host, the binary build and the HTTP probe sit behind
// interfaces so the sequence is testable without a server.
package deploy

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

//go:embed cloud-init.yaml
var cloudInit string

// Environment is a named deployment target such as prod or preprod. Each
// environment is one server, one database and the release running on it.
type Environment struct {
	Name string
	// Expires, when set, is when the environment is to be removed: a
	// temporary one such as a demo. Carried on the server's labels.
	Expires time.Time
}

// serverPrefix names every environment's server: gobank-<env>.
const serverPrefix = "gobank-"

// ServerName is the cloud server (and firewall) name for the environment.
func (e Environment) ServerName() string { return serverPrefix + e.Name }

// Server is a provisioned cloud server as the provider reports it.
type Server struct {
	Name     string
	IP       string
	Type     string
	Status   string
	Location string
	MemoryGB float64 // RAM of the server type; 0 when unknown
	Labels   map[string]string
}

// expiresLabel holds a temporary server's removal time in UTC as
// 20060102T150405Z: label values may only have letters, digits, - _ and .
const (
	expiresLabel  = "expires"
	expiresLayout = "20060102T150405Z0700"
)

// Expires is when the server is to be removed, or zero for a standing one.
func (s *Server) Expires() time.Time {
	t, err := time.Parse(expiresLayout, s.Labels[expiresLabel])
	if err != nil {
		return time.Time{}
	}
	return t
}

// Release is a built binary and the version baked into it.
type Release struct {
	Version string
	Binary  string // local path
}

// CreateSpec is everything the provider needs to create a server.
type CreateSpec struct {
	Name     string
	Type     string
	Image    string
	Location string
	Firewall string
	SSHKeys  []string
	UserData string
	Labels   map[string]string
}

// FirewallRule is one inbound allow rule.
type FirewallRule struct {
	Description string
	Protocol    string // tcp | icmp
	Port        string // empty for icmp
}

// Cloud is the provider: Hetzner in production, a fake in tests.
type Cloud interface {
	// Server returns nil, nil when no server has that name.
	Server(ctx context.Context, name string) (*Server, error)
	CreateServer(ctx context.Context, spec CreateSpec) (*Server, error)
	DeleteServer(ctx context.Context, name string) error
	FirewallExists(ctx context.Context, name string) (bool, error)
	CreateFirewall(ctx context.Context, name string, rules []FirewallRule) error
	DeleteFirewall(ctx context.Context, name string) error
	SSHKeys(ctx context.Context) ([]string, error)
	// Servers lists every server in the project.
	Servers(ctx context.Context) ([]*Server, error)
}

// Host is a root shell on a server.
type Host interface {
	Run(ctx context.Context, cmd string) error
	Put(ctx context.Context, local, remote string) error
}

// Dialer opens a Host. fresh means the server was just created, so any host
// key pinned for its IP by a previous incarnation must be forgotten.
type Dialer interface {
	Dial(ctx context.Context, ip string, fresh bool) (Host, error)
}

// Builder cross-compiles the release for the server's architecture.
type Builder interface {
	Build(ctx context.Context, goarch string) (Release, error)
	// Available is the version Build would produce, or "" when unknown.
	Available() string
}

// Prober reports whether the service answers at url, and which version
// it says it is ("" when it does not say).
type Prober interface {
	Probe(ctx context.Context, url string) (version string, serving bool)
}

// DNS publishes hostnames: Route53 in production, a fake in tests.
type DNS interface {
	// Set points name at ip, replacing whatever it pointed at.
	Set(ctx context.Context, name, ip string) error
	// Delete removes name; a name that does not exist is not an error.
	Delete(ctx context.Context, name string) error
}

var (
	// ErrNeedsCreate: the server does not exist and creating one starts
	// billing, so it needs an explicit request.
	ErrNeedsCreate = errors.New("server does not exist; creating one starts billing, so pass --create")
	// ErrNoSSHKeys: the cloud project has no ssh keys to install on a new server.
	ErrNoSSHKeys = errors.New("no ssh keys in the cloud project; add one first")
	// ErrNotServing: the service did not answer after deployment.
	ErrNotServing = errors.New("service not answering")
)

const (
	defaultImage    = "ubuntu-24.04"
	defaultLocation = "fsn1"
	servicePort     = "1347"
	binaryPath      = "/opt/gobank/demo"
	serviceName     = "gobank-demo"
	sshAttempts     = 60
	probeAttempts   = 12
	retryDelay      = 5 * time.Second
)

// Deployer runs the up, down and status sequences.
type Deployer struct {
	Cloud Cloud
	Dial  Dialer
	Build Builder
	Probe Prober

	// DNS and Domain, when set, give each environment the hostname
	// <env>.<Domain>, pointed at its server on up and removed on down.
	DNS    DNS
	Domain string

	Image    string // defaults to ubuntu-24.04
	Location string // defaults to fsn1
	Out      io.Writer
	Sleep    func(time.Duration) // defaults to time.Sleep
}

// UpOptions selects the environment, its size and whether creation is allowed.
type UpOptions struct {
	Env    Environment
	Scale  string // small | medium | large | xl | any provider server type
	Create bool
	// Expires labels a created server as temporary, to be removed then.
	Expires time.Time
}

// Status is what an environment looks like from outside.
type Status struct {
	Server    *Server // nil: not provisioned, nothing billing
	Host      string  // the environment's hostname; empty without DNS
	URL       string  // by hostname when there is one, else by address
	Serving   bool
	Version   string // what the service says it is running; "" when unknown
	Available string // what the next up would deploy; "" when unknown
}

// Behind: the next up would deploy a different release from the one running.
func (s Status) Behind() bool {
	return s.Available != "" && s.Version != s.Available
}

// ServerType maps a scale preset to a provider server type and the Go
// architecture it runs. Unknown scales are passed through as server types.
func ServerType(scale string) (typ, goarch string) {
	switch scale {
	case "small":
		typ = "cx23" // 2 vCPU / 4 GB shared x86
	case "medium":
		typ = "cx33" // 4 vCPU / 8 GB shared x86
	case "large":
		typ = "cx53" // 16 vCPU / 32 GB shared x86
	case "xl":
		typ = "ccx33" // 8 dedicated vCPU / 32 GB
	default:
		typ = scale
	}
	return typ, goarchFor(typ)
}

// goarchFor: cax = Ampere ARM, everything else x86.
func goarchFor(serverType string) string {
	if strings.HasPrefix(serverType, "cax") {
		return "arm64"
	}
	return "amd64"
}

func firewallRules() []FirewallRule {
	return []FirewallRule{
		{Description: "ssh", Protocol: "tcp", Port: "22"},
		{Description: "demo", Protocol: "tcp", Port: servicePort},
		{Description: "ping", Protocol: "icmp"},
	}
}

func serviceURL(host string) string { return "http://" + host + ":" + servicePort + "/" }

// hostname is the environment's DNS name, or empty without DNS.
func (d *Deployer) hostname(env Environment) string {
	if d.DNS == nil || d.Domain == "" {
		return ""
	}
	return env.Name + "." + d.Domain
}

// publish points the environment's hostname at the server, if there is DNS.
func (d *Deployer) publish(ctx context.Context, env Environment, srv *Server) error {
	host := d.hostname(env)
	if host == "" {
		return nil
	}
	d.printf("== dns %s -> %s\n", host, srv.IP)
	if err := d.DNS.Set(ctx, host, srv.IP); err != nil {
		return fmt.Errorf("dns %s: %w", host, err)
	}
	return nil
}

// publicURL is where people reach the environment: by hostname when it has one.
func (d *Deployer) publicURL(env Environment, srv *Server) string {
	if host := d.hostname(env); host != "" {
		return serviceURL(host)
	}
	return serviceURL(srv.IP)
}

func (d *Deployer) printf(format string, args ...any) {
	if d.Out != nil {
		fmt.Fprintf(d.Out, format, args...)
	}
}

func (d *Deployer) sleep(t time.Duration) {
	if d.Sleep != nil {
		d.Sleep(t)
	} else {
		time.Sleep(t)
	}
}

// Up creates the environment's server if asked to, then builds and installs
// the release on it and waits for the service to answer.
func (d *Deployer) Up(ctx context.Context, o UpOptions) (*Server, error) {
	name := o.Env.ServerName()
	srv, err := d.Cloud.Server(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("lookup server %s: %w", name, err)
	}
	created := false
	if srv == nil {
		if !o.Create {
			return nil, fmt.Errorf("%s: %w", name, ErrNeedsCreate)
		}
		srv, err = d.create(ctx, o)
		if err != nil {
			return nil, err
		}
		created = true
	} else {
		d.printf("== server %s already exists (%s) — redeploying binary only\n", name, srv.Type)
	}
	if err := d.publish(ctx, o.Env, srv); err != nil {
		return nil, err
	}

	goarch := goarchFor(srv.Type)
	d.printf("== build demo binary (linux/%s)\n", goarch)
	rel, err := d.Build.Build(ctx, goarch)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}

	d.printf("== wait for ssh (%s)\n", srv.IP)
	host, err := d.waitSSH(ctx, srv.IP, created)
	if err != nil {
		return nil, err
	}
	if created {
		d.printf("== wait for cloud-init (postgres install; first boot takes a minute or two)\n")
		// cloud-init exits non-zero on recoverable warnings; the install
		// step below is the real test.
		_ = host.Run(ctx, "cloud-init status --wait >/dev/null")
	}

	d.printf("== install binary + start service\n")
	if err := host.Put(ctx, rel.Binary, binaryPath+".new"); err != nil {
		return nil, fmt.Errorf("copy binary: %w", err)
	}
	if err := host.Run(ctx, installScript(AppMemoryLimit(srv.MemoryGB))); err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}

	d.printf("== check\n")
	url := serviceURL(srv.IP)
	if !d.waitServing(ctx, url) {
		return nil, fmt.Errorf("%s: %w (check: journalctl -u %s on the box)", url, ErrNotServing, serviceName)
	}
	d.printf("\nModel Bank %s (%s) on %s at %s\n", o.Env.Name, rel.Version, srv.Type, d.publicURL(o.Env, srv))
	return srv, nil
}

// appShareWithLocalPostgres is the fraction of the box the app may use
// when PostgreSQL shares it (25% goes to shared_buffers, the rest is OS
// cache for the database). With the database on its own box this rises.
const appShareWithLocalPostgres = 0.5

// AppMemoryLimit is the GOBANK_MEMORY_LIMIT value for a box with ramGB of
// memory, or empty when the size is unknown so the app keeps its default.
func AppMemoryLimit(ramGB float64) string {
	if ramGB <= 0 {
		return ""
	}
	return fmt.Sprintf("%dMB", int(ramGB*1024*appShareWithLocalPostgres))
}

// installScript installs the binary, writes the deployment's environment
// (sizing that changes per box or per deploy, as opposed to what cloud-init
// fixes at first boot), makes the unit read it, and restarts.
func installScript(memoryLimit string) string {
	env := ""
	if memoryLimit != "" {
		env = "GOBANK_MEMORY_LIMIT=" + memoryLimit + "\n"
	}
	return `set -e
install -m 0755 -o gobank -g gobank /opt/gobank/demo.new /opt/gobank/demo
rm /opt/gobank/demo.new
mkdir -p /etc/gobank
printf '%s' '` + env + `' > /etc/gobank/deploy.env
grep -q 'EnvironmentFile=-/etc/gobank/deploy.env' /etc/systemd/system/gobank-demo.service || \
  sed -i '/^\[Service\]/a EnvironmentFile=-/etc/gobank/deploy.env' /etc/systemd/system/gobank-demo.service
systemctl daemon-reload
systemctl enable gobank-demo >/dev/null 2>&1
systemctl restart gobank-demo
`
}

func (d *Deployer) create(ctx context.Context, o UpOptions) (*Server, error) {
	name := o.Env.ServerName()
	keys, err := d.Cloud.SSHKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ssh keys: %w", err)
	}
	if len(keys) == 0 {
		return nil, ErrNoSSHKeys
	}

	exists, err := d.Cloud.FirewallExists(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("lookup firewall %s: %w", name, err)
	}
	if !exists {
		d.printf("== firewall %s\n", name)
		if err := d.Cloud.CreateFirewall(ctx, name, firewallRules()); err != nil {
			return nil, fmt.Errorf("create firewall: %w", err)
		}
	}

	typ, _ := ServerType(o.Scale)
	image, location := d.Image, d.Location
	if image == "" {
		image = defaultImage
	}
	if location == "" {
		location = defaultLocation
	}
	d.printf("== create server %s (%s, %s)\n", name, typ, location)
	srv, err := d.Cloud.CreateServer(ctx, CreateSpec{
		Name:     name,
		Type:     typ,
		Image:    image,
		Location: location,
		Firewall: name,
		SSHKeys:  keys,
		UserData: cloudInit,
		Labels:   labels(o),
	})
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}
	return srv, nil
}

func labels(o UpOptions) map[string]string {
	l := map[string]string{"project": "gobank", "environment": o.Env.Name}
	if !o.Expires.IsZero() {
		l[expiresLabel] = o.Expires.UTC().Format(expiresLayout)
	}
	return l
}

func (d *Deployer) waitSSH(ctx context.Context, ip string, fresh bool) (Host, error) {
	var lastErr error
	for range sshAttempts {
		host, err := d.Dial.Dial(ctx, ip, fresh)
		if err == nil {
			return host, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		d.sleep(retryDelay)
	}
	return nil, fmt.Errorf("could not reach root@%s over ssh: %w", ip, lastErr)
}

func (d *Deployer) waitServing(ctx context.Context, url string) bool {
	for range probeAttempts {
		if _, serving := d.Probe.Probe(ctx, url); serving {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		d.sleep(retryDelay)
	}
	return false
}

// Down deletes the environment's server and firewall. Deletion is what stops
// billing; a powered-off server still bills. The database goes with it.
func (d *Deployer) Down(ctx context.Context, env Environment) error {
	name := env.ServerName()
	srv, err := d.Cloud.Server(ctx, name)
	if err != nil {
		return fmt.Errorf("lookup server %s: %w", name, err)
	}
	if srv == nil {
		d.printf("server %s does not exist\n", name)
	} else {
		d.printf("== delete server %s (%s)\n", name, srv.IP)
		if err := d.Cloud.DeleteServer(ctx, name); err != nil {
			return fmt.Errorf("delete server: %w", err)
		}
	}
	exists, err := d.Cloud.FirewallExists(ctx, name)
	if err != nil {
		return fmt.Errorf("lookup firewall %s: %w", name, err)
	}
	if exists {
		if err := d.Cloud.DeleteFirewall(ctx, name); err != nil {
			return fmt.Errorf("delete firewall: %w", err)
		}
	}
	if host := d.hostname(env); host != "" {
		d.printf("== dns remove %s\n", host)
		if err := d.DNS.Delete(ctx, host); err != nil {
			return fmt.Errorf("dns %s: %w", host, err)
		}
	}
	d.printf("done — nothing left billing\n")
	return nil
}

// Environments are the environments with a server in the cloud project,
// sorted by name: anything named gobank-<env>, whoever created it, with
// the expiry a temporary one carries.
func (d *Deployer) Environments(ctx context.Context) ([]Environment, error) {
	servers, err := d.Cloud.Servers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	var envs []Environment
	for _, s := range servers {
		if name, ok := strings.CutPrefix(s.Name, serverPrefix); ok {
			envs = append(envs, Environment{Name: name, Expires: s.Expires()})
		}
	}
	slices.SortFunc(envs, func(a, b Environment) int { return strings.Compare(a.Name, b.Name) })
	return envs, nil
}

// Status reports whether the environment is provisioned and answering.
func (d *Deployer) Status(ctx context.Context, env Environment) (Status, error) {
	srv, err := d.Cloud.Server(ctx, env.ServerName())
	if err != nil {
		return Status{}, fmt.Errorf("lookup server %s: %w", env.ServerName(), err)
	}
	if srv == nil {
		return Status{}, nil
	}
	// Probe by address: it works before the hostname has propagated.
	version, serving := d.Probe.Probe(ctx, serviceURL(srv.IP))
	return Status{
		Server: srv, Host: d.hostname(env), URL: d.publicURL(env, srv),
		Serving: serving, Version: version, Available: d.Build.Available(),
	}, nil
}
