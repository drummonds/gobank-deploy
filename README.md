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
tp secrets task ui ENVS=prod,preprod,demo
```

On a host that cannot build `cmd/demo` (no Go toolchain or no gobank
checkout at `-src`), the page shows status and offers Down only; Create and
Redeploy need a machine that can build. That is how it runs on hydrogen, the
LAN gokrazy appliance, as a package of its `gok_local` instance config:

```json
"git.bytestone.uk/hum3/gobank-deploy/cmd/gobank-deploy": {
  "CommandLineFlags": ["-build", "/perm/gobank-deploy", "ui", "-addr", ":1348", "-envs", "prod,preprod"],
  "Environment": ["HCLOUD_TOKEN=${HCLOUD_TOKEN}"],
  "WaitForClock": true
}
```

with Caddy proxying `gobank-deploy.lan.drummonds.net` to `:1348`.

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
  one job per environment with its log; tested against a fake `Operator`.
- `cmd/gobank-deploy` — the command.

## Links

- Documentation: (not yet published)
- Source: https://git.bytestone.uk/hum3/gobank-deploy
- Mirror: https://github.com/drummonds/gobank-deploy
