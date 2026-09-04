package bench

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"

	sdkingest "github.com/stellar/go-stellar-sdk/ingest"
	supportlog "github.com/stellar/go-stellar-sdk/support/log"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/adapters"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/event"
)

// This file builds the corpora the per-type bodies draw their work from. Every
// read here happens once, before any measurement, and is never timed.

// The tx-hash sampler's rule: read randomly chosen ledgers, take at most
// corpusMaxHashesPerLedger hashes from each, and stop once the pool holds
// corpusTargetHashes of them or the read cap runs out.
//
// The per-ledger cap spreads the pool over ledgers: a lookup reads the ledger
// the hash landed in, so a pool drawn from one ledger measures a warm read of
// one decompressed blob, not a served by-hash mix. A dataset dense enough to
// fill the pool covers corpusTargetHashes/corpusMaxHashesPerLedger ledgers. The
// read cap ends the sample on a dataset too sparse to reach the target at all.
const (
	corpusTargetHashes       = 512
	corpusMaxLedgerReads     = 512
	corpusMaxHashesPerLedger = 16
)

// eventScanCap bounds how many stored events the filter builder reads for the
// chunk's busiest contracts; a full chunk scan would dominate the run's wall.
const eventScanCap = 20_000

// eventFilterSets is how many filter sets the events corpus offers, including
// the unfiltered one.
const eventFilterSets = 4

// errNoTransactions means the sampled ledgers carried no transactions.
var errNoTransactions = errors.New("the sampled ledgers carry no transactions")

// txHashCorpus is the by-hash benchmark's work: a pool of hashes that really
// landed in the fixture's ledger range, and the fraction of lookups that should
// instead ask for a hash that never landed.
//
// A hit stops at the first index that knows the hash; a miss probes every hot
// index and then every cold window index, so a hit-only corpus reports the
// cheap half of what getTransaction serves.
type txHashCorpus struct {
	hashes       [][32]byte
	missFraction float64
}

// pick returns one hash to look up and whether it is expected to be found. A
// miss is 32 random bytes: what matters is that no index holds it.
func (c *txHashCorpus) pick(rng *rand.Rand) ([32]byte, bool) {
	if c.missFraction > 0 && rng.Float64() < c.missFraction {
		var h [32]byte
		for i := 0; i < len(h); i += 8 {
			binary.LittleEndian.PutUint64(h[i:], rng.Uint64())
		}
		return h, false
	}
	return c.hashes[rng.IntN(len(c.hashes))], true
}

// buildTxHashCorpus samples transaction hashes from the fixture's ledger range
// and checks that one of them resolves before any leg runs. Resolving pairs each
// TxSet envelope to its result by hashing the envelope under the passphrase;
// with the wrong one nothing pairs and the benchmark would publish the failed
// path's latency under the hit path's name.
func buildTxHashCorpus(
	ctx context.Context, logger *supportlog.Entry, f *queryFixture, missFraction float64, seed int64,
) (*txHashCorpus, error) {
	view, err := f.view()
	if err != nil {
		return nil, fmt.Errorf("acquire read view: %w", err)
	}
	defer view.Release()

	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed*31+7))) //nolint:gosec // seed mixing
	s := newTxHashSampler(rng)
	for _, c := range f.Chunks {
		if err := s.sampleChunk(view, c, f.FirstLedger, f.LastLedger); err != nil {
			return nil, err
		}
	}
	if len(s.hashes) == 0 {
		return nil, fmt.Errorf("%w: chunks %v, ledgers [%d, %d]",
			errNoTransactions, f.Chunks, f.FirstLedger, f.LastLedger)
	}
	hash, seq := s.first()
	if err := verifySampledHashResolves(ctx, view, f, hash, seq); err != nil {
		return nil, err
	}
	s.logCoverage(logger, missFraction)
	return &txHashCorpus{hashes: s.hashes, missFraction: missFraction}, nil
}

// txHashSampler draws transaction hashes from a fixture's ledgers and records
// which ledgers they came from. It reads each sequence at most once, so a ledger
// contributes at most corpusMaxHashesPerLedger hashes.
type txHashSampler struct {
	rng *rand.Rand

	// hashes is the pool.
	hashes [][32]byte

	// ledgers lists every ledger that contributed a hash, in sample order; read
	// holds every sequence already drawn, including ones with no stored ledger.
	ledgers []uint32
	read    map[uint32]struct{}
}

func newTxHashSampler(rng *rand.Rand) *txHashSampler {
	return &txHashSampler{rng: rng, read: map[uint32]struct{}{}}
}

// first returns the pool's first hash and the ledger it came from; sampleChunk
// appends to both slices in the same step. The pool must not be empty.
func (s *txHashSampler) first() ([32]byte, uint32) {
	return s.hashes[0], s.ledgers[0]
}

// sampleChunk reads randomly chosen ledgers of chunk c within the fixture's
// range and adds a random subset of each one's hashes to the pool, until the
// pool reaches corpusTargetHashes or the read cap runs out. A sequence with no
// stored ledger is skipped: a capped hot ingest leaves the chunk's tail empty.
// ExtractLedgerTxParts derives the hashes without a passphrase, which is what
// makes them usable to check the passphrase later.
func (s *txHashSampler) sampleChunk(view *query.ReadView, c chunk.ID, first, last uint32) error {
	lo := max(c.FirstLedger(), first)
	hi := min(c.LastLedger(), last)
	if lo > hi {
		return nil
	}
	reader, err := view.Ledgers(c)
	if err != nil {
		return fmt.Errorf("resolve ledgers of chunk %s: %w", c, err)
	}

	span := int(hi - lo + 1)
	for reads := 0; reads < corpusMaxLedgerReads && len(s.hashes) < corpusTargetHashes; reads++ {
		seq := lo + uint32(s.rng.IntN(span)) //nolint:gosec // span <= LedgersPerChunk
		if _, drawn := s.read[seq]; drawn {
			continue
		}
		s.read[seq] = struct{}{}
		// The ledger bytes are on loan inside the callback; the picked hashes are
		// value arrays and outlive the loan.
		var picked [][32]byte
		err := reader.WithLedger(seq, func(raw []byte) error {
			parts, err := sdkingest.ExtractLedgerTxParts(xdr.LedgerCloseMetaView(raw))
			if err != nil {
				return fmt.Errorf("extract tx parts of ledger %d: %w", seq, err)
			}
			picked = sampleHashesFromLedger(s.rng, parts)
			return nil
		})
		if errors.Is(err, stores.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read ledger %d: %w", seq, err)
		}
		if len(picked) == 0 {
			continue
		}
		s.hashes = append(s.hashes, picked...)
		s.ledgers = append(s.ledgers, seq)
	}
	return nil
}

// logCoverage reports how many hashes the pool holds, how many ledgers they came
// from, and the range those span. A pool from one ledger is legitimate on a
// dataset that holds one, and warned: every found lookup then reads that ledger.
func (s *txHashSampler) logCoverage(logger *supportlog.Entry, missFraction float64) {
	logger.Infof("txhash corpus: %d hashes over %d ledgers spanning %d..%d, miss fraction %.2f",
		len(s.hashes), len(s.ledgers), slices.Min(s.ledgers), slices.Max(s.ledgers), missFraction)
	if len(s.ledgers) == 1 {
		logger.Warnf("txhash corpus came from ledger %d alone: every found lookup reads that "+
			"one ledger, so this run's found rows measure a warm read", s.ledgers[0])
	}
}

// sampleHashesFromLedger returns at most corpusMaxHashesPerLedger of the
// transactions' hashes, drawn uniformly without replacement.
func sampleHashesFromLedger(rng *rand.Rand, parts []sdkingest.LedgerTxParts) [][32]byte {
	take := min(len(parts), corpusMaxHashesPerLedger)
	out := make([][32]byte, 0, take)
	for _, i := range rng.Perm(len(parts))[:take] {
		out = append(out, parts[i].Hash)
	}
	return out
}

// verifySampledHashResolves checks a hash the sampler read out of ledger seq
// two ways, in the order that tells the operator what to fix. Envelope pairing
// against that one ledger comes first: it is the only step the passphrase
// feeds, so a failure names --network-passphrase and nothing else. The served
// by-hash probe comes second: it can fail for reasons the passphrase has no
// part in (an index never committed, an .idx that will not open), so its error
// is reported as the probe failure it is.
func verifySampledHashResolves(
	ctx context.Context, view *query.ReadView, f *queryFixture, hash [32]byte, seq uint32,
) error {
	if err := verifyEnvelopePairing(view, f.Passphrase, hash, seq); err != nil {
		return err
	}
	reader := adapters.NewTransactionReader(f.Passphrase, nil)
	if _, err := reader.GetTransaction(query.WithView(ctx, view), xdr.Hash(hash)); err != nil {
		return fmt.Errorf("probe of a known transaction hash failed: %w "+
			"(the fixture's tx-hash index may be missing or unreadable)", err)
	}
	return nil
}

// verifyEnvelopePairing re-reads ledger seq and materializes hash out of it
// with the configured passphrase. The hash came from that ledger's own results
// and meta, so the pairing is the only thing that can go wrong, and a failure
// reports the passphrase.
func verifyEnvelopePairing(view *query.ReadView, passphrase string, hash [32]byte, seq uint32) error {
	reader, err := view.Ledgers(chunk.IDFromLedger(seq))
	if err != nil {
		return fmt.Errorf("resolve ledgers of the sampled ledger %d: %w", seq, err)
	}
	var found bool
	var pairErr error
	err = reader.WithLedger(seq, func(raw []byte) error {
		_, found, pairErr = sdkingest.LedgerTransactionViewByHash(xdr.LedgerCloseMetaView(raw), hash, passphrase)
		return nil
	})
	if err != nil {
		return fmt.Errorf("re-read the sampled ledger %d: %w", seq, err)
	}
	if err := pairErr; err != nil {
		return fmt.Errorf(
			"transaction %x does not pair with an envelope in ledger %d, the ledger it was sampled from: "+
				"--network-passphrase=%q is wrong for this dataset (%w)", hash, seq, passphrase, err)
	}
	if !found {
		return fmt.Errorf(
			"transaction %x is not in ledger %d, the ledger it was sampled from: "+
				"--network-passphrase=%q is wrong for this dataset", hash, seq, passphrase)
	}
	return nil
}

// eventFilterCorpus is the events benchmark's work: a handful of filter sets,
// one of which is unfiltered. An unfiltered page streams events in order while a
// filtered one intersects term postings first; rotating a fixed set keeps both
// shapes in the number without turning it into a selectivity sweep.
type eventFilterCorpus struct {
	sets [][]event.Filter
}

// pick returns one filter set to page with. A nil set is the unfiltered read.
func (c *eventFilterCorpus) pick(rng *rand.Rand) []event.Filter {
	return c.sets[rng.IntN(len(c.sets))]
}

// buildEventFilterCorpus derives filter sets from the events stored in the
// fixture's chunks: the busiest contracts, and the busiest contract narrowed by
// its most common first topic. The unfiltered set is always included.
func buildEventFilterCorpus(
	ctx context.Context, logger *supportlog.Entry, f *queryFixture,
) (*eventFilterCorpus, error) {
	view, err := f.view()
	if err != nil {
		return nil, fmt.Errorf("acquire read view: %w", err)
	}
	defer view.Release()

	contracts, topics, err := scanEventTerms(ctx, view, f.Chunks)
	if err != nil {
		return nil, err
	}
	sets := [][]event.Filter{nil} // the unfiltered read
	for _, cid := range contracts {
		if len(sets) >= eventFilterSets-1 {
			break
		}
		sets = append(sets, []event.Filter{{ContractID: cid}})
	}
	if len(contracts) > 0 && len(topics) > 0 {
		f := event.Filter{ContractID: contracts[0]}
		f.Topics[0] = topics[0]
		sets = append(sets, []event.Filter{f})
	}
	if err := validateFilterSets(sets); err != nil {
		return nil, err
	}
	logger.Infof("events corpus: %d filter sets (%d contracts, %d topic values seen)",
		len(sets), len(contracts), len(topics))
	return &eventFilterCorpus{sets: sets}, nil
}

// validateFilterSets runs the engine's own filter check over every set, so a
// malformed filter fails at build time rather than on every measured page.
func validateFilterSets(sets [][]event.Filter) error {
	for _, set := range sets {
		if err := event.ValidateFilters(set); err != nil {
			return fmt.Errorf("derived event filter is invalid: %w", err)
		}
	}
	return nil
}

// scanEventTerms reads up to eventScanCap stored events across the chunks and
// returns the contract IDs and first-topic values by descending frequency, as
// the store's canonical term bytes, so a filter built from them keys the same
// terms the events were indexed under.
func scanEventTerms(
	ctx context.Context, view *query.ReadView, chunks []chunk.ID,
) ([][]byte, [][]byte, error) {
	contractCounts := map[string]int{}
	topicCounts := map[string]int{}
	scanned := 0

	for _, c := range chunks {
		reader, rerr := view.Events(c)
		if rerr != nil {
			// A chunk with no events store still serves the unfiltered set.
			continue
		}
		for payload, perr := range reader.All(ctx) {
			if perr != nil {
				return nil, nil, fmt.Errorf("scan events of chunk %s: %w", c, perr)
			}
			cid, topic0, terr := eventTerms(payload.ContractEventBytes)
			if terr != nil {
				return nil, nil, fmt.Errorf("read event terms in chunk %s: %w", c, terr)
			}
			if cid != nil {
				contractCounts[string(cid)]++
			}
			if topic0 != nil {
				topicCounts[string(topic0)]++
			}
			scanned++
			if scanned >= eventScanCap {
				break
			}
		}
		if scanned >= eventScanCap {
			break
		}
	}
	return byDescendingCount(contractCounts), byDescendingCount(topicCounts), nil
}

// eventTerms reads one stored event's contract ID and first topic through the
// XDR views, as the events indexer derives its terms. Either is nil when absent.
func eventTerms(eventBytes []byte) ([]byte, []byte, error) {
	var cid []byte
	ev := xdr.ContractEventView(eventBytes)
	cidOpt, err := ev.ContractId()
	if err != nil {
		return nil, nil, fmt.Errorf("view ContractId: %w", err)
	}
	cidView, present, err := cidOpt.Unwrap()
	if err != nil {
		return nil, nil, fmt.Errorf("view ContractId unwrap: %w", err)
	}
	if present {
		v, verr := cidView.Value()
		if verr != nil {
			return nil, nil, fmt.Errorf("view ContractId value: %w", verr)
		}
		cid = slices.Clone(v[:])
	}

	body, err := ev.Body()
	if err != nil {
		return nil, nil, fmt.Errorf("view Body: %w", err)
	}
	v, err := body.V()
	if err != nil {
		return nil, nil, fmt.Errorf("view Body.V: %w", err)
	}
	if v != 0 {
		// Only body version 0 carries topics.
		return cid, nil, nil
	}
	v0, err := body.V0()
	if err != nil {
		return nil, nil, fmt.Errorf("view Body.V0: %w", err)
	}
	topicList, err := v0.Topics()
	if err != nil {
		return nil, nil, fmt.Errorf("view Body.V0.Topics: %w", err)
	}
	all, err := topicList.All()
	if err != nil {
		return nil, nil, fmt.Errorf("view Body.V0.Topics.All: %w", err)
	}
	if len(all) == 0 {
		return cid, nil, nil
	}
	// All trims each element to size, so the view's bytes are the topic's raw
	// XDR, the form the index keys on.
	return cid, slices.Clone([]byte(all[0])), nil
}

// byDescendingCount returns the keys of counts, most frequent first, ties broken
// by value so a run is reproducible.
func byDescendingCount(counts map[string]int) [][]byte {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return cmp.Compare(a, b)
	})
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = []byte(k)
	}
	return out
}
