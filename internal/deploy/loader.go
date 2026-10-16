package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
)

// roleLabel marks what a server is for: a loader carries roleLoader; a
// demo carries none.
const (
	roleLabel  = "role"
	roleLoader = "loader"
	// loaderBinary is where a loader keeps this program.
	loaderBinary = "/usr/local/bin/gobank-deploy"
)

// loaderType is the smallest box of the architecture: the program copied
// onto it is this host's own executable, so the loader must run it.
func loaderType(goarch string) string {
	if goarch == "arm64" {
		return "cax11"
	}
	return "cx23"
}

// Loader creates the environment's server as a loader for a load test: on
// the private network, ssh from the admitted ranges and nothing else, no
// PostgreSQL or demo, this program installed on it, and an expiry so a
// run that dies does not leave it billing.
func (d *Deployer) Loader(ctx context.Context, env Environment, expires time.Time) (*Server, error) {
	name := env.ServerName()
	if err := d.network(ctx); err != nil {
		return nil, err
	}
	if err := d.firewall(ctx, name, []FirewallRule{
		{Description: "ssh", Protocol: "tcp", Port: "22", Sources: d.Admit},
		{Description: "ping", Protocol: "icmp"},
	}); err != nil {
		return nil, err
	}
	self, arch := d.Self, d.SelfArch
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("this program's executable: %w", err)
		}
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	srv, err := d.Cloud.Server(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("lookup server %s: %w", name, err)
	}
	fresh := srv == nil
	if fresh {
		l := map[string]string{"project": "gobank", "environment": env.Name, roleLabel: roleLoader}
		if !expires.IsZero() {
			l[expiresLabel] = expires.UTC().Format(expiresLayout)
		}
		if srv, err = d.place(ctx, CreateSpec{Name: name, Type: loaderType(arch), Firewall: name, Network: PrivateNetwork.Name, Labels: l}); err != nil {
			return nil, err
		}
	} else if err := d.attach(ctx, srv); err != nil {
		return nil, err
	}
	d.printf("== wait for ssh (%s)\n", srv.IP)
	host, err := d.waitSSH(ctx, srv.IP, fresh)
	if err != nil {
		return nil, err
	}
	d.printf("== install this program on the loader\n")
	if err := host.Put(ctx, self, loaderBinary); err != nil {
		return nil, fmt.Errorf("copy this program: %w", err)
	}
	if err := host.Run(ctx, "chmod 0755 "+loaderBinary); err != nil {
		return nil, err
	}
	return srv, nil
}

// JourneyOptions are the load test's: the books and the ladder.
type JourneyOptions struct {
	Customers int
	Ladder    []int
	Reps      int
	Budget    time.Duration
}

// Journey runs the load test on the loader against the target's private
// address and returns its result.
func (d *Deployer) Journey(ctx context.Context, loader Environment, target *Server, o JourneyOptions) (load.Result, error) {
	var res load.Result
	srv, err := d.Cloud.Server(ctx, loader.ServerName())
	if err != nil || srv == nil {
		return res, fmt.Errorf("loader %s: %v", loader.ServerName(), err)
	}
	if target.PrivateIP == "" {
		return res, fmt.Errorf("%s is not on the private network", target.Name)
	}
	host, err := d.Dial.Dial(ctx, srv.IP, false)
	if err != nil {
		return res, fmt.Errorf("ssh to the loader: %w", err)
	}
	var ladder []string
	for _, n := range o.Ladder {
		ladder = append(ladder, strconv.Itoa(n))
	}
	cmd := fmt.Sprintf("GOBANK_APP_PASSWORD=%s %s journey -base http://%s:%s/ -customers %d -ladder %s -reps %d -budget %s",
		shellQuote(target.AppPassword()), loaderBinary, target.PrivateIP, servicePort, o.Customers, strings.Join(ladder, ","), o.Reps, o.Budget)
	d.printf("== load %s from %s: ladder %s, %d journeys a user a rung, budget p95 %s a screen\n", target.Name, srv.Name, strings.Join(ladder, ","), o.Reps, o.Budget)
	out, err := host.Output(ctx, cmd, d.Out)
	if err != nil {
		return res, fmt.Errorf("load test: %w", err)
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return res, fmt.Errorf("load test result: %w", err)
	}
	return res, nil
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
