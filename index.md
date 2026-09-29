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

## Environment states

| Server | Answers | Version vs available | State on the page | Offered |
|--------|---------|----------------------|-------------------|---------|
| none | – | – | Not provisioned, no billing | Create (scale, keep or remove after) |
| exists | no | – | Not answering, booting? | Redeploy, Down |
| exists | yes | same | Serving, current | Redeploy, Down |
| exists | yes | different | Serving, *vX available* | Redeploy, Down |
| exists, `expires` label | any | any | as above, plus *removed at* | the demo workflow's job, Down |

Where no release can be had (no toolchain and an empty store) only status
and Down are offered, and the page says why.
