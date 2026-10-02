# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.4.0] - 2026-10-02

 - Gen update

### Added
- The ui's navbar shows a small analogue clock set at render time, so the
  second hand jumps round on each poll (every 3s while a job runs, 15s
  idle) and the page is visibly live.

## [0.3.0] - 2026-10-02

 - App password per environment: up labels it, status and ui show it

### Added
- App password (story 1g). `up` gives each environment a 16-character
  password, kept on the server's `app-password` label so redeploys reuse
  it and a server from before this release gets one on its first
  redeploy. The install writes it as `GOBANK_APP_PASSWORD` in
  `/etc/gobank/deploy.env`, so the demo's customer BFF (gobank ADR-0002
  stage 1, `/v1/` on the demo's port) accepts logins; `status` and the ui
  show it. `Cloud` gains `SetLabels`.

## [0.2.0] - 2026-10-01

 - POST /fetch: gobank's release puts its binaries into the store straight away

### Added

- `POST /fetch` on the page: fetches gobank's newest release into the
  store now, for gobank's `tp release` to call as its `post_release` step,
  so the page shows the new version as deployable without a deploy first.
  Answers with the tag, or 502 with why it could not fetch.

### Fixed

- Two puts of the same release into the store at once (a fetch after a
  release and one before a deploy) no longer share a temporary file.

## [0.1.6] - 2026-10-01

 - Fetch gobank releases from the Forgejo into the store: release gobank, press Redeploy

 - The appliance fetches gobank's newest release itself: release gobank, press Redeploy

### Added
- Fetch as the stage before a deploy from a store: when gobank's newest
  tag is not in the store, its release's `demo-linux-amd64` and
  `demo-linux-arm64` (attached by gobank's `tp release`, now a goreleaser
  build) are fetched into the store and become the latest. A new gobank
  version reaches an environment without releasing gobank-deploy or
  updating hydrogen. A tag without binaries or a Forgejo out of reach is a
  notice; the deploy carries what the store has. A release the store
  already has is not fetched again, so `task push` of an older one is
  still a rollback.

### Changed
- With a store and a repo, an empty store no longer hides Create and
  Redeploy: the first deploy fills it. The page and `status` say the next
  deploy fetches a lagging release, rather than asking for a push.

## [0.1.5] - 2026-09-29

## [0.1.4] - 2026-09-29

 - More version info

### Changed
- A refused Route 53 change no longer fails `up` or `down`: it is reported
  and the deploy carries on by address.

### Added
- The page and `status` report gobank's newest tag on the Forgejo (public
  tags API, no token) against what can be deployed from this host, and
  say when the store or checkout is behind the repo. `-repo URL`.

## [0.1.3] - 2026-09-28

## [0.1.2] - 2026-09-28

 - Docs site, About page with component diagrams, expiry label fix

### Added
- Docs site: `index.md` with the component structure, the `up` sequence
  and the demo workflow as d2 diagrams, plus decision tables; `task
  docs:build` renders it, `tp pages` previews it, deployed to
  gobank-deploy.docs.bytestone.uk.
- About page on the ui with the same three diagrams, embedded in the
  binary so the appliance shows them.

## [0.1.1] - 2026-09-27

 - Route 53 hostnames, version comparison, demo workflow with automatic removal

### Added
- Demo workflow, the first on gobank-workflow's pipeline runner: "remove
  after" on the Create forms makes a temporary environment that is created
  with its expiry as a server label, served until then and removed. The
  page reconciles every minute, resuming the workflow for any temporary
  server without a job, so a restart does not leave one billing. Recent
  workflow runs are listed on the page; `cmd/flowd2` prints the d2.
- Versions: `status` and the page report the version each environment is
  running (from the demo's page footer) against what the next `up` would
  deploy (the store's latest, or the checkout's `git describe`), and flag
  an environment that is behind.
- Route 53: each environment is `<env>.gobank.drummonds.net`, an A record
  set on `up` (redeploy repairs it) and removed on `down`; `status` and the
  page link by hostname. `-dns DOMAIN` / `-dns ""`; skipped with a notice
  when there are no AWS credentials.

## [0.1.0] - 2026-09-26

 - ui lists the project's gobank servers and adds environments by name; demo back in the default

### Added
- `ui` lists every `gobank-*` server in the Hetzner project alongside the
  `-envs` names, and has a New environment form (name + scale) so the
  list is no longer fixed at startup. `demo` is back in the default
  `-envs` (`prod,preprod,demo`).
- `build` subcommand: cmd/demo for linux amd64 and arm64 into a release
  store (`build/releases/<version>/demo-linux-<goarch>` plus `latest`), and
  `task push` to copy the latest into hydrogen's `/perm/gobank-deploy/releases`.
- `-store DIR`: deploy from a release store instead of building, for a host
  with no toolchain. Create / Redeploy become available on the page as soon
  as the store has a release (checked per request, no restart).
- `GOBANK_DEPLOY_SSH_KEY`: base64 of a PEM private key to ssh with, for a
  host with no agent and no ~/.ssh (the appliance).
- `ui` on a host that cannot build `cmd/demo` (no Go toolchain, or no gobank
  checkout at `-src`) shows status and offers Down only, says why, and
  refuses create / redeploy requests with 403. This is the mode for the
  hydrogen gokrazy appliance.
- `up` writes `/etc/gobank/deploy.env` with `GOBANK_MEMORY_LIMIT` sized to
  half the box's RAM (the rest is PostgreSQL's), and makes the unit read it;
  works on boxes created before the unit had the `EnvironmentFile` line.
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
