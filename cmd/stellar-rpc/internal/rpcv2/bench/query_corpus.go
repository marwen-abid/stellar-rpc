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

// The corpora the request bodies draw their work from. Built once, before
// measurement; never timed.

// The tx-hash sampler reads randomly chosen ledgers, takes at most
// corpusMaxHashesPerLedger hashes from each, and stops once the pool holds
// corpusTargetHashes or corpusMaxLedgerReads reads are spent.
const (
	corpusTargetHashes       = 512
	corpusMaxLedgerReads     = 512
	corpusMaxHashesPerLedger = 16
)

// eventScanCap bounds how many stored events the filter builder reads.
const eventScanCap = 20_000

// eventFilterSets is how many filter sets the events corpus offers, the
// unfiltered one included.
const eventFilterSets = 4

// errNoTransactions means the sampled ledgers carried no transactions.
var errNoTransactions = errors.New("the sampled ledgers carry no transactions")

// txHashCorpus is the by-hash benchmark's work: hashes that landed in the
// fixture's ledger range, and the fraction of lookups that ask for a hash that
// never landed. A hit stops at the first index that knows the hash; a miss
// probes every hot index and then every cold window index.
type txHashCorpus struct {
	hashes       [][32]byte
	missFraction float64
}

// pick returns one hash to look up and whether it is expected to be found. A
// miss is 32 random bytes.
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
// and checks that one of them resolves under the passphrase.
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

// txHashSampler draws transaction hashes from a fixture's ledgers. It reads
// each sequence at most once.
type txHashSampler struct {
	rng *rand.Rand

	// hashes is the pool.
	hashes [][32]byte

	// ledgers lists every ledger that contributed a hash, in sample order; read
	// holds every sequence drawn, including ones with no stored ledger.
	ledgers []uint32
	read    map[uint32]struct{}
}

func newTxHashSampler(rng *rand.Rand) *txHashSampler {
	return &txHashSampler{rng: rng, read: map[uint32]struct{}{}}
}

// first returns the pool's first hash and the ledger it came from. The pool
// must not be empty.
func (s *txHashSampler) first() ([32]byte, uint32) {
	return s.hashes[0], s.ledgers[0]
}

// sampleChunk reads randomly chosen ledgers of chunk c within [first, last] and
// adds a random subset of each one's hashes to the pool. A sequence with no
// stored ledger is skipped: a capped hot ingest leaves the chunk's tail empty.
// ExtractLedgerTxParts derives the hashes without a passphrase.
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
		// The ledger bytes are on loan inside the callback; the hashes are value
		// arrays.
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

// logCoverage logs the pool's size and ledger span, and warns when one ledger
// supplied every hash.
func (s *txHashSampler) logCoverage(logger *supportlog.Entry, missFraction float64) {
	logger.Infof("txhash corpus: %d hashes over %d ledgers spanning %d..%d, miss fraction %.2f",
		len(s.hashes), len(s.ledgers), slices.Min(s.ledgers), slices.Max(s.ledgers), missFraction)
	if len(s.ledgers) == 1 {
		logger.Warnf("txhash corpus came from ledger %d alone: every found lookup reads that "+
			"one ledger, so this run's found rows measure a warm read", s.ledgers[0])
	}
}

// sampleHashesFromLedger returns at most corpusMaxHashesPerLedger hashes, drawn
// without replacement.
func sampleHashesFromLedger(rng *rand.Rand, parts []sdkingest.LedgerTxParts) [][32]byte {
	take := min(len(parts), corpusMaxHashesPerLedger)
	out := make([][32]byte, 0, take)
	for _, i := range rng.Perm(len(parts))[:take] {
		out = append(out, parts[i].Hash)
	}
	return out
}

// verifySampledHashResolves checks a sampled hash two ways: envelope pairing
// against its own ledger, the only step the passphrase feeds, then the served
// by-hash probe.
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

// verifyEnvelopePairing re-reads ledger seq and pairs hash with its envelope
// under passphrase. The hash came from that ledger, so a failure means the
// passphrase is wrong for the dataset.
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

// eventFilterCorpus is the events benchmark's work: filter sets, one of them
// unfiltered.
type eventFilterCorpus struct {
	sets [][]event.Filter
}

// pick returns one filter set. nil is the unfiltered read.
func (c *eventFilterCorpus) pick(rng *rand.Rand) []event.Filter {
	return c.sets[rng.IntN(len(c.sets))]
}

// buildEventFilterCorpus derives filter sets from the stored events: the
// unfiltered set, the busiest contracts, and the busiest contract narrowed by
// its most common first topic.
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

// validateFilterSets runs event.ValidateFilters over every set.
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
// the store's canonical term bytes.
func scanEventTerms(
	ctx context.Context, view *query.ReadView, chunks []chunk.ID,
) ([][]byte, [][]byte, error) {
	contractCounts := map[string]int{}
	topicCounts := map[string]int{}
	scanned := 0

	for _, c := range chunks {
		reader, rerr := view.Events(c)
		if rerr != nil {
			// A chunk may have no events store.
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
// XDR views, as the events indexer does. Either is nil when absent.
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
	// Each element of All is the topic's raw XDR, the form the index keys on.
	return cid, slices.Clone([]byte(all[0])), nil
}

// byDescendingCount returns the keys of counts, most frequent first, ties
// broken by value.
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
