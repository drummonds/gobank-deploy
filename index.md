# gobank-deploy

Deployment orchestrator for [gobank](https://git.bytestone.uk/hum3/gobank)
environments: one Hetzner Cloud server per environment (`prod`, `preprod`,
`demo`, …) running PostgreSQL and the Model Bank demo, created, deployed
to, reported on and deleted by this tool. Creating a server is an explicit
act; deleting it is what stops billing.

## Pages

- [README](README.html) — usage, flags, the release store, Route 53
- [Changelog](CHANGELOG.html)
- [Roadmap](ROADMAP.html)
- Source: <https://git.bytestone.uk/hum3/gobank-deploy> · mirror
  <https://github.com/drummonds/gobank-deploy>

## How a version reaches an environment

The code comes from the Forgejo. Releasing gobank there is the build
stage: goreleaser attaches the demo binaries to the release, and the
appliance, which cannot build, fetches them into its store before a
deploy. The laptop builds the demo from its own checkout and can push a
build to the appliance by hand. Either deploys to a server.

![Release path](release-path.svg)

## What the orchestrator talks to

From either place it runs: the same Hetzner project and Route 53 zone,
with secrets from Bitwarden.

![Components](components.svg)

| Part | Lives | Role |
|------|-------|------|
| gobank `cmd/demo` | Forgejo, checked out at `../gobank` | the Model Bank: what gets deployed |
| gobank-deploy | Forgejo; `go install` on the laptop, a gokrazy package on hydrogen | this orchestrator: `up`, `down`, `status`, `ui`, `build` |
| gobank release | Forgejo release of a tag | `demo-linux-amd64`, `demo-linux-arm64` from gobank's `tp release` (goreleaser): the build stage |
| release store | `build/releases` on the laptop, `/perm/gobank-deploy/releases` on hydrogen | `demo` for linux amd64 and arm64 per version, plus `latest`; filled from the gobank release on deploy, or by `task push` |
| Hetzner server `gobank-<env>` | Hetzner Cloud project | Ubuntu, PostgreSQL, the demo on :1347; a firewall of the same name; labels `environment` and, for a temporary one, `expires` |
| Route 53 record | `drummonds.net` zone | `<env>.gobank.drummonds.net` → the server, TTL 60 because addresses are reused |
| secrets | Bitwarden | `tp secrets` on the laptop; the gokrazy `Environment` on hydrogen |

## What `up` does

One sequence for create and redeploy; only the `-create` box starts billing.

![Deploy sequence](deploy-sequence.svg)

## The demo workflow

A temporary environment is a [gobank-workflow](https://gobank-workflow.docs.bytestone.uk/)
pipeline instance keyed `<env>@<expiry>`: created with its expiry as a
server label, served until then, removed. The page starts the workflow for
any temporary server nobody is looking after, so a restart does not leave
one billing.

![Demo workflow](demo-workflow.svg)

## The upgrade drill

gobank's [upgrade drill](https://gobank.docs.bytestone.uk/upgrade-drill.html)
(ADR-0003) is a second workflow: one instance per environment, release
pair and date. Step 3 of the manual drill, the release, stays with the
operator; the rest runs from the Drill button.

![Drill workflow](drill-workflow.svg)

| Term | Meaning |
|------|---------|
| drill | one rehearsal on an environment: upgrade from release N to N+1, roll back to N, forward to N+1 |
| hop | one redeploy inside a drill: upgrade, rollback, forward |
| observation | the demo as read at a moment (before, upgraded, rolled back, forward): version serving, position, newest restart row |
| position | the demo's day, day count, customers, savings, lending |
| restart row | the demo's own record of a process start: which release it followed, the downtime, whether day and customers matched across the stop |
| gate | a check on a hop's observation that fails the stage |

What a hop is gated on, and what is recorded but not gated:

| Observation after the hop | Outcome |
|---------------------------|---------|
| version serving is not the release fetched | fail: serving X, expected Y |
| no restart row | fail: the demo kept no record of this start |
| restart row follows a release other than the one before the hop (an unrecorded one is accepted) | fail: follows X, expected Y |
| downtime unknown | fail: the previous release did not stop cleanly |
| handover not intact | fail: day or customers differ across the stop |
| restart row's day more than one past the last observation | fail: the run went on before the stop (a clean stop finishes only the day in progress) |
| day now more than one past the restart row's | fail: the run went on after the start (a restart begins only one day) |
| release without `about.json` | version checked; position and restart row recorded as absent |
| customers, savings, lending differ from before | recorded side by side, not gated: the generator moves them within a day; the restart row already pins customers across the stop |
| all of the above hold | the hop completes |

Every drill is kept with its observations in the page's pglike database
beside the workflow runs; the Drills page is the history, the DB Explorer
the tables.

## The Workflows page

The engine's view of the three workflows: each definition's diagram, steps
and code, the count of its instances in every state, and the instances
themselves, newest first. An instance page lays the steps as they ran over
the definition, so a step not reached shows as *not run*, and offers the
instance as d2 with the steps coloured by state.

## Environment states

| Server | Answers | Version vs available | State on the page | Offered |
|--------|---------|----------------------|-------------------|---------|
| none | – | – | Not provisioned, no billing | Create (scale, keep or remove after) |
| exists | no | – | Not answering, booting? | Redeploy, Down |
| exists | yes | same | Serving, current | Redeploy, Down |
| exists | yes | different | Serving, *vX available* | Redeploy, Down |
| exists, `expires` label | any | any | as above, plus *removed at* | the demo workflow's job, Down |
| exists | yes | a newer release on the repo, and a store here | Serving, *vX available* | Redeploy, Drill to vX, Down |

Where no release can be had (no toolchain and an empty store) only status
and Down are offered, and the page says why.
