// Package store is a directory of demo releases: what a host that cannot
// build cmd/demo deploys from. The laptop builds into one and copies it
// to the appliance's /perm; the appliance's Deployer reads it as its
// Builder.
//
// Layout: <Dir>/<version>/demo-linux-<goarch>, and <Dir>/latest naming
// the version to deploy. Putting a version makes it the latest, so
// putting an older one again is a rollback.
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
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".tmp", dst)
}
