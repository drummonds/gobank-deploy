package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
)

const aboutJSON = `{"version":"v0.8.0","schema":[{"component":"simulation","version":2}],
"settings":{"day_length":"2h0m0s","max_customers":1000000},
"sim":{"running":true,"day_ends_in":"1h2m3s"},
"position":{"day":"2026-03-01","day_count":59,"customers":1200,"savings":"£1,000.00","lending":"£500.00"},
"restarts":[{"version":"v0.8.0","started_at":"2026-10-04T10:00:00Z","day_count":59,"customers":1200,"previous_version":"v0.7.0","previous_stopped_at":"2026-10-04T09:59:18Z","downtime":"42s","intact":true,"stopped_at":null,"stop_day_count":0,"stop_customers":0},
{"version":"v0.7.0","started_at":"2026-10-03T10:00:00Z","day_count":1,"customers":3,"previous_version":"","previous_stopped_at":null,"downtime":"","intact":null,"stopped_at":"2026-10-04T09:59:18Z","stop_day_count":59,"stop_customers":1200}]}`

func TestConsoleReadsTheDemoAtAboutJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/about.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(aboutJSON))
	}))
	defer ts.Close()
	rd, err := (&Console{}).Read(context.Background(), ts.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if rd.Version != "v0.8.0" || !rd.About || !rd.Running || rd.DayLength != 2*time.Hour || rd.DayEndsIn != time.Hour+2*time.Minute+3*time.Second {
		t.Errorf("reading = %+v", rd)
	}
	if rd.Position == nil || rd.Position.Day != "2026-03-01" || rd.Position.DayCount != 59 || rd.Position.Customers != 1200 || rd.Position.Savings != "£1,000.00" || rd.Position.Lending != "£500.00" {
		t.Errorf("position = %+v", rd.Position)
	}
	if rs := rd.Restart; rs == nil || rs.PreviousVersion != "v0.7.0" || rs.DayCount != 59 || rs.Downtime != 42*time.Second || !rs.DowntimeKnown || rs.Intact == nil || !*rs.Intact {
		t.Errorf("restart = %+v", rd.Restart)
	}
}

func TestConsoleReportsAReleaseWithoutAboutJSON(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	if _, err := (&Console{}).Read(context.Background(), ts.URL+"/"); !errors.Is(err, flows.ErrNoAbout) {
		t.Errorf("err = %v, want ErrNoAbout", err)
	}
}

func TestConsoleReadsAnUncleanStopAndNoRestartRow(t *testing.T) {
	body := `{"version":"v2","settings":{"day_length":"0s"},"sim":{"running":false,"day_ends_in":"0s"},"position":{"day":"2026-03-01"},"restarts":[{"version":"v2","previous_version":"v1","downtime":"","intact":null}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer ts.Close()
	rd, err := (&Console{}).Read(context.Background(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Restart == nil || rd.Restart.DowntimeKnown || rd.Restart.Intact != nil || rd.Restart.PreviousVersion != "v1" || rd.DayLength != 0 {
		t.Errorf("reading = %+v restart %+v", rd, rd.Restart)
	}
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"version":"v2","restarts":[]}`)) }))
	defer ts2.Close()
	if rd, _ := (&Console{}).Read(context.Background(), ts2.URL); rd.Restart != nil {
		t.Errorf("no rows is no restart: %+v", rd.Restart)
	}
}

// The day length is set as the settings page sets it: a form post, with
// the customer ceiling re-sent so it is not reset.
func TestConsoleSetsTheDayLengthThroughTheSettingsForm(t *testing.T) {
	var posted url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/about.json":
			w.Write([]byte(aboutJSON))
		case r.URL.Path == "/settings" && r.Method == "POST":
			r.ParseForm()
			posted = r.PostForm
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	if err := (&Console{}).SetDayLength(context.Background(), ts.URL+"/", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if posted.Get("day_length") != "2h0m0s" || posted.Get("max_customers") != "1000000" {
		t.Errorf("posted %v", posted)
	}
}

// The perf run reads the two rates and drives the console as an operator
// would: the settings form for the day length and customer ceiling
// together, the add-customers form, the Run and Stop buttons.
func TestConsoleReadsTheRatesAndDrivesTheSimulation(t *testing.T) {
	const body = `{"version":"v0.12.0","settings":{"day_length":"0s","max_customers":1000000},
"sim":{"running":true,"day_ends_in":"0s","adding_customers":true,"customers_per_sec":81.5,"account_days_per_12h":9000000,"last_day_duration":"1m25s","last_day_accounts":96000},
"position":{"day":"2026-03-01","day_count":59,"customers":48000,"savings":"£1.00","lending":"£0.50"},"restarts":[]}`
	var posts []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			r.ParseForm()
			posts = append(posts, r.URL.Path+"?"+r.PostForm.Encode())
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if r.URL.Path != "/about.json" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer ts.Close()
	c := &Console{}
	rd, err := c.Read(context.Background(), ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Adding || rd.CustomersPerSec != 81.5 || rd.AccountDaysPer12h != 9_000_000 || rd.LastDayDuration != 85*time.Second || rd.LastDayAccounts != 96000 {
		t.Errorf("rates = adding %v %.1f/s, %d account days, last day %s over %d", rd.Adding, rd.CustomersPerSec, rd.AccountDaysPer12h, rd.LastDayDuration, rd.LastDayAccounts)
	}
	for _, step := range []func() error{
		func() error { return c.SetSettings(context.Background(), ts.URL, 0, 1_000_000) },
		func() error { return c.AddCustomers(context.Background(), ts.URL, 1000) },
		func() error { return c.Start(context.Background(), ts.URL) },
		func() error { return c.Stop(context.Background(), ts.URL) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/settings?day_length=0s&max_customers=1000000", "/add-customers?n=1000", "/start?", "/stop?"}
	if len(posts) != len(want) {
		t.Fatalf("posts = %v, want %v", posts, want)
	}
	for i := range want {
		if posts[i] != want[i] {
			t.Errorf("post %d = %q, want %q", i, posts[i], want[i])
		}
	}
}
