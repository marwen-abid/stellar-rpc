package bench

import (
	"context"
	"errors"
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

// maxLegRequests caps a leg's measured positions. A measured request is
// retained twice: 48 bytes in the leg (a 40-byte cellSample plus its dispatch
// lag), freed when the leg ends, and 48 more in the sink — 16 each for
// total_r<rate>, service_r<rate> and the driver _lag row, plus 16 for a txhash
// found/miss row — held until the run reports. So the ceiling admits about
// 9.6 GB for one leg, 11.2 GB for a txhash leg, plus roughly 2.4 GB of
// transient copies while that leg is logged and aggregated. The leg share is
// per leg; the sink share is cumulative over types × rates. 1e8 positions is
// about 2.7 hours at 10k rps, or 11 days at 100 rps. Aggregation runs only
// after a leg ends.
const maxLegRequests = 100_000_000

// Mixing constants for legRNG: odd and mutually prime.
const (
	legSeedRateHi = 1000003
	legSeedRateLo = 73
	legSeedStride = 7919
)

// legResult is one paced leg's outcome.
type legResult struct {
	// samples holds the measured requests that succeeded.
	samples []cellSample
	// lags holds one dispatch lag per measured position, shed positions
	// included.
	lags []time.Duration
	// offered is measured positions * interval, the effective arrival window.
	offered time.Duration
	// wall spans the first measured due time to the last measured completion.
	// It is zero if no measured request completed; errors count as completions.
	wall time.Duration
	// elapsed is max(offered, wall), including the final arrival interval.
	elapsed time.Duration
	// drain is max(wall - offered, 0), completion time beyond the arrival window.
	drain time.Duration
	// scheduled is the planned number of measured positions.
	scheduled int
	// dispatched counts the measured requests that ran.
	dispatched int
	// shed counts the measured requests dropped at a full maxInFlight.
	shed int
	// warmupShed counts the warmup positions dropped at a full maxInFlight.
	// The warmup requests that ran are warmup minus this count.
	warmupShed int
	// warmupErrs counts the warmup requests that returned an error. A warmup
	// request is unmeasured, so it contributes no sample and no lag.
	warmupErrs int
	// firstWarmupErr is the first warmup error recorded. It is nil when no
	// warmup request failed; warmupErrs counts them all.
	firstWarmupErr error
	// errs counts the measured requests that returned an error.
	errs int
	// firstErr is the first request error recorded, "first" meaning first to
	// take the mutex. It is nil when no measured request failed; errs counts
	// them all.
	firstErr error
}

// finish sets the accounting window after the full schedule and all requests finish.
func (r *legResult) finish(measured int, interval time.Duration) {
	r.scheduled = measured
	r.offered = time.Duration(measured) * interval
	r.elapsed = max(r.offered, r.wall)
	r.drain = max(r.wall-r.offered, 0)
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
	warmupShed int

	// Written by the request goroutines, under mu.
	mu             sync.Mutex
	samples        []cellSample
	lastDone       time.Time
	errs           int
	firstErr       error
	warmupErrs     int
	firstWarmupErr error
}

// legPreallocCap bounds the up-front sample and lag allocation of a leg. A
// longer schedule grows both slices as it runs.
const legPreallocCap = 1 << 20

func newPacedLeg(req queryRequest, seed int64, rps float64, measured int) *pacedLeg {
	prealloc := min(measured, legPreallocCap)
	return &pacedLeg{
		req:     req,
		seed:    seed,
		rps:     rps,
		slots:   make(chan struct{}, maxInFlight),
		lags:    make([]time.Duration, 0, prealloc),
		samples: make([]cellSample, 0, prealloc),
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

func (l *pacedLeg) recordError(err error, done time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs++
	if l.firstErr == nil {
		l.firstErr = err
	}
	if done.After(l.lastDone) {
		l.lastDone = done
	}
}

// recordWarmupError counts a failed warmup request. It leaves lastDone alone,
// which bounds the measured window only.
func (l *pacedLeg) recordWarmupError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warmupErrs++
	if l.firstWarmupErr == nil {
		l.firstWarmupErr = err
	}
}

// result assembles the leg's outcome once every request has returned. wall is
// measured from firstDue and is zero when nothing completed.
func (l *pacedLeg) result(firstDue time.Time) legResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := legResult{
		samples:        l.samples,
		lags:           l.lags,
		dispatched:     l.dispatched,
		shed:           l.shed,
		warmupShed:     l.warmupShed,
		warmupErrs:     l.warmupErrs,
		firstWarmupErr: l.firstWarmupErr,
		errs:           l.errs,
		firstErr:       l.firstErr,
	}
	if !l.lastDone.IsZero() {
		out.wall = l.lastDone.Sub(firstDue)
	}
	return out
}

// legInterval is the arrival interval of a leg paced at rps requests per
// second. The schedule and the accounting window share this value.
func legInterval(rps float64) time.Duration {
	return time.Duration(math.Round(float64(time.Second) / rps))
}

// runPacedLeg issues req at rps requests per second: position i is due at
// anchor + i×interval, anchor being the first dispatch. Positions 0 to
// warmup-1 are unmeasured; the round(rps × duration) positions after them are
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
	intervalNS := float64(time.Second) / rps
	if intervalNS < 1 {
		return legResult{}, fmt.Errorf("paced leg rate %v is too high: its interval is less than 1ns", rps)
	}
	if intervalNS >= float64(math.MaxInt64) {
		return legResult{}, fmt.Errorf("paced leg rate %v is too low: its interval overflows a Duration", rps)
	}
	if rps*duration.Seconds() > maxLegRequests {
		return legResult{}, fmt.Errorf("paced leg at %v rps for %v schedules more than %d requests",
			rps, duration, maxLegRequests)
	}
	warmup = max(warmup, 0)
	measured := measuredRequests(rps, duration)
	if measured < 1 {
		return legResult{}, fmt.Errorf(
			"paced leg at %v rps for %v schedules no measured request; raise --duration or --target-rps",
			rps, duration)
	}
	interval := legInterval(rps)
	if warmup > math.MaxInt-measured {
		return legResult{}, errors.New("paced leg warmup plus measured request count overflows an int")
	}
	if warmup > maxLegRequests-measured {
		return legResult{}, fmt.Errorf(
			"paced leg schedules more than %d positions: %d warmup plus %d measured",
			maxLegRequests, warmup, measured)
	}
	if int64(warmup+measured) > math.MaxInt64/int64(interval) {
		return legResult{}, errors.New("paced leg schedule overflows a Duration")
	}
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
	res.finish(measured, interval)
	return res, nil
}

// launch runs position pos's request on its own goroutine when a slot is free
// and sheds it otherwise. A measured position records its lag, now minus due,
// whether or not it is shed; a warmup position records only its shedding and
// its error. Called from the dispatch goroutine only.
func (l *pacedLeg) launch(pos int, due, now time.Time, measured bool) {
	if measured {
		l.lags = append(l.lags, max(now.Sub(due), 0))
	}
	select {
	case l.slots <- struct{}{}:
	default:
		if measured {
			l.shed++
		} else {
			l.warmupShed++
		}
		return
	}
	if measured {
		l.dispatched++
	}
	l.wg.Go(func() {
		s, err := l.req(legRNG(l.seed, pos, l.rps))
		done := time.Now()
		<-l.slots
		switch {
		case !measured:
			if err != nil {
				l.recordWarmupError(err)
			}
		case err != nil:
			l.recordError(err, done)
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

// measuredRequests is a leg's measured position count, round(rps × duration).
func measuredRequests(rps float64, duration time.Duration) int {
	return int(math.Round(rps * duration.Seconds()))
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
