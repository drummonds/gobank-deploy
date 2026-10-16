package deploy

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
)

var home = []string{"77.104.145.192/29"}

// The firewall admits ssh and the demo from the admitted ranges (home)
// only, and ping from anywhere. Traffic on the private network does not
// pass the firewall at all.
func TestUpAdmitsSSHAndTheDemoFromTheAdmittedRangesOnly(t *testing.T) {
	h := newHarness()
	h.d.Admit = home
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatal(err)
	}
	want := []FirewallRule{
		{Description: "ssh", Protocol: "tcp", Port: "22", Sources: home},
		{Description: "demo", Protocol: "tcp", Port: "1347", Sources: home},
		{Description: "ping", Protocol: "icmp"},
	}
	if got := h.cloud.rules["gobank-prod"]; !slices.EqualFunc(got, want, equalRule) {
		t.Errorf("rules = %+v, want %+v", got, want)
	}
	if !strings.Contains(h.out.String(), "ssh 22 from 77.104.145.192/29") {
		t.Errorf("the log should say who is admitted:\n%s", h.out.String())
	}
}

// The rules are declared, not created once: an up of a server that
// exists sets its firewall to them.
func TestUpReconcilesTheFirewallOfAnExistingServer(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23", PrivateIP: "10.0.1.5"}
	h.cloud.firewalls["gobank-prod"] = true
	h.cloud.rules["gobank-prod"] = []FirewallRule{{Description: "ssh", Protocol: "tcp", Port: "22"}}
	h.d.Admit = home
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
		t.Fatal(err)
	}
	if got := h.cloud.rules["gobank-prod"]; len(got) != 3 || !slices.Equal(got[0].Sources, home) {
		t.Errorf("rules = %+v; want the declared three, ssh from home", got)
	}
}

// With no admitted range the firewall is open to the world as before,
// and the log says so.
func TestWithoutAdmittedRangesTheFirewallIsOpen(t *testing.T) {
	h := newHarness()
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatal(err)
	}
	for _, r := range h.cloud.rules["gobank-prod"] {
		if len(r.Sources) != 0 {
			t.Errorf("rule %+v; want no sources (anywhere)", r)
		}
	}
	if !strings.Contains(h.out.String(), "from anywhere") {
		t.Errorf("the log should say the box is open:\n%s", h.out.String())
	}
}

// Every server is on the private network gobank (10.0.0.0/16, its subnet
// 10.0.1.0/24 in eu-central, where every location the deployer tries
// is): a new one is created on it, an existing one without a private
// address is attached.
func TestUpPutsTheServerOnThePrivateNetwork(t *testing.T) {
	h := newHarness()
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatal(err)
	}
	want := Network{Name: "gobank", IPRange: "10.0.0.0/16", Subnet: "10.0.1.0/24", Zone: "eu-central"}
	if len(h.cloud.networks) != 1 || h.cloud.networks[0] != want {
		t.Errorf("networks = %+v, want %+v", h.cloud.networks, want)
	}
	if h.cloud.created[0].Network != "gobank" || h.cloud.servers["gobank-prod"].PrivateIP == "" {
		t.Errorf("created %+v; want it on the network with a private address", h.cloud.created[0])
	}
	if len(h.cloud.attached) != 0 {
		t.Errorf("a new server is created on the network, not attached: %v", h.cloud.attached)
	}

	h = newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.7", Type: "cx23"}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.cloud.attached, []string{"gobank-prod gobank"}) {
		t.Errorf("attached = %v; want the existing server attached", h.cloud.attached)
	}
}

// A loader is a box for the load test: on the private network, ssh from
// the admitted ranges and nothing else, no PostgreSQL or demo, this
// program copied onto it (so the box's architecture is this host's),
// and an expiry so a run that dies does not leave it billing.
func TestLoaderIsABoxOnTheNetworkWithThisProgramOnIt(t *testing.T) {
	h := newHarness()
	h.d.Admit = home
	h.d.Self, h.d.SelfArch = "/usr/bin/gobank-deploy", "arm64"
	env := Environment{Name: "load1-loader"}
	srv, err := h.d.Loader(context.Background(), env, noon)
	if err != nil {
		t.Fatal(err)
	}
	spec := h.cloud.created[0]
	if spec.Name != "gobank-load1-loader" || spec.Type != "cax11" || spec.Network != "gobank" || spec.UserData != "" || spec.Firewall != "gobank-load1-loader" {
		t.Errorf("spec = %+v; want an arm64 box (as this host) on the network with no cloud-init", spec)
	}
	if spec.Labels["role"] != "loader" || spec.Labels["expires"] != "20260927T120000Z" || spec.Labels["project"] != "gobank" {
		t.Errorf("labels = %v", spec.Labels)
	}
	want := []FirewallRule{{Description: "ssh", Protocol: "tcp", Port: "22", Sources: home}, {Description: "ping", Protocol: "icmp"}}
	if got := h.cloud.rules["gobank-load1-loader"]; !slices.EqualFunc(got, want, equalRule) {
		t.Errorf("loader rules = %+v, want %+v", got, want)
	}
	if !slices.Equal(h.host.puts, []string{"/usr/bin/gobank-deploy -> /usr/local/bin/gobank-deploy"}) || !slices.ContainsFunc(h.host.runs, func(c string) bool { return strings.Contains(c, "chmod 0755 /usr/local/bin/gobank-deploy") }) {
		t.Errorf("puts %v runs %v; want this program installed", h.host.puts, h.host.runs)
	}
	if srv == nil || srv.PrivateIP == "" {
		t.Errorf("loader = %+v", srv)
	}
	if h.build.builds != 0 {
		t.Error("a loader carries no demo build")
	}

	h.d.SelfArch = "amd64"
	h.cloud.servers = map[string]*Server{}
	if _, err := h.d.Loader(context.Background(), Environment{Name: "load2-loader"}, noon); err != nil {
		t.Fatal(err)
	}
	if h.cloud.created[1].Type != "cx23" {
		t.Errorf("an amd64 host's loader is %s; want cx23", h.cloud.created[1].Type)
	}
}

// The load test runs on the loader against the target's private address,
// the app password in the command's environment, and its result is read
// off the command's output.
func TestJourneyRunsOnTheLoaderAgainstThePrivateAddress(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-load1-loader"] = &Server{Name: "gobank-load1-loader", IP: "10.0.0.8", PrivateIP: "10.0.1.3"}
	out, _ := json.Marshal(load.Result{Capacity: 50, Breach: "100 users: login p95 2.4s over 2s"})
	h.host.output = string(out)
	target := &Server{Name: "gobank-load1", PrivateIP: "10.0.1.2", Labels: map[string]string{"app-password": "pw123"}}
	res, err := h.d.Journey(context.Background(), Environment{Name: "load1-loader"}, target, JourneyOptions{Customers: 2000, Ladder: []int{1, 5, 50, 100}, Reps: 10, Budget: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Capacity != 50 || res.Breach == "" {
		t.Errorf("result = %+v", res)
	}
	cmd := h.host.runs[len(h.host.runs)-1]
	for _, want := range []string{"GOBANK_APP_PASSWORD='pw123'", "/usr/local/bin/gobank-deploy journey", "-base http://10.0.1.2:1347/", "-customers 2000", "-ladder 1,5,50,100", "-reps 10", "-budget 2s"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q lacks %q", cmd, want)
		}
	}
}

// A loader is not a demo: its status is the server alone, never probed.
func TestALoadersStatusIsNotProbed(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-load1-loader"] = &Server{Name: "gobank-load1-loader", IP: "10.0.0.8", Labels: map[string]string{"role": "loader"}}
	st, err := h.d.Status(context.Background(), Environment{Name: "load1-loader"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Server == nil || st.Serving || h.probe.probes != 0 || !st.Loader {
		t.Errorf("status = %+v probes %d; want the server, a loader, unprobed", st, h.probe.probes)
	}
}

func equalRule(a, b FirewallRule) bool {
	return a.Description == b.Description && a.Protocol == b.Protocol && a.Port == b.Port && slices.Equal(a.Sources, b.Sources)
}
