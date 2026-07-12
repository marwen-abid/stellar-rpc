package bench

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sort"

	sdkingest "github.com/stellar/go-stellar-sdk/ingest"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/eventstore"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/txhash"
)

// The events corpus keeps the chunk's 3 busiest contracts as anchors plus up
// to 12 of their most frequent (topic position, value) pairs — a 15-term
// universe, wide enough to draw the worst-case getEvents request shape
// (many filters, every indexed field constrained).
const (
	corpusContractTerms = 3
	corpusTopicTerms    = 12
)

// kBuckets are the filter counts a drawn events query may use; each
// iteration picks one uniformly (clamped to the corpus size), so a sweep
// cell mixes cheap single-filter queries with wide multi-filter ones the way
// real traffic does.
//
//nolint:gochecknoglobals // fixed workload shape, read-only
var kBuckets = []int{1, 2, 3, 5, 8, 12, 15}

// eventTerm is one indexed (field, value) pair the corpus can constrain a
// filter with: category 0 is the contract ID, categories 1..4 are topic
// positions 0..3.
type eventTerm struct {
	category int
	value    []byte
}

// eventCorpus draws realistic filter sets for one chunk, built from the
// chunk's own busiest contracts and topic values rather than hand-picked
// constants.
type eventCorpus struct {
	terms []eventTerm
}

// buildEventCorpus scans r's events (twice — the second pass only counts the
// anchor contracts' topics) and returns the chunk's term universe. Payloads
// yielded by All are borrowed, so every kept term value is copied.
func buildEventCorpus(ctx context.Context, r eventstore.Reader) (*eventCorpus, error) {
	contractEvents := make(map[[32]byte]int)
	for p, err := range r.All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("events corpus scan: %w", err)
		}
		cid, _, ferr := eventFieldsOf(p.ContractEventBytes)
		if ferr != nil {
			return nil, ferr
		}
		if cid != nil {
			contractEvents[[32]byte(cid)]++
		}
	}
	if len(contractEvents) == 0 {
		return nil, errors.New("events corpus: chunk has no contract events to derive query terms from")
	}
	anchors := topContracts(contractEvents, corpusContractTerms)
	topicCounts, err := countAnchorTopics(ctx, r, anchors)
	if err != nil {
		return nil, err
	}

	terms := make([]eventTerm, 0, corpusContractTerms+corpusTopicTerms)
	for _, a := range anchors {
		terms = append(terms, eventTerm{category: 0, value: bytes.Clone(a[:])})
	}
	for _, tk := range topTopics(topicCounts, corpusTopicTerms) {
		terms = append(terms, eventTerm{category: tk.pos + 1, value: []byte(tk.val)})
	}
	return &eventCorpus{terms: terms}, nil
}

// topicKey identifies one (topic position, raw value) pair; the raw bytes
// are held as a string so the pair can key a map.
type topicKey struct {
	pos int
	val string
}

// countAnchorTopics is the corpus's second scan: a histogram of the anchor
// contracts' (topic position, value) pairs, so the topic terms actually
// co-occur with the contract anchors and combined filters can match events.
func countAnchorTopics(ctx context.Context, r eventstore.Reader, anchors [][32]byte) (map[topicKey]int, error) {
	anchorSet := make(map[[32]byte]bool, len(anchors))
	for _, a := range anchors {
		anchorSet[a] = true
	}
	counts := make(map[topicKey]int)
	for p, err := range r.All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("events corpus topic scan: %w", err)
		}
		cid, topics, ferr := eventFieldsOf(p.ContractEventBytes)
		if ferr != nil {
			return nil, ferr
		}
		if cid == nil || !anchorSet[[32]byte(cid)] {
			continue
		}
		for pos, t := range topics {
			if len(t) > 0 {
				counts[topicKey{pos: pos, val: string(t)}]++
			}
		}
	}
	return counts, nil
}

// topContracts returns the n highest-count contract IDs, ties broken by ID
// bytes so corpus construction is deterministic.
func topContracts(counts map[[32]byte]int, n int) [][32]byte {
	ids := slices.SortedFunc(maps.Keys(counts), func(a, b [32]byte) int {
		if c := cmp.Compare(counts[b], counts[a]); c != 0 {
			return c
		}
		return bytes.Compare(a[:], b[:])
	})
	return ids[:min(n, len(ids))]
}

// topTopics returns the n highest-count (position, value) pairs, ties broken
// by position then value bytes for determinism.
func topTopics(counts map[topicKey]int, n int) []topicKey {
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b topicKey) int {
		if c := cmp.Compare(counts[b], counts[a]); c != 0 {
			return c
		}
		if c := cmp.Compare(a.pos, b.pos); c != 0 {
			return c
		}
		return cmp.Compare(a.val, b.val)
	})
	return keys[:min(n, len(keys))]
}

// eventFieldsOf pulls one event's indexable raw fields out of its XDR via
// the SDK view (no decode): the 32-byte contract ID (nil when absent) and
// each topic position's raw ScVal bytes. The topic slices alias raw — copy
// to retain.
func eventFieldsOf(raw []byte) ([]byte, [][]byte, error) {
	ev := xdr.ContractEventView(raw)
	var cid []byte
	cidOpt, err := ev.ContractId()
	if err != nil {
		return nil, nil, fmt.Errorf("events corpus: ContractId: %w", err)
	}
	cidView, present, err := cidOpt.Unwrap()
	if err != nil {
		return nil, nil, fmt.Errorf("events corpus: ContractId unwrap: %w", err)
	}
	if present {
		v, verr := cidView.Value()
		if verr != nil {
			return nil, nil, fmt.Errorf("events corpus: ContractId value: %w", verr)
		}
		cid = v[:]
	}
	topics, err := topicRawBytes(ev)
	if err != nil {
		return nil, nil, err
	}
	return cid, topics, nil
}

// topicRawBytes walks the event body's topics once and returns each indexed
// position's raw bytes (aliasing the event buffer). Non-V0 bodies have no
// topics.
func topicRawBytes(ev xdr.ContractEventView) ([][]byte, error) {
	body, err := ev.Body()
	if err != nil {
		return nil, fmt.Errorf("events corpus: Body: %w", err)
	}
	bodyV, err := body.V()
	if err != nil {
		return nil, fmt.Errorf("events corpus: Body.V: %w", err)
	}
	if bodyV != 0 {
		return nil, nil
	}
	v0, err := body.V0()
	if err != nil {
		return nil, fmt.Errorf("events corpus: Body.V0: %w", err)
	}
	topicsArr, err := v0.Topics()
	if err != nil {
		return nil, fmt.Errorf("events corpus: Topics: %w", err)
	}
	var out [][]byte
	for topic, ierr := range topicsArr.Iter() {
		if ierr != nil {
			return nil, fmt.Errorf("events corpus: topic iter: %w", ierr)
		}
		if len(out) >= protocol.MaxTopicCount {
			break
		}
		rawBytes, rerr := topic.Raw()
		if rerr != nil {
			return nil, fmt.Errorf("events corpus: topic raw: %w", rerr)
		}
		out = append(out, rawBytes)
	}
	return out, nil
}

// draw materializes one query's filter set: a filter count from kBuckets
// (clamped to the term universe), the whole universe shuffled and dealt
// round-robin across the filters, a taken slot probing forward to the next
// filter. Surplus terms drop, and filters left with no constraint are
// compacted out so none accidentally becomes match-all.
func (c *eventCorpus) draw(rng *rand.Rand) []eventstore.Filter {
	k := min(kBuckets[rng.IntN(len(kBuckets))], len(c.terms))
	filters := make([]eventstore.Filter, k)
	for i, idx := range rng.Perm(len(c.terms)) {
		term := c.terms[idx]
		for try := range k {
			slot := filterSlot(&filters[(i+try)%k], term.category)
			if len(*slot) == 0 {
				*slot = term.value
				break
			}
		}
	}
	out := filters[:0]
	for i := range filters {
		if !filterIsEmpty(&filters[i]) {
			out = append(out, filters[i])
		}
	}
	return out
}

// filterSlot returns the filter field a term category constrains.
func filterSlot(f *eventstore.Filter, category int) *[]byte {
	if category == 0 {
		return &f.ContractID
	}
	return &f.Topics[category-1]
}

func filterIsEmpty(f *eventstore.Filter) bool {
	if len(f.ContractID) > 0 {
		return false
	}
	for _, t := range f.Topics {
		if len(t) > 0 {
			return false
		}
	}
	return true
}

// hashPool is the tx-hash query corpus: hashes sampled from the benchmarked
// store's own ledgers, each tagged with the chunk it came from so the cold
// driver can evict that chunk's ledger pack before the lookup.
type hashPool struct {
	hashes []poolHash
}

type poolHash struct {
	hash  [32]byte
	chunk chunk.ID
}

// samplePoolHashes draws up to sampleLedgers distinct random ledgers of
// [first, last] from src and adds every transaction hash they carry (SDK
// view extraction, no decode) to pool.
func samplePoolHashes(
	src txhash.LedgerSource, first, last uint32, sampleLedgers int, rng *rand.Rand, pool *hashPool,
) error {
	span := int(last - first + 1)
	order := rng.Perm(span)
	for _, off := range order[:min(sampleLedgers, span)] {
		seq := first + uint32(off) //nolint:gosec // off < span, which fits uint32
		raw, err := src.GetLedgerRaw(seq)
		if err != nil {
			return fmt.Errorf("txhash corpus: fetch seq %d: %w", seq, err)
		}
		hashes, err := sdkingest.ExtractTxHashes(xdr.LedgerCloseMetaView(raw))
		if err != nil {
			return fmt.Errorf("txhash corpus: extract seq %d: %w", seq, err)
		}
		for _, h := range hashes {
			pool.hashes = append(pool.hashes, poolHash{hash: h, chunk: chunk.IDFromLedger(seq)})
		}
	}
	return nil
}

// pageCursors holds one chunk's per-ledger transaction counts, so cursor
// draws can guarantee a full page of transactions ahead of every start
// position.
type pageCursors struct {
	first  uint32
	starts []int // starts[i] = transactions before ledger first+i
	total  int
}

// buildPageCursors preflights [first, last]: one untimed pass over every
// ledger, counting its transactions via the SDK view extractor.
func buildPageCursors(src txhash.LedgerSource, first, last uint32) (*pageCursors, error) {
	span := int(last - first + 1)
	starts := make([]int, span)
	total := 0
	for i := range span {
		starts[i] = total
		seq := first + uint32(i)
		raw, err := src.GetLedgerRaw(seq)
		if err != nil {
			return nil, fmt.Errorf("txpage preflight: fetch seq %d: %w", seq, err)
		}
		hashes, err := sdkingest.ExtractTxHashes(xdr.LedgerCloseMetaView(raw))
		if err != nil {
			return nil, fmt.Errorf("txpage preflight: extract seq %d: %w", seq, err)
		}
		total += len(hashes)
	}
	return &pageCursors{first: first, starts: starts, total: total}, nil
}

// draw picks a uniformly random page start with at least pageSize
// transactions ahead of it: a global transaction position mapped back to
// (ledger seq, index within that ledger) through the prefix sums.
func (p *pageCursors) draw(rng *rand.Rand, pageSize int) (uint32, int) {
	pos := rng.IntN(p.total - pageSize + 1)
	i := sort.Search(len(p.starts), func(i int) bool { return p.starts[i] > pos }) - 1
	return p.first + uint32(i), pos - p.starts[i] //nolint:gosec // i < span, which fits uint32
}

// walkPage reads consecutive raw ledgers from src starting at seq — skipping
// the cursor's first skipTx transactions — until pageSize transactions have
// been covered, walking each fetched ledger's transactions through the SDK
// view extractor. This is the store-side cost of serving one transactions
// page as production ships it today; response materialization has no
// production home yet (the v2 read seam), so it is deliberately not
// simulated here.
func walkPage(src txhash.LedgerSource, seq uint32, skipTx, pageSize int) error {
	covered := -skipTx
	for covered < pageSize {
		raw, err := src.GetLedgerRaw(seq)
		if err != nil {
			return fmt.Errorf("txpage fetch seq %d: %w", seq, err)
		}
		hashes, err := sdkingest.ExtractTxHashes(xdr.LedgerCloseMetaView(raw))
		if err != nil {
			return fmt.Errorf("txpage walk seq %d: %w", seq, err)
		}
		covered += len(hashes)
		seq++
	}
	return nil
}
