package bench

import (
	"context"
	"errors"
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
	assert.Len(t, res.samples, 20)
	assert.Len(t, res.lags, 20)
	assert.Equal(t, 0, res.shed)
	assert.Equal(t, 0, res.errs)
	assert.Equal(t, int64(25), fake.calls.Load(), "warmup requests run at the leg's rate")
	assert.Positive(t, res.wall)
	assert.Equal(t, offeredWindow(rps, res), res.offered)

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
}
