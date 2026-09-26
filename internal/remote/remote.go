// Package remote provides the pieces of a deployment that touch the real
// world: an ssh Dialer with per-project host-key pinning, a Builder that
// cross-compiles the gobank demo, and an HTTP Prober.
package remote

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kevinburke/ssh_config"
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
	// Identity is a private key in PEM, for a host with no ssh agent and no
	// ~/.ssh (the gokrazy appliance). Tried alongside whatever else is found.
	Identity []byte
}

func (d *Dialer) Dial(ctx context.Context, ip string, fresh bool) (deploy.Host, error) {
	if fresh {
		if err := forgetHost(d.KnownHosts, ip); err != nil {
			return nil, err
		}
	}
	auth, err := authMethods(ip, d.Identity)
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
		// Negotiate the key types already pinned for this host, as OpenSSH
		// does; otherwise a host pinned as ed25519 fails as "changed" when
		// Go's default order picks ecdsa.
		HostKeyAlgorithms: pinnedAlgos(d.KnownHosts, ip),
		Timeout:           timeout,
	}
	// The address given to NewClientConn is what the host-key callback
	// sees, and knownhosts insists on host:port.
	hostport := net.JoinHostPort(ip, "22")
	conn, err := net.DialTimeout("tcp", hostport, timeout)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, hostport, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &host{client: ssh.NewClient(c, chans, reqs)}, nil
}

// authMethods gathers every signer we can find — the agent named by
// IdentityAgent in ~/.ssh/config (Bitwarden's, for instance), the default
// agent, and the IdentityFile / default key files — into ONE publickey
// method. x/crypto/ssh tries each method name once, so two "publickey"
// methods would leave the second untried.
func authMethods(host string, identity []byte) ([]ssh.AuthMethod, error) {
	var signers []ssh.Signer
	if len(identity) > 0 {
		signer, err := ssh.ParsePrivateKey(identity)
		if err != nil {
			return nil, fmt.Errorf("identity: %w", err)
		}
		signers = append(signers, signer)
	}
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}

	sockets := []string{os.Getenv("SSH_AUTH_SOCK")}
	if a := ssh_config.Get(host, "IdentityAgent"); a != "" && a != "none" {
		sockets = append([]string{expand(a)}, sockets...)
	}
	for _, sock := range sockets {
		if sock == "" {
			continue
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			continue
		}
		if s, err := agent.NewClient(conn).Signers(); err == nil {
			signers = append(signers, s...)
		}
	}

	files := []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_rsa")}
	if f, _ := ssh_config.GetStrict(host, "IdentityFile"); f != "" {
		files = append([]string{expand(f)}, files...)
	}
	seen := map[string]bool{}
	for _, name := range files {
		if seen[name] {
			continue
		}
		seen[name] = true
		pem, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		if signer, err := ssh.ParsePrivateKey(pem); err == nil {
			signers = append(signers, signer)
		}
	}
	if len(signers) == 0 {
		return nil, errors.New("no ssh keys found: nothing in the agent (IdentityAgent / SSH_AUTH_SOCK) and no readable ~/.ssh/id_ed25519 or id_rsa, and no identity given")
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
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
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				_, err = fmt.Fprintln(f, knownhosts.Line([]string{hostport}, key))
				return err
			}
			return fmt.Errorf("host key for %s changed (server replaced?): %w — if expected, remove its line from %s", hostport, err, file)
		}
		return err
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

// hostMatches handles plain and comma-separated patterns and OpenSSH's
// hashed form (|1|salt|hmac-sha1), for the host as knownhosts writes it:
// bare for port 22, [host]:port otherwise.
func hostMatches(pattern, ip string) bool {
	for p := range strings.SplitSeq(pattern, ",") {
		if p == ip || p == "["+ip+"]:22" {
			return true
		}
		if strings.HasPrefix(p, "|1|") && (hashedMatches(p, ip) || hashedMatches(p, "["+ip+"]:22")) {
			return true
		}
	}
	return false
}

func hashedMatches(pattern, host string) bool {
	parts := strings.Split(pattern, "|") // "", "1", salt, hash
	if len(parts) != 4 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return hmac.Equal(mac.Sum(nil), want)
}

// pinnedAlgos lists the key types already on file for ip, in file order.
// Empty when nothing is pinned, which leaves the client's default order.
func pinnedAlgos(file, ip string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var algos []string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") {
			continue
		}
		if hostMatches(fields[0], ip) {
			algos = append(algos, fields[1])
		}
	}
	return algos
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

// CanBuild reports why a Builder cannot work on this host, or "" when it
// can: it needs the go tool on PATH and a gobank checkout at src.
func CanBuild(src string) string {
	if _, err := exec.LookPath("go"); err != nil {
		return "no Go toolchain on this host"
	}
	if _, err := os.Stat(filepath.Join(src, "cmd", "demo")); err != nil {
		return "no gobank checkout at " + src
	}
	return ""
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
