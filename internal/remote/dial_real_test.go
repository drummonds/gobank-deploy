package remote

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDialReal connects to a live server; gated so `go test ./...` never
// needs one:  GOBANK_DEPLOY_SSH_IP=1.2.3.4 go test -run TestDialReal -v ./internal/remote
func TestDialReal(t *testing.T) {
	ip := os.Getenv("GOBANK_DEPLOY_SSH_IP")
	if ip == "" {
		t.Skip("GOBANK_DEPLOY_SSH_IP not set")
	}
	d := &Dialer{KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := d.Dial(ctx, ip, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := h.Run(ctx, "uptime"); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, _ := os.ReadFile(d.KnownHosts)
	if len(data) == 0 {
		t.Fatal("host key was not pinned")
	}
}
