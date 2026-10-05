# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

 - The performance run as a workflow: a fresh environment at a scale, measured for twenty minutes, removed; a Performance page with the row for gobank's benchmark.md

### Added
- The **perf** workflow (story 1k): `create` a fresh environment at a
  scale (an existing server is kept, so a failed run resumes on the same
  box), `add` customers flat out in batches of 1,000 for ten minutes with
  the rate taken from the demo batch by batch, `days` for ten minutes
  reading account days per 12h and the last day it is made of, `remove`.
  Started by **Measure** on the environments page (a fresh name and a
  scale) or `perf <env>` on the command line, which measures every scale
  at once (`-scales small,large` by default) on a server each, named
  `<env>-<scale>`, and prints the rows at the end; kept in the `-db`
  database with where it ran (scale, server type, RAM, gobank version),
  its schema versioned in `perf_schema_migrations`. The **Performance**
  page is the history, each run with its row for gobank's `benchmark.md`;
  the run page shows a perf run's figures.
- The console reads gobank v0.12's rates off `/about.json`
  (`customers_per_sec`, `account_days_per_12h`, `adding_customers`,
  `last_day_duration`, `last_day_accounts`) and drives the demo's forms:
  the settings together (`SetSettings`), `AddCustomers`, `Start`, `Stop`.

## [0.9.0] - 2026-10-05

 - Drill accepts the day a stop finishes and the day a restart begins; sets the day length again after each hop

 - Drill accepts the day a stop finishes and the day a restart begins; sets the day length again after each hop

### Fixed
- The drill failed its first hop on prod with "landed on a day boundary"
  although the upgrade was sound: the demo's day length set by prepare
  lasted only as long as the process, so v0.10.2 came back flat out and
  ran two days before the reading. The gate pinned a property the demo
  does not have: a clean stop finishes the day in progress and a restart
  begins a new day, so the position moves on every hop. The hop now gates
  on the restart row's day count being at most one past the last
  observation (the stop) and the position at most one past that (the
  start); further is the run going on at full speed. And the drill sets
  the day length again after every hop, since a release before gobank
  v0.10.3 forgets it on restart (gobank v0.10.3 records it with the run).

## [0.8.0] - 2026-10-05

 - Serving check waits while the service is alive, not for a fixed five minutes

### Changed
- The environment's address on the main page opens the deployed site in
  a new tab, so the dashboard stays put

### Fixed
- The check after a start waits while the service is alive (active and
  not restarted by systemd), up to half an hour, instead of a fixed five
  minutes: the demo's resume grows with its history (1m32s at 4,010 days,
  2m21s at 5,811), so a clock would keep failing drills whose upgrade had
  landed. The job log says so each minute; a service that dies or
  restarts fails the check at once

## [0.7.0] - 2026-10-04

 - Serving check outlasts the demo's resume; drills page shows each run's state and steps

### Fixed
- The check after a start waits five minutes, not one, for the demo to
  answer: it rebuilds the bank from its database before it listens (a
  minute and a half at four thousand days, growing with the history), so
  the first drill on prod failed its upgrade hop with "service not
  answering" while the upgrade landed anyway

### Changed
- The Drills page shows each drill's run: its state and error, and the
  steps with how long each took, so a failed drill reads in full there
  rather than only on the environments page; its link to the run page
  reads "details" ("run" looked like a button), and each drill is headed
  with the time it started, not just the day

## [0.6.0] - 2026-10-04

 - Story 1j: the upgrade drill as a workflow, with a pglike database and the DB explorer

### Added
- The upgrade drill (story 1j): **Drill to vX** on an environment's row
  runs gobank's `upgrade-drill.md` as a gobank-workflow pipeline keyed
  `<env> N→N+1 <date>`: `prepare` (a day of at least 30m, set to 2h
  otherwise, then wait for a day with 10m left), `before`, `upgrade`,
  `rollback`, `forward`. Each hop fetches the release, redeploys, reads
  the demo's `/about.json` and is gated on the version serving, a restart
  row following the expected release with a known downtime and an intact
  handover, and the same simulated day as before; a release without
  `about.json` has its version checked only. A failed hop resumes on the
  next press the same day. Offered only with a release store to roll back
  from and a newer release than the one serving
- A database: `ui -db FILE` (default `<-build>/gobank-deploy.db`, so
  `/perm/gobank-deploy/gobank-deploy.db` on hydrogen) is a pglike file
  holding gobank-workflow's run records and this program's `drills` and
  `drill_observations`, each component's schema versioned in its own
  migrations table through gobank-db `ApplyTo`. Runs and drills survive a
  restart; closes issue #1
- Pages: **Drills**, the history with each drill's observations and the
  line for the story's record; `/workflows/<id>`, a run's steps and, for
  a drill, its observations (the runs table links there); **DB Explorer**
  at `/internal/explorer` (go-dbexplorer) over the live database. The
  About page draws the drill workflow
- `internal/remote.Console`: the demo's `/about.json` and settings form
  as the drill uses them

### Changed
- `cmd/flowd2` writes one `<type>-workflow.d2` per definition instead of
  printing one to stdout

## [0.5.0] - 2026-10-04

 - Story 1i: rollback from the store (fetch a named tag) and a stop timeout that outlasts a day

### Added
- `POST /fetch?tag=vX` (story 1i): makes a named release the next deploy,
  fetching it into the store if it is not there, so a rollback from
  hydrogen is a fetch of the previous tag and a Redeploy. `Store.Use`
  marks a held version the latest.
- The demo's systemd unit has `TimeoutStopSec=900`, from cloud-init on a
  fresh server and from the install script on an existing one: the demo
  finishes the simulated day in progress on SIGTERM, and the default 90s
  would have killed it mid-day.

### Changed
- ui on htmx (story 1h). The page no longer reloads itself: the
  environment table, job logs and clock are a fragment at `/fragment`,
  polled at the same 3s / 15s rate and paused while a form has focus, so
  a Create click or a half-chosen scale is no longer thrown away by a
  refresh. Buttons post over htmx and swap the fragment back; a refused
  action shows its message above the table instead of a bare error page.
  Without JavaScript the page works as before. htmx 2.0.4 is embedded
  in the binary

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
