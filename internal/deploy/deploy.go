// Package deploy orchestrates a gobank environment on a single cloud server:
// create it, put a release on it, report on it, and delete it. The cloud
// provider, the ssh host, the binary build and the HTTP probe sit behind
// interfaces so the sequence is testable without a server.
package deploy

import (
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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

// appPasswordLabel carries the environment's app password: the one
// password every simulated customer logs in to the demo's BFF with
// (GOBANK_APP_PASSWORD). It lives on the server so redeploys keep it and
// status can show it without ssh; the cloud token already gives root on
// the box, so the label adds no exposure.
const appPasswordLabel = "app-password"

// AppPassword is the environment's app password, or "" before one is set.
func (s *Server) AppPassword() string { return s.Labels[appPasswordLabel] }

// adminPasswordLabel carries the environment's admin password: the first
// admin's login to the demo's staff web (GOBANK_ADMIN_PASSWORD, gobank
// story 1.7.1). Kept on the server like the app password, for the same
// reasons.
const adminPasswordLabel = "admin-password"

// AdminPassword is the environment's admin password, or "" before one is
// set.
func (s *Server) AdminPassword() string { return s.Labels[adminPasswordLabel] }

// appPasswordAlphabet keeps passwords label-safe and easy to type on a phone.
const appPasswordAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newAppPassword is 16 characters from appPasswordAlphabet, uniformly.
func newAppPassword() string {
	out := make([]byte, 0, 16)
	buf := make([]byte, 32)
	for len(out) < 16 {
		if _, err := rand.Read(buf); err != nil {
			panic(err) // the OS random source is gone; nothing sensible to do
		}
		for _, b := range buf {
			// Reject the top of the byte range so every letter is equally likely.
			if b < 252 && len(out) < 16 {
				out = append(out, appPasswordAlphabet[int(b)%len(appPasswordAlphabet)])
			}
		}
	}
	return string(out)
}

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
	// SetLabels merges labels into the named server's labels.
	SetLabels(ctx context.Context, name string, labels map[string]string) error
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

// Repo is the source repository's release state: its newest tag, and the
// binaries its release of a tag carries (the build stage, run when the
// tag is released).
type Repo interface {
	LatestTag(ctx context.Context) (string, error)
	// Download writes tag's built demo for goarch to dst, executable.
	// ErrNoRelease when the tag has no such build.
	Download(ctx context.Context, tag, goarch, dst string) error
}

// ErrNoRelease: the tag has no built binary on the repo (a tag from before
// the build stage existed, or a release still in progress).
var ErrNoRelease = errors.New("no built release for this tag on the repo")

// Architectures are the server architectures a release is built for: x86
// and Ampere ARM.
var Architectures = []string{"amd64", "arm64"}

// ReleaseStore is a Builder that keeps built releases rather than making
// them: the appliance's. A release the repo has built is fetched into it
// before a deploy.
type ReleaseStore interface {
	Has(version string) bool
	// Put adds version's binary for goarch and makes version the latest.
	Put(version, goarch, binary string) error
	// Use makes a version the store holds the latest (a rollback when it
	// is older); an error when the store lacks it.
	Use(version string) error
}

// Releases is where versions stand: the newest tag on the repo, and what
// this host would deploy (the store's latest, or the checkout's describe).
type Releases struct {
	Repo      string // newest tag; "" without a repo
	Available string // what the next up deploys from here; "" when unknown
	Fetches   bool   // the next up fetches the repo's release itself (a store)
}

// Lagging: the repo has a tag that cannot be deployed from here yet — the
// store needs a push, or the checkout a pull. A checkout past the tag
// (describe with a suffix) is not lagging.
func (r Releases) Lagging() bool {
	return r.Repo != "" && r.Available != "" && r.Available != r.Repo && !strings.HasPrefix(r.Available, r.Repo+"-")
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
	// ErrNoCapacity: the provider has no capacity for the server type at
	// the location right now; the deployer tries its other locations.
	ErrNoCapacity = errors.New("no capacity for this server type at the location")
)

// defaultLocations are tried in order when creating a server: the
// provider is sometimes out of a type at one location.
var defaultLocations = []string{"fsn1", "nbg1", "hel1"}

const (
	defaultImage = "ubuntu-24.04"
	servicePort  = "1347"
	binaryPath   = "/opt/gobank/demo"
	serviceName  = "gobank-demo"
	sshAttempts  = 60
	retryDelay   = 5 * time.Second
	// servingWait is the ceiling on the check after a start. The demo
	// rebuilds the bank from its database before it listens (a minute and
	// a half at four thousand days, growing with the history), so the
	// check waits while the service is alive and gives up here.
	servingWait   = 30 * time.Minute
	probeAttempts = int(servingWait / retryDelay)
)

// serviceAlive succeeds while the service is still starting: active, and
// not restarted by systemd since the install (Restart=on-failure would
// otherwise hide a crash loop behind an active unit).
const serviceAlive = "systemctl is-active --quiet " + serviceName + " && test \"$(systemctl show -p NRestarts --value " + serviceName + ")\" = 0"

// Deployer runs the up, down and status sequences.
type Deployer struct {
	Cloud Cloud
	Dial  Dialer
	Build Builder
	Probe Prober

	// Repo, when set, is the source repository, for its newest tag and
	// the binaries it has built.
	Repo Repo
	// Store, when set with Repo, is Build as a release store: the newest
	// release on Repo is fetched into it before a deploy.
	Store ReleaseStore

	// DNS and Domain, when set, give each environment the hostname
	// <env>.<Domain>, pointed at its server on up and removed on down.
	DNS    DNS
	Domain string

	Image     string   // defaults to ubuntu-24.04
	Locations []string // tried in order when one has no capacity; defaults to fsn1, nbg1, hel1
	Out       io.Writer
	Sleep     func(time.Duration) // defaults to time.Sleep
	// Resolve looks a hostname up, as the world would; nil means the
	// system resolver. Status gives the hostname only when it points at
	// the server, else the address.
	Resolve func(ctx context.Context, host string) ([]string, error)
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
	// AppPassword logs any customer in to the demo's BFF; "" before the
	// first up since the story that introduced it. AdminPassword logs the
	// user "admin" in to the staff web, likewise.
	AppPassword   string
	AdminPassword string
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
		typ = "ccx33" // 8 dedicated vCPU / 32 GB: cores that are really there, for a measurement
	case "xl":
		typ = "ccx43" // 16 dedicated vCPU / 64 GB
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

// publish points the environment's hostname at the server, if there is
// DNS. A refused change is reported, not fatal: the address still works,
// and a deploy must not stop, nor a server be left billing, over a name.
// It returns whether the name can be relied on.
func (d *Deployer) publish(ctx context.Context, env Environment, srv *Server) bool {
	host := d.hostname(env)
	if host == "" {
		return false
	}
	d.printf("== dns %s -> %s\n", host, srv.IP)
	if err := d.DNS.Set(ctx, host, srv.IP); err != nil {
		d.printf("dns %s failed: %v — carrying on by address\n", host, err)
		return false
	}
	return true
}

// publicURL is where people reach the environment: by hostname when it has
// one that is known to be set, else by address.
func (d *Deployer) publicURL(env Environment, srv *Server, named bool) string {
	if host := d.hostname(env); host != "" && named {
		return serviceURL(host)
	}
	return serviceURL(srv.IP)
}

// named reports whether the environment's hostname points at its server
// now: set by a deploy, propagated, and not left over from an earlier
// server at an address the provider has since reused.
func (d *Deployer) named(ctx context.Context, env Environment, srv *Server) bool {
	host := d.hostname(env)
	if host == "" {
		return false
	}
	resolve := d.Resolve
	if resolve == nil {
		resolve = net.DefaultResolver.LookupHost
	}
	addrs, err := resolve(ctx, host)
	return err == nil && slices.Contains(addrs, srv.IP)
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
		srv, err = d.create(ctx, o, newAppPassword(), newAppPassword())
		if err != nil {
			return nil, err
		}
		created = true
	} else {
		d.printf("== server %s already exists (%s) — redeploying binary only\n", name, srv.Type)
	}
	named := d.publish(ctx, o.Env, srv)

	d.fetch(ctx)
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
	// A server from before app or admin passwords gets them on its first
	// redeploy.
	appPassword, adminPassword := srv.AppPassword(), srv.AdminPassword()
	missing := map[string]string{}
	if appPassword == "" {
		appPassword = newAppPassword()
		missing[appPasswordLabel] = appPassword
	}
	if adminPassword == "" {
		adminPassword = newAppPassword()
		missing[adminPasswordLabel] = adminPassword
	}
	if len(missing) > 0 {
		if err := d.Cloud.SetLabels(ctx, name, missing); err != nil {
			return nil, fmt.Errorf("label %s with its passwords: %w", name, err)
		}
	}
	if err := host.Run(ctx, installScript(AppMemoryLimit(srv.MemoryGB), appPassword, adminPassword)); err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}

	d.printf("== check\n")
	url := serviceURL(srv.IP)
	if err := d.waitServing(ctx, host, url); err != nil {
		return nil, fmt.Errorf("%s: %w (check: journalctl -u %s on the box)", url, err, serviceName)
	}
	d.printf("\nModel Bank %s (%s) on %s at %s\n", o.Env.Name, rel.Version, srv.Type, d.publicURL(o.Env, srv, named))
	return srv, nil
}

// fetch is the build stage before a deploy from a store: Fetch, with a
// release that cannot be fetched (a tag without binaries, a repo out of
// reach) a notice, so the deploy carries what the store has.
func (d *Deployer) fetch(ctx context.Context) {
	if d.Repo == nil || d.Store == nil {
		return
	}
	if _, err := d.Fetch(ctx, ""); err != nil {
		d.printf("%v — deploying %s from the store\n", err, d.Build.Available())
	}
}

// ErrNoStore: there is no release store to fetch into (the laptop builds
// from its checkout instead).
var ErrNoStore = errors.New("no release store to fetch into")

// Fetch puts a release into the store for every architecture and makes
// it the latest, the next deploy; it returns the tag. With no tag named
// it is the repo's newest, fetched when the store lacks it and otherwise
// left alone, so an older release put back (a rollback) stays the latest.
// Run before a deploy, and on its own after gobank's release. A named tag
// is fetched when the store lacks it and becomes the latest either way:
// naming the previous release is the rollback (gobank ADR-0003), and a
// redeploy then carries it.
func (d *Deployer) Fetch(ctx context.Context, tag string) (string, error) {
	if d.Store == nil {
		return "", ErrNoStore
	}
	if d.Repo == nil {
		return "", errors.New("no repo to fetch from")
	}
	named := tag != ""
	if !named {
		var err error
		if tag, err = d.Repo.LatestTag(ctx); err != nil {
			return "", fmt.Errorf("repo: %w", err)
		}
		if tag == "" {
			return "", errors.New("the repo has no tags")
		}
	}
	if d.Store.Has(tag) {
		if named {
			if err := d.Store.Use(tag); err != nil {
				return "", fmt.Errorf("store: %w", err)
			}
			d.printf("== %s is the next deploy\n", tag)
		}
		return tag, nil
	}
	d.printf("== fetch %s from the repo's release\n", tag)
	dir, err := os.MkdirTemp("", "gobank-deploy-fetch-")
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", tag, err)
	}
	defer os.RemoveAll(dir)
	binaries := map[string]string{}
	for _, goarch := range Architectures {
		dst := filepath.Join(dir, "demo-linux-"+goarch)
		switch err := d.Repo.Download(ctx, tag, goarch, dst); {
		case errors.Is(err, ErrNoRelease):
			return "", fmt.Errorf("%s has no built demo for linux/%s on the repo: %w", tag, goarch, err)
		case err != nil:
			return "", fmt.Errorf("fetch %s: %w", tag, err)
		}
		binaries[goarch] = dst
	}
	for _, goarch := range Architectures {
		if err := d.Store.Put(tag, goarch, binaries[goarch]); err != nil {
			return "", fmt.Errorf("store %s: %w", tag, err)
		}
	}
	return tag, nil
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
// (what changes per box or per deploy — sizing, the app and admin
// passwords — as opposed to what cloud-init fixes at first boot), makes
// the unit read it and gives it a stop timeout that outlasts a simulated
// day (the demo finishes the day in progress on SIGTERM; a SIGKILL would
// lose it), and restarts.
func installScript(memoryLimit, appPassword, adminPassword string) string {
	env := ""
	if memoryLimit != "" {
		env = "GOBANK_MEMORY_LIMIT=" + memoryLimit + "\n"
	}
	if appPassword != "" {
		env += "GOBANK_APP_PASSWORD=" + appPassword + "\n"
	}
	if adminPassword != "" {
		env += "GOBANK_ADMIN_PASSWORD=" + adminPassword + "\n"
	}
	return `set -e
install -m 0755 -o gobank -g gobank /opt/gobank/demo.new /opt/gobank/demo
rm /opt/gobank/demo.new
mkdir -p /etc/gobank
printf '%s' '` + env + `' > /etc/gobank/deploy.env
grep -q 'EnvironmentFile=-/etc/gobank/deploy.env' /etc/systemd/system/gobank-demo.service || \
  sed -i '/^\[Service\]/a EnvironmentFile=-/etc/gobank/deploy.env' /etc/systemd/system/gobank-demo.service
grep -q 'TimeoutStopSec=' /etc/systemd/system/gobank-demo.service || \
  sed -i '/^\[Service\]/a TimeoutStopSec=900' /etc/systemd/system/gobank-demo.service
systemctl daemon-reload
systemctl enable gobank-demo >/dev/null 2>&1
systemctl restart gobank-demo
`
}

func (d *Deployer) create(ctx context.Context, o UpOptions, appPassword, adminPassword string) (*Server, error) {
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
	image, locations := d.Image, d.Locations
	if image == "" {
		image = defaultImage
	}
	if len(locations) == 0 {
		locations = defaultLocations
	}
	// Every location for the type asked for, then the next size down at
	// every location again: a smaller box beats no box, and the server's
	// actual type is what is recorded.
	for {
		var err error
		for i, location := range locations {
			d.printf("== create server %s (%s, %s)\n", name, typ, location)
			var srv *Server
			srv, err = d.Cloud.CreateServer(ctx, CreateSpec{
				Name:     name,
				Type:     typ,
				Image:    image,
				Location: location,
				Firewall: name,
				SSHKeys:  keys,
				UserData: cloudInit,
				Labels:   labels(o, appPassword, adminPassword),
			})
			if err == nil {
				return srv, nil
			}
			if !errors.Is(err, ErrNoCapacity) {
				return nil, fmt.Errorf("create server: %w", err)
			}
			if i < len(locations)-1 {
				d.printf("no capacity for %s in %s: trying %s\n", typ, location, locations[i+1])
			}
		}
		smaller := sizeDown(typ)
		if smaller == "" {
			return nil, fmt.Errorf("create server: %w", err)
		}
		d.printf("no capacity for %s anywhere: trying %s\n", typ, smaller)
		typ = smaller
	}
}

// sizeLadder is the range a type steps down through when no location has
// it, largest first: the dedicated line, then the shared x86 line. A type
// outside it has no size to step down to. A first cut: the performance
// data will say whether the order is right.
var sizeLadder = []string{"ccx43", "ccx33", "cx53", "cx43", "cx33", "cx23"}

// sizeDown is the next smaller type in the ladder, or "" at the bottom or
// off it.
func sizeDown(typ string) string {
	for i, t := range sizeLadder {
		if t == typ && i+1 < len(sizeLadder) {
			return sizeLadder[i+1]
		}
	}
	return ""
}

func labels(o UpOptions, appPassword, adminPassword string) map[string]string {
	l := map[string]string{"project": "gobank", "environment": o.Env.Name, appPasswordLabel: appPassword, adminPasswordLabel: adminPassword}
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

// waitServing waits for the service at url to answer: as long as it is
// alive on host (the resume takes as long as the history is), up to
// servingWait. A service that dies or is restarted by systemd fails at
// once.
func (d *Deployer) waitServing(ctx context.Context, host Host, url string) error {
	for attempt := range probeAttempts {
		if _, serving := d.Probe.Probe(ctx, url); serving {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := host.Run(ctx, serviceAlive); err != nil {
			return fmt.Errorf("%w: the service stopped or restarted", ErrNotServing)
		}
		if waited := time.Duration(attempt+1) * retryDelay; waited%time.Minute == 0 {
			d.printf("still starting after %s (the demo rebuilds the bank from its database before it listens)\n", waited)
		}
		d.sleep(retryDelay)
	}
	return fmt.Errorf("%w after %s", ErrNotServing, servingWait)
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
			d.printf("dns remove %s failed: %v — the record is stale until removed by hand\n", host, err)
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

// Releases reports the newest tag on the repo against what this host
// would deploy. The repo failing still returns what is available here.
func (d *Deployer) Releases(ctx context.Context) (Releases, error) {
	rel := Releases{Available: d.Build.Available(), Fetches: d.Store != nil}
	if d.Repo == nil {
		return rel, nil
	}
	tag, err := d.Repo.LatestTag(ctx)
	rel.Repo = tag
	return rel, err
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
	named := d.named(ctx, env, srv)
	var host string
	if named {
		host = d.hostname(env)
	}
	return Status{
		Server: srv, Host: host, URL: d.publicURL(env, srv, named),
		Serving: serving, Version: version, Available: d.Build.Available(),
		AppPassword: srv.AppPassword(), AdminPassword: srv.AdminPassword(),
	}, nil
}
