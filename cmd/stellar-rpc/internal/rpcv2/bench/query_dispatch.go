package bench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"
)

// cellSample is one measured request.
type cellSample struct {
	// service is the request body's run time.
	service time.Duration
	// scheduled spans due time to completion: service plus dispatch lag.
	scheduled time.Duration
	// items counts what the response carried.
	items int
	// stage is the txhash sub-stage row (found or miss), or stageNone.
	stage sampleStage
}

type sampleStage uint8

const (
	stageNone sampleStage = iota

	// txhash sub-stage rows.
	stageFound //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
	stageMiss  //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
)

// queryRequest issues one request. Calls run concurrently on separate
// goroutines; rng is per call. Read-view acquisition is inside the timer.
type queryRequest func(rng *rand.Rand) (cellSample, error)

// maxInFlight caps a leg's outstanding requests. A request due at a full cap
// is shed, so a slow server cannot grow the goroutine count without bound.
const maxInFlight = 512

// minLegRPS is the lowest leg rate. A lower rate stores zero in the report's
// milli-rps rows.
const minLegRPS = 1.0 / milliPerUnit

// maxLegRPS is the highest leg rate: one request per nanosecond.
const maxLegRPS = float64(time.Second)

// maxLegRequests caps a leg's measured positions, about 2.7 hours at 10k rps.
// At the cap the leg holds about 4 GB of samples and lags until it ends, and
// the sink about 6 GB (8 GB for txhash) until the run reports. The sink share
// adds up over types × rates.
const maxLegRequests = 100_000_000

// phaseStats counts one phase's requests. Every position is dispatched or
// shed; errs counts the dispatched requests that failed, and firstErr is nil
// when errs is zero.
type phaseStats struct {
	dispatched int
	shed       int
	errs       int
	firstErr   error
}

// legRecord holds what a leg records as it runs. The dispatch goroutine writes
// lags and each phase's dispatched and shed without the lock. The request
// goroutines write samples and each phase's errs and firstErr under
// pacedLeg.mu.
type legRecord struct {
	lags    []time.Duration // dispatch lag per measured position, shed ones included
	samples []cellSample    // measured requests that succeeded

	warmup   phaseStats
	measured phaseStats
}

// legResult is one paced leg's outcome.
type legResult struct {
	legRecord

	// positions counts the measured positions: the planned count, or the
	// positions reached before a cancel.
	positions int
	// arrival is positions × interval, the effective arrival window.
	arrival time.Duration
	// wall spans the first measured due time to the last measured completion.
	// It is zero if no measured request completed; errors count as completions.
	wall time.Duration
	// elapsed is max(arrival, wall).
	elapsed time.Duration
	// drain is the completion time beyond the arrival window, max(wall - arrival, 0).
	drain time.Duration
}

// legClock is the dispatcher's time source.
type legClock interface {
	now() time.Time
	// waitUntil returns at or after t, or with ctx.Err() once ctx is done. A
	// past t still returns ctx.Err() if ctx is done.
	waitUntil(ctx context.Context, t time.Time) error
}

// spinWindow is how long before a due time spinClock stops sleeping and spins.
// On Linux an idle Go runtime waits for timers in epoll_wait, which takes a
// millisecond timeout, so a sleep wakes up to about 1.5ms late. The cost: the
// dispatcher keeps one P busy for up to spinWindow before each due time, so
// above about 500 rps it uses one core all the time.
const spinWindow = 2 * time.Millisecond

// spinClock is the real legClock. It sleeps until spinWindow before t, then
// spins on time.Now.
type spinClock struct{}

func (spinClock) now() time.Time { return time.Now() }

func (spinClock) waitUntil(ctx context.Context, t time.Time) error {
	if d := time.Until(t) - spinWindow; d > 0 {
		if err := contextSleep(ctx, d); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(t) {
			return nil
		}
		// Lets request goroutines on this P run.
		runtime.Gosched()
	}
}

// pacedLeg is one leg's dispatch state.
type pacedLeg struct {
	legRecord

	clock  legClock
	req    queryRequest
	rngKey uint64
	wg     sync.WaitGroup
	slots  chan struct{}

	// mu guards lastDone and the legRecord fields of the request goroutines.
	mu       sync.Mutex
	lastDone time.Time
}

func newPacedLeg(clock legClock, req queryRequest, seed int64, rps float64, measured int) *pacedLeg {
	return &pacedLeg{
		clock:  clock,
		req:    req,
		rngKey: legRNGKey(seed, rps),
		slots:  make(chan struct{}, maxInFlight),
		legRecord: legRecord{
			lags:    make([]time.Duration, 0, measured),
			samples: make([]cellSample, 0, measured),
		},
	}
}

func (l *pacedLeg) recordSample(s cellSample, done time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples = append(l.samples, s)
	if done.After(l.lastDone) {
		l.lastDone = done
	}
}

// recordError counts a failed request in phase p. Only a measured error moves
// lastDone, which bounds the measured window.
func (l *pacedLeg) recordError(p *phaseStats, err error, done time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p.errs++
	if p.firstErr == nil {
		p.firstErr = err
	}
	if p == &l.measured && done.After(l.lastDone) {
		l.lastDone = done
	}
}

// result assembles the leg's outcome once every request has returned. Each
// measured position reached has one lag, so len(lags) is the position count.
func (l *pacedLeg) result(firstDue time.Time, interval time.Duration) legResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := legResult{legRecord: l.legRecord, positions: len(l.lags)}
	if !l.lastDone.IsZero() {
		out.wall = l.lastDone.Sub(firstDue)
	}
	out.arrival = time.Duration(out.positions) * interval
	out.elapsed = max(out.arrival, out.wall)
	out.drain = max(out.wall-out.arrival, 0)
	return out
}

// legInterval is the arrival interval of a leg paced at rps requests per
// second. The schedule and the accounting window share this value.
func legInterval(rps float64) time.Duration {
	return time.Duration(math.Round(float64(time.Second) / rps))
}

// validateLeg checks a leg's arguments and returns its interval and measured
// position count. warmup must be non-negative.
func validateLeg(rps float64, duration time.Duration, warmup int) (time.Duration, int, error) {
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		return 0, 0, fmt.Errorf("paced leg needs a positive finite rate, got %v", rps)
	}
	if rps < minLegRPS {
		return 0, 0, fmt.Errorf(
			"paced leg rate %v is too low: the report's milli-rps rows need at least %v rps", rps, minLegRPS)
	}
	if rps > maxLegRPS {
		return 0, 0, fmt.Errorf("paced leg rate %v is too high: its interval is less than 1ns", rps)
	}
	if duration <= 0 {
		return 0, 0, fmt.Errorf("paced leg needs a positive duration, got %v", duration)
	}
	if rps*duration.Seconds() > maxLegRequests {
		return 0, 0, fmt.Errorf("paced leg at %v rps for %v schedules more than %d requests",
			rps, duration, maxLegRequests)
	}
	measured := measuredRequests(rps, duration)
	if measured < 1 {
		return 0, 0, fmt.Errorf(
			"paced leg at %v rps for %v schedules no measured request; raise --duration or --target-rps",
			rps, duration)
	}
	if warmup > maxLegRequests-measured {
		return 0, 0, fmt.Errorf(
			"paced leg schedules more than %d positions: %d warmup plus %d measured",
			maxLegRequests, warmup, measured)
	}
	interval := legInterval(rps)
	if int64(warmup+measured) > math.MaxInt64/int64(interval) {
		return 0, 0, errors.New("paced leg schedule overflows a Duration")
	}
	return interval, measured, nil
}

// runPacedLeg issues req at rps requests per second. Position i is due at
// anchor + i×interval, where anchor is clock.now() at the start. Positions 0 to
// warmup-1 are warmup; the round(rps × duration) positions after them are
// measured. A negative warmup counts as zero.
//
// A failing request is counted in errs and does not end the leg. A non-nil
// error is a bad argument, with an empty result, or a context error, with a
// result that covers the positions reached before the cancel.
func runPacedLeg(
	ctx context.Context, clock legClock, rps float64, duration time.Duration, warmup int, seed int64,
	req queryRequest,
) (legResult, error) {
	warmup = max(warmup, 0)
	interval, measured, err := validateLeg(rps, duration, warmup)
	if err != nil {
		return legResult{}, err
	}

	leg := newPacedLeg(clock, req, seed, rps, measured)
	anchor := clock.now()
	due := func(pos int) time.Time { return anchor.Add(time.Duration(pos) * interval) }
	for pos := range warmup + measured {
		if err = clock.waitUntil(ctx, due(pos)); err != nil {
			break
		}
		leg.launch(pos, due(pos), clock.now(), pos >= warmup)
	}

	leg.wg.Wait()
	if err == nil {
		// A cancel while the last requests finish still ends the leg as canceled.
		err = ctx.Err()
	}
	return leg.result(due(warmup), interval), err
}

// launch runs position pos's request on its own goroutine when a slot is free
// and sheds it otherwise. A measured position records its lag, now minus due,
// whether or not it is shed. Called from the dispatch goroutine only.
func (l *pacedLeg) launch(pos int, due, now time.Time, measured bool) {
	phase := &l.warmup
	if measured {
		phase = &l.measured
		l.lags = append(l.lags, max(now.Sub(due), 0))
	}
	select {
	case l.slots <- struct{}{}:
	default:
		phase.shed++
		return
	}
	phase.dispatched++
	l.wg.Go(func() {
		s, err := l.req(requestRNG(l.rngKey, pos))
		done := l.clock.now()
		<-l.slots
		switch {
		case err != nil:
			l.recordError(phase, err, done)
		case measured:
			s.scheduled = done.Sub(due)
			l.recordSample(s, done)
		}
	})
}

// legRNGKey mixes the run seed and the leg rate into one key. Legs at
// different rates get different keys.
func legRNGKey(seed int64, rps float64) uint64 {
	return splitmix64(uint64(seed) ^ splitmix64(math.Float64bits(rps))) //nolint:gosec // seed mixing, not cryptography
}

// requestRNG returns the RNG of position pos in the leg with key. Distinct
// positions or keys give distinct PCG states, so distinct streams.
func requestRNG(key uint64, pos int) *rand.Rand {
	return rand.New(rand.NewPCG(key, splitmix64(key^uint64(pos)))) //nolint:gosec // seed mixing, not cryptography
}

// splitmix64 is the SplitMix64 finalizer, a bijection on uint64.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func measuredRequests(rps float64, duration time.Duration) int {
	return int(math.Round(rps * duration.Seconds()))
}

// timed runs fn and returns a sample with fn's run time as service.
//
//nolint:unparam // stage is set by the txhash body in bench-query/02-read-path, the next PR in this stack
func timed(stage sampleStage, fn func() (int, error)) (cellSample, error) {
	start := time.Now()
	items, err := fn()
	service := time.Since(start)
	if err != nil {
		return cellSample{}, err
	}
	return cellSample{service: service, items: items, stage: stage}, nil
}
