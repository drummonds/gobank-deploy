// gobank-deploy manages gobank environments (prod, preprod, ...) on Hetzner
// Cloud: one server per environment with PostgreSQL and the Model Bank
// demo on it.
//
//	gobank-deploy up <env> [-create] [-scale small|medium|large|xl|<type>]
//	gobank-deploy down <env> [-y]
//	gobank-deploy status <env>
//	gobank-deploy ui [-addr :1348] [-envs prod,preprod,demo] [-db build/gobank-deploy.db]
//	gobank-deploy build [-out build/releases]
//
// The ui keeps its workflow runs and gobank's upgrade drills in a pglike
// (SQLite file) database, -db, so they survive a restart; on hydrogen it
// is on /perm beside the release store.
//
// A host that cannot build cmd/demo (the hydrogen appliance) deploys from
// a release store instead: -store DIR. Before a deploy the repo's newest
// release is fetched into it (the binaries gobank's tp release puts on the
// Forgejo release), so a new gobank version needs nothing else deployed;
// `build` on the laptop and `task push` fill it by hand for a release the
// repo has not built, or a rollback. Its ssh identity comes from
// GOBANK_DEPLOY_SSH_KEY (base64 of a private key in PEM) since there is
// no agent or ~/.ssh.
//
// Each environment gets the hostname <env>.<-dns domain> in Route 53 (A
// record, set on up, removed on down) when AWS credentials are present;
// without them DNS is skipped and said so. -dns "" turns it off.
//
// Needs HCLOUD_TOKEN in the environment: run via `tp secrets gobank-deploy ...`.
// Creating a server starts billing, so `up` on a missing server refuses
// unless -create is given; `down` deletes the server, which is what stops
// billing (a powered-off server still bills).
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	dbexplorer "git.bytestone.uk/hum3/go-dbexplorer"
	_ "git.bytestone.uk/hum3/go-postgres"
	"git.bytestone.uk/hum3/gobank-workflow/sqlstore"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
	"git.bytestone.uk/hum3/gobank-deploy/internal/forge"
	"git.bytestone.uk/hum3/gobank-deploy/internal/hetzner"
	"git.bytestone.uk/hum3/gobank-deploy/internal/remote"
	"git.bytestone.uk/hum3/gobank-deploy/internal/route53"
	"git.bytestone.uk/hum3/gobank-deploy/internal/store"
	"git.bytestone.uk/hum3/gobank-deploy/internal/ui"
)

var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gobank-deploy up <env> [-create] [-scale small|medium|large|xl|<hcloud type>]
  gobank-deploy down <env> [-y]
  gobank-deploy status <env>
  gobank-deploy ui [-addr :1348] [-envs prod,preprod,demo] [-db FILE]
                                                       web page: states and controls for -envs plus every
                                                       gobank-* server in the project, and a form to add one
                                                       (status and down only where no release can be had);
                                                       workflow runs and upgrade drills kept in the pglike
                                                       database FILE (default <-build>/gobank-deploy.db)
  gobank-deploy build [-out DIR]                        build cmd/demo for linux amd64+arm64 into a release
                                                       store (default build/releases); no token needed
  gobank-deploy version

Global flags (before the subcommand):
  -src DIR     gobank checkout to build cmd/demo from (default ../gobank)
  -build DIR   build directory for the binary and pinned host keys (default build)
  -store DIR   deploy from this release store instead of building (the appliance);
               the repo's newest release is fetched into it before a deploy
  -dns DOMAIN  each environment is <env>.DOMAIN in Route 53 (default gobank.drummonds.net; "" for none)
  -repo URL    gobank on the Forgejo: its newest tag is reported, and its release of that tag
               fetched into -store (default https://git.bytestone.uk/hum3/gobank; "" for none)

HCLOUD_TOKEN must be set: run via  tp secrets gobank-deploy ...
AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY for -dns; without them DNS is skipped.
GOBANK_DEPLOY_SSH_KEY, if set, is base64 of a private key in PEM to ssh with.`)
	os.Exit(2)
}

func main() {
	global := flag.NewFlagSet("gobank-deploy", flag.ExitOnError)
	src := global.String("src", "../gobank", "gobank checkout to build from")
	buildDir := global.String("build", "build", "build directory")
	storeDir := global.String("store", "", "release store to deploy from instead of building")
	domain := global.String("dns", "gobank.drummonds.net", "Route 53 domain for <env>.DOMAIN hostnames; empty for none")
	repoURL := global.String("repo", "https://git.bytestone.uk/hum3/gobank", "gobank repo on the Forgejo, for its newest tag; empty for none")
	global.Usage = usage
	_ = global.Parse(os.Args[1:])
	args := global.Args()
	if len(args) < 1 {
		usage()
	}
	cmd, args := args[0], args[1:]
	if cmd == "version" {
		fmt.Println(version)
		return
	}
	if cmd == "build" {
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		out := fs.String("out", filepath.Join(*buildDir, "releases"), "release store to build into")
		_ = fs.Parse(args)
		if err := buildRelease(context.Background(), *src, *buildDir, *out); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	identity, err := identityFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "HCLOUD_TOKEN not set — run via: tp secrets gobank-deploy", strings.Join(os.Args[1:], " "))
		os.Exit(1)
	}
	cloud := hetzner.New(token)
	dns := dnsFor(context.Background(), *domain)
	var repo deploy.Repo
	if *repoURL != "" {
		repo = &forge.Repo{URL: *repoURL}
	}
	var builder deploy.Builder = &remote.Builder{Src: *src, Dir: *buildDir}
	var releaseStore deploy.ReleaseStore
	upUnavailable := func() string { return remote.CanBuild(*src) }
	if *storeDir != "" {
		st := &store.Store{Dir: *storeDir}
		builder, releaseStore, upUnavailable = st, st, st.Unavailable
		if repo != nil {
			// An empty store is filled from the repo by the deploy itself.
			upUnavailable = func() string { return "" }
		}
	}
	newDeployer := func(out io.Writer) *deploy.Deployer {
		return &deploy.Deployer{
			Cloud: cloud,
			Repo:  repo,
			Store: releaseStore,
			DNS:   dns, Domain: *domain,
			Dial:  &remote.Dialer{KnownHosts: filepath.Join(*buildDir, "known_hosts"), Identity: identity},
			Build: builder,
			Probe: &remote.Prober{},
			Out:   out,
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cmd == "ui" {
		fs := flag.NewFlagSet("ui", flag.ExitOnError)
		addr := fs.String("addr", ":1348", "listen address")
		names := fs.String("envs", "prod,preprod,demo", "environments always shown, provisioned or not")
		dbFile := fs.String("db", filepath.Join(*buildDir, "gobank-deploy.db"), "pglike database for workflow runs and drills")
		_ = fs.Parse(args)
		var envs []deploy.Environment
		for n := range strings.SplitSeq(*names, ",") {
			if n = strings.TrimSpace(n); n != "" {
				envs = append(envs, deploy.Environment{Name: n})
			}
		}
		factory := ui.DeployerFactory(newDeployer)
		database, runs, drillStore, err := openDatabase(ctx, *dbFile)
		if err != nil {
			log.Fatal(err)
		}
		defer database.Close()
		demo := &flows.Demo{Ops: factory, Store: runs}
		drill := &flows.Drill{Ops: factory, Console: &remote.Console{}, Store: runs, Drills: drillStore}
		page, err := ui.New(ui.Ops{DeployerFactory: factory, Flows: demo, Drill_: drill}, envs)
		if err != nil {
			log.Fatal(err)
		}
		explorer := &dbexplorer.Explorer{DB: database, BasePath: "/internal/explorer", UUIDLen: 8, TimeFormat: "2006-01-02 15:04:05"}
		page.Explorer = explorer.Render
		go page.Run(ctx, time.Minute)
		page.Version = "gobank-deploy " + version
		page.UpUnavailable = upUnavailable
		if why := upUnavailable(); why != "" {
			fmt.Println("status and down only:", why)
		}
		fmt.Printf("gobank environments UI on http://localhost%s/\n", *addr)
		srv := &http.Server{Addr: *addr, Handler: page}
		go func() { <-ctx.Done(); srv.Close() }()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
		return
	}

	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		usage()
	}
	env := deploy.Environment{Name: args[0]}
	args = args[1:]
	d := newDeployer(os.Stdout)

	switch cmd {
	case "up":
		fs := flag.NewFlagSet("up", flag.ExitOnError)
		create := fs.Bool("create", false, "create the server if it does not exist (starts billing)")
		scale := fs.String("scale", "small", "small|medium|large|xl or any hcloud server type")
		_ = fs.Parse(args)
		_, err = d.Up(ctx, deploy.UpOptions{Env: env, Scale: *scale, Create: *create})
		if err == nil {
			fmt.Printf("Turn off (and stop billing) with: tp secrets gobank-deploy down %s\n", env.Name)
		}
	case "down":
		fs := flag.NewFlagSet("down", flag.ExitOnError)
		yes := fs.Bool("y", false, "delete without asking")
		_ = fs.Parse(args)
		if !*yes && !confirm(fmt.Sprintf("Delete server %s and its database? [y/N] ", env.ServerName())) {
			fmt.Println("aborted")
			os.Exit(1)
		}
		err = d.Down(ctx, env)
	case "status":
		var st deploy.Status
		st, err = d.Status(ctx, env)
		if err == nil {
			printStatus(env, st)
			printReleases(ctx, d)
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openDatabase opens the pglike file (created if absent, its directory
// too) and brings both components' schemas up to date: gobank-workflow's
// run records and this program's drills, each versioned in its own
// migrations table.
func openDatabase(ctx context.Context, file string) (*sql.DB, *sqlstore.Store, *drills.Store, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, nil, nil, err
	}
	d, err := sql.Open("pglike", file)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open %s: %w", file, err)
	}
	runs, err := sqlstore.New(d)
	if err != nil {
		d.Close()
		return nil, nil, nil, fmt.Errorf("%s: %w", file, err)
	}
	ds, err := drills.New(ctx, d)
	if err != nil {
		d.Close()
		return nil, nil, nil, fmt.Errorf("%s: %w", file, err)
	}
	return d, runs, ds, nil
}

// dnsFor is the Route 53 provider for domain, or nil (with a notice) when
// there is no domain or no way to reach AWS. A missing hosted zone is a
// configuration error and fatal.
func dnsFor(ctx context.Context, domain string) deploy.DNS {
	if domain == "" {
		return nil
	}
	d, err := route53.New(ctx, domain)
	if errors.Is(err, route53.ErrNoCredentials) {
		fmt.Fprintf(os.Stderr, "dns off: %v\n", err)
		return nil
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return d
}

// identityFromEnv decodes GOBANK_DEPLOY_SSH_KEY (base64 of a PEM private
// key), or returns nil when unset.
func identityFromEnv() ([]byte, error) {
	v := os.Getenv("GOBANK_DEPLOY_SSH_KEY")
	if v == "" {
		return nil, nil
	}
	pem, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("GOBANK_DEPLOY_SSH_KEY is not base64: %w", err)
	}
	return pem, nil
}

// buildRelease builds cmd/demo for every server architecture into the
// release store at out, as one version.
func buildRelease(ctx context.Context, src, buildDir, out string) error {
	if why := remote.CanBuild(src); why != "" {
		return fmt.Errorf("cannot build: %s", why)
	}
	st := &store.Store{Dir: out}
	var ver string
	for _, goarch := range deploy.Architectures {
		b := &remote.Builder{Src: src, Dir: filepath.Join(buildDir, "linux-"+goarch)}
		rel, err := b.Build(ctx, goarch)
		if err != nil {
			return err
		}
		if err := st.Put(rel.Version, goarch, rel.Binary); err != nil {
			return err
		}
		ver = rel.Version
	}
	fmt.Printf("release %s in %s (latest)\n", ver, out)
	return nil
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.TrimSpace(line) {
	case "y", "Y":
		return true
	}
	return false
}

// printReleases says where the repo stands against what is deployable here.
func printReleases(ctx context.Context, d *deploy.Deployer) {
	rel, err := d.Releases(ctx)
	if err != nil {
		fmt.Printf("repo:   %v\n", err)
	}
	if rel.Repo != "" {
		fmt.Printf("repo:   newest tag %s", rel.Repo)
		switch {
		case rel.Lagging() && rel.Fetches:
			fmt.Printf(" — the store has %s; the next up fetches %s", rel.Available, rel.Repo)
		case rel.Lagging():
			fmt.Printf(" — %s is what can be deployed from here (task push, or pull the checkout)", rel.Available)
		}
		fmt.Println()
	}
}

func printStatus(env deploy.Environment, st deploy.Status) {
	if st.Server == nil {
		fmt.Printf("server %s: not provisioned (no billing)\n", env.ServerName())
		return
	}
	s := st.Server
	fmt.Printf("server %s: %s %s %s %s\n", s.Name, s.Type, s.Status, s.IP, s.Location)
	if st.Host != "" {
		fmt.Printf("dns:    %s\n", st.Host)
	}
	fmt.Printf("demo:   %s\n", st.URL)
	if st.Serving {
		fmt.Println("state:  serving")
		switch {
		case st.Version == "" && st.Available == "":
			fmt.Println("version: unknown")
		case st.Behind():
			fmt.Printf("version: %s — %s available: tp secrets gobank-deploy up %s\n", st.Version, st.Available, env.Name)
		default:
			fmt.Printf("version: %s (current)\n", st.Version)
		}
	} else {
		fmt.Println("state:  NOT answering (booting? ssh in and check: journalctl -u gobank-demo)")
	}
	if st.AppPassword != "" {
		fmt.Printf("app:    any customer ID, password %s (BFF under %sv1/)\n", st.AppPassword, strings.TrimSuffix(st.URL, "/")+"/")
	}
	fmt.Printf("note:   server bills until deleted — tp secrets gobank-deploy down %s\n", env.Name)
}
