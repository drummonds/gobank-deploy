package deploy

import (
	"bytes"
	"context"
	"errors"
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
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{
		servers:   map[string]*Server{},
		firewalls: map[string]bool{},
		sshKeys:   []string{"laptop", "jeeves"},
	}
}

func (c *fakeCloud) Server(_ context.Context, name string) (*Server, error) {
	return c.servers[name], nil
}

func (c *fakeCloud) CreateServer(_ context.Context, spec CreateSpec) (*Server, error) {
	c.created = append(c.created, spec)
	s := &Server{Name: spec.Name, IP: "10.0.0.7", Type: spec.Type, Status: "running", Location: spec.Location, MemoryGB: 4}
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

type fakeHost struct {
	runs []string
	puts []string
}

func (h *fakeHost) Run(_ context.Context, cmd string) error {
	h.runs = append(h.runs, cmd)
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
	goarch string
	builds int
}

func (b *fakeBuilder) Build(_ context.Context, goarch string) (Release, error) {
	b.builds++
	b.goarch = goarch
	return Release{Version: "v0.3.44", Binary: "/tmp/demo"}, nil
}

type fakeProber struct {
	failFirst int
	probes    int
	url       string
}

func (p *fakeProber) Serving(_ context.Context, url string) bool {
	p.probes++
	p.url = url
	return p.probes > p.failFirst
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

var prod = Environment{Name: "prod"}

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

func TestUpFailsWhenServiceNeverAnswers(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx23"}
	h.probe.failFirst = 1000

	_, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"})
	if !errors.Is(err, ErrNotServing) {
		t.Fatalf("err = %v, want ErrNotServing", err)
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

// --- Status -------------------------------------------------------------------

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
