// Package hetzner implements deploy.Cloud on Hetzner Cloud via hcloud-go.
package hetzner

import (
	"context"
	"fmt"
	"net"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// Cloud is a deploy.Cloud backed by one Hetzner project.
type Cloud struct {
	c *hcloud.Client
}

// New connects with an API token (HCLOUD_TOKEN, supplied by tp secrets).
func New(token string) *Cloud {
	return &Cloud{c: hcloud.NewClient(hcloud.WithToken(token))}
}

func (h *Cloud) Server(ctx context.Context, name string) (*deploy.Server, error) {
	s, _, err := h.c.Server.GetByName(ctx, name)
	if err != nil || s == nil {
		return nil, err
	}
	return toServer(s), nil
}

func toServer(s *hcloud.Server) *deploy.Server {
	out := &deploy.Server{Name: s.Name, Status: string(s.Status)}
	if s.ServerType != nil {
		out.Type = s.ServerType.Name
		out.MemoryGB = float64(s.ServerType.Memory)
	}
	if s.PublicNet.IPv4.IP != nil {
		out.IP = s.PublicNet.IPv4.IP.String()
	}
	if s.Location != nil {
		out.Location = s.Location.Name
	}
	return out
}

func (h *Cloud) CreateServer(ctx context.Context, spec deploy.CreateSpec) (*deploy.Server, error) {
	st, _, err := h.c.ServerType.GetByName(ctx, spec.Type)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("unknown server type %q (see: hcloud server-type list)", spec.Type)
	}
	img, _, err := h.c.Image.GetByNameAndArchitecture(ctx, spec.Image, st.Architecture)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, fmt.Errorf("no image %q for %s", spec.Image, st.Architecture)
	}
	loc, _, err := h.c.Location.GetByName(ctx, spec.Location)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		return nil, fmt.Errorf("unknown location %q", spec.Location)
	}
	fw, _, err := h.c.Firewall.GetByName(ctx, spec.Firewall)
	if err != nil {
		return nil, err
	}
	if fw == nil {
		return nil, fmt.Errorf("firewall %q missing", spec.Firewall)
	}
	var keys []*hcloud.SSHKey
	for _, name := range spec.SSHKeys {
		k, _, err := h.c.SSHKey.GetByName(ctx, name)
		if err != nil {
			return nil, err
		}
		if k == nil {
			return nil, fmt.Errorf("ssh key %q missing", name)
		}
		keys = append(keys, k)
	}

	res, _, err := h.c.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       spec.Name,
		ServerType: st,
		Image:      img,
		Location:   loc,
		SSHKeys:    keys,
		UserData:   spec.UserData,
		Labels:     spec.Labels,
		Firewalls:  []*hcloud.ServerCreateFirewall{{Firewall: *fw}},
	})
	if err != nil {
		return nil, err
	}
	actions := append([]*hcloud.Action{res.Action}, res.NextActions...)
	if err := h.c.Action.WaitFor(ctx, actions...); err != nil {
		return nil, fmt.Errorf("server create action: %w", err)
	}
	// Re-read so the IP is populated.
	s, _, err := h.c.Server.GetByID(ctx, res.Server.ID)
	if err != nil {
		return nil, err
	}
	return toServer(s), nil
}

func (h *Cloud) DeleteServer(ctx context.Context, name string) error {
	s, _, err := h.c.Server.GetByName(ctx, name)
	if err != nil || s == nil {
		return err
	}
	res, _, err := h.c.Server.DeleteWithResult(ctx, s)
	if err != nil {
		return err
	}
	return h.c.Action.WaitFor(ctx, res.Action)
}

func (h *Cloud) FirewallExists(ctx context.Context, name string) (bool, error) {
	fw, _, err := h.c.Firewall.GetByName(ctx, name)
	return fw != nil, err
}

func anywhere() []net.IPNet {
	_, v4, _ := net.ParseCIDR("0.0.0.0/0")
	_, v6, _ := net.ParseCIDR("::/0")
	return []net.IPNet{*v4, *v6}
}

func (h *Cloud) CreateFirewall(ctx context.Context, name string, rules []deploy.FirewallRule) error {
	var hr []hcloud.FirewallRule
	for _, r := range rules {
		rule := hcloud.FirewallRule{
			Direction:   hcloud.FirewallRuleDirectionIn,
			SourceIPs:   anywhere(),
			Protocol:    hcloud.FirewallRuleProtocol(r.Protocol),
			Description: new(r.Description),
		}
		if r.Port != "" {
			rule.Port = new(r.Port)
		}
		hr = append(hr, rule)
	}
	res, _, err := h.c.Firewall.Create(ctx, hcloud.FirewallCreateOpts{Name: name, Rules: hr})
	if err != nil {
		return err
	}
	return h.c.Action.WaitFor(ctx, res.Actions...)
}

func (h *Cloud) DeleteFirewall(ctx context.Context, name string) error {
	fw, _, err := h.c.Firewall.GetByName(ctx, name)
	if err != nil || fw == nil {
		return err
	}
	_, err = h.c.Firewall.Delete(ctx, fw)
	return err
}

func (h *Cloud) SSHKeys(ctx context.Context) ([]string, error) {
	keys, err := h.c.SSHKey.All(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.Name)
	}
	return names, nil
}
