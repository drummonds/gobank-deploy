package deploy

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"testing"
)

// SetLabels on the fake merges into the server's labels, as the provider does.
func (c *fakeCloud) SetLabels(_ context.Context, name string, labels map[string]string) error {
	s := c.servers[name]
	if s == nil {
		return fmt.Errorf("no server %s", name)
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	maps.Copy(s.Labels, labels)
	return nil
}

// Label values may only hold letters, digits, - _ and .; the password is
// also what a tester types on a phone, so keep it to lower-case alnum.
var appPasswordShape = regexp.MustCompile(`^[a-z0-9]{16}$`)

// A new environment gets an app password: on the server's labels, and in
// the box's deploy env so the demo's BFF accepts it.
func TestUpGeneratesAnAppPasswordForANewServer(t *testing.T) {
	h := newHarness()
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatal(err)
	}
	pw := h.cloud.created[0].Labels[appPasswordLabel]
	if !appPasswordShape.MatchString(pw) {
		t.Fatalf("created server's %s label = %q, want 16 lower-case alnum", appPasswordLabel, pw)
	}
	if runs := strings.Join(h.host.runs, "\n"); !strings.Contains(runs, "GOBANK_APP_PASSWORD="+pw) {
		t.Errorf("install should write the password to the deploy env file:\n%s", runs)
	}
}

// Redeploying keeps the password a tester already has.
func TestRedeployKeepsTheExistingAppPassword(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33", MemoryGB: 8,
		Labels: map[string]string{appPasswordLabel: "keepthisone12345"}}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatal(err)
	}
	if got := h.cloud.servers["gobank-prod"].Labels[appPasswordLabel]; got != "keepthisone12345" {
		t.Errorf("label changed to %q", got)
	}
	if runs := strings.Join(h.host.runs, "\n"); !strings.Contains(runs, "GOBANK_APP_PASSWORD=keepthisone12345") {
		t.Errorf("install should write the existing password:\n%s", runs)
	}
}

// A server from before this story has no password; the first redeploy
// gives it one and labels it, so the next redeploy keeps it.
func TestRedeployLabelsAServerThatHasNoAppPassword(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33", MemoryGB: 8}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatal(err)
	}
	pw := h.cloud.servers["gobank-prod"].Labels[appPasswordLabel]
	if !appPasswordShape.MatchString(pw) {
		t.Fatalf("server should now carry a generated password label, got %q", pw)
	}
	if runs := strings.Join(h.host.runs, "\n"); !strings.Contains(runs, "GOBANK_APP_PASSWORD="+pw) {
		t.Errorf("install should write the generated password:\n%s", runs)
	}
}

// A new environment gets an admin password too (gobank story 1.7.1: the
// first admin's login to the staff web): on the server's labels, and in
// the box's deploy env so the demo creates its admin with it.
func TestUpGeneratesAnAdminPasswordForANewServer(t *testing.T) {
	h := newHarness()
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small", Create: true}); err != nil {
		t.Fatal(err)
	}
	labels := h.cloud.created[0].Labels
	pw := labels[adminPasswordLabel]
	if !appPasswordShape.MatchString(pw) {
		t.Fatalf("created server's %s label = %q, want 16 lower-case alnum", adminPasswordLabel, pw)
	}
	if pw == labels[appPasswordLabel] {
		t.Error("the admin password is the app password")
	}
	if runs := strings.Join(h.host.runs, "\n"); !strings.Contains(runs, "GOBANK_ADMIN_PASSWORD="+pw) {
		t.Errorf("install should write the admin password to the deploy env file:\n%s", runs)
	}
}

// A server from before admin passwords keeps its app password and gets
// an admin password on its first redeploy.
func TestRedeployLabelsAServerThatHasNoAdminPassword(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33", MemoryGB: 8,
		Labels: map[string]string{appPasswordLabel: "keepthisone12345"}}
	if _, err := h.d.Up(context.Background(), UpOptions{Env: prod, Scale: "small"}); err != nil {
		t.Fatal(err)
	}
	labels := h.cloud.servers["gobank-prod"].Labels
	if labels[appPasswordLabel] != "keepthisone12345" {
		t.Errorf("app password changed to %q", labels[appPasswordLabel])
	}
	pw := labels[adminPasswordLabel]
	if !appPasswordShape.MatchString(pw) {
		t.Fatalf("server should now carry a generated admin password label, got %q", pw)
	}
	runs := strings.Join(h.host.runs, "\n")
	if !strings.Contains(runs, "GOBANK_APP_PASSWORD=keepthisone12345") || !strings.Contains(runs, "GOBANK_ADMIN_PASSWORD="+pw) {
		t.Errorf("install should write both passwords:\n%s", runs)
	}
}

// Status tells a tester how to log the staff web in.
func TestStatusShowsTheAdminPassword(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33",
		Labels: map[string]string{adminPasswordLabel: "staffpassword123"}}
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.AdminPassword != "staffpassword123" {
		t.Errorf("Status.AdminPassword = %q", st.AdminPassword)
	}
}

// Status tells a tester how to log the app in.
func TestStatusShowsTheAppPassword(t *testing.T) {
	h := newHarness()
	h.cloud.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.9", Type: "cx33",
		Labels: map[string]string{appPasswordLabel: "keepthisone12345"}}
	st, err := h.d.Status(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	if st.AppPassword != "keepthisone12345" {
		t.Errorf("Status.AppPassword = %q", st.AppPassword)
	}
}
