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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	"git.bytestone.uk/hum3/gobank-deploy/internal/load"
	"git.bytestone.uk/hum3/gobank-deploy/internal/perf"
	"git.bytestone.uk/hum3/gobank-deploy/internal/remote"
	"git.bytestone.uk/hum3/gobank-deploy/internal/route53"
	"git.bytestone.uk/hum3/gobank-deploy/internal/scenario"
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
  gobank-deploy perf <env> [-scales small,large] [-db FILE] [-doc PATH]
                                                       performance run at every scale at once, one server
                                                       each (<env>-<scale>): create, add customers flat out
                                                       to 10,000, run days for seven years or six hours,
                                                       read both rates off the demo, remove; the figures
                                                       are kept in FILE and the
                                                       rows written into gobank's performance doc PATH
                                                       (default <-src>/benchmark.md) for you to commit
  gobank-deploy scenario <name> [-scale large] [-customers 1000] [-years 7] [-day-length 24h]
                        [-search Jones] [-keep] [-db FILE] [-journal DIR]
                                                       run the named scenario (seven-years) on a fresh
                                                       environment <name>-<scale>: create, add the customers
                                                       flat out, run the years flat out, set the day length
                                                       to wall clock, probe the staff pages from outside,
                                                       remove (unless -keep); the run is kept in FILE and
                                                       written as a page of the journal folder DIR (default
                                                       journal) with its line in DIR/index.md, for you to
                                                       annotate and commit; -ladder 1,4,16 the concurrencies
                                                       the staff pages are probed at
  gobank-deploy scan <env> [-ladder 1,4,16] [-samples 20] [-search Jones] [-db FILE] [-journal DIR]
                        [-doc PATH]                    full performance scan of <env> as it stands: every
                                                       route the demo's API document lists that the scan can
                                                       drive, loaded at each rung of the ladder, the rest
                                                       listed as not scanned; a page of the journal, and the
                                                       measured document written to PATH (default
                                                       <-src>/api/measured.json) for gobank's API pages,
                                                       for you to commit and release
  gobank-deploy load <env> [-scale small] [-customers 10000] [-ladder 1,2,5,...] [-reps 10] [-budget 2s]
                                                       load test on a fresh environment <env>: create it,
                                                       fill its books, make <env>-loader beside it on the
                                                       private network, walk the customer journey up the
                                                       ladder from the loader until a screen's p95 is over
                                                       -budget, remove both; the climb is kept in FILE
  gobank-deploy journey -base URL [-customers 1000] [-ladder 1,2,5,...] [-reps 10] [-budget 2s]
                                                       load test from this host: the customer journey (login,
                                                       accounts, one account) walked by each rung's users
                                                       -reps times, customers in sequence, until a screen's
                                                       p95 is over -budget; the app password from
                                                       GOBANK_APP_PASSWORD; the result as JSON on stdout,
                                                       a line a rung on stderr; no token needed
  gobank-deploy version

Global flags (before the subcommand):
  -src DIR     gobank checkout to build cmd/demo from (default ../gobank)
  -build DIR   build directory for the binary and pinned host keys (default build)
  -store DIR   deploy from this release store instead of building (the appliance);
               the repo's newest release is fetched into it before a deploy
  -dns DOMAIN  each environment is <env>.DOMAIN in Route 53 (default gobank.drummonds.net; "" for none)
  -admit CIDRS the ranges every firewall admits to ssh (22) and the demo (1347), comma separated
               (default $GOBANK_DEPLOY_ADMIT; empty admits anywhere); set on every up. The private
               network gobank (10.0.0.0/16) is not filtered: a loader reaches a demo there
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
	admit := global.String("admit", os.Getenv("GOBANK_DEPLOY_ADMIT"), "CIDRs the firewalls admit to ssh and the demo, comma separated; empty admits anywhere")
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
	if cmd == "journey" {
		fs := flag.NewFlagSet("journey", flag.ExitOnError)
		base := fs.String("base", "", "the customer BFF, e.g. http://10.0.1.2:1347/")
		customers := fs.Int("customers", 1000, "customers on the books, walked in sequence")
		ladder := fs.String("ladder", "", "concurrent users at each rung (default 1,2,5,10,20,50,100,200,500)")
		reps := fs.Int("reps", 10, "journeys each user walks at a rung")
		budget := fs.Duration("budget", 2*time.Second, "the p95 a screen may reach and the rung still hold")
		_ = fs.Parse(args)
		if *base == "" {
			usage()
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		res := load.Test(ctx, load.Config{Base: *base, Password: os.Getenv("GOBANK_APP_PASSWORD"), Customers: *customers,
			Ladder: parseLadder(*ladder), Reps: *reps, Budget: *budget, Progress: os.Stderr})
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
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
	// The project as last listed: one request answers the page and the
	// flows for 30s, within Hetzner's 3,600 an hour (#28).
	cloud := deploy.NewCachedCloud(hetzner.New(token), 30*time.Second)
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
			Admit: cidrs(*admit),
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
		st, err := openDatabase(ctx, *dbFile)
		if err != nil {
			log.Fatal(err)
		}
		database, runs := st.db, st.runs
		defer database.Close()
		demo := &flows.Demo{Ops: factory, Store: runs}
		drill := &flows.Drill{Ops: factory, Console: &remote.Console{}, Store: runs, Drills: st.drills}
		perfFlow := &flows.Perf{Ops: factory, Console: &remote.Console{}, Store: runs, Perfs: st.perfs}
		loadFlow := &flows.Load{Ops: factory, Console: &remote.Console{}, Store: runs, Loads: st.loads}
		scenarioFlow := &flows.Scenario{Ops: factory, Console: &remote.Console{}, Prober: &remote.Browser{}, Store: runs, Scenarios: st.scenarios}
		page, err := ui.New(ui.Ops{DeployerFactory: factory, Flows: demo, Drill_: drill, Perf_: perfFlow, Load_: loadFlow, Scenario_: scenarioFlow}, envs)
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
	case "perf":
		fs := flag.NewFlagSet("perf", flag.ExitOnError)
		scales := fs.String("scales", "small,large", "scales to measure at once, one server each (small|medium|large|xl or any hcloud server type)")
		dbFile := fs.String("db", filepath.Join(*buildDir, "gobank-deploy.db"), "pglike database the runs are recorded in")
		doc := fs.String("doc", filepath.Join(*src, "benchmark.md"), "gobank's performance doc to put the rows into; \"\" to only print them")
		_ = fs.Parse(args)
		var st *stores
		if st, err = openDatabase(ctx, *dbFile); err != nil {
			break
		}
		defer st.db.Close()
		runs, perfStore := st.runs, st.perfs
		var list []string
		for s := range strings.SplitSeq(*scales, ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		perfFlow := &flows.Perf{Ops: ui.DeployerFactory(newDeployer), Console: &remote.Console{}, Store: runs, Perfs: perfStore}
		var records []perf.Run
		records, err = perfFlow.RunAll(ctx, env, list, os.Stdout)
		if len(records) > 0 {
			var rows []string
			for _, r := range records {
				rows = append(rows, ui.BenchmarkRow(r))
			}
			fmt.Println("\nFor benchmark.md:")
			fmt.Println(strings.Join(rows, "\n"))
			if *doc != "" {
				if derr := updateBenchmarkDoc(*doc, rows); derr != nil {
					fmt.Fprintln(os.Stderr, "benchmark.md not updated:", derr)
				} else {
					fmt.Printf("\n%s updated: commit it and release gobank so the doc is published\n", *doc)
				}
			}
		}
	case "load":
		fs := flag.NewFlagSet("load", flag.ExitOnError)
		scale := fs.String("scale", "small", "scale of the target (small|medium|large|xl or any hcloud server type)")
		customers := fs.Int("customers", 10_000, "customers on the target's books, walked in sequence")
		ladder := fs.String("ladder", "", "concurrent users at each rung (default 1,2,5,10,20,50,100,200,500)")
		reps := fs.Int("reps", 10, "journeys each user walks at a rung")
		budget := fs.Duration("budget", 2*time.Second, "the p95 a screen may reach and the rung still hold")
		dbFile := fs.String("db", filepath.Join(*buildDir, "gobank-deploy.db"), "pglike database the runs are recorded in")
		_ = fs.Parse(args)
		var st *stores
		if st, err = openDatabase(ctx, *dbFile); err != nil {
			break
		}
		defer st.db.Close()
		flow := &flows.Load{Ops: ui.DeployerFactory(newDeployer), Console: &remote.Console{}, Store: st.runs, Loads: st.loads,
			Customers: *customers, Ladder: parseLadder(*ladder), Reps: *reps, Budget: *budget}
		err = flow.Run(ctx, env, *scale, os.Stdout)
	case "scenario":
		fs := flag.NewFlagSet("scenario", flag.ExitOnError)
		scale := fs.String("scale", "large", "scale of the one server (small|medium|large|xl or any hcloud server type)")
		customers := fs.Int("customers", 1000, "customers added flat out before the years")
		years := fs.Int("years", 7, "simulated years run flat out")
		dayLength := fs.Duration("day-length", 24*time.Hour, "day length set after the years: wall clock")
		search := fs.String("search", "Jones", "the name the customer search is probed with (posted, never in a URL)")
		keep := fs.Bool("keep", false, "leave the server up at the end")
		ladder := fs.String("ladder", "1,4,16", "concurrencies the staff pages are probed at")
		dbFile := fs.String("db", filepath.Join(*buildDir, "gobank-deploy.db"), "pglike database the runs are recorded in")
		journalDir := fs.String("journal", "journal", "folder the run's page and index line are written into; \"\" to only print the page")
		_ = fs.Parse(args)
		name := env.Name
		if name != "seven-years" {
			err = fmt.Errorf("unknown scenario %q: the scenarios are seven-years", name)
			break
		}
		cfg := scenario.Config{Name: name, Customers: *customers, Years: *years, DayLength: *dayLength, Search: *search}
		env = deploy.Environment{Name: name + "-" + *scale}
		var st *stores
		if st, err = openDatabase(ctx, *dbFile); err != nil {
			break
		}
		defer st.db.Close()
		runs, scenarioStore := st.runs, st.scenarios
		flow := &flows.Scenario{Ops: ui.DeployerFactory(newDeployer), Console: &remote.Console{}, Prober: &remote.Browser{}, Store: runs, Scenarios: scenarioStore, Ladder: parseLadder(*ladder)}
		var rec *scenario.Run
		rec, err = flow.Run(ctx, env, *scale, cfg, *keep, os.Stdout)
		if rec != nil {
			reportRun(*journalDir, *rec)
		}
	case "scan":
		fs := flag.NewFlagSet("scan", flag.ExitOnError)
		ladder := fs.String("ladder", "1,4,16", "concurrencies each route is loaded at")
		samples := fs.Int("samples", 20, "calls per route per rung")
		search := fs.String("search", "Jones", "the name the customer search is posted with")
		dbFile := fs.String("db", filepath.Join(*buildDir, "gobank-deploy.db"), "pglike database the runs are recorded in")
		journalDir := fs.String("journal", "journal", "folder the run's page and index line are written into; \"\" to only print the page")
		doc := fs.String("doc", filepath.Join(*src, "api", "measured.json"), "gobank's measured document, for its API pages; \"\" to not write it")
		_ = fs.Parse(args)
		var st *stores
		if st, err = openDatabase(ctx, *dbFile); err != nil {
			break
		}
		defer st.db.Close()
		runs, scenarioStore := st.runs, st.scenarios
		flow := &flows.Scenario{Ops: ui.DeployerFactory(newDeployer), Console: &remote.Console{}, Prober: &remote.Browser{}, Store: runs, Scenarios: scenarioStore, Ladder: parseLadder(*ladder), Samples: *samples}
		var rec *scenario.Run
		rec, err = flow.Scan(ctx, env, *search, os.Stdout)
		if rec != nil {
			reportRun(*journalDir, *rec)
			if *doc != "" {
				if b, merr := scenario.MeasuredJSON(*rec); merr != nil {
					fmt.Fprintln(os.Stderr, "measured document not written:", merr)
				} else if werr := os.WriteFile(*doc, b, 0o644); werr != nil {
					fmt.Fprintln(os.Stderr, "measured document not written:", werr)
				} else {
					fmt.Printf("%s written: commit it and release gobank so its API pages carry the scan\n", *doc)
				}
			}
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// updateBenchmarkDoc puts the rows into gobank's performance doc at path.
func updateBenchmarkDoc(path string, rows []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := perf.UpdateBenchmark(string(b), rows...)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

// cidrs reads "a/29,b/32"; blanks are dropped.
func cidrs(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseLadder reads "1,4,16"; a rung that does not parse is dropped and
// an empty ladder leaves the flow's default.
func parseLadder(s string) []int {
	var out []int
	for part := range strings.SplitSeq(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// reportRun puts the run into the journal folder, or prints its page
// when there is no folder or the page could not be written.
func reportRun(journalDir string, rec scenario.Run) {
	if journalDir == "" {
		fmt.Println("\n" + scenario.Page(rec))
		return
	}
	path, err := writeJournal(journalDir, rec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "journal not written:", err)
		fmt.Println("\n" + scenario.Page(rec))
		return
	}
	fmt.Printf("\n%s written and listed in %s: annotate it and commit\n", path, filepath.Join(journalDir, "index.md"))
}

// writeJournal writes the run's page into the journal folder, unless a
// page of that name exists (a page is never touched after it is
// written), and puts its line at the top of the folder's index.
func writeJournal(dir string, r scenario.Run) (string, error) {
	path := filepath.Join(dir, scenario.Filename(r))
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s exists and is not overwritten", path)
	}
	if err := os.WriteFile(path, []byte(scenario.Page(r)), 0o644); err != nil {
		return "", err
	}
	indexPath := filepath.Join(dir, "index.md")
	b, err := os.ReadFile(indexPath)
	if err != nil {
		return path, err
	}
	out, err := scenario.UpdateIndex(string(b), r)
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(indexPath, []byte(out), 0o644)
}

// openDatabase opens the pglike file (created if absent, its directory
// too) and brings every component's schema up to date: gobank-workflow's
// run records, this program's drills, its performance runs and its
// scenario runs, each versioned in its own migrations table.
// stores are this program's records, all in one pglike database.
type stores struct {
	db        *sql.DB
	runs      *sqlstore.Store
	drills    *drills.Store
	perfs     *perf.Store
	scenarios *scenario.Store
	loads     *load.Store
}

func openDatabase(ctx context.Context, file string) (*stores, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	d, err := sql.Open("pglike", file)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", file, err)
	}
	st := &stores{db: d}
	fail := func(err error) (*stores, error) {
		d.Close()
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if st.runs, err = sqlstore.New(d); err != nil {
		return fail(err)
	}
	if st.drills, err = drills.New(ctx, d); err != nil {
		return fail(err)
	}
	if st.perfs, err = perf.New(ctx, d); err != nil {
		return fail(err)
	}
	if st.scenarios, err = scenario.New(ctx, d); err != nil {
		return fail(err)
	}
	if st.loads, err = load.NewStore(ctx, d); err != nil {
		return fail(err)
	}
	return st, nil
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
	if st.AdminPassword != "" {
		fmt.Printf("staff:  login admin, password %s\n", st.AdminPassword)
	}
	if st.APIToken != "" {
		fmt.Printf("api:    token %s (drives the console as the operator)\n", st.APIToken)
	}
	fmt.Printf("note:   server bills until deleted — tp secrets gobank-deploy down %s\n", env.Name)
}
