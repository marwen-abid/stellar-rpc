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
	return timed("", func() (int, error) {
		time.Sleep(countingRequestService)
		return 1, nil
	})
}

// TestRunPacedLegMeasuredCount: a clean leg measures round(rps × duration)
// requests, runs warmup requests without sampling them, and sheds nothing.
func TestRunPacedLegMeasuredCount(t *testing.T) {
	const rps = 200.0
	fake := &countingRequest{}
	res, err := runPacedLeg(t.Context(), rps, 100*time.Millisecond, 5, 42, fake.run)
	require.NoError(t, err)

	assert.Equal(t, 20, res.dispatched)
	assert.Equal(t, 20, res.scheduled)
	assert.Len(t, res.samples, 20)
	assert.Len(t, res.lags, 20)
	assert.Equal(t, 0, res.shed)
	assert.Equal(t, 0, res.errs)
	assert.Equal(t, int64(25), fake.calls.Load(), "warmup requests run at the leg's rate")
	assert.Positive(t, res.wall)
	assert.Equal(t, offeredWindow(rps, res), res.offered)
	assert.Equal(t, max(res.offered, res.wall), res.elapsed)
	assert.Equal(t, max(res.wall-res.offered, 0), res.drain)

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
	return timed("", func() (int, error) { return 1, nil })
}

// TestRunPacedLegRNGIndependence: requests of one leg, and legs at different
// rates, draw distinct values.
func TestRunPacedLegRNGIndependence(t *testing.T) {
	first := &drawRecorder{}
	_, err := runPacedLeg(t.Context(), 500, 40*time.Millisecond, 0, 7, first.run)
	require.NoError(t, err)
	require.Len(t, first.draws, 20)

	seen := make(map[uint64]bool, len(first.draws))
	for _, v := range first.draws {
		assert.False(t, seen[v], "two requests of one leg drew %d", v)
		seen[v] = true
	}

	second := &drawRecorder{}
	_, err = runPacedLeg(t.Context(), 250, 80*time.Millisecond, 0, 7, second.run)
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
	return timed("", func() (int, error) {
		<-b.release
		return 1, nil
	})
}

// TestRunPacedLegSheds: with every slot held, the first maxInFlight measured
// requests run and the rest are shed; lags cover every measured position.
func TestRunPacedLegSheds(t *testing.T) {
	fake := &blockingRequest{release: make(chan struct{})}
	// Released after the 100ms schedule ends: every slot stays held for the
	// whole dispatch loop.
	timer := time.AfterFunc(500*time.Millisecond, func() { close(fake.release) })
	defer timer.Stop()

	res, err := runPacedLeg(t.Context(), 10000, 100*time.Millisecond, 0, 3, fake.run)
	require.NoError(t, err)

	assert.Equal(t, maxInFlight, res.dispatched)
	assert.Equal(t, 1000-maxInFlight, res.shed)
	assert.Equal(t, 1000, res.dispatched+res.shed)
	assert.Equal(t, res.scheduled, res.dispatched+res.shed)
	assert.Equal(t, res.dispatched, len(res.samples)+res.errs)
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
		return timed("", func() (int, error) { return 1, nil })
	}

	res, err := runPacedLeg(t.Context(), rps, 100*time.Millisecond, 0, 11, req)
	require.NoError(t, err)

	assert.Equal(t, 20, res.dispatched)
	assert.Equal(t, res.scheduled, res.dispatched+res.shed)
	assert.Equal(t, res.dispatched, len(res.samples)+res.errs)
	assert.Equal(t, 10, res.errs)
	assert.Len(t, res.samples, 10)
	assert.Len(t, res.lags, 20)
	assert.Equal(t, 0, res.shed)
	assert.Equal(t, offeredWindow(rps, res), res.offered)
}

// offeredWindow is the expected legResult.offered of a leg at rps.
func offeredWindow(rps float64, res legResult) time.Duration {
	return time.Duration(res.dispatched+res.shed) * time.Duration(float64(time.Second)/rps)
}

// TestRunPacedLegContextCancel: a canceled context ends the leg with the
// context's error and no request running.
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
	_, err := runPacedLeg(ctx, 2, 10*time.Second, 0, 5, req)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, time.Second, "the leg ends at the cancel, not at its full duration")
	assert.Equal(t, int64(0), inFlight.Load())
}

// TestRunPacedLegLagStaysSmall: requests longer than the arrival interval hold
// up no later dispatch. It asserts on the median lag and the wall, not the
// worst lag: one dispatch can lose the CPU for tens of milliseconds on a busy
// machine.
func TestRunPacedLegLagStaysSmall(t *testing.T) {
	const service = 20 * time.Millisecond
	req := func(*rand.Rand) (cellSample, error) {
		return timed("", func() (int, error) {
			time.Sleep(service)
			return 1, nil
		})
	}

	res, err := runPacedLeg(t.Context(), 1000, 50*time.Millisecond, 0, 13, req)
	require.NoError(t, err)

	assert.Equal(t, 50, res.dispatched)
	assert.Len(t, res.lags, res.dispatched)
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
		return timed("", func() (int, error) { return 1, nil })
	}

	leg := newPacedLeg(req, 1, 1, 1)
	due := time.Now().Add(-late)
	leg.launch(0, due, time.Now(), true)
	leg.wg.Wait()

	res := leg.result(due)
	require.Len(t, res.samples, 1)
	require.Len(t, res.lags, 1)
	assert.GreaterOrEqual(t, res.samples[0].scheduled, late, "the client waited from the due time")
	assert.Less(t, res.samples[0].service, res.samples[0].scheduled-40*time.Millisecond,
		"the request itself took almost none of that wait")
	assert.GreaterOrEqual(t, res.lags[0], late, "the dispatch lag is charged too")
}

// TestRunPacedLegRejectsBadArguments: a zero rate or a zero duration is an
// error.
func TestRunPacedLegRejectsBadArguments(t *testing.T) {
	req := func(*rand.Rand) (cellSample, error) {
		return timed("", func() (int, error) { return 1, nil })
	}
	_, err := runPacedLeg(t.Context(), 0, time.Second, 0, 1, req)
	assert.Error(t, err)
	_, err = runPacedLeg(t.Context(), 10, 0, 0, 1, req)
	assert.Error(t, err)
	// 1e-12 rps is one request per 1e12 s, an interval no Duration holds.
	_, err = runPacedLeg(t.Context(), 1e-12, time.Second, 0, 1, req)
	assert.ErrorContains(t, err, "too low")
	// 1e6 rps over a century is more positions than an int32 counts.
	_, err = runPacedLeg(t.Context(), 1e6, 100*365*24*time.Hour, 0, 1, req)
	assert.ErrorContains(t, err, "more than")
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
		{"duration conversion boundary", float64(time.Second) / float64(math.MaxInt64), time.Second, 0, "too low"},
		{"position count", 1, time.Second, math.MaxInt, "count overflows"},
		{"warmup window", 0.1, time.Second, int(math.MaxInt32), "schedule overflows"},
		{"measured window", 2e-10, time.Duration(math.MaxInt64), 0, "schedule overflows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runPacedLeg(t.Context(), tc.rps, tc.duration, tc.warmup, 1, nil)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, legResult{}, res)
		})
	}
}

func TestRunPacedLegNanosecondInterval(t *testing.T) {
	for _, rps := range []float64{math.Nextafter(1e9, 0), 1e9} {
		t.Run(formatRPS(rps), func(t *testing.T) {
			req := func(*rand.Rand) (cellSample, error) { return cellSample{items: 1}, nil }
			res, err := runPacedLeg(t.Context(), rps, time.Nanosecond, 0, 1, req)
			require.NoError(t, err)
			assert.Equal(t, 1, res.scheduled)
			assert.Equal(t, 1, res.dispatched)
			assert.Len(t, res.samples, 1)
			assert.Zero(t, res.shed)
			assert.Zero(t, res.errs)
			assert.Equal(t, time.Nanosecond, res.offered)
			assert.Equal(t, max(res.offered, res.wall), res.elapsed)
			assert.Equal(t, max(res.wall-res.offered, 0), res.drain)
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
			leg := newPacedLeg(nil, 1, 100, 2)
			if tc.success > 0 {
				leg.recordSample(cellSample{}, firstDue.Add(tc.success))
				leg.dispatched++
			}
			if tc.fail > 0 {
				for leg.dispatched < 2 {
					leg.recordError(firstDue.Add(tc.fail))
					leg.dispatched++
				}
			}
			// Fill all slots so the remaining positions are deterministically shed.
			for range maxInFlight {
				leg.slots <- struct{}{}
			}
			for pos := leg.dispatched; pos < 2; pos++ {
				leg.launch(pos, firstDue.Add(time.Duration(pos)*interval), firstDue, true)
			}
			res := leg.result(firstDue)
			res.finish(2, interval)
			assert.Equal(t, 2, res.scheduled)
			assert.Equal(t, res.scheduled, res.dispatched+res.shed)
			assert.Equal(t, res.dispatched, len(res.samples)+res.errs)
			assert.Equal(t, 2*interval, res.offered)
			assert.Equal(t, tc.wall, res.wall)
			assert.Equal(t, 2*interval+tc.drain, res.elapsed)
			assert.Equal(t, tc.drain, res.drain)
		})
	}
}
