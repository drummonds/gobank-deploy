package probe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Measure makes n calls and reports how many it made with the median and
// the 95th percentile (nearest rank) of the durations the calls report.
func TestMeasureReportsPercentiles(t *testing.T) {
	var mu sync.Mutex
	i := 0
	call := func(ctx context.Context) (time.Duration, error) {
		mu.Lock()
		defer mu.Unlock()
		i++
		return time.Duration(i) * time.Millisecond, nil
	}
	s, err := Measure(context.Background(), 100, 1, call)
	if err != nil {
		t.Fatal(err)
	}
	if s.N != 100 || s.P50 != 50*time.Millisecond || s.P95 != 95*time.Millisecond {
		t.Errorf("sample = %+v; want 100 calls, p50 50ms, p95 95ms", s)
	}
}

// The calls run at the concurrency asked for: never more in flight, and
// with enough calls, that many.
func TestMeasureRunsCallsAtTheConcurrency(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	call := func(ctx context.Context) (time.Duration, error) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return 5 * time.Millisecond, nil
	}
	s, err := Measure(context.Background(), 40, 4, call)
	if err != nil {
		t.Fatal(err)
	}
	if s.N != 40 || peak != 4 {
		t.Errorf("made %d calls with %d in flight at the peak; want 40 at 4", s.N, peak)
	}
	// Forty 5ms calls four at a time take about 50ms of wall clock: the
	// throughput is what was completed over that, not over the calls' own
	// time.
	if s.Elapsed < 50*time.Millisecond || s.Elapsed > 500*time.Millisecond || s.PerSecond < 80 || s.PerSecond > 800 {
		t.Errorf("elapsed %s, %.0f/s; want about 50ms and 800/s", s.Elapsed, s.PerSecond)
	}
}

// A failing call fails the measurement: a route that errors has no
// percentile worth recording.
func TestMeasureFailsOnACallError(t *testing.T) {
	boom := errors.New("502")
	call := func(ctx context.Context) (time.Duration, error) { return 0, boom }
	if _, err := Measure(context.Background(), 5, 2, call); !errors.Is(err, boom) {
		t.Errorf("err = %v; want the call's", err)
	}
}
