// Package store is a directory of demo releases: what a host that cannot
// build cmd/demo deploys from. The appliance's Deployer reads it as its
// Builder and fetches the repo's releases into it; the laptop can also
// build into one and copy it over.
//
// Layout: <Dir>/<version>/demo-linux-<goarch>, and <Dir>/latest naming
// the version to deploy. Putting a version makes it the latest, so
// putting an older one again is a rollback. The Deployer fetches the
// repo's newest release into the store itself (deploy.ReleaseStore), so
// the laptop's push is for a release the repo has not built, or a
// rollback.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

type Store struct {
	Dir string
}

func (s *Store) binaryPath(version, goarch string) string {
	return filepath.Join(s.Dir, version, "demo-linux-"+goarch)
}

func (s *Store) latestPath() string { return filepath.Join(s.Dir, "latest") }

// Latest is the version to deploy, or "" when the store is empty.
func (s *Store) Latest() string {
	b, err := os.ReadFile(s.latestPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Available implements deploy.Builder: the latest release is what Build deploys.
func (s *Store) Available() string { return s.Latest() }

// Has: version was put into the store (whatever the latest is now).
func (s *Store) Has(version string) bool {
	if version == "" || strings.ContainsAny(version, `/\`) {
		return false
	}
	st, err := os.Stat(filepath.Join(s.Dir, version))
	return err == nil && st.IsDir()
}

// Unavailable is why up cannot run from this store, or "" when it can.
func (s *Store) Unavailable() string {
	if s.Latest() == "" {
		return "no release in the store at " + s.Dir
	}
	return ""
}

// Put copies binary into the store as version's build for goarch and
// makes version the latest.
func (s *Store) Put(version, goarch, binary string) error {
	if version == "" || strings.ContainsAny(version, `/\`) {
		return fmt.Errorf("bad version %q", version)
	}
	dst := s.binaryPath(version, goarch)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := copyFile(binary, dst); err != nil {
		return err
	}
	return os.WriteFile(s.latestPath(), []byte(version+"\n"), 0o644)
}

// Use makes a version the store already holds the latest, so the next
// deploy is that version: a rollback when it is older than the latest.
func (s *Store) Use(version string) error {
	if !s.Has(version) {
		return fmt.Errorf("the store has no %s", version)
	}
	return os.WriteFile(s.latestPath(), []byte(version+"\n"), 0o644)
}

// Build implements deploy.Builder: the latest release's binary for goarch.
func (s *Store) Build(_ context.Context, goarch string) (deploy.Release, error) {
	version := s.Latest()
	if version == "" {
		return deploy.Release{}, errors.New(s.Unavailable())
	}
	bin, err := filepath.Abs(s.binaryPath(version, goarch))
	if err != nil {
		return deploy.Release{}, err
	}
	if _, err := os.Stat(bin); err != nil {
		return deploy.Release{}, fmt.Errorf("release %s has no linux/%s build in the store at %s", version, goarch, s.Dir)
	}
	return deploy.Release{Version: version, Binary: bin}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// A temporary file of its own: a fetch after a release and one before a
	// deploy can put the same binary at once.
	out, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(0o755); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}
