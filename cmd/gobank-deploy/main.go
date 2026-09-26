// gobank-deploy manages gobank environments (prod, preprod, ...) on Hetzner
// Cloud: one server per environment with PostgreSQL and the Model Bank
// demo on it.
//
//	gobank-deploy up <env> [-create] [-scale small|medium|large|xl|<type>]
//	gobank-deploy down <env> [-y]
//	gobank-deploy status <env>
//	gobank-deploy ui [-addr :1348] [-envs prod,preprod]
//
// Needs HCLOUD_TOKEN in the environment: run via `tp secrets gobank-deploy ...`.
// Creating a server starts billing, so `up` on a missing server refuses
// unless -create is given; `down` deletes the server, which is what stops
// billing (a powered-off server still bills).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
	"git.bytestone.uk/hum3/gobank-deploy/internal/hetzner"
	"git.bytestone.uk/hum3/gobank-deploy/internal/remote"
	"git.bytestone.uk/hum3/gobank-deploy/internal/ui"
)

var version = "dev"

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  gobank-deploy up <env> [-create] [-scale small|medium|large|xl|<hcloud type>]
  gobank-deploy down <env> [-y]
  gobank-deploy status <env>
  gobank-deploy ui [-addr :1348] [-envs prod,preprod]   web page: states and controls
                                                       (status and down only where cmd/demo cannot be built)
  gobank-deploy version

Global flags (before the subcommand):
  -src DIR     gobank checkout to build cmd/demo from (default ../gobank)
  -build DIR   build directory for the binary and pinned host keys (default build)

HCLOUD_TOKEN must be set: run via  tp secrets gobank-deploy ...`)
	os.Exit(2)
}

func main() {
	global := flag.NewFlagSet("gobank-deploy", flag.ExitOnError)
	src := global.String("src", "../gobank", "gobank checkout to build from")
	buildDir := global.String("build", "build", "build directory")
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
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "HCLOUD_TOKEN not set — run via: tp secrets gobank-deploy", strings.Join(os.Args[1:], " "))
		os.Exit(1)
	}
	cloud := hetzner.New(token)
	newDeployer := func(out io.Writer) *deploy.Deployer {
		return &deploy.Deployer{
			Cloud: cloud,
			Dial:  &remote.Dialer{KnownHosts: filepath.Join(*buildDir, "known_hosts")},
			Build: &remote.Builder{Src: *src, Dir: *buildDir},
			Probe: &remote.Prober{},
			Out:   out,
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if cmd == "ui" {
		fs := flag.NewFlagSet("ui", flag.ExitOnError)
		addr := fs.String("addr", ":1348", "listen address")
		names := fs.String("envs", "prod,preprod", "environments to show")
		_ = fs.Parse(args)
		var envs []deploy.Environment
		for n := range strings.SplitSeq(*names, ",") {
			if n = strings.TrimSpace(n); n != "" {
				envs = append(envs, deploy.Environment{Name: n})
			}
		}
		page, err := ui.New(ui.DeployerFactory(newDeployer), envs)
		if err != nil {
			log.Fatal(err)
		}
		page.Version = "gobank-deploy " + version
		if why := remote.CanBuild(*src); why != "" {
			page.UpUnavailable = why
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

	var err error
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
		}
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
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

func printStatus(env deploy.Environment, st deploy.Status) {
	if st.Server == nil {
		fmt.Printf("server %s: not provisioned (no billing)\n", env.ServerName())
		return
	}
	s := st.Server
	fmt.Printf("server %s: %s %s %s %s\n", s.Name, s.Type, s.Status, s.IP, s.Location)
	fmt.Printf("demo:   %s\n", st.URL)
	if st.Serving {
		fmt.Println("state:  serving")
	} else {
		fmt.Println("state:  NOT answering (booting? ssh in and check: journalctl -u gobank-demo)")
	}
	fmt.Printf("note:   server bills until deleted — tp secrets gobank-deploy down %s\n", env.Name)
}
