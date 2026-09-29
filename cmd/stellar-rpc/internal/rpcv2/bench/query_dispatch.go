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
	// service is the request body's run time.
	service time.Duration
	// scheduled spans due time to completion: service plus dispatch lag.
	scheduled time.Duration
	// items counts what the response carried.
	items int
	// stage names a sub-stage row (txhash: found or miss); stageNone otherwise.
	stage sampleStage
}

// sampleStage names a sub-stage row.
type sampleStage uint8

const (
	// stageNone is a sample with no sub-stage row.
	stageNone sampleStage = iota
	// stageFound is a txhash sample for the txHashStageFound row.
	stageFound //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
	// stageMiss is a txhash sample for the txHashStageMiss row.
	stageMiss //nolint:unused // consumed by bench-query/02-read-path, the next PR in this stack
)

// queryRequest issues one request. Calls run concurrently on separate
// goroutines; rng is per call. Read-view acquisition is inside the timer.
type queryRequest func(rng *rand.Rand) (cellSample, error)

// maxInFlight caps a leg's outstanding requests; a request due at a full cap
// is shed.
const maxInFlight = 512

// minLegRPS is the lowest leg rate. A lower rate stores zero in the report's
// milli-rps rows.
const minLegRPS = 1.0 / milliPerUnit

// maxLegRequests caps a leg's measured positions, about 2.7 hours at 10k rps.
// At the cap the leg holds about 4 GB of samples and lags until it ends, and
// the sink about 6 GB (8 GB for txhash) until the run reports. The sink share
// adds up over types × rates.
const maxLegRequests = 100_000_000

// Mixing constants for legRNG: odd and mutually prime.
const (
	legSeedRateHi = 1000003
	legSeedRateLo = 73
	legSeedStride = 7919
)

// legRecord holds what a leg records as it runs. pacedLeg fills it; legResult
// carries it out.
type legRecord struct {
	// Written by the dispatch goroutine only.
	lags       []time.Duration // dispatch lag per measured position, shed ones included
	dispatched int             // measured requests that ran
	shed       int             // measured positions dropped at a full maxInFlight
	warmupShed int             // warmup positions dropped at a full maxInFlight

	// Written by the request goroutines, under pacedLeg.mu. A first error is
	// the first to take the mutex; it is nil when its count is zero.
	samples        []cellSample // measured requests that succeeded
	errs           int
	firstErr       error
	warmupErrs     int
	firstWarmupErr error
}

// legResult is one paced leg's outcome.
type legResult struct {
	legRecord

	// arrival is measured positions * interval, the effective arrival window.
	arrival time.Duration
	// wall spans the first measured due time to the last measured completion.
	// It is zero if no measured request completed; errors count as completions.
	wall time.Duration
	// elapsed is max(arrival, wall), including the final arrival interval.
	elapsed time.Duration
	// drain is max(wall - arrival, 0), completion time beyond the arrival window.
	drain time.Duration
	// scheduled counts the measured positions: the planned count, or the
	// positions reached before a cancel.
	scheduled int
}

// finish sets the accounting window for measured positions after all requests
// finish.
func (r *legResult) finish(measured int, interval time.Duration) {
	r.scheduled = measured
	r.arrival = time.Duration(measured) * interval
	r.elapsed = max(r.arrival, r.wall)
	r.drain = max(r.wall-r.arrival, 0)
}

// pacedLeg is one leg's dispatch state.
type pacedLeg struct {
	legRecord

	req   queryRequest
	seed  int64
	rps   float64
	wg    sync.WaitGroup
	slots chan struct{}

	// mu guards the request goroutines' legRecord fields and lastDone.
	mu       sync.Mutex
	lastDone time.Time
}

func newPacedLeg(req queryRequest, seed int64, rps float64, measured int) *pacedLeg {
	return &pacedLeg{
		req:   req,
		seed:  seed,
		rps:   rps,
		slots: make(chan struct{}, maxInFlight),
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
	out := legResult{legRecord: l.legRecord}
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
// A non-nil error is a bad argument or a context error. After a bad argument
// the result is empty. After a context error the result covers the positions
// dispatched before the cancel, and scheduled counts them, so a caller can
// report the leg as PARTIAL. A failing request is counted in errs and does
// not end the leg.
func runPacedLeg(
	ctx context.Context, rps float64, duration time.Duration, warmup int, seed int64, req queryRequest,
) (legResult, error) {
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		return legResult{}, fmt.Errorf("paced leg needs a positive finite rate, got %v", rps)
	}
	if rps < minLegRPS {
		return legResult{}, fmt.Errorf(
			"paced leg rate %v is too low: the report's milli-rps rows need at least %v rps", rps, minLegRPS)
	}
	if duration <= 0 {
		return legResult{}, fmt.Errorf("paced leg needs a positive duration, got %v", duration)
	}
	intervalNS := float64(time.Second) / rps
	if intervalNS < 1 {
		return legResult{}, fmt.Errorf("paced leg rate %v is too high: its interval is less than 1ns", rps)
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
	var err error
	for pos := range warmup + measured {
		if err = ctx.Err(); err != nil {
			break
		}
		due := schedule.dueForPos(pos)
		if err = contextSleep(ctx, due.Sub(schedule.clock())); err != nil {
			break
		}
		leg.launch(pos, due, schedule.clock(), pos >= warmup)
	}
	leg.wg.Wait()
	res := leg.result(schedule.dueForPos(warmup))
	// lags has one entry per measured position reached: measured at a normal
	// end, fewer after a cancel.
	res.finish(len(res.lags), interval)
	return res, err
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
func timed(stage sampleStage, fn func() (int, error)) (cellSample, error) {
	start := time.Now()
	items, err := fn()
	service := time.Since(start)
	if err != nil {
		return cellSample{}, err
	}
	return cellSample{service: service, items: items, stage: stage}, nil
}
