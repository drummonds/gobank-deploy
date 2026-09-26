package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"

	"golang.org/x/crypto/ssh"
)

func noKeysAnywhere(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SSH_AUTH_SOCK", "")
}

func TestAnIdentityPEMIsEnoughOnItsOwn(t *testing.T) {
	noKeysAnywhere(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "gobank-deploy@hydrogen")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := authMethods("203.0.113.1", pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	if len(auth) != 1 {
		t.Fatalf("want one publickey auth method, got %d", len(auth))
	}
}

func TestNoAgentNoHomeKeysNoIdentityIsAnError(t *testing.T) {
	noKeysAnywhere(t)
	if _, err := authMethods("203.0.113.1", nil); err == nil {
		t.Fatal("want an error when there is nothing to authenticate with")
	}
}

func TestAnUnparseableIdentityIsAnError(t *testing.T) {
	noKeysAnywhere(t)
	if _, err := authMethods("203.0.113.1", []byte("not a key")); err == nil {
		t.Fatal("want an error for a bad identity rather than silently ignoring it")
	}
}
