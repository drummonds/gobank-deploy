# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added
- `up`, `down`, `status` for a named environment on Hetzner Cloud, ported
  from gobank's `deploy/hetzner` scripts: explicit `-create` gate, scale
  presets, cloud-init Postgres setup, binary install over ssh, HTTP check.
- Orchestration in `internal/deploy` behind `Cloud`, `Dialer`, `Builder`
  and `Prober` interfaces, tested with fakes.
- Project-local host-key pinning with accept-new semantics and forget-on-
  recreate, tested.
