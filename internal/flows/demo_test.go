package flows

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	wf "git.bytestone.uk/hum3/gobank-workflow"

	"git.bytestone.uk/hum3/gobank-deploy/internal/deploy"
)

// fakeOps is the deployer as the workflow sees it: a server that exists or
// not, and the ups and downs asked for.
type fakeOps struct {
	server *deploy.Server
	ups    []deploy.UpOptions
	downs  int
	upErr  error
}

func (f *fakeOps) Status(context.Context, deploy.Environment) (deploy.Status, error) {
	return deploy.Status{Server: f.server}, nil
}

func (f *fakeOps) Up(_ context.Context, o deploy.UpOptions, out io.Writer) error {
	f.ups = append(f.ups, o)
	if f.upErr != nil {
		err := f.upErr
		f.upErr = nil
		return err
	}
	f.server = &deploy.Server{Name: o.Env.ServerName(), IP: "10.0.0.7"}
	io.WriteString(out, "deployed\n")
	return nil
}

func (f *fakeOps) Down(context.Context, deploy.Environment, io.Writer) error {
	f.downs++
	f.server = nil
	return nil
}

var start = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

type harness struct {
	ops   *fakeOps
	store *wf.MemStore
	now   time.Time
	out   strings.Builder
	demo  *Demo
	env   deploy.Environment
}

func newHarness() *harness {
	h := &harness{ops: &fakeOps{}, store: wf.NewMemStore(), now: start}
	h.demo = &Demo{
		Ops:   h.ops,
		Store: h.store,
		Now:   func() time.Time { return h.now },
		Sleep: func(ctx context.Context, d time.Duration) error { h.now = h.now.Add(d); return ctx.Err() },
		Poll:  time.Minute,
	}
	h.env = deploy.Environment{Name: "demo", Expires: start.Add(2 * time.Hour)}
	return h
}

func (h *harness) steps(t *testing.T) map[string]wf.RunStatus {
	t.Helper()
	runs, _ := h.store.ListRuns(context.Background(), 1)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	steps, _ := h.store.ListSteps(context.Background(), runs[0].ID)
	out := map[string]wf.RunStatus{}
	for _, s := range steps {
		out[s.StepName] = s.Status
	}
	return out
}

func TestDemoCreatesServesUntilExpiryThenRemoves(t *testing.T) {
	h := newHarness()
	if err := h.demo.Run(context.Background(), h.env, "medium", &h.out); err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 1 || !h.ops.ups[0].Create || h.ops.ups[0].Scale != "medium" || !h.ops.ups[0].Expires.Equal(h.env.Expires) {
		t.Errorf("ups = %+v", h.ops.ups)
	}
	if h.ops.downs != 1 {
		t.Errorf("downs = %d", h.ops.downs)
	}
	if h.now.Before(h.env.Expires) {
		t.Errorf("removed at %v, before expiry %v", h.now, h.env.Expires)
	}
	runs, _ := h.store.ListRuns(context.Background(), 1)
	if runs[0].WorkflowType != WorkflowDemo || runs[0].Key != "demo@2026-09-27T14:00:00Z" || runs[0].Status != wf.StatusCompleted {
		t.Errorf("run = %+v", runs[0])
	}
	for _, step := range []string{"create", "serve", "remove"} {
		if h.steps(t)[step] != wf.StatusCompleted {
			t.Errorf("step %s = %v", step, h.steps(t)[step])
		}
	}
	for _, want := range []string{"== create", "deployed", "== serve until 2026-09-27T14:00:00Z", "== remove"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("log missing %q:\n%s", want, h.out.String())
		}
	}
}

// After a restart nothing remembers the run, but the server carries its
// expiry: the workflow picks it up from where it is.
func TestDemoOfAnExistingServerSkipsCreate(t *testing.T) {
	h := newHarness()
	h.ops.server = &deploy.Server{Name: "gobank-demo"}
	if err := h.demo.Run(context.Background(), h.env, "", &h.out); err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 0 || h.ops.downs != 1 {
		t.Errorf("ups = %d downs = %d", len(h.ops.ups), h.ops.downs)
	}
}

func TestDemoEndsEarlyWhenTheServerIsRemovedElsewhere(t *testing.T) {
	h := newHarness()
	h.demo.Sleep = func(ctx context.Context, d time.Duration) error {
		h.now = h.now.Add(d)
		h.ops.server = nil // someone pressed Down
		return nil
	}
	if err := h.demo.Run(context.Background(), h.env, "small", &h.out); err != nil {
		t.Fatal(err)
	}
	if !h.now.Before(h.env.Expires) {
		t.Error("should not have waited for expiry")
	}
	if h.steps(t)["remove"] != wf.StatusCompleted {
		t.Error("remove still runs, and is a no-op")
	}
}

func TestDemoFailedCreateIsResumedByTheNextRun(t *testing.T) {
	h := newHarness()
	h.ops.upErr = errors.New("hcloud: out of stock")
	err := h.demo.Run(context.Background(), h.env, "small", &h.out)
	if err == nil || !strings.Contains(err.Error(), "out of stock") {
		t.Fatalf("err = %v", err)
	}
	if h.steps(t)["create"] != wf.StatusFailed {
		t.Errorf("steps = %v", h.steps(t))
	}
	if err := h.demo.Run(context.Background(), h.env, "small", &h.out); err != nil {
		t.Fatal(err)
	}
	if len(h.ops.ups) != 2 || h.ops.downs != 1 {
		t.Errorf("ups = %d downs = %d", len(h.ops.ups), h.ops.downs)
	}
	runs, _ := h.store.ListRuns(context.Background(), 5)
	if len(runs) != 1 || runs[0].Status != wf.StatusCompleted {
		t.Errorf("one instance, completed: %+v", runs)
	}
}

func TestDemoCancelledWhileServingFailsAtServe(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	h.demo.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if err := h.demo.Run(ctx, h.env, "small", &h.out); err == nil {
		t.Fatal("cancel should fail the run")
	}
	steps := h.steps(t)
	if steps["create"] != wf.StatusCompleted || steps["serve"] != wf.StatusFailed || h.ops.downs != 0 {
		t.Errorf("steps = %v downs = %d", steps, h.ops.downs)
	}
}

func TestDemoDefinitionIsRegistered(t *testing.T) {
	for _, d := range wf.Definitions {
		if d.Type == WorkflowDemo && len(d.Steps) == 3 {
			return
		}
	}
	t.Error("demo definition not in the registry")
}
