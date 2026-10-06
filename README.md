# gobank-deploy

Deployment orchestrator for [gobank](https://git.bytestone.uk/hum3/gobank)
environments. One environment (`prod`, `preprod`, ...) is one Hetzner Cloud
server running PostgreSQL and the Model Bank demo. The orchestrator
creates it, puts a release on it, reports on it and deletes it.

It replaces the `deploy/hetzner/*.sh` scripts that lived in gobank, with
the same behaviour and the same billing rule: creating a server is an
explicit act (`-create`), and deleting it is what stops billing.

## Usage

`HCLOUD_TOKEN` is needed, so run everything through `tp secrets`:

```sh
tp secrets gobank-deploy up preprod -create              # cx23: 2 vCPU / 4 GB
tp secrets gobank-deploy up preprod -create -scale large # ccx33: 8 dedicated vCPU / 32 GB
tp secrets gobank-deploy up preprod                      # redeploy binary only
tp secrets gobank-deploy status preprod
tp secrets gobank-deploy down preprod                    # asks; -y to skip
```

A web page with the same controls, one row per environment, polling
every few seconds while a job runs:

```sh
tp secrets task ui                       # http://localhost:1348/
tp secrets task ui ENVS=prod,preprod     # rows always shown, even when not provisioned
```

The rows are the `-envs` names (default `prod,preprod,demo`) plus every
`gobank-*` server that exists in the Hetzner project, whoever created it.
A form on the page creates a further environment by name; it stays listed
while its server exists. So `-envs` is only the standing environments
worth a "Not provisioned" row.

On a host that cannot build `cmd/demo` (no Go toolchain or no gobank
checkout at `-src`), the page deploys from a release store filled from the
Forgejo, or, without one, shows status and offers Down only. That is how it
runs on hydrogen, the LAN gokrazy appliance, as a package of its `gok_local`
instance config:

```json
"git.bytestone.uk/hum3/gobank-deploy/cmd/gobank-deploy": {
  "CommandLineFlags": ["-build", "/perm/gobank-deploy", "-store", "/perm/gobank-deploy/releases",
                       "ui", "-addr", ":1348", "-envs", "prod,preprod,demo"],
  "Environment": ["HCLOUD_TOKEN=${HCLOUD_TOKEN}", "GOBANK_DEPLOY_SSH_KEY=${GOBANK_DEPLOY_SSH_KEY}",
                  "AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}", "AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}"],
  "WaitForClock": true
}
```

with Caddy proxying `gobank-deploy.lan.drummonds.net` to `:1348`. The
appliance cannot build `cmd/demo`, so it deploys from a **release store**
(`-store`), which it fills from the Forgejo itself: gobank's `tp release`
runs goreleaser (the **build stage**, `.goreleaser.yaml` in gobank) and
attaches `demo-linux-amd64` and `demo-linux-arm64` to the release of the
tag, and before every Create or Redeploy the page fetches the newest tag's
binaries into the store when it lacks them. gobank's release also tells the
page straight away: its `post_release` step (`task deploy:fetch`) posts to
`/fetch`, which fetches the newest release into the store now and answers
with its tag, or an error status the release prints as a warning. So a new
gobank version reaches an environment with nothing else released or
updated: release gobank, press Redeploy. A tag without binaries (from before the build stage) or a Forgejo
out of reach is a notice in the job log, and the deploy carries what the
store already has.

The laptop can fill the store by hand for a release the repo has not
built, or to roll back:

```sh
task build    # cmd/demo for linux amd64+arm64 into build/releases/<version>/, marks it latest
task push     # build, then copy the latest release to hydrogen:/perm/gobank-deploy/releases
```

Pushing an older version again makes it the store's latest, and since the
store still has the newer one it is not fetched again: the next deploy is
the rollback. From the store itself, `POST /fetch?tag=vX` makes `vX` the
next deploy, fetching it first if the store lacks it, so a rollback on
hydrogen is:

```sh
curl -X POST https://gobank-deploy.lan.drummonds.net/fetch?tag=v0.6.0   # then Redeploy
```

The unit's `TimeoutStopSec=900` lets the demo finish the simulated day in
progress before it exits (it does so on SIGTERM); the install script adds
it to units from before it. The page and `status` show gobank's newest tag on the Forgejo
beside what this host can deploy, and say whether the next deploy fetches
it (a store) or the laptop's checkout needs a pull. Without a store and a
toolchain the
page offers status and Down only, and says why. The appliance's ssh
identity is `GOBANK_DEPLOY_SSH_KEY` (base64 of the private key in PEM,
from the gokrazy secrets note); its public half is registered in the
Hetzner project so new servers accept it, and was added to servers that
predate it by hand. Pinned host keys live in `/perm/gobank-deploy/known_hosts`.

### The upgrade drill

**Drill to vX** on an environment's row runs gobank's
[upgrade drill](https://gobank.docs.bytestone.uk/upgrade-drill.html)
(ADR-0003) as a workflow: a day long enough to upgrade inside (set to 2h
when under 30m, then wait for a day with 10m left), observe the demo,
upgrade to the newest release, roll back, forward again, observing after
each hop. The demo is read at its `/about.json`; each hop is gated on the
version serving, a restart row that follows the expected release with a
known downtime and an intact handover, and the run no further on than a
restart takes it (the stop finishes the day in progress, the start begins
a new one: one day at each). The day length is set again after every hop,
since a release before gobank v0.10.3 forgets it on restart. It is offered
where the manual drill runs: a store to roll back
from and a newer release than the one serving. A failed hop fails the run
and says why; pressing Drill again the same day resumes from that hop.
The **Drills** page is the history, one line per drill ready for the
story's record; each run's steps are at `/workflows/<id>`, the tables at
`/internal/explorer`.

The **Workflows** page is the engine's view of all three workflows: each
definition with its diagram, its steps and the code behind each, the
count of its instances in every state and the instances themselves,
newest first. An instance page lays the steps as they ran over the
definition, so a step not reached shows as *not run*, and offers the
instance as d2 with the steps coloured by state
(`/workflows/<id>/diagram.d2`) for `d2 diagram.d2 out.svg`.

The page keeps its workflow runs and drills in a pglike (SQLite file)
database, `-db`, default `<-build>/gobank-deploy.db`: on hydrogen that is
`/perm/gobank-deploy/gobank-deploy.db`, beside the release store, so no
flag is needed there. gobank-workflow's tables and this program's drill
tables share the file, each component's schema versioned in its own
migrations table (`schema_migrations`, `deploy_schema_migrations`,
`perf_schema_migrations`).

### Performance runs: the perf workflow

**Measure** on the environments page (one scale) or `tp secrets
gobank-deploy perf <env>` (every scale at once: `-scales small,large` by
default, one server each, named `<env>-<scale>`, the rows written into
gobank's `benchmark.md` in the `-src` checkout — `-doc` to say where, or
`""` to only print them — for you to commit and release) makes gobank's
[performance run](https://gobank.docs.bytestone.uk/benchmark.html) as a
workflow: `create` (up -create at the scale; a server that exists is kept,
so a failed run resumes on the same box), `add` (the demo set flat out with
room for a million customers, then batches of 1,000 customers for ten
minutes, the rate taken from the demo batch by batch so this program's
polling is not in it), `days` (Run for ten minutes, read account days per
12h and the last day it is made of, Stop), `remove` (down). The two rates
are gobank v0.12's `/about.json` `sim` block. Each run is kept in the `-db`
database with where it ran — scale, server type, RAM, gobank version — and
the **Performance** page is the history, each run with its row for
gobank's `benchmark.md`. A run creates a billable server for about half an
hour, so it is started on purpose, never by a schedule.

### Temporary environments: the demo workflow

"Remove after" on either Create form runs the **demo** workflow instead of
a plain create: `create` (up with the removal time as an `expires` label on
the server), `serve` (until then, or until someone presses Down), `remove`
(down). It is a [gobank-workflow](https://git.bytestone.uk/hum3/gobank-workflow)
pipeline: one keyed instance (`<env>@<expiry>`) whose stages are recorded,
so a failed stage is resumed by the next run and a finished one is final.
The run records are in the `-db` database; what must survive is on the
server itself, and every minute the page starts the workflow for any temporary
server nobody is looking after, so a demo still goes after the page
restarts. `demo-workflow.d2`, `drill-workflow.d2` and `perf-workflow.d2`
(from `go run ./cmd/flowd2`) draw the three workflows.

Each environment gets the hostname `<env>.gobank.drummonds.net`: an A
record in Route 53 (TTL 60s, since Hetzner reuses addresses) set on `up`
and removed on `down`. `-dns DOMAIN` changes the domain, `-dns ""` turns
it off. It needs `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (`tp secrets`
on the laptop, the gokrazy `Environment` on the appliance, as gokcaddy);
without them DNS is skipped and the start-up says so. The hosted zone is
the one whose name is the domain's longest suffix, so `drummonds.net`. The
key's IAM policy must allow `route53:ChangeResourceRecordSets` on that zone
for A records under the domain, UPSERT and DELETE (on top of
`ListHostedZones`, `ListHostedZonesByName`, `ListResourceRecordSets` and
`GetChange`); a key scoped to another use, such as the LAN Caddy's
`_acme-challenge` TXT records, is refused with AccessDenied and the deploy
carries on by address.

Scale presets: `small` (cx23), `medium` (cx33), `large` (ccx33: 8
dedicated vCPU, 32 GB, so a measurement sees cores that are really there),
`xl` (ccx43: 16 dedicated vCPU, 64 GB), or any `hcloud server-type list`
name. `cax*` types build for arm64. When Hetzner has no capacity for the
type ("error during placement"), create tries fsn1, nbg1 and hel1 in turn,
then the next size down the ladder (ccx43, ccx33, cx53, cx43, cx33, cx23)
at every location again; the server's actual type is what status and a
perf run report. The ladder is a first cut, to be revised once there is
performance data.

`up` sizes the demo's memory to the box: `GOBANK_MEMORY_LIMIT` is set to
half the server type's RAM (PostgreSQL shares the box) in
`/etc/gobank/deploy.env`, which the systemd unit reads, so a redeploy can
change it. The demo's own default, 800MB, is what a browser tab holds.

`up` builds `cmd/demo` from the gobank checkout given by `-src` (default
`../gobank`) with `CGO_ENABLED=0`, so that checkout's local replace
directives still apply. Host keys are pinned per project in
`build/known_hosts`, accept-new style; a freshly created server's IP is
forgotten first because Hetzner reuses addresses.

## Requirements

- An SSH key registered in the Hetzner project and loaded in your agent
  (or at `~/.ssh/id_ed25519` / `id_rsa`).
- `tp unlock` session for the Bitwarden-held `HCLOUD_TOKEN`.

## Layout

- `internal/deploy` — the up / down / status sequences against `Cloud`,
  `Dialer`, `Builder` and `Prober` interfaces; tested with fakes.
- `internal/hetzner` — `Cloud` on hcloud-go.
- `internal/remote` — ssh `Dialer` with host-key pinning, Go `Builder`,
  HTTP `Prober`, and the demo's `Console` (its `/about.json` and settings).
- `internal/ui` — the lofigui page: states, create / redeploy / down / cancel,
  one job per environment with its log; the Drills, Performance and
  Workflows pages and an About page with the component diagrams; tested
  against a fake `Operator`.
- `internal/flows` — the workflows on gobank-workflow's pipeline runner:
  the temporary demo environment, and gobank's upgrade drill.
- `internal/drills` — the drill record: each drill with what the demo
  looked like before and after every hop, in the shared pglike database.
- `internal/route53`, `internal/store` — `DNS` on Route 53; the release store.
- `cmd/gobank-deploy` — the command.

## Links

- Documentation: https://gobank-deploy.docs.bytestone.uk/ (`tp pages` for a local preview)
- Source: https://git.bytestone.uk/hum3/gobank-deploy
- Mirror: https://github.com/drummonds/gobank-deploy
