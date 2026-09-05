package bench

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// The query bench's dispatcher: it issues a queryRequest at a fixed arrival
// rate and collects what each request observed.

// cellSample is one measured request.
type cellSample struct {
	// service is the request body's run time (see timed).
	service time.Duration
	// scheduled spans due time to completion: service plus dispatch lag.
	scheduled time.Duration
	// items counts what the response carried.
	items int
	// stage names a sub-stage row (txhash: found or miss); empty otherwise.
	stage string
}

// queryRequest issues one request. Calls run concurrently on separate
// goroutines; rng is per call. Read-view acquisition is inside the timer.
type queryRequest func(rng *rand.Rand) (cellSample, error)

// maxInFlight caps a leg's outstanding requests; a request due at a full cap
// is shed.
const maxInFlight = 512

// Mixing constants for legRNG: odd and mutually prime.
const (
	legSeedRateHi = 1000003
	legSeedRateLo = 73
	legSeedStride = 7919
)

// legResult is one paced leg's outcome.
type legResult struct {
	// samples holds the measured requests that answered.
	samples []cellSample
	// lags holds one dispatch lag per measured position, shed positions
	// included.
	lags []time.Duration
	// offered is measured positions × interval, the denominator of the
	// achieved rate.
	offered time.Duration
	// wall spans the first measured due time to the last measured completion.
	wall time.Duration
	// dispatched counts the measured requests that ran.
	dispatched int
	// shed counts the measured requests dropped at a full maxInFlight.
	shed int
	// errs counts the measured requests that returned an error.
	errs int
}

// pacedLeg is one leg's dispatch state.
type pacedLeg struct {
	req   queryRequest
	seed  int64
	rps   float64
	wg    sync.WaitGroup
	slots chan struct{}

	// Written by the dispatch goroutine only.
	lags       []time.Duration
	dispatched int
	shed       int

	// Written by the request goroutines, under mu.
	mu       sync.Mutex
	samples  []cellSample
	lastDone time.Time
	errs     int
}

func newPacedLeg(req queryRequest, seed int64, rps float64, measured int) *pacedLeg {
	return &pacedLeg{
		req:     req,
		seed:    seed,
		rps:     rps,
		slots:   make(chan struct{}, maxInFlight),
		lags:    make([]time.Duration, 0, measured),
		samples: make([]cellSample, 0, measured),
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

func (l *pacedLeg) recordError(done time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs++
	if done.After(l.lastDone) {
		l.lastDone = done
	}
}

// result assembles the leg's outcome once every request has returned. wall is
// measured from firstDue and is zero when nothing completed.
func (l *pacedLeg) result(firstDue time.Time) legResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := legResult{
		samples:    l.samples,
		lags:       l.lags,
		dispatched: l.dispatched,
		shed:       l.shed,
		errs:       l.errs,
	}
	if !l.lastDone.IsZero() {
		out.wall = l.lastDone.Sub(firstDue)
	}
	return out
}

// runPacedLeg issues req at rps requests per second: position i is due at
// anchor + i×interval, anchor being the first dispatch. Positions 0 to
// warmup-1 record nothing; the round(rps × duration) positions after them are
// measured.
//
// A non-nil error is a bad argument or a context error; discard the result. A
// failing request is counted in errs and does not end the leg.
func runPacedLeg(
	ctx context.Context, rps float64, duration time.Duration, warmup int, seed int64, req queryRequest,
) (legResult, error) {
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		return legResult{}, fmt.Errorf("paced leg needs a positive finite rate, got %v", rps)
	}
	if duration <= 0 {
		return legResult{}, fmt.Errorf("paced leg needs a positive duration, got %v", duration)
	}
	if float64(time.Second)/rps > math.MaxInt64 {
		return legResult{}, fmt.Errorf("paced leg rate %v is too low: its interval overflows a Duration", rps)
	}
	if rps*duration.Seconds() > math.MaxInt32 {
		return legResult{}, fmt.Errorf("paced leg at %v rps for %v schedules more than %d requests",
			rps, duration, math.MaxInt32)
	}
	warmup = max(warmup, 0)
	measured := measuredRequests(rps, duration)
	interval := time.Duration(float64(time.Second) / rps)
	schedule := newPaceSchedule(interval, 0)

	leg := newPacedLeg(req, seed, rps, measured)
	for pos := range warmup + measured {
		if err := ctx.Err(); err != nil {
			leg.wg.Wait()
			return legResult{}, err
		}
		due := schedule.dueForPos(pos)
		if err := contextSleep(ctx, due.Sub(schedule.clock())); err != nil {
			leg.wg.Wait()
			return legResult{}, err
		}
		leg.launch(pos, due, schedule.clock(), pos >= warmup)
	}
	leg.wg.Wait()
	res := leg.result(schedule.dueForPos(warmup))
	res.offered = time.Duration(measured) * interval
	return res, nil
}

// launch runs position pos's request on its own goroutine when a slot is free
// and sheds it otherwise. A measured position records its lag, now minus due,
// whether or not it is shed; a warmup position records nothing. Called from the
// dispatch goroutine only.
func (l *pacedLeg) launch(pos int, due, now time.Time, measured bool) {
	if measured {
		l.lags = append(l.lags, max(now.Sub(due), 0))
	}
	select {
	case l.slots <- struct{}{}:
	default:
		if measured {
			l.shed++
		}
		return
	}
	if measured {
		l.dispatched++
	}
	l.wg.Go(func() {
		defer func() { <-l.slots }()
		s, err := l.req(legRNG(l.seed, pos, l.rps))
		done := time.Now()
		switch {
		case !measured:
		case err != nil:
			l.recordError(done)
		default:
			s.scheduled = done.Sub(due)
			l.recordSample(s, done)
		}
	})
}

// legRNG derives a request's RNG from the run seed, the position and the rate.
// Two positions of one leg, or one position of two legs at different rates,
// draw different sequences.
func legRNG(seed int64, pos int, rps float64) *rand.Rand {
	rateBits := math.Float64bits(rps)
	//nolint:gosec // seed mixing, not cryptography
	return rand.New(rand.NewPCG(
		uint64(seed)+uint64(pos)+rateBits*legSeedRateHi,
		uint64(seed*legSeedStride)+uint64(pos)+rateBits*legSeedRateLo,
	))
}

func measuredRequests(rps float64, duration time.Duration) int {
	return max(1, int(math.Round(rps*duration.Seconds())))
}

// timed runs fn and returns a sample with fn's run time as service.
//
//nolint:unparam // stage is set by the txhash body in bench-query/02-read-path, the next PR in this stack
func timed(stage string, fn func() (int, error)) (cellSample, error) {
	start := time.Now()
	items, err := fn()
	service := time.Since(start)
	if err != nil {
		return cellSample{}, err
	}
	return cellSample{service: service, items: items, stage: stage}, nil
}
