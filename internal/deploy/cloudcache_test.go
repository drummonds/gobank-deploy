package deploy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// countingCloud is the provider behind the cache: the fake, counting the
// calls that cost a request, with the list made to fail on demand.
type countingCloud struct {
	*fakeCloud
	lists, byName int
	listErr       error
}

func (c *countingCloud) Servers(ctx context.Context) ([]*Server, error) {
	c.lists++
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.fakeCloud.Servers(ctx)
}

func (c *countingCloud) Server(ctx context.Context, name string) (*Server, error) {
	c.byName++
	return c.fakeCloud.Server(ctx, name)
}

func newCachedCloud() (*CachedCloud, *countingCloud, *time.Time) {
	inner := &countingCloud{fakeCloud: newFakeCloud()}
	inner.servers["gobank-prod"] = &Server{Name: "gobank-prod", IP: "10.0.0.1", Status: "running"}
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	c := NewCachedCloud(inner, 30*time.Second)
	c.now = func() time.Time { return now }
	return c, inner, &now
}

// The project is listed once and the list answers every call for the
// next thirty seconds; the page's poll and the flows' reads then cost
// Hetzner nothing between refreshes.
func TestCachedCloudListsOncePerInterval(t *testing.T) {
	c, inner, now := newCachedCloud()
	ctx := context.Background()
	for range 3 {
		if _, err := c.Servers(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if inner.lists != 1 {
		t.Errorf("lists = %d; want the project listed once inside the interval", inner.lists)
	}
	*now = now.Add(31 * time.Second)
	if _, err := c.Servers(ctx); err != nil {
		t.Fatal(err)
	}
	if inner.lists != 2 {
		t.Errorf("lists = %d; want a fresh list once the interval has passed", inner.lists)
	}
}

// A server by name is read off the list, never asked for on its own: one
// request serves every environment on the page.
func TestCachedCloudAnswersServerByNameFromTheList(t *testing.T) {
	c, inner, _ := newCachedCloud()
	ctx := context.Background()
	for range 3 {
		srv, err := c.Server(ctx, "gobank-prod")
		if err != nil || srv == nil || srv.IP != "10.0.0.1" {
			t.Fatalf("Server = %+v, %v", srv, err)
		}
	}
	if srv, err := c.Server(ctx, "gobank-none"); err != nil || srv != nil {
		t.Errorf("a name not in the list is nil, nil: %+v, %v", srv, err)
	}
	if inner.lists != 1 || inner.byName != 0 {
		t.Errorf("lists %d, by name %d; want one list and no lookup by name", inner.lists, inner.byName)
	}
}

// What this program changes it sees at once: creating, deleting or
// relabelling a server forgets the list, so the next read lists again.
func TestCachedCloudForgetsTheListOnAChange(t *testing.T) {
	c, inner, _ := newCachedCloud()
	ctx := context.Background()
	if _, err := c.Server(ctx, "gobank-prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateServer(ctx, CreateSpec{Name: "gobank-perf", Type: "ccx33", Location: "fsn1"}); err != nil {
		t.Fatal(err)
	}
	if srv, _ := c.Server(ctx, "gobank-perf"); srv == nil || inner.lists != 2 {
		t.Errorf("after create: server %+v, lists %d; want it found from a fresh list", srv, inner.lists)
	}
	if err := c.DeleteServer(ctx, "gobank-perf"); err != nil {
		t.Fatal(err)
	}
	if srv, _ := c.Server(ctx, "gobank-perf"); srv != nil || inner.lists != 3 {
		t.Errorf("after delete: server %+v, lists %d; want it gone from a fresh list", srv, inner.lists)
	}
	if err := c.SetLabels(ctx, "gobank-prod", map[string]string{"expires": "20261015T120000Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Servers(ctx); err != nil || inner.lists != 4 {
		t.Errorf("after relabel: lists %d, %v; want a fresh list", inner.lists, err)
	}
}

// A failed list is not kept: the next read asks again.
func TestCachedCloudDoesNotKeepAFailure(t *testing.T) {
	c, inner, _ := newCachedCloud()
	ctx := context.Background()
	inner.listErr = errors.New("rate_limit_exceeded")
	if _, err := c.Servers(ctx); err == nil {
		t.Fatal("want the provider's error")
	}
	inner.listErr = nil
	if srv, err := c.Server(ctx, "gobank-prod"); err != nil || srv == nil || inner.lists != 2 {
		t.Errorf("after the failure: %+v, %v, lists %d; want a fresh list that succeeds", srv, err, inner.lists)
	}
}
