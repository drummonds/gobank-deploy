// Package probe measures a route from outside, as a browser would: n
// calls at a concurrency, the median and the 95th percentile of how long
// each took.
package probe

import (
	"context"
	"math"
	"slices"
	"sync"
	"time"
)

// Sample is what one measurement found: the calls made, the percentiles
// of their durations, the wall clock the measurement took and so the
// throughput — calls completed a second at that concurrency.
type Sample struct {
	N         int
	P50, P95  time.Duration
	Elapsed   time.Duration
	PerSecond float64
}

// Measure makes n calls, at most concurrency of them at once, and reports
// the percentiles (nearest rank) of the durations the calls report. The
// first error ends the measurement with that error.
func Measure(ctx context.Context, n, concurrency int, call func(ctx context.Context) (time.Duration, error)) (Sample, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	var (
		mu    sync.Mutex
		took  []time.Duration
		first error
		wg    sync.WaitGroup
		slots = make(chan struct{}, concurrency)
	)
	for range n {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			d, err := call(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			took = append(took, d)
		})
	}
	wg.Wait()
	if first != nil {
		return Sample{}, first
	}
	slices.Sort(took)
	s := Sample{N: len(took), P50: percentile(took, 0.50), P95: percentile(took, 0.95), Elapsed: time.Since(started)}
	if s.Elapsed > 0 {
		s.PerSecond = float64(s.N) / s.Elapsed.Seconds()
	}
	return s, nil
}

// percentile is the nearest-rank percentile of sorted durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(rank, len(sorted)-1))]
}
