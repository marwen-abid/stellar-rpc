package bench

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// The query bench's dispatcher: it issues a per-request closure at a fixed
// arrival rate and collects what every request observed. It does no I/O of its
// own, so the only thing between the timer and the store is the query itself.

// cellSample is one measured request.
type cellSample struct {
	// service is how long the request body itself ran (see timed).
	service time.Duration
	// scheduled spans the request's due time to its completion: service time
	// plus dispatch lag, the latency a client sending at the target rate sees.
	scheduled time.Duration
	// items counts what the response carried: ledgers, transactions or events.
	items int
	// stage names a sub-stage reported apart from the type's blended row. Only
	// txhash sets it (found vs miss).
	stage string
}

// queryRequest issues one request against the fixture. It runs on its own
// goroutine with the RNG the dispatcher hands it, so it must treat its corpus
// as read-only and share no mutable state between concurrent requests.
// Read-view acquisition is inside the timer: a served request pays for its view.
type queryRequest func(rng *rand.Rand) (cellSample, error)

// maxInFlight caps the requests a paced leg has outstanding. A request arriving
// at a full cap is shed and counted, so a rate the store cannot keep up with
// shows as a shed count and the leg's goroutine count stays bounded.
const maxInFlight = 512

// Mixing constants for the per-request seeds: odd and mutually prime, so the
// seed, the position and the rate each move both PCG words.
const (
	legSeedRateHi = 1000003
	legSeedRateLo = 73
	legSeedStride = 7919
)

// legResult is one paced leg's outcome.
type legResult struct {
	// samples holds the measured requests that answered; a failed one has no
	// latency to report.
	samples []cellSample
	// lags holds one dispatch lag per measured position in schedule order,
	// shed positions included. A zero is an on-time dispatch and is kept.
	lags []time.Duration
	// offered is the leg's offered window: measured positions times the
	// arrival interval, the denominator of the achieved rate.
	offered time.Duration
	// wall spans the first measured due time to the last measured completion,
	// drain tail included.
	wall time.Duration
	// dispatched counts the measured requests that got a slot and ran.
	dispatched int
	// shed counts the measured requests dropped at a full maxInFlight.
	shed int
	// errs counts the measured requests that returned an error.
	errs int
}

// legCollector gathers a paced leg's outcome; the dispatch loop and the
// request goroutines write to it concurrently.
type legCollector struct {
	mu         sync.Mutex
	samples    []cellSample
	lags       []time.Duration
	lastDone   time.Time
	dispatched int
	shed       int
	errs       int
}

// recordLag charges a measured position its lag behind its due time, whether
// its request ran or was shed.
func (c *legCollector) recordLag(lag time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lags = append(c.lags, lag)
}

func (c *legCollector) recordDispatch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dispatched++
}

func (c *legCollector) recordShed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shed++
}

// recordSample stores a sample; the latest completion is where the wall ends.
func (c *legCollector) recordSample(s cellSample, done time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, s)
	if done.After(c.lastDone) {
		c.lastDone = done
	}
}

// recordError counts a failed request. It occupied the leg like any other, so
// its completion counts toward the wall.
func (c *legCollector) recordError(done time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs++
	if done.After(c.lastDone) {
		c.lastDone = done
	}
}

// result assembles the leg's outcome with the wall measured from firstDue. A
// leg in which nothing completed reports a zero wall.
func (c *legCollector) result(firstDue time.Time) legResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := legResult{
		samples:    c.samples,
		lags:       c.lags,
		dispatched: c.dispatched,
		shed:       c.shed,
		errs:       c.errs,
	}
	if !c.lastDone.IsZero() {
		out.wall = c.lastDone.Sub(firstDue)
	}
	return out
}

// runPacedLeg issues req at rps requests per second. Position i is due at
// anchor + i×interval (interval = 1 s / rps, anchor = the first dispatch). Due
// times are absolute, so a slow request delays no later one and the leg keeps
// its rate whatever the store does with it. Positions 0 to warmup-1 record
// nothing; the round(rps × duration) positions after them are measured, and
// the leg offers the store measured×interval of arrivals.
//
// The returned error is a context error: the leg was cut short, discard its
// result. A request that fails is counted in errs and does not end the leg.
func runPacedLeg(
	ctx context.Context, rps float64, duration time.Duration, warmup int, seed int64, req queryRequest,
) (legResult, error) {
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		return legResult{}, fmt.Errorf("paced leg needs a positive finite rate, got %v", rps)
	}
	if duration <= 0 {
		return legResult{}, fmt.Errorf("paced leg needs a positive duration, got %v", duration)
	}
	warmup = max(warmup, 0)
	measured := measuredRequests(rps, duration)
	interval := time.Duration(float64(time.Second) / rps)
	schedule := newPaceSchedule(interval, 0)

	collector := &legCollector{}
	slots := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for pos := range warmup + measured {
		// Checked every iteration: a lagging dispatcher's sleeps return at once.
		if err := ctx.Err(); err != nil {
			wg.Wait()
			return legResult{}, err
		}
		due := schedule.dueForPos(pos)
		if err := contextSleep(ctx, due.Sub(schedule.clock())); err != nil {
			wg.Wait()
			return legResult{}, err
		}
		launchPacedRequest(&wg, slots, collector, req, legRNG(seed, pos, rps), due, pos >= warmup)
	}
	wg.Wait()
	res := collector.result(schedule.dueForPos(warmup))
	res.offered = time.Duration(measured) * interval
	return res, nil
}

// launchPacedRequest runs one request on its own goroutine after taking a slot;
// with none free the request is shed and runs nothing. A measured position is
// charged its lag before the cap check, so the lag distribution covers shed
// positions too. A warmup request runs in full and records nothing.
func launchPacedRequest(
	wg *sync.WaitGroup, slots chan struct{}, collector *legCollector,
	req queryRequest, rng *rand.Rand, due time.Time, measured bool,
) {
	if measured {
		collector.recordLag(max(time.Since(due), 0))
	}
	select {
	case slots <- struct{}{}:
	default:
		if measured {
			collector.recordShed()
		}
		return
	}
	if measured {
		collector.recordDispatch()
	}
	wg.Go(func() {
		defer func() { <-slots }()
		s, err := req(rng)
		done := time.Now()
		switch {
		case !measured:
		case err != nil:
			collector.recordError(done)
		default:
			s.scheduled = done.Sub(due)
			collector.recordSample(s, done)
		}
	})
}

// legRNG mixes the rate and the position into the run's seed: the rate keeps
// two legs of one query type drawing different sequences, the position keeps
// two requests of one leg drawing different work, and no measured position
// replays a warmup draw.
func legRNG(seed int64, pos int, rps float64) *rand.Rand {
	rateBits := math.Float64bits(rps)
	//nolint:gosec // seed mixing, not cryptography
	return rand.New(rand.NewPCG(
		uint64(seed)+uint64(pos)+rateBits*legSeedRateHi,
		uint64(seed*legSeedStride)+uint64(pos)+rateBits*legSeedRateLo,
	))
}

// measuredRequests is round(rps × duration), never fewer than one, so a rate
// too slow to schedule one request in the leg still measures something.
func measuredRequests(rps float64, duration time.Duration) int {
	return max(1, int(math.Round(rps*duration.Seconds())))
}

// timed runs fn and returns a sample whose service time is how long fn took,
// so a per-type body times exactly the request and nothing around it.
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
