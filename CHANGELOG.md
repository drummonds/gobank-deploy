# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added
- `ui`: lofigui page listing each environment's state with Create (scale
  preset, marked as starting billing), Redeploy, Down (confirmation tick
  required) and Cancel; one job per environment, log on the page, polling
  while a job runs.
- `up`, `down`, `status` for a named environment on Hetzner Cloud, ported
  from gobank's `deploy/hetzner` scripts: explicit `-create` gate, scale
  presets, cloud-init Postgres setup, binary install over ssh, HTTP check.
- Orchestration in `internal/deploy` behind `Cloud`, `Dialer`, `Builder`
  and `Prober` interfaces, tested with fakes.
- Project-local host-key pinning with accept-new semantics and forget-on-
  recreate, tested. Negotiates the key types already pinned for a host (as
  OpenSSH does) and understands hashed entries, so a manual `ssh` against
  the same file does not turn into a false "host key changed".
- ssh credentials: the agent named by `IdentityAgent` in `~/.ssh/config`,
  the default agent and the key files are merged into one publickey
  method (x/crypto/ssh tries each method name once).
- `TestDialReal`, gated by `GOBANK_DEPLOY_SSH_IP`, exercises the dialer
  against a live server.
