package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/drills"
	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
)

// Console is the demo's simulation console over HTTP: about.json for what
// the demo is doing, and the console's own forms for what an operator
// would press — the settings, the batch add, Run and Stop (gobank's
// default session is the admin's, so no login is needed).
type Console struct {
	Timeout time.Duration // per request; defaults to 10s
}

// about is gobank's /about.json as this program reads it.
type about struct {
	Version  string `json:"version"`
	Settings struct {
		DayLength    string `json:"day_length"`
		MaxCustomers int    `json:"max_customers"`
	} `json:"settings"`
	Sim struct {
		Running           bool    `json:"running"`
		DayEndsIn         string  `json:"day_ends_in"`
		AddingCustomers   bool    `json:"adding_customers"`
		CustomersPerSec   float64 `json:"customers_per_sec"`
		AccountDaysPer12h int64   `json:"account_days_per_12h"`
		LastDayDuration   string  `json:"last_day_duration"`
		LastDayAccounts   int     `json:"last_day_accounts"`
	} `json:"sim"`
	Position struct {
		Day       string `json:"day"`
		DayCount  int    `json:"day_count"`
		Customers int    `json:"customers"`
		Savings   string `json:"savings"`
		Lending   string `json:"lending"`
	} `json:"position"`
	Restarts []struct {
		PreviousVersion string `json:"previous_version"`
		DayCount        int    `json:"day_count"`
		Downtime        string `json:"downtime"`
		Intact          *bool  `json:"intact"`
	} `json:"restarts"`
}

func (c *Console) client() *http.Client {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

func (c *Console) fetch(ctx context.Context, base string) (about, error) {
	var a about
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/about.json", nil)
	if err != nil {
		return a, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return a, flows.ErrNoAbout
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return a, fmt.Errorf("about.json: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return a, fmt.Errorf("about.json: %w", err)
	}
	return a, nil
}

// Read is the demo at base now; flows.ErrNoAbout when the release has no
// about.json.
func (c *Console) Read(ctx context.Context, base string) (drills.Reading, error) {
	a, err := c.fetch(ctx, base)
	if err != nil {
		return drills.Reading{}, err
	}
	dayLength, _ := time.ParseDuration(a.Settings.DayLength)
	endsIn, _ := time.ParseDuration(a.Sim.DayEndsIn)
	lastDay, _ := time.ParseDuration(a.Sim.LastDayDuration)
	rd := drills.Reading{
		Version: a.Version, About: true, Running: a.Sim.Running, DayLength: dayLength, DayEndsIn: endsIn,
		Position: &drills.Position{Day: a.Position.Day, DayCount: a.Position.DayCount, Customers: a.Position.Customers, Savings: a.Position.Savings, Lending: a.Position.Lending},
		Adding:   a.Sim.AddingCustomers, CustomersPerSec: a.Sim.CustomersPerSec, AccountDaysPer12h: a.Sim.AccountDaysPer12h,
		LastDayDuration: lastDay, LastDayAccounts: a.Sim.LastDayAccounts,
	}
	if len(a.Restarts) > 0 {
		r := a.Restarts[0]
		rs := &drills.Restart{PreviousVersion: r.PreviousVersion, DayCount: r.DayCount, Intact: r.Intact}
		if r.Downtime != "" {
			if d, err := time.ParseDuration(r.Downtime); err == nil {
				rs.Downtime, rs.DowntimeKnown = d, true
			}
		}
		rd.Restart = rs
	}
	return rd, nil
}

// SetDayLength posts the settings form with the new day length and the
// current customer ceiling, as the settings page does.
func (c *Console) SetDayLength(ctx context.Context, base string, d time.Duration) error {
	a, err := c.fetch(ctx, base)
	if err != nil {
		return err
	}
	return c.SetSettings(ctx, base, d, a.Settings.MaxCustomers)
}

// SetSettings posts the settings form: the day length (zero is flat out)
// and the customer ceiling together, as the page does.
func (c *Console) SetSettings(ctx context.Context, base string, dayLength time.Duration, maxCustomers int) error {
	return c.post(ctx, base, "/settings", url.Values{"day_length": {dayLength.String()}, "max_customers": {strconv.Itoa(maxCustomers)}})
}

// AddCustomers starts a batch add of n customers, as the dashboard's form
// does; the demo reports it running at about.json until it is done.
func (c *Console) AddCustomers(ctx context.Context, base string, n int) error {
	return c.post(ctx, base, "/add-customers", url.Values{"n": {strconv.Itoa(n)}})
}

// Start presses Run: the day loop goes.
func (c *Console) Start(ctx context.Context, base string) error {
	return c.post(ctx, base, "/start", url.Values{})
}

// Stop presses Stop: the day loop ends, the day in progress left for the
// next Run to resume.
func (c *Console) Stop(ctx context.Context, base string) error {
	return c.post(ctx, base, "/stop", url.Values{})
}

// post submits one of the console's forms and accepts the redirect it
// answers with.
func (c *Console) post(ctx context.Context, base, path string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", strings.TrimPrefix(path, "/"), resp.Status)
	}
	return nil
}
