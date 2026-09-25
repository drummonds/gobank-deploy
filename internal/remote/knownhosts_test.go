package remote

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var addr = &net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: 22}

func TestAcceptNewPinsUnknownHostAndRejectsChangedKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "build", "known_hosts")
	cb := acceptNew(file)
	k1, k2 := newKey(t), newKey(t)

	if err := cb("10.0.0.7:22", addr, k1); err != nil {
		t.Fatalf("first contact should be accepted: %v", err)
	}
	if err := cb("10.0.0.7:22", addr, k1); err != nil {
		t.Fatalf("same key again should be accepted: %v", err)
	}
	err := cb("10.0.0.7:22", addr, k2)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed key must be rejected, got %v", err)
	}
}

func TestForgetHostDropsOnlyThatIP(t *testing.T) {
	file := filepath.Join(t.TempDir(), "known_hosts")
	cb := acceptNew(file)
	if err := cb("10.0.0.7:22", addr, newKey(t)); err != nil {
		t.Fatal(err)
	}
	other := &net.TCPAddr{IP: net.ParseIP("10.0.0.8"), Port: 22}
	if err := cb("10.0.0.8:22", other, newKey(t)); err != nil {
		t.Fatal(err)
	}

	if err := forgetHost(file, "10.0.0.7"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), "10.0.0.7") || !strings.Contains(string(data), "10.0.0.8") {
		t.Errorf("known_hosts after forget:\n%s", data)
	}
	// The replaced server's new key is now accepted as a first contact.
	if err := cb("10.0.0.7:22", addr, newKey(t)); err != nil {
		t.Fatalf("new key after forget should be accepted: %v", err)
	}
}

func TestForgetHostOnMissingFileIsFine(t *testing.T) {
	if err := forgetHost(filepath.Join(t.TempDir(), "nope"), "10.0.0.7"); err != nil {
		t.Fatal(err)
	}
}

// hashedPattern builds an OpenSSH HashKnownHosts entry (|1|salt|hmac) for host.
func hashedPattern(t *testing.T, host string) string {
	t.Helper()
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestPinnedAlgosPreferKeyTypesAlreadyOnFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "known_hosts")
	content := strings.Join([]string{
		"10.0.0.7 ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBHnothing",
		hashedPattern(t, "10.0.0.7") + " ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHnothing",
		"10.0.0.8 ssh-rsa AAAAB3nothing",
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := pinnedAlgos(file, "10.0.0.7")
	if strings.Join(got, ",") != "ecdsa-sha2-nistp256,ssh-ed25519" {
		t.Errorf("pinnedAlgos = %v", got)
	}
	if got := pinnedAlgos(file, "10.0.0.9"); len(got) != 0 {
		t.Errorf("unknown host should pin nothing, got %v", got)
	}
	if got := pinnedAlgos(filepath.Join(t.TempDir(), "missing"), "10.0.0.7"); len(got) != 0 {
		t.Errorf("missing file should pin nothing, got %v", got)
	}
}

func TestForgetHostDropsHashedEntriesToo(t *testing.T) {
	file := filepath.Join(t.TempDir(), "known_hosts")
	content := hashedPattern(t, "10.0.0.7") + " ssh-ed25519 AAAAC3nothing\n10.0.0.8 ssh-rsa AAAAB3nothing\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := forgetHost(file, "10.0.0.7"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(file)
	if strings.Contains(string(data), "ssh-ed25519") || !strings.Contains(string(data), "10.0.0.8") {
		t.Errorf("after forget:\n%s", data)
	}
}
