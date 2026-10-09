# Roadmap

The goal: rehearse every gobank release on preprod with a fresh copy of
prod's data, gate it, then promote the same release to prod. Blue/green
inside the box first; expand/contract migrations through gobank-db.

## Prerequisites in gobank

- [ ] Demo resumes from an existing PostgreSQL database instead of
      dropping all tables on start (nothing below is real until this lands)
- [x] Version and applied migrations as JSON on the about endpoint
      (gobank `/about.json`, with the position and the restart record)
- [ ] Schema applied through gobank-db `Apply` (expand / cutover / contract)

## Stories

- [x] Story 1 — `up`, `down`, `status` for a named environment (port of
      the bash scripts, tested behind interfaces)
- [x] Story 1a — `ui`: environment states and controls on a polling page
- [x] Story 1b — `ui` as a status-and-down console on hydrogen (gokrazy):
      no toolchain there, so nothing that needs a release is offered
- [x] Story 1c — `up` from a release store: `build` on the laptop puts
      `demo` for linux/amd64 and arm64 (built where the charts-fork replace
      applies) into `build/releases`, `task push` copies the latest into
      hydrogen's `/perm/gobank-deploy/releases`, and `-store` makes that the
      Deployer's Builder there. Create / Redeploy on hydrogen deploy what
      was last pushed; pushing an older version again is a rollback
- [x] Story 1d — Versions on the page and in `status`: what each
      environment runs against what the next `up` would deploy
- [x] Story 1e — First workflow on gobank-workflow: the temporary demo
      environment (create, serve until expiry, remove), resumed from the
      server's expiry label after a restart. Run records in memory until
      the go-postgres store lands (issue #1)
- [x] Story 1f — The gobank release is the build stage: gobank's `tp
      release` runs goreleaser and attaches `demo-linux-amd64/arm64` to
      the Forgejo release; `up` from a store fetches the newest tag's
      binaries into it first. A new gobank version needs no gobank-deploy
      release or hydrogen update
- [x] (v0.3.0) Story 1g — App password: the demo's customer BFF (`/v1/` on the
      demo's port, gobank ADR-0002 stage 1) refuses every login until
      `GOBANK_APP_PASSWORD` is set. `up` generates one per environment and
      writes it beside `GOBANK_MEMORY_LIMIT`; `status` and the ui show it,
      so a tester can log the app in. Per-environment generation rather
      than one secret in gokrazy's secrets: nothing committed, nothing
      shared between environments. It is a demo password for simulated
      customers, not a credential store — that is gobank's stage 2
- [x] (unreleased) Story 1h — `ui` on htmx (lofigui tier 3): the page stops reloading
      itself. A stable shell holds the forms; the environment table, job
      logs and clock are one fragment polled at the current 3s / 15s
      rate, paused while a form has focus, so a Create click or a
      half-chosen scale is never thrown away by a poll. Buttons post over
      htmx and swap the fragment back, errors land in the row instead of
      a bare error page; without JavaScript the page still works as today.
      htmx is embedded, not fetched from a CDN
- [x] (unreleased) Story 1i — Rollback from the store and a stop that
      waits: `POST /fetch?tag=vX` makes a named release the next deploy
      (fetched if absent), so gobank's upgrade drill (ADR-0003, stage 2
      story (c)) rolls back with a fetch and a Redeploy; the systemd unit
      gets `TimeoutStopSec=900` (fresh servers from cloud-init, existing
      ones from the install script) so the demo finishes the day in
      progress instead of being killed
- [x] (unreleased) Story 1j — The upgrade drill as a workflow, with a
      database: gobank's `upgrade-drill.md` steps 1, 2 and 4 to 8 run from a
      Drill button as a gobank-workflow pipeline (prepare, before, upgrade,
      rollback, forward), each hop observed at the demo's `/about.json` and
      gated on version, clean stop, downtime, intact handover and same day.
      Workflow runs and drills live in a pglike file (`-db`, on /perm on
      hydrogen), two components' schemas in one database (gobank-db
      `ApplyTo`), browsed with go-dbexplorer; a Drills page is the history
      with the line for each story's record. Closes issue #1
- [x] (unreleased) Story 1k — The performance run as a workflow: a fresh
      environment at a scale, the demo flat out, customers added for ten
      minutes and days run for ten, the two rates (customers per second,
      account days per 12h) read off gobank v0.12's `/about.json`, the
      server removed; a Performance page with the row for gobank's
      `benchmark.md`, and `perf <env>` from the laptop measuring every
      scale at once on a server each
- [x] (unreleased) Story 1l — A Workflows tab: the engine's view of this
      program's three workflows from their definitions, with instance
      counts by state, the instances, and each instance's steps laid over
      its definition (not-run steps visible), downloadable as d2. The
      vantage point for making the workflows better
- [x] (unreleased) Story 1n — Admin password: the demo's staff web needs a
      login from gobank v0.28.0 (story 1.7.1), its first admin's password
      coming from `GOBANK_ADMIN_PASSWORD`. `up` generates one per
      environment as it does the app password, labels the server with it,
      writes it beside the app password, and `status` and the ui show it
- [ ] Story 1m — The release as a pipeline, keyed by version: one
      instance per gobank tag spanning tp release (check, bump, changelog,
      tag, push: the laptop, where the working tree and the human are) and
      this engine (everything after the tag is a fact in the forge). The
      engine polls the Forgejo for a new tag on the minute tick the demo
      workflow already uses, no webhook and no inbound path, so hydrogen
      stays the engine; `post_release` becomes optional. Stages: `build`
      (a throwaway builder server clones the tag and builds demo for amd64
      and arm64, since `cmd/demo`'s replace is `..` and works from a clean
      clone; attached to the Forgejo release, so goreleaser leaves the
      laptop; built once, never on the target box, so prod gets the same
      binary as preprod), `fetch`, `deploy preprod`, `drill` (Story 1j as
      a stage), the automatic gates (the drill's own, then Story 3's),
      `promote` (a person), `deploy prod`, `report` (the outcome as a
      comment or status on the Forgejo release). The ideas taken from
      Woodpecker: the forge as the trigger, the definition held apart from
      the execution with `when` conditions, status back to the forge, a
      log per step, a build worker with a toolchain; not its containers,
      YAML or plugins. Past Woodpecker, which has no approval step: a
      manual gate that waits days across a restart, so the runner gets a
      persisted waiting status re-entered by the tick, the same shape as
      the temporary environments. Which gates apply is a decision table
      on the bump kind (patch versus minor) with a per-run override, in
      the repo and rendered on the Workflows tab, so a skipped gate is
      visible. Lands before the gobank prerequisite, with promote being
      today's redeploy of prod; Stories 2 and 3 slot in as stages and
      gates. Open: who may press promote — logins and roles may come from
      gobank's own, ADR-0002 stage 2, rather than Caddy `basic_auth` at
      the page
- [ ] Story 2 — Snapshot: copy prod's database into preprod over ssh
- [ ] Story 3 — Gates: version, schema, invariants (account count and
      total balances unchanged, trial balance balances), BFF journeys
- [ ] Story 4 — Rehearse then promote: preprod on a fresh snapshot, gates,
      same release to prod
- [ ] Story 5 — Blue/green in the box: second systemd unit, port switch,
      rollback is redeploying the previous release
- [x] Story 6 — Run on gokrazy (pure Go; ssh agent replaced by a key on
      the appliance): `GOBANK_DEPLOY_SSH_KEY` from the gokrazy secrets,
      known_hosts and the release store on /perm. Done by 1b + 1c

## Later

- gobank-deploy is itself a system with a past now: its database has
  versioned schemas, so its own upgrade on hydrogen (a gokrazy update) is
  a hop with migrations to rehearse, without the simulation. The same
  workflow shape, a different console
- Two things to deploy, itself and gobank, from one place: the
  orchestrator may want to become a general deploy tool, with the
  environment and release vocabulary kept and the gobank specifics
  (the demo's console, the Hetzner box) behind an interface
- The engine on Hetzner, so environments can be managed and shown when
  the house is off: gokrazy on a cx23 from the `~/gokrazy/hetzner`
  instance (rescue mode + `dd`, proven 2026-09) with gokcaddy, tailscaled,
  mkfs and the update UI on 1961; Tailscale for management and `gok
  update`, a Hetzner firewall allowing 22 and 1961 from the tailnet only,
  Caddy `basic_auth` at `deploy.gobank.drummonds.net` for demos (the page
  shows app passwords and has Down buttons, so that is the floor). The
  store refills itself from the Forgejo, so the server is deleted after
  use and rebuilt in minutes. No code change. Prerequisite: the gobank
  environments in their own Hetzner project with their own token —
  tokens scope to a project, and today's reaches woodpecker-ci (the
  Forgejo and docs host), which the engine's UI could then delete.
  Open: whether demo viewers go on the tailnet, which removes the public
  port altogether

- Separate database box: the app's memory share rises from 50% of the
  box (see `appShareWithLocalPostgres`) once PostgreSQL is elsewhere;
  logical replication for Postgres version upgrades and zone moves
  (researched in gobank-db)
- Masking step in the snapshot once real PII exists
- Kubernetes targets (gobank Phase 3) behind the same vocabulary
