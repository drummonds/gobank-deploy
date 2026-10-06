package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- fakes -----------------------------------------------------------------

type fakeCloud struct {
	servers   map[string]*Server
	firewalls map[string]bool
	sshKeys   []string
	created   []CreateSpec
	deleted   []string
	fwDeleted []string
	full      map[string]bool // "loc" with no capacity for anything, or "type@loc" for that type
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{
		servers:   map[string]*Server{},
		firewalls: map[string]bool{},
		sshKeys:   []string{"laptop", "jeeves"},
		full:      map[string]bool{},
	}
}

func (c *fakeCloud) Server(_ context.Context, name string) (*Server, error) {
	return c.servers[name], nil
}

func (c *fakeCloud) CreateServer(_ context.Context, spec CreateSpec) (*Server, error) {
	c.created = append(c.created, spec)
	if c.full[spec.Location] || c.full[spec.Type+"@"+spec.Location] {
		return nil, fmt.Errorf("%w: error during placement (resource_unavailable)", ErrNoCapacity)
	}
	s := &Server{Name: spec.Name, IP: "10.0.0.7", Type: spec.Type, Status: "running", Location: spec.Location, MemoryGB: 4, Labels: spec.Labels}
	c.servers[spec.Name] = s
	return s, nil
}

func (c *fakeCloud) DeleteServer(_ context.Context, name string) error {
	c.deleted = append(c.deleted, name)
	delete(c.servers, name)
	return nil
}

func (c *fakeCloud) FirewallExists(_ context.Context, name string) (bool, error) {
	return c.firewalls[name], nil
}

func (c *fakeCloud) CreateFirewall(_ context.Context, name string, _ []FirewallRule) error {
	c.firewalls[name] = true
	return nil
}

func (c *fakeCloud) DeleteFirewall(_ context.Context, name string) error {
	c.fwDeleted = append(c.fwDeleted, name)
	delete(c.firewalls, name)
	return nil
}

func (c *fakeCloud) SSHKeys(context.Context) ([]string, error) { return c.sshKeys, nil }

func (c *fakeCloud) Servers(context.Context) ([]*Server, error) {
	var out []*Server
	for _, s := range c.servers {
		out = append(out, s)
	}
	return out, nil
}

// fakeDNS records name → ip.
type fakeDNS struct {
	records map[string]string
	deleted []string
	err     error // every call fails with it
}

func (d *fakeDNS) Set(_ context.Context, name, ip string) error {
	if d.err != nil {
		return d.err
	}
	d.records[name] = ip
	return nil
}

func (d *fakeDNS) Delete(_ context.Context, name string) error {
	if d.err != nil {
		return d.err
	}
	d.deleted = append(d.deleted, name)
	delete(d.records, name)
	return nil
}

type fakeHost struct {
	runs    []string
	puts    []string
	failing string // a command containing this fails
}

func (h *fakeHost) Run(_ context.Context, cmd string) error {
	h.runs = append(h.runs, cmd)
	if h.failing != "" && strings.Contains(cmd, h.failing) {
		return errors.New("exit status 1")
	}
	return nil
}

func (h *fakeHost) Put(_ context.Context, local, remote string) error {
	h.puts = append(h.puts, local+" -> "+remote)
	return nil
}

type fakeDialer struct {
	host      *fakeHost
	failFirst int // number of dials that fail before one succeeds
	dials     int
	fresh     []bool
}

func (d *fakeDialer) Dial(_ context.Context, _ string, fresh bool) (Host, error) {
	d.dials++
	d.fresh = append(d.fresh, fresh)
	if d.dials <= d.failFirst {
		return nil, errors.New("connection refused")
	}
	return d.host, nil
}

type fakeBuilder struct {
	goarch    string
	builds    int
	available string
}

func (b *fakeBuilder) Available() string { return b.available }

func (b *fakeBuilder) Build(_ context.Context, goarch string) (Release, error) {
	b.builds++
	b.goarch = goarch
	return Release{Version: "v0.3.44", Binary: "/tmp/demo"}, nil
}

type fakeProber struct {
	failFirst int
	probes    int
	url       string
	version   string
}

func (p *fakeProber) Probe(_ context.Context, url string) (string, bool) {
	p.probes++
	p.url = url
	if p.probes > p.failFirst {
		return p.version, true
	}
	return "", false
}

type harness struct {
	cloud  *fakeCloud
	dialer *fakeDialer
	host   *fakeHost
	build  *fakeBuilder
	probe  *fakeProber
	out    *bytes.Buffer
	d      *Deployer
	slept  time.Duration
}

func newHarness() *harness {
	h := &harness{
		cloud: newFakeCloud(),
		host:  &fakeHost{},
		build: &fakeBuilder{},
		probe: &fakeProber{},
		out:   &bytes.Buffer{},
	}
	h.dialer = &fakeDialer{host: h.host}
	h.d = &Deployer{
		Cloud: h.cloud, Dial: h.dialer, Build: h.build, Probe: h.probe,
		Out:   h.out,
		Sleep: func(d time.Duration) { h.slept += d },
	}
	return h
}

// withDNS gives the harness a DNS provider for the gobank.test domain, and
// a resolver that answers from its records, as the world would.
func (h *harness) withDNS() *fakeDNS {
	dns := &fakeDNS{records: map[string]string{}}
	h.d.DNS, h.d.Domain = dns, "gobank.test"
	h.d.Resolve = func(_ context.Context, host string) ([]string, error) {
		if ip, ok := dns.records[host]; ok {
			return []string{ip}, nil
		}
		return nil, errors.New("no such host")
	}
	return dns
}

var prod = Environment{Name: "prod"}

// --- Expiry -------------------------------------------------------------------

var noon = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// A temporary environment carries its expiry on the server itself, so it
// survives whatever process asked for it.
func TestUpLabelsATemporaryServerWithItsExpiry(t *testing.T) {
	h := newHarness()
	if _, err := h.d.Up(context.Background(), UpOptions{Env: Environment{Name: "demo"}, Scale: "small", Create: true, Expires: noon}); err != nil {
		t.Fatal(err)
	}
	// Hetzner label values allow letters, digits, - _ . only: no colons.
	if got := h.cloud.created[0].Labels["expires"]; got != "20260927T120000Z" {
		t.Errorf("expires label = %q", got)
	}
	if got := h.cloud.created[0].Labels["expires"]; got != "" && !h.cloud.servers["gobank-demo"].Expires().Equal(noon) {
		t.Errorf("server expiry = %v", h.cloud.servers["gobank-demo"].Expires())
	}
	h2 := newHarness()
	_, _ = h2.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true})
	if _, has := h2.cloud.created[0].Labels["expires"]; has {
		t.Error("a standing environment has no expiry")
	}
}

func TestEnvironmentsCarryTheirExpiry(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod"}
	h.cloud.servers["gobank-demo"] = &Server{Name: "gobank-demo", Labels: map[string]string{"expires": "20260927T120000Z"}}
	h.cloud.servers["gobank-odd"] = &Server{Name: "gobank-odd", Labels: map[string]string{"expires": "junk"}}
	envs, err := h.d.Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Environment{{Name: "demo", Expires: noon}, {Name: "odd"}, {Name: "prod"}}
	if !slices.Equal(envs, want) {
		t.Errorf("environments = %v, want %v", envs, want)
	}
}

// --- DNS ----------------------------------------------------------------------

func TestUpPointsTheEnvironmentsHostnameAtTheServer(t *testing.T) {
	h := newHarness()
	dns := h.withDNS()
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatal(err)
	}
	if got := dns.records["prod.gobank.test"]; got != "10.0.0.7" {
		t.Errorf("prod.gobank.test = %q, want the server's address", got)
	}
	if !strings.Contains(h.out.String(), "http://prod.gobank.test:1347/") {
		t.Errorf("output should give the hostname URL:\n%s", h.out.String())
	}
}

// A redeploy repairs the record: the server's address is the truth.
func TestRedeployResetsTheHostname(t *testing.T) {
	h := newHarness()
	dns := h.withDNS()
	dns.records["prod.gobank.test"] = "10.9.9.9"
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23"}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
		t.Fatal(err)
	}
	if got := dns.records["prod.gobank.test"]; got != "10.0.0.7" {
		t.Errorf("prod.gobank.test = %q", got)
	}
}

func TestDownRemovesTheHostname(t *testing.T) {
	h := newHarness()
	dns := h.withDNS()
	dns.records["prod.gobank.test"] = "10.0.0.7"
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7"}
	if err := h.d.Down(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	if _, still := dns.records["prod.gobank.test"]; still {
		t.Error("record should be gone")
	}
	// Idempotent: a second down asks the provider again, which tolerates absence.
	if err := h.d.Down(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
}

// DNS is a convenience: a refused record must not stop a deploy or leave
// a server billing, it is reported and the address still works.
func TestARefusedDNSChangeIsAWarningNotAFailure(t *testing.T) {
	h := newHarness()
	dns := h.withDNS()
	dns.err = errors.New("AccessDenied")
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatalf("up should succeed without DNS: %v", err)
	}
	if h.build.builds != 1 || !strings.Contains(h.out.String(), "dns prod.gobank.test failed: AccessDenied") {
		t.Errorf("builds = %d, output:\n%s", h.build.builds, h.out.String())
	}
	if !strings.Contains(h.out.String(), "http://10.0.0.7:1347/") {
		t.Errorf("the address is what works when DNS did not:\n%s", h.out.String())
	}
	if err := h.d.Down(context.Background(), prod); err != nil {
		t.Fatalf("down should succeed without DNS: %v", err)
	}
	if len(h.cloud.deleted) != 1 || !strings.Contains(h.out.String(), "dns remove prod.gobank.test failed") {
		t.Errorf("deleted = %v, output:\n%s", h.cloud.deleted, h.out.String())
	}
}

func TestStatusGivesTheHostnameWhenItPointsAtTheServer(t *testing.T) {
	h := newHarness()
	dns := h.withDNS()
	dns.records["prod.gobank.test"] = "10.0.0.7"
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7"}
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.Host != "prod.gobank.test" || st.URL != "http://prod.gobank.test:1347/" {
		t.Errorf("status = %+v", st)
	}
	if h.probe.url != "http://10.0.0.7:1347/" {
		t.Errorf("probed %q: the address is what is known to work before DNS propagates", h.probe.url)
	}
}

// A hostname the deploy could not set (DNS refused, or not yet
// propagated), or one left pointing at an address Hetzner has since given
// to someone else, is no way to reach the environment: status gives the
// address instead, so whatever reads the URL — the page's link, the
// drill, a performance run — reaches the box.
func TestStatusIsByAddressWhenTheNameDoesNotPointAtTheServer(t *testing.T) {
	h := newHarness()
	dns := h.withDNS()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7"}
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.Host != "" || st.URL != "http://10.0.0.7:1347/" {
		t.Errorf("name not set: status = %+v, want by address", st)
	}
	dns.records["prod.gobank.test"] = "10.0.0.8" // stale: someone else's server now
	st, _ = h.d.Status(context.Background(), prod)
	if st.Host != "" || st.URL != "http://10.0.0.7:1347/" {
		t.Errorf("name stale: status = %+v, want by address", st)
	}
}

// --- Capacity -----------------------------------------------------------------

// Hetzner sometimes has no capacity for a server type at a location
// ("error during placement"): the next location is tried, in order, and
// only when every one is full does create fail.
func TestCreateTriesTheNextLocationWhenOneHasNoCapacity(t *testing.T) {
	h := newHarness()
	h.cloud.full["fsn1"] = true
	srv, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "large", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Location != "nbg1" || len(h.cloud.created) != 2 || h.cloud.created[0].Location != "fsn1" || h.cloud.created[1].Location != "nbg1" {
		t.Errorf("server in %s after creates %+v; want fsn1 tried then nbg1", srv.Location, h.cloud.created)
	}
	if !strings.Contains(h.out.String(), "no capacity for cx53 in fsn1: trying nbg1") {
		t.Errorf("output should say why the location changed:\n%s", h.out.String())
	}

	h = newHarness()
	for _, loc := range []string{"fsn1", "nbg1", "hel1"} {
		h.cloud.full[loc] = true
	}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "large", Create: true}); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("every location full: err = %v, want ErrNoCapacity", err)
	}
	if len(h.cloud.servers) != 0 {
		t.Error("nothing should be left billing")
	}
}

// When no location has the type asked for, the next size down is tried,
// at every location again, down to the smallest: a measurement on a
// smaller box beats none, and the server's actual type is on record.
func TestCreateStepsDownASizeWhenNoLocationHasIt(t *testing.T) {
	h := newHarness()
	for _, loc := range []string{"fsn1", "nbg1", "hel1"} {
		h.cloud.full["cx53@"+loc] = true
	}
	srv, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "large", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Type != "cx43" || srv.Location != "fsn1" || len(h.cloud.created) != 4 {
		t.Errorf("server %s in %s after %d creates; want cx43 in fsn1 after three cx53 tries", srv.Type, srv.Location, len(h.cloud.created))
	}
	if !strings.Contains(h.out.String(), "no capacity for cx53 anywhere: trying cx43") {
		t.Errorf("output should say the size changed:\n%s", h.out.String())
	}

	h = newHarness()
	h.cloud.full["cx23@fsn1"] = true // a raw type, not a preset: no size to step down to
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "cx23", Create: true}); err != nil {
		t.Fatalf("a raw type still tries the other locations: %v", err)
	}
	h = newHarness()
	for _, loc := range []string{"fsn1", "nbg1", "hel1"} {
		h.cloud.full["cx23@"+loc] = true
	}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("the smallest type full everywhere: err = %v, want ErrNoCapacity", err)
	}
}

func TestWithoutDNSStatusIsByAddress(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7"}
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.Host != "" || st.URL != "http://10.0.0.7:1347/" {
		t.Errorf("status = %+v", st)
	}
}

// --- ServerType --------------------------------------------------------------

func TestServerTypeScalePresets(t *testing.T) {
	cases := []struct{ scale, typ, goarch string }{
		{"small", "cx23", "amd64"},
		{"medium", "cx33", "amd64"},
		{"large", "cx53", "amd64"},
		{"xl", "ccx33", "amd64"},
		{"cax31", "cax31", "arm64"},
		{"ccx43", "ccx43", "amd64"},
	}
	for _, c := range cases {
		typ, goarch := ServerType(c.scale)
		if typ != c.typ || goarch != c.goarch {
			t.Errorf("ServerType(%q) = %q,%q want %q,%q", c.scale, typ, goarch, c.typ, c.goarch)
		}
	}
}

// Environments are whatever gobank servers exist in the cloud project,
// whoever created them (the old scripts made gobank-demo), by name.
func TestEnvironmentsAreTheGobankServersInTheProject(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod"}
	h.cloud.servers["gobank-demo"] = &Server{Name: "gobank-demo"}
	h.cloud.servers["woodpecker-ci"] = &Server{Name: "woodpecker-ci"}
	envs, err := h.d.Environments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []Environment{{Name: "demo"}, {Name: "prod"}}; !slices.Equal(envs, want) {
		t.Errorf("environments = %v, want %v", envs, want)
	}
}

func TestEnvironmentServerName(t *testing.T) {
	if got := prod.ServerName(); got != "gobank-prod" {
		t.Errorf("ServerName = %q", got)
	}
}

// --- Up -----------------------------------------------------------------------

func TestUpRefusesToCreateWithoutExplicitRequest(t *testing.T) {
	h := newHarness()
	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"})
	if !errors.Is(err, ErrNeedsCreate) {
		t.Fatalf("err = %v, want ErrNeedsCreate", err)
	}
	if len(h.cloud.created) != 0 || h.build.builds != 0 {
		t.Errorf("nothing should be created or built: created=%v builds=%d", h.cloud.created, h.build.builds)
	}
}

func TestUpRefusesToCreateWithNoSSHKeys(t *testing.T) {
	h := newHarness()
	h.cloud.sshKeys = nil
	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true})
	if !errors.Is(err, ErrNoSSHKeys) {
		t.Fatalf("err = %v, want ErrNoSSHKeys", err)
	}
	if len(h.cloud.created) != 0 {
		t.Errorf("server must not be created without ssh keys")
	}
}

func TestUpCreatesServerThenDeploys(t *testing.T) {
	h := newHarness()
	h.dialer.failFirst = 3 // ssh not up for the first few tries
	h.probe.failFirst = 2

	srv, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "medium", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if srv.IP != "10.0.0.7" {
		t.Errorf("server = %+v", srv)
	}

	if !h.cloud.firewalls["gobank-prod"] {
		t.Error("firewall not created")
	}
	if len(h.cloud.created) != 1 {
		t.Fatalf("created = %d servers", len(h.cloud.created))
	}
	spec := h.cloud.created[0]
	if spec.Name != "gobank-prod" || spec.Type != "cx33" || spec.Firewall != "gobank-prod" {
		t.Errorf("spec = %+v", spec)
	}
	if strings.Join(spec.SSHKeys, ",") != "laptop,jeeves" {
		t.Errorf("ssh keys = %v", spec.SSHKeys)
	}
	if !strings.HasPrefix(spec.UserData, "#cloud-config") {
		t.Errorf("user data should be the cloud-init file, got %.20q", spec.UserData)
	}
	if spec.Labels["project"] != "gobank" || spec.Labels["environment"] != "prod" {
		t.Errorf("labels = %v", spec.Labels)
	}

	if h.build.goarch != "amd64" {
		t.Errorf("built for %q", h.build.goarch)
	}
	if h.dialer.fresh[0] != true {
		t.Error("a freshly created server must forget any pinned host key")
	}
	if h.dialer.dials != 4 {
		t.Errorf("dials = %d, want 4 (3 failures then success)", h.dialer.dials)
	}

	runs := strings.Join(h.host.runs, "\n")
	for _, want := range []string{"cloud-init status --wait", "install -m 0755", "systemctl restart gobank-demo"} {
		if !strings.Contains(runs, want) {
			t.Errorf("host runs missing %q:\n%s", want, runs)
		}
	}
	if len(h.host.puts) != 1 || h.host.puts[0] != "/tmp/demo -> /opt/gobank/demo.new" {
		t.Errorf("puts = %v", h.host.puts)
	}
	if h.probe.url != "http://10.0.0.7:1347/" {
		t.Errorf("probed %q", h.probe.url)
	}
	if !strings.Contains(h.out.String(), "http://10.0.0.7:1347/") {
		t.Errorf("output should print the URL:\n%s", h.out.String())
	}
}

func TestUpReusesExistingServer(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cax31"}

	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.cloud.created) != 0 {
		t.Error("must not create a second server")
	}
	if h.build.goarch != "arm64" {
		t.Errorf("must build for the existing server's architecture, got %q", h.build.goarch)
	}
	if h.dialer.fresh[0] != false {
		t.Error("an existing server keeps its pinned host key")
	}
	if strings.Contains(strings.Join(h.host.runs, "\n"), "cloud-init") {
		t.Error("no cloud-init wait on a redeploy")
	}
}

// The demo rebuilds the bank from its database before it listens: a
// minute and a half at four thousand days, growing with the history. The
// check must outlast that.
func TestUpWaitsForASlowResume(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx23"}
	h.probe.failFirst = 40 // answers after 200s of probing

	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatalf("a resume of a few minutes must pass the check: %v", err)
	}
}

// The resume grows with the history, so the check is bounded by the
// service staying alive rather than by a short clock: a service that is
// up and has not been restarted by systemd is still starting.
func TestUpWaitsForALongResumeWhileTheServiceIsAlive(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx23"}
	h.probe.failFirst = 240 // answers after 20 minutes of probing

	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatalf("a resume of twenty minutes must pass the check: %v", err)
	}
	if !strings.Contains(h.out.String(), "still starting") {
		t.Errorf("the log must say the wait is deliberate:\n%s", h.out.String())
	}
}

func TestUpFailsAsSoonAsTheServiceDies(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx23"}
	h.probe.failFirst = 1000
	h.host.failing = "systemctl is-active"

	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"})
	if !errors.Is(err, ErrNotServing) {
		t.Fatalf("err = %v, want ErrNotServing", err)
	}
	if !strings.Contains(err.Error(), "stopped or restarted") {
		t.Errorf("the error must say the service died: %v", err)
	}
	if h.slept > time.Minute {
		t.Errorf("a dead service is reported at once, not after %s", h.slept)
	}
}

func TestUpFailsWhenServiceNeverAnswers(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx23"}
	h.probe.failFirst = 100000

	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"})
	if !errors.Is(err, ErrNotServing) {
		t.Fatalf("err = %v, want ErrNotServing", err)
	}
	if h.slept < 30*time.Minute || h.slept > 31*time.Minute {
		t.Errorf("an alive service that never answers is given up after half an hour, not %s", h.slept)
	}
}

func TestUpFailsWhenSSHNeverAnswers(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx23"}
	h.dialer.failFirst = 1000

	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"})
	if err == nil || !strings.Contains(err.Error(), "ssh") {
		t.Fatalf("err = %v, want an ssh error", err)
	}
}

// --- Down ---------------------------------------------------------------------

func TestDownDeletesServerAndFirewall(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9"}
	h.cloud.firewalls["gobank-prod"] = true

	if err := h.d.Down(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	if len(h.cloud.deleted) != 1 || len(h.cloud.fwDeleted) != 1 {
		t.Errorf("deleted=%v fwDeleted=%v", h.cloud.deleted, h.cloud.fwDeleted)
	}
}

func TestDownIsIdempotent(t *testing.T) {
	h := newHarness()
	if err := h.d.Down(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	if len(h.cloud.deleted) != 0 || len(h.cloud.fwDeleted) != 0 {
		t.Error("nothing to delete")
	}
	if !strings.Contains(h.out.String(), "nothing left billing") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

// --- Releases -------------------------------------------------------------------

type fakeRepo struct {
	tag       string
	err       error
	built     bool     // the tag's release carries demo binaries
	older     []string // earlier tags whose releases also carry binaries
	downloads []string // "<tag> <goarch>"
}

func (r *fakeRepo) LatestTag(context.Context) (string, error) { return r.tag, r.err }

func (r *fakeRepo) Download(_ context.Context, tag, goarch, dst string) error {
	if r.err != nil {
		return r.err
	}
	if !r.built || (tag != r.tag && !slices.Contains(r.older, tag)) {
		return ErrNoRelease
	}
	r.downloads = append(r.downloads, tag+" "+goarch)
	return os.WriteFile(dst, []byte(tag+" "+goarch), 0o755)
}

// fakeStore is a release store: what it has, and what was put into it.
type fakeStore struct {
	fakeBuilder
	has  map[string]bool
	puts []string // "<version> <goarch> <binary content>"
	uses []string // versions made the latest without a put
}

func (s *fakeStore) Has(version string) bool { return s.has[version] }

func (s *fakeStore) Use(version string) error {
	if !s.has[version] {
		return fmt.Errorf("store lacks %s", version)
	}
	s.uses = append(s.uses, version)
	s.available = version
	return nil
}

func (s *fakeStore) Put(version, goarch, binary string) error {
	b, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	s.has[version] = true
	s.puts = append(s.puts, version+" "+goarch+" "+string(b))
	return nil
}

// withStore makes the harness deploy from a release store holding v0.3.44.
func (h *harness) withStore() *fakeStore {
	st := &fakeStore{fakeBuilder: fakeBuilder{available: "v0.3.44"}, has: map[string]bool{"v0.3.44": true}}
	h.d.Build, h.d.Store = st, st
	return st
}

// --- Fetch: the build stage before a deploy from a store ------------------------

// A release the repo has built that the store lacks is fetched into the
// store, for every server architecture, before the deploy; the deploy
// then carries it.
func TestUpFetchesTheReposNewestReleaseIntoTheStore(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23"}
	st := h.withStore()
	h.d.Repo = &fakeRepo{tag: "v0.3.48", built: true}

	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
		t.Fatal(err)
	}
	want := []string{"v0.3.48 amd64 v0.3.48 amd64", "v0.3.48 arm64 v0.3.48 arm64"}
	if !slices.Equal(st.puts, want) {
		t.Errorf("puts = %v, want %v", st.puts, want)
	}
	if st.builds != 1 {
		t.Errorf("builds = %d", st.builds)
	}
	if !strings.Contains(h.out.String(), "== fetch v0.3.48") {
		t.Errorf("output should show the fetch stage:\n%s", h.out.String())
	}
}

// A release the store already has is not fetched again, so putting an
// older release back (a rollback) is what the next deploy carries.
func TestUpDoesNotRefetchAReleaseTheStoreHas(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23"}
	st := h.withStore()
	st.has["v0.3.48"] = true // fetched earlier; latest was then put back to v0.3.44
	repo := &fakeRepo{tag: "v0.3.48", built: true}
	h.d.Repo = repo

	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
		t.Fatal(err)
	}
	if len(repo.downloads) != 0 || len(st.puts) != 0 {
		t.Errorf("downloads = %v, puts = %v", repo.downloads, st.puts)
	}
	if strings.Contains(h.out.String(), "== fetch") {
		t.Errorf("no fetch stage expected:\n%s", h.out.String())
	}
}

// A tag without built binaries (releases before the build stage existed)
// or a repo that cannot be reached is a notice; the deploy carries what
// the store has.
func TestAReleaseThatCannotBeFetchedIsANoticeNotAFailure(t *testing.T) {
	for name, repo := range map[string]*fakeRepo{
		"tag without binaries": {tag: "v0.3.48"},
		"repo down":            {tag: "v0.3.48", err: errors.New("forge down")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23"}
			st := h.withStore()
			h.d.Repo = repo
			if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
				t.Fatal(err)
			}
			if len(st.puts) != 0 || st.builds != 1 {
				t.Errorf("puts = %v, builds = %d", st.puts, st.builds)
			}
			if out := h.out.String(); !strings.Contains(out, "v0.3.44") {
				t.Errorf("output should say the store's v0.3.44 is deployed instead:\n%s", out)
			}
		})
	}
}

// Without a store (the laptop builds from its checkout) nothing is fetched.
func TestUpWithoutAStoreDoesNotFetch(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23"}
	repo := &fakeRepo{tag: "v0.3.48", built: true}
	h.d.Repo = repo
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
		t.Fatal(err)
	}
	if len(repo.downloads) != 0 {
		t.Errorf("downloads = %v", repo.downloads)
	}
}

// Fetch, on its own (after the repo's release), puts the newest release
// into the store and makes it the latest, and says which it was.
func TestFetchPutsTheReposNewestReleaseIntoTheStore(t *testing.T) {
	h := newHarness()
	st := h.withStore()
	h.d.Repo = &fakeRepo{tag: "v0.3.48", built: true}

	tag, err := h.d.Fetch(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v0.3.48" {
		t.Errorf("tag = %q", tag)
	}
	want := []string{"v0.3.48 amd64 v0.3.48 amd64", "v0.3.48 arm64 v0.3.48 arm64"}
	if !slices.Equal(st.puts, want) {
		t.Errorf("puts = %v, want %v", st.puts, want)
	}
}

// Fetching a release the store already has is not an error: it is there.
func TestFetchOfAReleaseTheStoreHasDoesNothing(t *testing.T) {
	h := newHarness()
	st := h.withStore()
	st.has["v0.3.48"] = true
	repo := &fakeRepo{tag: "v0.3.48", built: true}
	h.d.Repo = repo

	tag, err := h.d.Fetch(context.Background(), "")
	if err != nil || tag != "v0.3.48" {
		t.Errorf("Fetch = %q, %v", tag, err)
	}
	if len(repo.downloads) != 0 || len(st.puts) != 0 {
		t.Errorf("downloads = %v, puts = %v", repo.downloads, st.puts)
	}
}

// Asked for directly, a release that cannot be fetched is a failure, so
// whoever asked (the release's post_release step) hears of it.
func TestFetchThatCannotFetchFails(t *testing.T) {
	for name, tc := range map[string]struct {
		repo *fakeRepo
		is   error
	}{
		"tag without binaries": {&fakeRepo{tag: "v0.3.48"}, ErrNoRelease},
		"repo down":            {&fakeRepo{tag: "v0.3.48", err: errors.New("forge down")}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			st := h.withStore()
			h.d.Repo = tc.repo
			_, err := h.d.Fetch(context.Background(), "")
			if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Errorf("err = %v, want %v", err, tc.is)
			}
			if len(st.puts) != 0 {
				t.Errorf("puts = %v", st.puts)
			}
		})
	}
}

func TestFetchWithoutAStoreFails(t *testing.T) {
	h := newHarness()
	h.d.Repo = &fakeRepo{tag: "v0.3.48", built: true}
	if _, err := h.d.Fetch(context.Background(), ""); !errors.Is(err, ErrNoStore) {
		t.Errorf("err = %v, want ErrNoStore", err)
	}
}

// Releases says whether the next up fetches a lagging release itself.
func TestReleasesSaysWhetherTheNextUpFetches(t *testing.T) {
	h := newHarness()
	h.d.Repo = &fakeRepo{tag: "v0.3.48"}
	h.build.available = "v0.3.44"
	if rel, _ := h.d.Releases(context.Background()); rel.Fetches {
		t.Errorf("a checkout does not fetch: %+v", rel)
	}
	h.withStore()
	if rel, _ := h.d.Releases(context.Background()); !rel.Fetches {
		t.Errorf("a store fetches: %+v", rel)
	}
}

// Where versions stand: the newest tag on the repo against what this
// host would deploy. The store or checkout can lag the repo.
func TestReleasesComparesTheRepoWithWhatCanBeDeployedFromHere(t *testing.T) {
	h := newHarness()
	h.d.Repo = &fakeRepo{tag: "v0.3.48"}
	h.build.available = "v0.3.44"
	rel, err := h.d.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Repo != "v0.3.48" || rel.Available != "v0.3.44" || !rel.Lagging() {
		t.Errorf("releases = %+v", rel)
	}
	h.build.available = "v0.3.48"
	rel, _ = h.d.Releases(context.Background())
	if rel.Lagging() {
		t.Errorf("up to date: %+v", rel)
	}
	// A checkout past the tag (git describe with a suffix) is not lagging.
	h.build.available = "v0.3.48-2-gabc1234"
	if rel, _ = h.d.Releases(context.Background()); rel.Lagging() {
		t.Errorf("ahead of the tag is not lagging: %+v", rel)
	}
}

func TestReleasesWithoutARepoOrWhenItIsDown(t *testing.T) {
	h := newHarness()
	h.build.available = "v0.3.44"
	rel, err := h.d.Releases(context.Background())
	if err != nil || rel.Repo != "" || rel.Lagging() {
		t.Errorf("no repo: %+v, %v", rel, err)
	}
	h.d.Repo = &fakeRepo{err: errors.New("forge down")}
	rel, err = h.d.Releases(context.Background())
	if err == nil || rel.Available != "v0.3.44" {
		t.Errorf("repo down: still says what is available here: %+v, %v", rel, err)
	}
}

// --- Status -------------------------------------------------------------------

// Status compares what is running with what the next up would deploy.
func TestStatusReportsRunningAndAvailableVersions(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7"}
	h.probe.version = "v0.3.46"
	h.build.available = "v0.3.47"
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "v0.3.46" || st.Available != "v0.3.47" || !st.Behind() {
		t.Errorf("status = %+v", st)
	}
	h.probe.version = "v0.3.47"
	st, _ = h.d.Status(context.Background(), prod)
	if st.Behind() {
		t.Errorf("same version is not behind: %+v", st)
	}
	h.build.available = ""
	st, _ = h.d.Status(context.Background(), prod)
	if st.Behind() {
		t.Errorf("nothing known to be available is not behind: %+v", st)
	}
}

func TestStatusNotProvisioned(t *testing.T) {
	h := newHarness()
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.Server != nil || st.Serving {
		t.Errorf("status = %+v", st)
	}
}

func TestStatusServing(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9"}
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.Server == nil || !st.Serving || st.URL != "http://10.0.0.9:1347/" {
		t.Errorf("status = %+v", st)
	}
}

// --- Memory sizing ------------------------------------------------------------

func TestAppMemoryLimitIsHalfTheBoxWithLocalPostgres(t *testing.T) {
	cases := []struct {
		ramGB float64
		want  string
	}{
		{4, "2048MB"},
		{8, "4096MB"},
		{32, "16384MB"},
		{0, ""}, // unknown: leave the app's own default
	}
	for _, c := range cases {
		if got := AppMemoryLimit(c.ramGB); got != c.want {
			t.Errorf("AppMemoryLimit(%v) = %q, want %q", c.ramGB, got, c.want)
		}
	}
}

func TestUpWritesMemoryLimitSizedToTheServer(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33", MemoryGB: 8}

	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatal(err)
	}
	runs := strings.Join(h.host.runs, "\n")
	if !strings.Contains(runs, "GOBANK_MEMORY_LIMIT=4096MB") || !strings.Contains(runs, "/etc/gobank/deploy.env") {
		t.Errorf("install should write the sized limit to the deploy env file:\n%s", runs)
	}
}

func TestUpOnAServerOfUnknownSizeLeavesTheDefault(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33"}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(h.host.runs, "\n"), "GOBANK_MEMORY_LIMIT=") {
		t.Error("no RAM figure, so no limit should be written")
	}
}

// A named tag the store already has becomes the next deploy without a
// download: the rollback path (ADR-0003 in gobank), from the store.
func TestFetchOfANamedTagMakesItTheNextDeploy(t *testing.T) {
	h := newHarness()
	st := h.withStore()
	st.has["v0.3.48"] = true
	st.available = "v0.3.48"
	repo := &fakeRepo{tag: "v0.3.48", built: true}
	h.d.Repo = repo

	tag, err := h.d.Fetch(context.Background(), "v0.3.44")
	if err != nil || tag != "v0.3.44" {
		t.Fatalf("Fetch = %q, %v", tag, err)
	}
	if len(repo.downloads) != 0 || len(st.puts) != 0 {
		t.Errorf("a release the store has is not fetched again: downloads = %v, puts = %v", repo.downloads, st.puts)
	}
	if !slices.Equal(st.uses, []string{"v0.3.44"}) || st.Available() != "v0.3.44" {
		t.Errorf("uses = %v, available = %s; want v0.3.44 as the next deploy", st.uses, st.Available())
	}
}

// A named tag the store lacks is downloaded from the repo's release of
// that tag, not the newest, and becomes the latest as a put does.
func TestFetchOfANamedTagTheStoreLacksDownloadsThatTag(t *testing.T) {
	h := newHarness()
	st := h.withStore()
	repo := &fakeRepo{tag: "v0.3.48", built: true, older: []string{"v0.3.47"}}
	h.d.Repo = repo

	tag, err := h.d.Fetch(context.Background(), "v0.3.47")
	if err != nil || tag != "v0.3.47" {
		t.Fatalf("Fetch = %q, %v", tag, err)
	}
	want := []string{"v0.3.47 amd64 v0.3.47 amd64", "v0.3.47 arm64 v0.3.47 arm64"}
	if !slices.Equal(st.puts, want) {
		t.Errorf("puts = %v, want %v", st.puts, want)
	}
}

// The demo finishes the simulated day in progress before it exits, which
// on the Hetzner box can take minutes; the unit's stop timeout must
// outlast that or the stop becomes a SIGKILL and the day's writes are
// lost. Fresh servers get it from cloud-init, existing ones from the
// install script.
func TestStopTimeoutOutlastsADay(t *testing.T) {
	const want = "TimeoutStopSec=900"
	if !strings.Contains(cloudInit, want) {
		t.Errorf("cloud-init unit lacks %s", want)
	}
	if script := installScript("", ""); !strings.Contains(script, want) || !strings.Contains(script, "grep -q 'TimeoutStopSec=") {
		t.Errorf("install script does not add %s to an existing unit:\n%s", want, script)
	}
}
