package load

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBFF is gobank's customer BFF as the journey sees it: a login that
// takes the customer's ID and the app password and answers a token, and
// screens that want the token.
type fakeBFF struct {
	mu     sync.Mutex
	seen   []string // "POST /v1/login cust-001", "GET /v1/screen/accounts cust-001"
	tokens map[string]string
}

func (f *fakeBFF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == "POST" && r.URL.Path == "/v1/login" {
		var req struct{ CustomerID, Password string }
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		req.CustomerID, req.Password = body["customer_id"], body["password"]
		if req.Password != "s3cret" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		token := "tok-" + req.CustomerID
		f.tokens[token] = req.CustomerID
		f.seen = append(f.seen, "POST /v1/login "+req.CustomerID)
		fmt.Fprintf(w, `{"token":%q}`, token)
		return
	}
	customer, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+customer)
	w.Write([]byte(`{"screen":"ok"}`))
}

// The customer journey is login as the customer with the app password,
// then the accounts screen (the balances) and one account's screen (its
// details and transactions), signed with the token the login answered;
// each screen is timed.
func TestWalkerWalksTheCustomerJourney(t *testing.T) {
	bff := &fakeBFF{tokens: map[string]string{}}
	ts := httptest.NewServer(bff)
	defer ts.Close()
	w := &Walker{Base: ts.URL + "/", Password: "s3cret"}
	took, err := w.Walk(context.Background(), Customer, "cust-042")
	if err != nil {
		t.Fatal(err)
	}
	if len(took) != 3 || slices.Contains(took, 0) {
		t.Errorf("took = %v; want a duration for each of the three screens", took)
	}
	want := []string{"POST /v1/login cust-042", "GET /v1/screen/accounts cust-042", "GET /v1/screen/product/0 cust-042"}
	if !slices.Equal(bff.seen, want) {
		t.Errorf("seen = %v, want %v", bff.seen, want)
	}
	if names := Customer.Screens(); !slices.Equal(names, []string{"login", "accounts", "account"}) {
		t.Errorf("screens = %v", names)
	}
}

// A screen that is refused ends the walk with an error naming it.
func TestWalkerFailsAtARefusedScreen(t *testing.T) {
	ts := httptest.NewServer(&fakeBFF{tokens: map[string]string{}})
	defer ts.Close()
	_, err := (&Walker{Base: ts.URL, Password: "wrong"}).Walk(context.Background(), Customer, "cust-001")
	if err == nil || !strings.Contains(err.Error(), "login") || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v; want the login refused with 401", err)
	}
}

// The customers are taken in sequence through the books, starting again
// at the first after the last, so no two journeys close together are the
// same customer's and nothing is answered from a cache.
func TestCustomersAreTakenInSequenceThroughTheBooks(t *testing.T) {
	next := Sequence(3)
	var got []string
	for range 5 {
		got = append(got, next())
	}
	if want := []string{"cust-001", "cust-002", "cust-003", "cust-001", "cust-002"}; !slices.Equal(got, want) {
		t.Errorf("customers = %v, want %v", got, want)
	}
	if id := Sequence(5000)(); id != "cust-001" {
		t.Errorf("first = %s", id)
	}
	big := Sequence(1500)
	for range 1233 {
		big()
	}
	if id := big(); id != "cust-1234" {
		t.Errorf("1234th = %s; gobank's IDs are cust-%%03d", id)
	}
}

// At a rung, each of the users walks the journey reps times, one walk
// after another, every walk the next customer; the rung's figures are
// each screen's percentiles over every walk, the journey's end to end,
// and the walks completed a second.
func TestRungWalksEachUserRepsTimes(t *testing.T) {
	var mu sync.Mutex
	var customers []string
	walk := func(_ context.Context, customer string) ([]time.Duration, error) {
		mu.Lock()
		defer mu.Unlock()
		customers = append(customers, customer)
		n := len(customers) // 1..30
		return []time.Duration{time.Duration(n) * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}, nil
	}
	r := RunRung(context.Background(), Customer, 3, 10, walk, Sequence(1000))
	if r.Users != 3 || r.Journeys != 30 || r.Failed != 0 {
		t.Errorf("rung = %+v; want 3 users, 30 journeys, none failed", r)
	}
	slices.Sort(customers)
	if len(customers) != 30 || customers[0] != "cust-001" || customers[29] != "cust-030" || len(slices.Compact(customers)) != 30 {
		t.Errorf("customers = %v; want cust-001 to cust-030, each once", customers)
	}
	if len(r.Screens) != 3 || r.Screens[0].Name != "login" || r.Screens[0].N != 30 || r.Screens[0].P95 != 29*time.Millisecond || r.Screens[0].Max != 30*time.Millisecond || r.Screens[2].P50 != 3*time.Millisecond {
		t.Errorf("screens = %+v; want login p95 29ms (nearest rank of 1..30ms), max 30ms", r.Screens)
	}
	if r.Journey.P95 != 34*time.Millisecond {
		t.Errorf("journey p95 = %s; want 34ms (29+2+3)", r.Journey.P95)
	}
	if r.Elapsed <= 0 || r.PerSecond <= 0 {
		t.Errorf("elapsed %s per second %f", r.Elapsed, r.PerSecond)
	}
}

// A walk that fails is counted and its error kept; the user carries on
// with its next walk.
func TestRungCountsFailedWalks(t *testing.T) {
	var mu sync.Mutex
	n := 0
	walk := func(context.Context, string) ([]time.Duration, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n%5 == 0 {
			return nil, errors.New("accounts: 502 Bad Gateway")
		}
		return []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}, nil
	}
	r := RunRung(context.Background(), Customer, 2, 10, walk, Sequence(100))
	if r.Journeys != 16 || r.Failed != 4 || r.FirstError != "accounts: 502 Bad Gateway" {
		t.Errorf("rung = %+v; want 16 completed, 4 failed with the error kept", r)
	}
}

// A rung holds when every screen's p95 is within the budget and no walk
// failed; otherwise it says why it breached.
func TestRungBreach(t *testing.T) {
	held := Rung{Users: 5, Screens: []Screen{{Name: "login", P95: 2 * time.Second}, {Name: "accounts", P95: time.Second}}}
	if why := held.Breach(2 * time.Second); why != "" {
		t.Errorf("a p95 at the budget holds: %q", why)
	}
	slow := Rung{Users: 5, Screens: []Screen{{Name: "login", P95: 2100 * time.Millisecond}, {Name: "accounts", P95: 2500 * time.Millisecond}}}
	if why := slow.Breach(2 * time.Second); why != "login p95 2.1s, accounts p95 2.5s over 2s" {
		t.Errorf("why = %q", why)
	}
	failed := Rung{Users: 5, Failed: 1, FirstError: "login: 429", Screens: []Screen{{Name: "login", P95: time.Second}}}
	if why := failed.Breach(2 * time.Second); why != "1 journeys failed (first: login: 429)" {
		t.Errorf("why = %q", why)
	}
}

// The ladder climbs the rungs in order and stops at the first that
// breaches: the capacity is the users of the last rung that held, and
// the breach says why the climb stopped. No rung above the breach is
// measured.
func TestClimbStopsAtTheFirstBreach(t *testing.T) {
	var measured []int
	measure := func(_ context.Context, users int) Rung {
		measured = append(measured, users)
		return Rung{Users: users, Screens: []Screen{{Name: "login", P95: time.Duration(users) * 100 * time.Millisecond}}}
	}
	// 20 users at 100ms each is 2s, at the budget: it holds; 50 does not.
	res := Climb(context.Background(), []int{1, 2, 5, 10, 20, 50}, 2*time.Second, measure)
	if res.Capacity != 20 || res.Breach != "50 users: login p95 5s over 2s" || !slices.Equal(measured, []int{1, 2, 5, 10, 20, 50}) {
		t.Errorf("result = %+v measured %v; want capacity 20, stopped at 50", res, measured)
	}
	measured = nil
	res = Climb(context.Background(), []int{1, 2, 5, 10, 20, 50}, 1500*time.Millisecond, measure)
	if res.Capacity != 10 || res.Breach != "20 users: login p95 2s over 1.5s" || len(res.Rungs) != 5 || !slices.Equal(measured, []int{1, 2, 5, 10, 20}) {
		t.Errorf("result = %+v measured %v; want capacity 10, stopped at 20, nothing above it measured", res, measured)
	}
	res = Climb(context.Background(), []int{1, 2}, time.Hour, measure)
	if res.Capacity != 2 || res.Breach != "" {
		t.Errorf("a ladder that holds to the top: %+v", res)
	}
	res = Climb(context.Background(), []int{30}, time.Second, measure)
	if res.Capacity != 0 || res.Breach == "" {
		t.Errorf("a ladder that breaches at once: %+v", res)
	}
}

// A load test is the ladder climbed with the customer journey walked over
// HTTP, the customers in sequence through the books; its result crosses
// from the loader box as JSON and reads back the same.
func TestTestClimbsTheLadderOverHTTPAndTheResultCrossesAsJSON(t *testing.T) {
	bff := &fakeBFF{tokens: map[string]string{}}
	ts := httptest.NewServer(bff)
	defer ts.Close()
	res := Test(context.Background(), Config{Base: ts.URL, Password: "s3cret", Customers: 100, Ladder: []int{1, 2}, Reps: 2, Budget: time.Minute})
	if res.Capacity != 2 || res.Breach != "" || len(res.Rungs) != 2 || res.Rungs[1].Journeys != 4 {
		t.Fatalf("result = %+v", res)
	}
	logins := 0
	for _, s := range bff.seen {
		if strings.HasPrefix(s, "POST /v1/login") {
			logins++
		}
	}
	if logins != 6 || !slices.Contains(bff.seen, "POST /v1/login cust-006") {
		t.Errorf("seen = %v; want six logins, cust-001 to cust-006", bff.seen)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(back) != fmt.Sprint(res) {
		t.Errorf("round trip:\n%+v\n%+v", back, res)
	}
}
