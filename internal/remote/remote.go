// Package remote provides the pieces of a deployment that touch the real
// world: an ssh Dialer with per-project host-key pinning, a Builder that
// cross-compiles the gobank demo, and an HTTP Prober.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// Dialer opens root ssh sessions. Host keys are pinned in KnownHosts (a
// project-local file, not ~/.ssh/known_hosts, because servers come and go)
// with accept-new semantics: unknown hosts are recorded, changed keys fail.
type Dialer struct {
	KnownHosts string
	User       string        // defaults to root
	Timeout    time.Duration // per connection attempt; defaults to 5s
}

func (d *Dialer) Dial(ctx context.Context, ip string, fresh bool) (deploy.Host, error) {
	if fresh {
		if err := forgetHost(d.KnownHosts, ip); err != nil {
			return nil, err
		}
	}
	auth, err := authMethods()
	if err != nil {
		return nil, err
	}
	user, timeout := d.User, d.Timeout
	if user == "" {
		user = "root"
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: acceptNew(d.KnownHosts),
		Timeout:         timeout,
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), timeout)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, ip, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &host{client: ssh.NewClient(c, chans, reqs)}, nil
}

// authMethods prefers the ssh agent (the keys registered in the cloud
// project are normally loaded there) and falls back to the default key files.
func authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	home, _ := os.UserHomeDir()
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		pem, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err != nil {
			continue
		}
		if signer, err := ssh.ParsePrivateKey(pem); err == nil {
			methods = append(methods, ssh.PublicKeys(signer))
		}
	}
	if len(methods) == 0 {
		return nil, errors.New("no ssh credentials: no agent (SSH_AUTH_SOCK) and no ~/.ssh/id_ed25519 or id_rsa")
	}
	return methods, nil
}

// acceptNew pins unknown hosts into file and rejects changed keys, like
// ssh's StrictHostKeyChecking=accept-new.
func acceptNew(file string) ssh.HostKeyCallback {
	return func(hostport string, remote net.Addr, key ssh.PublicKey) error {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		check, err := knownhosts.New(file)
		if err != nil {
			return err
		}
		err = check(hostport, remote, key)
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			_, err = fmt.Fprintln(f, knownhosts.Line([]string{hostport}, key))
			return err
		}
		if err != nil {
			return fmt.Errorf("host key for %s changed (server replaced?): %w — if expected, remove its line from %s", hostport, err, file)
		}
		return nil
	}
}

// forgetHost drops any pinned key for ip: Hetzner reuses IPs, so a freshly
// created server has a new key under an address we may have seen before.
func forgetHost(file, ip string) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var keep []string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && hostMatches(fields[0], ip) {
			continue
		}
		keep = append(keep, line)
	}
	return os.WriteFile(file, []byte(strings.Join(keep, "\n")), 0o600)
}

func hostMatches(pattern, ip string) bool {
	for p := range strings.SplitSeq(pattern, ",") {
		if p == ip || p == "["+ip+"]:22" {
			return true
		}
	}
	return false
}

type host struct {
	client *ssh.Client
}

func (h *host) Run(ctx context.Context, cmd string) error {
	sess, err := h.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var out bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &out
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Put streams a local file to remote over a session's stdin; no scp or sftp
// needed on either side.
func (h *host) Put(ctx context.Context, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	sess, err := h.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin = f
	var out bytes.Buffer
	sess.Stderr = &out
	if err := sess.Run("cat > " + remote); err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// Builder cross-compiles cmd/demo from a gobank checkout with CGO disabled,
// so local replace directives in that checkout still apply.
type Builder struct {
	Src string // gobank checkout
	Dir string // where the binary is written
}

func (b *Builder) Build(ctx context.Context, goarch string) (deploy.Release, error) {
	version := "dev"
	if out, err := exec.CommandContext(ctx, "git", "-C", b.Src, "describe", "--tags", "--always").Output(); err == nil {
		version = strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(b.Dir, 0o755); err != nil {
		return deploy.Release{}, err
	}
	bin, err := filepath.Abs(filepath.Join(b.Dir, "demo"))
	if err != nil {
		return deploy.Release{}, err
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", "-X main.version="+version, "-o", bin, ".")
	cmd.Dir = filepath.Join(b.Src, "cmd", "demo")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return deploy.Release{}, fmt.Errorf("go build in %s: %w", cmd.Dir, err)
	}
	return deploy.Release{Version: version, Binary: bin}, nil
}

// Prober answers true when url returns a 2xx within Timeout.
type Prober struct {
	Timeout time.Duration // defaults to 5s
}

func (p *Prober) Serving(ctx context.Context, url string) bool {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
