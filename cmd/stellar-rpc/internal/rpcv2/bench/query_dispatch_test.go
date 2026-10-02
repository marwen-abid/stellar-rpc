package bench

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRequest is a queryRequest that counts calls, warmup included, and
// sleeps countingRequestService so service is never zero.
type countingRequest struct {
	calls atomic.Int64
}

const countingRequestService = 50 * time.Microsecond

func (c *countingRequest) run(*rand.Rand) (cellSample, error) {
	c.calls.Add(1)
	return timed(stageNone, func() (int, error) {
		time.Sleep(countingRequestService)
		return 1, nil
	})
}

// TestRunPacedLegMeasuredCount: a clean leg measures round(rps × duration)
// requests, runs warmup requests without sampling them, and sheds nothing.
func TestRunPacedLegMeasuredCount(t *testing.T) {
	const rps = 200.0
	fake := &countingRequest{}
	res, err := runPacedLeg(t.Context(), spinClock{}, rps, 100*time.Millisecond, 5, 42, fake.run)
	require.NoError(t, err)

	assert.Equal(t, 20, res.measured.dispatched)
	assert.Equal(t, 20, res.positions)
	assert.Len(t, res.samples, 20)
	assert.Len(t, res.lags, 20)
	assert.Equal(t, 0, res.measured.shed)
	assert.Equal(t, 0, res.measured.errs)
	assert.Equal(t, int64(25), fake.calls.Load(), "warmup requests run at the leg's rate")
	assert.Positive(t, res.wall)
	assert.Equal(t, arrivalWindow(rps, res), res.arrival)

	for i, s := range res.samples {
		assert.Positive(t, s.service, "sample %d", i)
		assert.GreaterOrEqual(t, s.scheduled, s.service, "sample %d", i)
		assert.Equal(t, 1, s.items, "sample %d", i)
	}
	for i, lag := range res.lags {
		assert.GreaterOrEqual(t, lag, time.Duration(0), "lag %d", i)
	}
}

// drawRecorder is a queryRequest that records each request's first RNG draw.
type drawRecorder struct {
	mu    sync.Mutex
	draws []uint64
}

func (d *drawRecorder) run(rng *rand.Rand) (cellSample, error) {
	v := rng.Uint64()
	d.mu.Lock()
	d.draws = append(d.draws, v)
	d.mu.Unlock()
	return timed(stageNone, func() (int, error) { return 1, nil })
}

// TestRunPacedLegRNGIndependence: requests of one leg, and legs at different
// rates, draw distinct values.
func TestRunPacedLegRNGIndependence(t *testing.T) {
	first := &drawRecorder{}
	_, err := runPacedLeg(t.Context(), spinClock{}, 500, 40*time.Millisecond, 0, 7, first.run)
	require.NoError(t, err)
	require.Len(t, first.draws, 20)

	seen := make(map[uint64]bool, len(first.draws))
	for _, v := range first.draws {
		assert.False(t, seen[v], "two requests of one leg drew %d", v)
		seen[v] = true
	}

	second := &drawRecorder{}
	_, err = runPacedLeg(t.Context(), spinClock{}, 250, 80*time.Millisecond, 0, 7, second.run)
	require.NoError(t, err)
	require.Len(t, second.draws, 20)
	for _, v := range second.draws {
		assert.False(t, seen[v], "a leg at another rate redrew %d", v)
	}
}

// blockingRequest is a queryRequest that blocks until release is closed.
type blockingRequest struct {
	release  chan struct{}
	inFlight atomic.Int64
	calls    atomic.Int64
}

func (b *blockingRequest) run(*rand.Rand) (cellSample, error) {
	b.calls.Add(1)
	b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	return timed(stageNone, func() (int, error) {
		<-b.release
		return 1, nil
	})
}

// TestRunPacedLegSheds: with every slot held, the first maxInFlight measured
// requests run and the rest are shed; lags cover every measured position.
func TestRunPacedLegSheds(t *testing.T) {
	fake := &blockingRequest{release: make(chan struct{})}
	// The counts hold while every slot is held at the last of the 1000
	// positions; the 2s release is 200x the 10ms schedule.
	timer := time.AfterFunc(2*time.Second, func() { close(fake.release) })
	defer timer.Stop()

	res, err := runPacedLeg(t.Context(), spinClock{}, 100000, 10*time.Millisecond, 0, 3, fake.run)
	require.NoError(t, err)

	assert.Equal(t, maxInFlight, res.measured.dispatched)
	assert.Equal(t, 1000-maxInFlight, res.measured.shed)
	assert.Equal(t, 1000, res.measured.dispatched+res.measured.shed)
	assert.Equal(t, res.positions, res.measured.dispatched+res.measured.shed)
	assert.Equal(t, res.measured.dispatched, len(res.samples)+res.measured.errs)
	assert.Len(t, res.samples, maxInFlight)
	assert.Len(t, res.lags, 1000, "every measured position is charged a dispatch lag")
	assert.Equal(t, int64(maxInFlight), fake.calls.Load())
	assert.Equal(t, int64(0), fake.inFlight.Load())
}

// TestRunPacedLegCountsErrors: a failed request is counted, leaves no sample
// and does not end the leg.
func TestRunPacedLegCountsErrors(t *testing.T) {
	const rps = 200.0
	var ordinal atomic.Int64
	fail := errors.New("request failed")
	req := func(*rand.Rand) (cellSample, error) {
		if ordinal.Add(1)%2 == 0 {
			return cellSample{}, fail
		}
		return timed(stageNone, func() (int, error) { return 1, nil })
	}

	res, err := runPacedLeg(t.Context(), spinClock{}, rps, 100*time.Millisecond, 0, 11, req)
	require.NoError(t, err)

	assert.Equal(t, 20, res.measured.dispatched)
	assert.Equal(t, res.positions, res.measured.dispatched+res.measured.shed)
	assert.Equal(t, res.measured.dispatched, len(res.samples)+res.measured.errs)
	assert.Equal(t, 10, res.measured.errs)
	assert.Len(t, res.samples, 10)
	assert.Len(t, res.lags, 20)
	assert.Equal(t, 0, res.measured.shed)
	assert.Equal(t, arrivalWindow(rps, res), res.arrival)
}

// arrivalWindow is the expected legResult.arrival of a leg at rps.
func arrivalWindow(rps float64, res legResult) time.Duration {
	return time.Duration(res.measured.dispatched+res.measured.shed) * legInterval(rps)
}

// TestRunPacedLegRoundsTheInterval: the schedule and the arrival window use the
// rounded interval.
func TestRunPacedLegRoundsTheInterval(t *testing.T) {
	assert.Equal(t, 142857143*time.Nanosecond, legInterval(7))

	req := func(*rand.Rand) (cellSample, error) { return cellSample{items: 1}, nil }
	res, err := runPacedLeg(t.Context(), spinClock{}, 7, 150*time.Millisecond, 0, 1, req)
	require.NoError(t, err)
	assert.Equal(t, 1, res.positions)
	assert.Equal(t, 142857143*time.Nanosecond, res.arrival)
}

// TestRunPacedLegContextCancel: a canceled context ends the leg with the
// context's error and no request running. The result covers the positions
// dispatched before the cancel.
func TestRunPacedLegContextCancel(t *testing.T) {
	fake := &countingRequest{}
	inFlight := &atomic.Int64{}
	req := func(rng *rand.Rand) (cellSample, error) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		return fake.run(rng)
	}

	ctx, cancel := context.WithCancel(t.Context())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	res, err := runPacedLeg(ctx, spinClock{}, 2, 10*time.Second, 0, 5, req)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, time.Second, "the leg ends at the cancel, not at its full duration")
	assert.Equal(t, int64(0), inFlight.Load())

	// Position 0 runs at once; position 1 is due at 500ms, after the cancel.
	assert.Equal(t, 1, res.positions)
	assert.Equal(t, 1, res.measured.dispatched)
	assert.Len(t, res.samples, 1)
	assert.Len(t, res.lags, 1)
	assert.Equal(t, legInterval(2), res.arrival)
}

// TestRunPacedLegLagStaysSmall: requests longer than the arrival interval hold
// up no later dispatch. It asserts on the median lag and the wall, not the
// worst lag: one dispatch can lose the CPU for tens of milliseconds on a busy
// machine.
func TestRunPacedLegLagStaysSmall(t *testing.T) {
	const service = 20 * time.Millisecond
	req := func(*rand.Rand) (cellSample, error) {
		return timed(stageNone, func() (int, error) {
			time.Sleep(service)
			return 1, nil
		})
	}

	res, err := runPacedLeg(t.Context(), spinClock{}, 1000, 50*time.Millisecond, 0, 13, req)
	require.NoError(t, err)

	assert.Equal(t, 50, res.measured.dispatched)
	assert.Len(t, res.lags, res.measured.dispatched)
	for i, lag := range res.lags {
		assert.GreaterOrEqual(t, lag, time.Duration(0), "lag %d", i)
	}

	sorted := slices.Clone(res.lags)
	slices.Sort(sorted)
	assert.Less(t, sorted[len(sorted)/2], service, "median dispatch lag")
	// 50 serialized 20ms requests would take 1s.
	assert.Less(t, res.wall, 500*time.Millisecond, "leg wall")
}

// TestLaunchPacedRequestChargesLateDispatch: a late dispatch shows in
// scheduled and in the lag, not in service.
func TestLaunchPacedRequestChargesLateDispatch(t *testing.T) {
	const late = 50 * time.Millisecond
	req := func(*rand.Rand) (cellSample, error) {
		return timed(stageNone, func() (int, error) { return 1, nil })
	}

	leg := newPacedLeg(spinClock{}, req, 1, 1, 1)
	due := time.Now().Add(-late)
	leg.launch(0, due, time.Now(), true)
	leg.wg.Wait()

	res := leg.result(due, legInterval(1))
	require.Len(t, res.samples, 1)
	require.Len(t, res.lags, 1)
	assert.GreaterOrEqual(t, res.samples[0].scheduled, late, "the client waited from the due time")
	assert.Less(t, res.samples[0].service, res.samples[0].scheduled-40*time.Millisecond,
		"the request itself took almost none of that wait")
	assert.GreaterOrEqual(t, res.lags[0], late, "the dispatch lag is charged too")
}

// TestRunPacedLegRejectsBadArguments: a bad rate, duration or warmup count is
// an error.
func TestRunPacedLegRejectsBadArguments(t *testing.T) {
	req := func(*rand.Rand) (cellSample, error) {
		return timed(stageNone, func() (int, error) { return 1, nil })
	}
	_, err := runPacedLeg(t.Context(), spinClock{}, 0, time.Second, 0, 1, req)
	assert.Error(t, err)
	_, err = runPacedLeg(t.Context(), spinClock{}, 10, 0, 0, 1, req)
	assert.Error(t, err)
	// 1e-12 rps is below minLegRPS: the report's milli-rps rows would store zero.
	_, err = runPacedLeg(t.Context(), spinClock{}, 1e-12, time.Second, 0, 1, req)
	assert.ErrorContains(t, err, "too low")
	// 1e6 rps over a century is more positions than the leg ceiling allows.
	_, err = runPacedLeg(t.Context(), spinClock{}, 1e6, 100*365*24*time.Hour, 0, 1, req)
	assert.ErrorContains(t, err, "more than")
	// 0.01 rps for a second rounds to no measured position at all.
	_, err = runPacedLeg(t.Context(), spinClock{}, 0.01, time.Second, 0, 1, req)
	assert.ErrorContains(t, err, "no measured request")
	// A warmup at the leg ceiling leaves no room for a measured position.
	_, err = runPacedLeg(t.Context(), spinClock{}, 10, time.Second, maxLegRequests, 1, req)
	assert.ErrorContains(t, err, "warmup plus")
}

func TestRunPacedLegRejectsScheduleOverflow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rps      float64
		duration time.Duration
		warmup   int
		want     string
	}{
		{"sub-nanosecond interval", 1e9 + 1, time.Nanosecond, 0, "less than 1ns"},
		{"below the milli-rps floor", 0.0005, 10000 * time.Second, 0, "too low"},
		{"position count", 1, time.Second, math.MaxInt, "more than 100000000 positions"},
		{"warmup window", 0.1, 10 * time.Second, int(math.MaxInt32), "more than 100000000 positions"},
		{"schedule window", 0.001, 1000 * time.Second, 10_000_000, "schedule overflows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runPacedLeg(t.Context(), spinClock{}, tc.rps, tc.duration, tc.warmup, 1, nil)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, legResult{}, res)
		})
	}
}

func TestRunPacedLegNanosecondInterval(t *testing.T) {
	for _, rps := range []float64{math.Nextafter(1e9, 0), 1e9} {
		t.Run(formatRPS(rps), func(t *testing.T) {
			req := func(*rand.Rand) (cellSample, error) { return cellSample{items: 1}, nil }
			res, err := runPacedLeg(t.Context(), spinClock{}, rps, time.Nanosecond, 0, 1, req)
			require.NoError(t, err)
			assert.Equal(t, 1, res.positions)
			assert.Equal(t, 1, res.measured.dispatched)
			assert.Len(t, res.samples, 1)
			assert.Zero(t, res.measured.shed)
			assert.Zero(t, res.measured.errs)
			assert.Equal(t, time.Nanosecond, res.arrival)
		})
	}
}

func TestPacedLegAccountingWindows(t *testing.T) {
	const interval = 10 * time.Millisecond
	firstDue := time.Unix(1, 0)
	for _, tc := range []struct {
		name          string
		success, fail time.Duration
		wall, drain   time.Duration
	}{
		{"before end", 15 * time.Millisecond, 0, 15 * time.Millisecond, 0},
		{"at end", 20 * time.Millisecond, 0, 20 * time.Millisecond, 0},
		{"after end", 35 * time.Millisecond, 0, 35 * time.Millisecond, 15 * time.Millisecond},
		{"error finishes last", 15 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond, 20 * time.Millisecond},
		{"success finishes last", 40 * time.Millisecond, 15 * time.Millisecond, 40 * time.Millisecond, 20 * time.Millisecond},
		{"all errors", 0, 40 * time.Millisecond, 40 * time.Millisecond, 20 * time.Millisecond},
		{"all shed", 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leg := newPacedLeg(spinClock{}, nil, 1, 100, 2)
			if tc.success > 0 {
				leg.recordSample(cellSample{}, firstDue.Add(tc.success))
				leg.lags = append(leg.lags, 0)
				leg.measured.dispatched++
			}
			if tc.fail > 0 {
				for leg.measured.dispatched < 2 {
					leg.recordError(&leg.measured, errors.New("request failed"), firstDue.Add(tc.fail))
					leg.lags = append(leg.lags, 0)
					leg.measured.dispatched++
				}
			}
			// Fill all slots so the remaining positions are deterministically shed.
			for range maxInFlight {
				leg.slots <- struct{}{}
			}
			for pos := leg.measured.dispatched; pos < 2; pos++ {
				leg.launch(pos, firstDue.Add(time.Duration(pos)*interval), firstDue, true)
			}
			res := leg.result(firstDue, interval)
			assert.Equal(t, 2, res.positions)
			assert.Equal(t, res.positions, res.measured.dispatched+res.measured.shed)
			assert.Equal(t, res.measured.dispatched, len(res.samples)+res.measured.errs)
			assert.Equal(t, 2*interval, res.arrival)
			assert.Equal(t, tc.wall, res.wall)
			assert.Equal(t, 2*interval+tc.drain, res.elapsed)
			assert.Equal(t, tc.drain, res.drain)
		})
	}
}

// latenessClock is a fake legClock. Its k-th waitUntil(t) sets the time to
// t + lateness[k].
type latenessClock struct {
	mu       sync.Mutex
	t        time.Time
	lateness []time.Duration
	waits    []time.Time
}

func (c *latenessClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *latenessClock) waitUntil(_ context.Context, t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.Add(c.lateness[len(c.waits)])
	c.waits = append(c.waits, t)
	return nil
}

// TestRunPacedLegAbsoluteDueTimes: position i is due at anchor + i×interval,
// whatever the lateness of earlier positions. Every request completes at the
// final fake time, so each sample's scheduled is that time minus its due time.
func TestRunPacedLegAbsoluteDueTimes(t *testing.T) {
	const (
		rps      = 1000
		interval = time.Millisecond
		warmup   = 3
		measured = 6
		total    = warmup + measured
	)
	anchor := time.Unix(100, 0)
	clock := &latenessClock{
		t: anchor,
		lateness: []time.Duration{
			0, 300 * time.Microsecond, 0,
			50 * time.Microsecond, 0, 700 * time.Microsecond, 10 * time.Microsecond, 0, 250 * time.Microsecond,
		},
	}
	due := func(pos int) time.Time { return anchor.Add(time.Duration(pos) * interval) }

	// The request of the last position releases all of them, so every request
	// reads the time after the last waitUntil.
	release := make(chan struct{})
	var calls atomic.Int64
	req := func(*rand.Rand) (cellSample, error) {
		if calls.Add(1) == total {
			close(release)
		}
		<-release
		return cellSample{items: 1}, nil
	}

	res, err := runPacedLeg(t.Context(), clock, rps, measured*interval, warmup, 1, req)
	require.NoError(t, err)

	wantWaits := make([]time.Time, total)
	for pos := range total {
		wantWaits[pos] = due(pos)
	}
	assert.Equal(t, wantWaits, clock.waits)

	done := due(total - 1).Add(clock.lateness[total-1])
	wantScheduled := make([]time.Duration, 0, measured)
	for pos := warmup; pos < total; pos++ {
		wantScheduled = append(wantScheduled, done.Sub(due(pos)))
	}
	gotScheduled := make([]time.Duration, 0, len(res.samples))
	for _, s := range res.samples {
		gotScheduled = append(gotScheduled, s.scheduled)
	}
	slices.Sort(wantScheduled)
	slices.Sort(gotScheduled)

	assert.Equal(t, clock.lateness[warmup:], res.lags)
	assert.Equal(t, wantScheduled, gotScheduled)
	assert.Equal(t, measured, res.positions)
	assert.Equal(t, measured*interval, res.arrival)
	assert.Equal(t, done.Sub(due(warmup)), res.wall)
}

// TestSpinClockPrecision: with the real clock and no-op requests, the median
// dispatch lag stays well under a millisecond timer tick.
func TestSpinClockPrecision(t *testing.T) {
	req := func(*rand.Rand) (cellSample, error) { return cellSample{items: 1}, nil }
	res, err := runPacedLeg(t.Context(), spinClock{}, 2000, 200*time.Millisecond, 0, 1, req)
	require.NoError(t, err)
	require.Len(t, res.lags, 400)
	assert.Zero(t, res.measured.shed)

	lags := slices.Clone(res.lags)
	slices.Sort(lags)
	t.Logf("lag p50=%v p99=%v max=%v", lags[len(lags)/2], lags[len(lags)*99/100], lags[len(lags)-1])
	assert.Less(t, lags[len(lags)/2], 100*time.Microsecond, "median dispatch lag")
}
