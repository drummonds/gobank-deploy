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
// the demo is doing, the settings form for the day length (gobank's
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
		Running   bool   `json:"running"`
		DayEndsIn string `json:"day_ends_in"`
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
	rd := drills.Reading{
		Version: a.Version, About: true, Running: a.Sim.Running, DayLength: dayLength, DayEndsIn: endsIn,
		Position: &drills.Position{Day: a.Position.Day, DayCount: a.Position.DayCount, Customers: a.Position.Customers, Savings: a.Position.Savings, Lending: a.Position.Lending},
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
	form := url.Values{"day_length": {d.String()}, "max_customers": {strconv.Itoa(a.Settings.MaxCustomers)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/settings", strings.NewReader(form.Encode()))
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
		return fmt.Errorf("settings: %s", resp.Status)
	}
	return nil
}
