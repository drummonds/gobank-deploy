// Package flows holds gobank-deploy's workflows on the gobank-workflow
// engine. Each is a pipeline of stages recorded as one keyed instance, so
// a failed stage is resumed by the next run and a finished one is final.
package flows

import (
	"context"
	"fmt"
	"io"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// WorkflowDemo is a temporary environment: created, served until its
// expiry, then removed. The instance key is <env>@<expiry>.
const WorkflowDemo wf.WorkflowType = "demo"

const src = "https://git.bytestone.uk/hum3/gobank-deploy/src/branch/main/internal/flows/demo.go"

// DemoDefinition describes the workflow for the registry and the page.
var DemoDefinition = wf.Definition{
	Type:        WorkflowDemo,
	Description: "A temporary environment: created with its expiry on the server, served until then, removed. Whatever restarts finds the server by its expiry and resumes.",
	InstanceKey: "environment@expiry",
	Steps: []wf.Step{
		{Name: "create", Description: "up -create with the expiry label; skipped when the server already exists", Source: src},
		{Name: "serve", Description: "wait until the expiry, or until the server is gone", Source: src},
		{Name: "remove", Description: "down: delete the server, firewall and hostname", Source: src},
	},
}

func init() { wf.Definitions = append(wf.Definitions, DemoDefinition) }

// Ops is the deployer as the workflows drive it.
type Ops interface {
	Status(ctx context.Context, env deploy.Environment) (deploy.Status, error)
	Up(ctx context.Context, o deploy.UpOptions, out io.Writer) error
	Down(ctx context.Context, env deploy.Environment, out io.Writer) error
}

// Demo runs the demo workflow.
type Demo struct {
	Ops   Ops
	Store wf.Store

	Now   func() time.Time                                 // defaults to time.Now
	Sleep func(ctx context.Context, d time.Duration) error // defaults to a timer; returns ctx's error when cancelled
	Poll  time.Duration                                    // how often serve looks at the server; defaults to a minute
}

func (d *Demo) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Demo) sleep(ctx context.Context, t time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, t)
	}
	select {
	case <-time.After(t):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Demo) poll() time.Duration {
	if d.Poll > 0 {
		return d.Poll
	}
	return time.Minute
}

// Key is the instance key for env: its name and expiry.
func Key(env deploy.Environment) string {
	return env.Name + "@" + env.Expires.UTC().Format(time.RFC3339)
}

// Run creates env (at scale, unless its server exists), serves it until
// env.Expires and removes it, logging to out. A run for a key that failed
// before resumes from the failed stage.
func (d *Demo) Run(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	stages := []wf.Stage{
		{Name: "create", Run: func(ctx context.Context) error { return d.create(ctx, env, scale, out) }},
		{Name: "serve", Run: func(ctx context.Context) error { return d.serve(ctx, env, out) }},
		{Name: "remove", Run: func(ctx context.Context) error {
			fmt.Fprintln(out, "== remove")
			return d.Ops.Down(ctx, env, out)
		}},
	}
	runner := wf.NewRunner(d.Store, nil)
	rec, err := runner.RunPipeline(ctx, WorkflowDemo, Key(env), wf.BusinessDateFrom(d.now()), stages)
	if err != nil {
		return err
	}
	if rec == nil {
		fmt.Fprintf(out, "%s already completed\n", Key(env))
	}
	return nil
}

func (d *Demo) create(ctx context.Context, env deploy.Environment, scale string, out io.Writer) error {
	fmt.Fprintln(out, "== create")
	st, err := d.Ops.Status(ctx, env)
	if err != nil {
		return err
	}
	if st.Server != nil {
		fmt.Fprintf(out, "server %s exists, keeping it\n", st.Server.Name)
		return nil
	}
	if scale == "" {
		scale = "small"
	}
	return d.Ops.Up(ctx, deploy.UpOptions{Env: env, Scale: scale, Create: true, Expires: env.Expires}, out)
}

func (d *Demo) serve(ctx context.Context, env deploy.Environment, out io.Writer) error {
	fmt.Fprintf(out, "== serve until %s\n", env.Expires.UTC().Format(time.RFC3339))
	for {
		left := env.Expires.Sub(d.now())
		if left <= 0 {
			fmt.Fprintln(out, "expired")
			return nil
		}
		st, err := d.Ops.Status(ctx, env)
		if err != nil {
			return err
		}
		if st.Server == nil {
			fmt.Fprintln(out, "server already gone")
			return nil
		}
		if err := d.sleep(ctx, min(left, d.poll())); err != nil {
			return err
		}
	}
}
