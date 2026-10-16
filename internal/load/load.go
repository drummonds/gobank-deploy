// Package load is gobank's load test: the customer journey walked by
// many customers at once, at each rung of a ladder of concurrent users,
// until a screen's p95 is over the budget. The journey is data, so the
// gates can walk it too; the walking is HTTP against the customer BFF.
package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/probe"
)

// Step is one screen of a journey: the request that fetches it.
type Step struct {
	Screen string // what the customer sees: login, accounts, account
	Method string
	Path   string
	// Login posts the customer's ID and the app password; the token it
	// answers signs every later step.
	Login bool
}

// Journey is the screens a customer goes through, in order.
type Journey []Step

// Customer is the customer app's journey: sign in, see the balances, open
// one account for its details and transactions.
var Customer = Journey{
	{Screen: "login", Method: http.MethodPost, Path: "/v1/login", Login: true},
	{Screen: "accounts", Method: http.MethodGet, Path: "/v1/screen/accounts"},
	{Screen: "account", Method: http.MethodGet, Path: "/v1/screen/product/0"},
}

// Screens are the journey's screen names in order.
func (j Journey) Screens() []string {
	var out []string
	for _, s := range j {
		out = append(out, s.Screen)
	}
	return out
}

// Sequence hands out the books' customers in order, cust-001 to the n-th
// and round again: gobank's customer IDs (core.CustomerID). Safe for
// many users at once.
func Sequence(n int) func() string {
	var next atomic.Int64
	return func() string {
		i := (next.Add(1) - 1) % int64(max(n, 1))
		return fmt.Sprintf("cust-%03d", i+1)
	}
}

// Walker walks journeys against a BFF over HTTP.
type Walker struct {
	Base     string // the BFF, e.g. http://10.128.1.2:1347/
	Password string // the environment's app password, every customer's
	Client   *http.Client
}

func (w *Walker) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Walk takes the customer through the journey and returns how long each
// screen took, body and all; the first screen refused or unanswered ends
// the walk with an error naming it.
func (w *Walker) Walk(ctx context.Context, j Journey, customer string) ([]time.Duration, error) {
	var took []time.Duration
	token := ""
	for _, s := range j {
		var body io.Reader
		if s.Login {
			b, _ := json.Marshal(map[string]string{"customer_id": customer, "password": w.Password})
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, s.Method, strings.TrimSuffix(w.Base, "/")+s.Path, body)
		if err != nil {
			return took, fmt.Errorf("%s: %w", s.Screen, err)
		}
		if s.Login {
			req.Header.Set("Content-Type", "application/json")
		} else if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		started := time.Now()
		resp, err := w.client().Do(req)
		if err != nil {
			return took, fmt.Errorf("%s: %w", s.Screen, err)
		}
		answer, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		d := time.Since(started)
		if err != nil {
			return took, fmt.Errorf("%s: %w", s.Screen, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return took, fmt.Errorf("%s: %s", s.Screen, resp.Status)
		}
		if s.Login {
			var a struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(answer, &a); err != nil || a.Token == "" {
				return took, fmt.Errorf("%s: no token in the answer", s.Screen)
			}
			token = a.Token
		}
		took = append(took, d)
	}
	return took, nil
}

// Screen is one screen's figures over a rung's walks.
type Screen struct {
	Name string        `json:"name"`
	N    int           `json:"n"`
	P50  time.Duration `json:"p50_ns"`
	P95  time.Duration `json:"p95_ns"`
	Max  time.Duration `json:"max_ns"`
}

// Rung is the journey walked by so many users at once, each reps times.
type Rung struct {
	Users      int           `json:"users"`
	Journeys   int           `json:"journeys"` // walks completed
	Failed     int           `json:"failed"`   // walks that ended in an error
	FirstError string        `json:"first_error,omitempty"`
	Screens    []Screen      `json:"screens"`
	Journey    Screen        `json:"journey"` // end to end, the screens' sum
	Elapsed    time.Duration `json:"elapsed_ns"`
	PerSecond  float64       `json:"per_second"` // walks completed a second
}

// WalkFunc walks the journey once for a customer: each screen's time.
type WalkFunc func(ctx context.Context, customer string) ([]time.Duration, error)

// RunRung has users walk the journey at once, each reps times one walk
// after another, every walk the next customer.
func RunRung(ctx context.Context, j Journey, users, reps int, walk WalkFunc, next func() string) Rung {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		walks [][]time.Duration
		r     = Rung{Users: users}
	)
	started := time.Now()
	for range users {
		wg.Go(func() {
			for range reps {
				if ctx.Err() != nil {
					return
				}
				took, err := walk(ctx, next())
				mu.Lock()
				if err != nil {
					r.Failed++
					if r.FirstError == "" {
						r.FirstError = err.Error()
					}
				} else {
					walks = append(walks, took)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	r.Elapsed = time.Since(started)
	r.Journeys = len(walks)
	if r.Elapsed > 0 {
		r.PerSecond = float64(r.Journeys) / r.Elapsed.Seconds()
	}
	for i, name := range j.Screens() {
		var ds []time.Duration
		for _, w := range walks {
			if i < len(w) {
				ds = append(ds, w[i])
			}
		}
		r.Screens = append(r.Screens, figures(name, ds))
	}
	var total []time.Duration
	for _, w := range walks {
		var sum time.Duration
		for _, d := range w {
			sum += d
		}
		total = append(total, sum)
	}
	r.Journey = figures("journey", total)
	return r
}

func figures(name string, ds []time.Duration) Screen {
	slices.Sort(ds)
	s := Screen{Name: name, N: len(ds), P50: probe.Percentile(ds, 0.50), P95: probe.Percentile(ds, 0.95)}
	if len(ds) > 0 {
		s.Max = ds[len(ds)-1]
	}
	return s
}

// Breach is why the rung is unacceptable — a failed walk, or a screen
// whose p95 is over the budget — or "" when it held.
func (r Rung) Breach(budget time.Duration) string {
	if r.Failed > 0 {
		return fmt.Sprintf("%d journeys failed (first: %s)", r.Failed, r.FirstError)
	}
	var over []string
	for _, s := range r.Screens {
		if s.P95 > budget {
			over = append(over, fmt.Sprintf("%s p95 %s", s.Name, s.P95.Round(time.Millisecond)))
		}
	}
	if len(over) == 0 {
		return ""
	}
	return strings.Join(over, ", ") + " over " + budget.String()
}

// Result is a climb of the ladder.
type Result struct {
	Budget   time.Duration `json:"budget_ns"`
	Rungs    []Rung        `json:"rungs"`
	Capacity int           `json:"capacity"` // users at the last rung that held; 0 when the first breached
	Breach   string        `json:"breach,omitempty"`
}

// Climb measures the rungs in order and stops at the first that breaches
// the budget; nothing above it is measured.
func Climb(ctx context.Context, rungs []int, budget time.Duration, measure func(ctx context.Context, users int) Rung) Result {
	res := Result{Budget: budget}
	for _, users := range rungs {
		if ctx.Err() != nil {
			res.Breach = ctx.Err().Error()
			return res
		}
		r := measure(ctx, users)
		res.Rungs = append(res.Rungs, r)
		if why := r.Breach(budget); why != "" {
			res.Breach = fmt.Sprintf("%d users: %s", users, why)
			return res
		}
		res.Capacity = users
	}
	return res
}

// Config is a load test: where, as whom, and the ladder.
type Config struct {
	Base      string        // the BFF
	Password  string        // the environment's app password
	Customers int           // on the books, walked in sequence
	Ladder    []int         // concurrent users at each rung
	Reps      int           // walks per user per rung
	Budget    time.Duration // the p95 a screen may reach and the rung still hold
	Progress  io.Writer     // a line per rung; nil for none
}

// Ladder is the default ladder of concurrent users.
var Ladder = []int{1, 2, 5, 10, 20, 50, 100, 200, 500}

// Test climbs the ladder with the customer journey: a load test.
func Test(ctx context.Context, c Config) Result {
	w := &Walker{Base: c.Base, Password: c.Password}
	next := Sequence(c.Customers)
	ladder := c.Ladder
	if len(ladder) == 0 {
		ladder = Ladder
	}
	return Climb(ctx, ladder, c.Budget, func(ctx context.Context, users int) Rung {
		r := RunRung(ctx, Customer, users, c.Reps, func(ctx context.Context, customer string) ([]time.Duration, error) {
			return w.Walk(ctx, Customer, customer)
		}, next)
		if c.Progress != nil {
			fmt.Fprintf(c.Progress, "%d users: %d journeys, %d failed, %.1f/s;", users, r.Journeys, r.Failed, r.PerSecond)
			for _, s := range r.Screens {
				fmt.Fprintf(c.Progress, " %s p95 %s", s.Name, s.P95.Round(time.Millisecond))
			}
			fmt.Fprintln(c.Progress)
		}
		return r
	})
}
