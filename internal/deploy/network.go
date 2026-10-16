package deploy

import (
	"context"
	"fmt"
	"strings"
)

// Network is a private network the servers are attached to: traffic on it
// stays inside the provider and does not pass the servers' firewalls.
type Network struct {
	Name    string
	IPRange string // the network's
	Subnet  string // the servers' addresses come from here
	Zone    string // the provider's network zone; every location tried must be in it
}

// PrivateNetwork is the one network every gobank server is on, so a
// loader reaches a demo without the demo's port being open to the world.
// eu-central holds fsn1, nbg1 and hel1, the locations tried.
var PrivateNetwork = Network{Name: "gobank", IPRange: "10.128.0.0/16", Subnet: "10.128.1.0/24", Zone: "eu-central"}

// network makes sure the private network exists.
func (d *Deployer) network(ctx context.Context) error {
	if err := d.Cloud.EnsureNetwork(ctx, PrivateNetwork); err != nil {
		return fmt.Errorf("network %s: %w", PrivateNetwork.Name, err)
	}
	return nil
}

// attach puts a server made before the network existed on it.
func (d *Deployer) attach(ctx context.Context, srv *Server) error {
	if srv.PrivateIP != "" {
		return nil
	}
	d.printf("== attach %s to network %s\n", srv.Name, PrivateNetwork.Name)
	if err := d.Cloud.AttachNetwork(ctx, srv.Name, PrivateNetwork.Name); err != nil {
		return fmt.Errorf("attach %s to network %s: %w", srv.Name, PrivateNetwork.Name, err)
	}
	return nil
}

// firewall sets the server's firewall to the rules, saying who they
// admit.
func (d *Deployer) firewall(ctx context.Context, name string, rules []FirewallRule) error {
	var says []string
	for _, r := range rules {
		from := "anywhere"
		if len(r.Sources) > 0 {
			from = strings.Join(r.Sources, ", ")
		}
		says = append(says, strings.TrimSpace(r.Description+" "+r.Port)+" from "+from)
	}
	d.printf("== firewall %s: %s\n", name, strings.Join(says, "; "))
	if err := d.Cloud.SetFirewall(ctx, name, rules); err != nil {
		return fmt.Errorf("firewall %s: %w", name, err)
	}
	return nil
}
