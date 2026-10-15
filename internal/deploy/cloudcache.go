package deploy

import (
	"context"
	"sync"
	"time"
)

// CachedCloud is the provider as this program last listed it. Hetzner
// allows a token 3,600 requests an hour, and the page asked for the
// server list and then every environment by name on each poll, five
// requests every three seconds while a job ran. Here one list answers
// every read for the interval: Servers is the list, Server reads a name
// off it, and whatever this program changes — a server created, deleted
// or relabelled — forgets the list so the change shows at once. What
// changes outside this program shows within the interval.
type CachedCloud struct {
	Cloud
	interval time.Duration
	now      func() time.Time

	mu     sync.Mutex
	listed time.Time // zero when there is no list
	list   []*Server
}

// NewCachedCloud wraps cloud, listing it at most once per interval.
func NewCachedCloud(cloud Cloud, interval time.Duration) *CachedCloud {
	return &CachedCloud{Cloud: cloud, interval: interval, now: time.Now}
}

// Servers is the list, fetched when there is none or the interval has
// passed. A failed fetch keeps nothing, so the next call asks again.
func (c *CachedCloud) Servers(ctx context.Context) ([]*Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.listed.IsZero() && c.now().Sub(c.listed) < c.interval {
		return c.list, nil
	}
	list, err := c.Cloud.Servers(ctx)
	if err != nil {
		return nil, err
	}
	c.list, c.listed = list, c.now()
	return list, nil
}

// Server is the named server off the list; nil, nil when the list has
// no such name.
func (c *CachedCloud) Server(ctx context.Context, name string) (*Server, error) {
	list, err := c.Servers(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, nil
}

func (c *CachedCloud) CreateServer(ctx context.Context, spec CreateSpec) (*Server, error) {
	defer c.forget()
	return c.Cloud.CreateServer(ctx, spec)
}

func (c *CachedCloud) DeleteServer(ctx context.Context, name string) error {
	defer c.forget()
	return c.Cloud.DeleteServer(ctx, name)
}

func (c *CachedCloud) SetLabels(ctx context.Context, name string, labels map[string]string) error {
	defer c.forget()
	return c.Cloud.SetLabels(ctx, name, labels)
}

func (c *CachedCloud) forget() {
	c.mu.Lock()
	c.listed, c.list = time.Time{}, nil
	c.mu.Unlock()
}
