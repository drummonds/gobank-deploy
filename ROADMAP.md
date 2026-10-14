# Roadmap

The goal: rehearse every gobank release on preprod with a fresh copy of
prod's data, gate it, then promote the same release to prod. Blue/green
inside the box first; expand/contract migrations through gobank-db. On
the way the orchestrator becomes a deploy server for more than gobank:
the forge release is the contract between build and deploy, a build is a
job in a container on the engine's own box, and the engine moves from the
LAN appliance to Hetzner so deployment works remotely and can be shown.

Each item is an open issue labelled `roadmap` on the Forgejo, grouped
into named epics (task-plus ADR 0004); `tp roadmap check` keeps file and
forge in step, and a finished item moves into the changelog under the
version that finished it. The stories that built the orchestrator
(lettered Story 1 to 1o and 6 until 2026-10-13) are in `CHANGELOG.md`
under their versions as `### Roadmap` lists; Story 1m is #6 and
Stories 2 to 5 are #12 to #15.

Prerequisites in gobank, not items here:

| Prerequisite | State | Needed by |
|---|---|---|
| Demo resumes from an existing PostgreSQL database instead of dropping all tables on start | open | everything in Rehearse then promote |
| Version and applied migrations as JSON on `/about.json`, with the position and the restart record | done | — |
| Schema applied through gobank-db `Apply` (expand / cutover / contract) | open | Rehearse then promote |
| Builds from a clean clone of its tag: go.mod replaces gobank-products with `../gobank-products` (two commits past v0.9.0 on the laptop while go.mod requires v0.8.0), so today's release binaries carry whatever the laptop had and a clone in a container does not build; goreleaser's before hook (`task wasm:pta2svg`) must run from a clone too | open | gobank's build as a job |

## Build and deploy any project from its forge release

The engine as the CI/CD server for the dashboard's projects. A release on
the forge (a tag with assets attached) is the contract: everything before
it is building, everything after it is deploying. A build is a job in a
container on the engine's own box, so the engine needs no toolchain of
its own and the runner is replaceable. The laptop keeps the human part of
`tp release` (check, bump, changelog, the pull request); Woodpecker stays
for the go-postgres soak only. Proved on hydrogen first, where the LAN
removes the security question; the next epic takes the same image public.
homesite goes first because its deploy is the simplest there is, a
directory swapped on the box the engine runs on, so the job primitive
and the project interface land without cross-compilation, Hetzner or
release assets; gobank's build then reuses both. Done when a homesite tag
and a gobank tag each reach their target through this engine with no
laptop step after the pull request.
- #4 homesite as the second project, and the first job: podman on
  hydrogen (gokrazy, Pi arm64, 16 GB, 232 GB /perm) runs the build this
  engine has had no toolchain for. A job is a command in a clone of a
  ref on a runner; its result is an exit status, a log in the step and
  files; a stage that needs a toolchain is a job and nothing else is, so
  the stages above never know which runner ran it. The engine execs the
  podman binary and streams the log (no containers/* bindings); the
  container gets the clone and a cache volume, never /perm or the
  engine's environment, and the one secret a job needs enters as its env
  for the duration, here the NAS Forgejo token (or a deploy key) to
  clone a private repo. The job: clone homesite at a tag from
  git.lan.drummonds.net, `task build` then `hugo` in a container,
  `public/` into the project's store on /perm; the deploy is today's
  swap (`/perm/homesite.new` → `/perm/homesite`, `.old` kept) done
  locally, so a rollback is the swap of an older build from the store.
  homesite's `task deploy` from the laptop retires. With a second
  project on the page, a project becomes a thing: a forge (host, repo,
  token), an asset or build, a target kind (a directory on this box; a
  Hetzner server) and its environments, with the gobank specifics (the
  demo's console, the Hetzner box, the drill) behind an interface and
  the environment and release vocabulary kept. Spike first (feasibility,
  no tests): the container and its image, cold and warm timed.
  homesite's build needs Go, hugo, uv and node, so the open question of
  the image is decided here rather than deferred: our own image in the
  Forgejo registry, or one built on the box; and whether `linkicons` and
  `domains`, which fetch from the internet, stay in the build or their
  output is committed
- #5 gobank's build as a job: clone gobank at a tag, build demo for
  linux amd64 and arm64 in `golang:1.26` with a module cache volume
  (CGO_ENABLED=0, so the arm64 host cross-compiles), attach the binaries
  to the Forgejo release, so goreleaser leaves the laptop; built once,
  never on the target box, so prod gets the same binary as preprod. The
  store's fetch then needs no change. Needs the clean-clone prerequisite
  above
- #6 The release as a pipeline, keyed by version: one instance per tag
  spanning tp release (check, bump, changelog, tag, push: the laptop,
  where the working tree and the human are) and this engine (everything
  after the tag is a fact in the forge). The engine polls the forge for
  a new tag on the minute tick the demo workflow already uses, no
  webhook and no inbound path; `post_release` becomes optional.
  homesite's pipeline is the trivial case and comes first: `build` (#4),
  `deploy`. gobank's stages: `build` (#5), and the post-merge half of
  `tp release` (tag, push mirror, verify install, docs) can be a job
  too, leaving the laptop only check, bump, changelog and the pull
  request; `fetch`, `deploy preprod`, `drill` (the drill as a stage),
  the automatic gates (the drill's own, then #13's), `promote` (a
  person), `deploy prod`, `report` (the outcome as a comment or status
  on the Forgejo release). The ideas taken from Woodpecker: the forge as
  the trigger, the definition held apart from the execution with `when`
  conditions, status back to the forge, a log per step; not its
  containers, YAML or plugins. Past Woodpecker, which has no approval
  step: a manual gate that waits days across a restart, so the runner
  gets a persisted waiting status re-entered by the tick, the same shape
  as the temporary environments. Which gates apply is a decision table
  on the bump kind (patch versus minor) with a per-run override, in the
  repo and rendered on the Workflows tab, so a skipped gate is visible.
  Lands before the gobank prerequisite, with promote being today's
  redeploy of prod; #12 and #13 slot in as stages and gates. Promote is
  operator-only, #10's role
- #7 gobank-deploy is itself a system with a past now: its database has
  versioned schemas, so its own upgrade on hydrogen or Hetzner (a
  gokrazy update) is a hop with migrations to rehearse, without the
  simulation. The same workflow shape, a different console; the third
  project of #4
- #8 Kubernetes targets (gobank Phase 3) behind the same vocabulary

## The engine on Hetzner

The same gokrazy+podman image on a cx23 (`~/gokrazy/hetzner`, rtr7
kernel, rescue-mode dd proven 2026-09), so environments can be managed
and shown when the house is off. gokrazy because the footprint is small:
no shell, no package manager, a read-only root, only the Go services put
on it, the toolchain inside a container. The store refills from the
Forgejo, so the server is deleted after use and rebuilt in minutes.
hydrogen keeps homesite and the current release meanwhile. Done when the
page shows and drives the environments with the house off, a viewer URL
can be handed out, and only an operator can press anything.
- #9 The gobank environments in their own Hetzner project with their own
  token: today's token reaches woodpecker-ci (the Forgejo and docs
  host), which a public Down button must not be able to delete. An
  afternoon of ops, no code
- #10 Viewer and operator: a viewer sees environments, drills,
  performance and pipeline progress; an operator presses Create, Down,
  Drill and promote. Caddy `basic_auth` (gokcaddy) is the viewer floor,
  the operator role behind the engine's own login (gobank ADR-0002 stage
  2, or a shared operator secret until it lands): the route split is not
  retrofittable, the login mechanism is
- #11 The engine public: the image on the cx23 with a Hetzner firewall
  allowing 22 and 1961 from the tailnet only, Tailscale for management
  and `gok update`, Caddy at `deploy.gobank.drummonds.net`. A job on the
  engine host is one container boundary from the tokens; when that
  matters, the external runner is this same image on a second server
  with a runner flag, created at will. Open: whether demo viewers go on
  the tailnet, which removes the public port altogether

## Rehearse then promote

The goal at the top: every gobank release rehearsed on preprod against
prod's data and gated before the same binary goes to prod. Waits on the
gobank prerequisites above.
- #12 Snapshot: copy prod's database into preprod over ssh
- #13 Gates: version, schema, invariants (account count and total
  balances unchanged, trial balance balances), BFF journeys
- #14 Rehearse then promote: preprod on a fresh snapshot, gates, same
  release to prod
- #15 Blue/green in the box: second systemd unit, port switch, rollback
  is redeploying the previous release
- #16 Separate database box: the app's memory share rises from 50% of
  the box (see `appShareWithLocalPostgres`) once PostgreSQL is
  elsewhere; logical replication for Postgres version upgrades and zone
  moves (researched in gobank-db)
- #17 Masking step in the snapshot once real PII exists

## Engine hardening

Small correctness and simplification items in the engine's own code,
found in review rather than by a failing deploy; none changes behaviour
an operator sees.
- #22 probe.Measure on errgroup: limit, first error and cancel from the library instead of hand-rolled slots, WaitGroup and mutex
- #23 prefixWriter.Write returns 0 after a full write, breaking the io.Writer contract
