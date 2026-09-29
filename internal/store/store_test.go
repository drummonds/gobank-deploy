package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func binary(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "demo")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEmptyStoreHasNoRelease(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "releases")} // need not exist yet
	if why := s.Unavailable(); why == "" {
		t.Fatal("empty store should say why up is unavailable")
	}
	if _, err := s.Build(context.Background(), "amd64"); err == nil {
		t.Fatal("Build on an empty store should fail")
	}
}

func TestAvailableIsTheLatestPut(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "releases")}
	if s.Available() != "" {
		t.Fatal("empty store has nothing available")
	}
	if err := s.Put("v0.3.47", "amd64", binary(t, "x")); err != nil {
		t.Fatal(err)
	}
	if got := s.Available(); got != "v0.3.47" {
		t.Errorf("available = %q", got)
	}
}

func TestPutThenBuildReturnsTheRelease(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "releases")}
	if err := s.Put("v1.2.3", "amd64", binary(t, "amd64 bits")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("v1.2.3", "arm64", binary(t, "arm64 bits")); err != nil {
		t.Fatal(err)
	}
	if why := s.Unavailable(); why != "" {
		t.Fatalf("store with a release should be available, got %q", why)
	}
	rel, err := s.Build(context.Background(), "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", rel.Version)
	}
	got, err := os.ReadFile(rel.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "arm64 bits" {
		t.Errorf("binary content = %q, want the arm64 build", got)
	}
}

func TestBuildForAnArchNotInTheReleaseFails(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if err := s.Put("v1", "amd64", binary(t, "x")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Build(context.Background(), "arm64")
	if err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Fatalf("want an error naming the missing arch, got %v", err)
	}
}

func TestTheLastPutVersionIsWhatBuildReturns(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, v := range []string{"v1", "v2", "v1"} { // pushing v1 again is a rollback
		if err := s.Put(v, "amd64", binary(t, v)); err != nil {
			t.Fatal(err)
		}
	}
	rel, err := s.Build(context.Background(), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v1" {
		t.Errorf("version = %q, want v1 (the last put)", rel.Version)
	}
}

func TestHasIsWhetherTheVersionWasPut(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "releases")}
	if s.Has("v1") {
		t.Fatal("empty store has nothing")
	}
	if err := s.Put("v1", "amd64", binary(t, "x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("v2", "amd64", binary(t, "y")); err != nil {
		t.Fatal(err)
	}
	if !s.Has("v1") || !s.Has("v2") || s.Has("v3") {
		t.Errorf("has v1 %v v2 %v v3 %v", s.Has("v1"), s.Has("v2"), s.Has("v3"))
	}
}
