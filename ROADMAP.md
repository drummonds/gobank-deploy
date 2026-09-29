# Roadmap

The goal: rehearse every gobank release on preprod with a fresh copy of
prod's data, gate it, then promote the same release to prod. Blue/green
inside the box first; expand/contract migrations through gobank-db.

## Prerequisites in gobank

- [ ] Demo resumes from an existing PostgreSQL database instead of
      dropping all tables on start (nothing below is real until this lands)
- [ ] Version and applied migrations as JSON on the about endpoint
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

- Separate database box: the app's memory share rises from 50% of the
  box (see `appShareWithLocalPostgres`) once PostgreSQL is elsewhere;
  logical replication for Postgres version upgrades and zone moves
  (researched in gobank-db)
- Masking step in the snapshot once real PII exists
- Kubernetes targets (gobank Phase 3) behind the same vocabulary
