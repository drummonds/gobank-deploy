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
tp secrets gobank-deploy up preprod -create -scale large # cx53: 16 vCPU / 32 GB
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
checkout at `-src`), the page shows status and offers Down only; Create and
Redeploy need a machine that can build. That is how it runs on hydrogen, the
LAN gokrazy appliance, as a package of its `gok_local` instance config:

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
appliance cannot build `cmd/demo`, so it deploys from a **release store**:

```sh
task build    # cmd/demo for linux amd64+arm64 into build/releases/<version>/, marks it latest
task push     # build, then copy the latest release to hydrogen:/perm/gobank-deploy/releases
```

Create and Redeploy on the page deploy whatever was pushed last; pushing
an older version again is a rollback. Until something has been pushed the
page offers status and Down only, and says why. The appliance's ssh
identity is `GOBANK_DEPLOY_SSH_KEY` (base64 of the private key in PEM,
from the gokrazy secrets note); its public half is registered in the
Hetzner project so new servers accept it, and was added to servers that
predate it by hand. Pinned host keys live in `/perm/gobank-deploy/known_hosts`.

### Temporary environments: the demo workflow

"Remove after" on either Create form runs the **demo** workflow instead of
a plain create: `create` (up with the removal time as an `expires` label on
the server), `serve` (until then, or until someone presses Down), `remove`
(down). It is a [gobank-workflow](https://git.bytestone.uk/hum3/gobank-workflow)
pipeline: one keyed instance (`<env>@<expiry>`) whose stages are recorded,
so a failed stage is resumed by the next run and a finished one is final.
The run records live in memory for now; what must survive is on the server
itself, and every minute the page starts the workflow for any temporary
server nobody is looking after, so a demo still goes after the page
restarts. `demo-workflow.d2` (from `go run ./cmd/flowd2`) draws it.

Each environment gets the hostname `<env>.gobank.drummonds.net`: an A
record in Route 53 (TTL 60s, since Hetzner reuses addresses) set on `up`
and removed on `down`. `-dns DOMAIN` changes the domain, `-dns ""` turns
it off. It needs `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (`tp secrets`
on the laptop, the gokrazy `Environment` on the appliance, as gokcaddy);
without them DNS is skipped and the start-up says so. The hosted zone is
the one whose name is the domain's longest suffix, so `drummonds.net`.

Scale presets: `small` (cx23), `medium` (cx33), `large` (cx53), `xl`
(ccx33), or any `hcloud server-type list` name. `cax*` types build for
arm64.

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
  HTTP `Prober`.
- `internal/ui` — the lofigui page: states, create / redeploy / down / cancel,
  one job per environment with its log, and an About page with the
  component diagrams; tested against a fake `Operator`.
- `internal/flows` — the workflows on gobank-workflow's pipeline runner:
  the temporary demo environment.
- `internal/route53`, `internal/store` — `DNS` on Route 53; the release store.
- `cmd/gobank-deploy` — the command.

## Links

- Documentation: https://gobank-deploy.docs.bytestone.uk/ (`tp pages` for a local preview)
- Source: https://git.bytestone.uk/hum3/gobank-deploy
- Mirror: https://github.com/drummonds/gobank-deploy
