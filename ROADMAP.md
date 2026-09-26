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
- [ ] Story 1c — `up` from a release: gobank publishes `demo` binaries
      (linux/amd64, arm64) as Forgejo release assets, built where the
      charts-fork replace applies; a Fetcher beside the Builder installs a
      named version. Unblocks Create / Redeploy on hydrogen and is what
      Story 4 promotes
- [ ] Story 2 — Snapshot: copy prod's database into preprod over ssh
- [ ] Story 3 — Gates: version, schema, invariants (account count and
      total balances unchanged, trial balance balances), BFF journeys
- [ ] Story 4 — Rehearse then promote: preprod on a fresh snapshot, gates,
      same release to prod
- [ ] Story 5 — Blue/green in the box: second systemd unit, port switch,
      rollback is redeploying the previous release
- [ ] Story 6 — Run on gokrazy (pure Go; ssh agent replaced by a key on
      the appliance)

## Later

- Separate database box: the app's memory share rises from 50% of the
  box (see `appShareWithLocalPostgres`) once PostgreSQL is elsewhere;
  logical replication for Postgres version upgrades and zone moves
  (researched in gobank-db)
- Masking step in the snapshot once real PII exists
- Kubernetes targets (gobank Phase 3) behind the same vocabulary
