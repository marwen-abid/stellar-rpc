package bench

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"

	sdkingest "github.com/stellar/go-stellar-sdk/ingest"
	supportlog "github.com/stellar/go-stellar-sdk/support/log"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/adapters"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/query"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/store"
)

// The four measured request bodies, one per query type. Each takes its own
// read view, reads through query.ReadView, and returns how many items came
// back.

// newQueryRequest builds one query type's measured request. Its corpus is
// built first, outside every timer.
func newQueryRequest(
	ctx context.Context, logger *supportlog.Entry, f *queryFixture, p queryPlan, qtype string,
) (queryRequest, error) {
	switch qtype {
	case queryTypeLedgers:
		return ledgersRequest(f, p), nil
	case queryTypeTxPage:
		return txPageRequest(f, p), nil
	case queryTypeTxHash:
		corpus, err := buildTxHashCorpus(ctx, logger, f, p.MissFraction, p.Seed, p.TxHashCorpusSize)
		if err != nil {
			return nil, err
		}
		if p.Extra != nil {
			p.Extra["txhashCorpusHashes"] = strconv.Itoa(len(corpus.hashes))
			p.Extra["txhashCorpusLedgers"] = strconv.Itoa(corpus.ledgerCount)
		}
		return txHashRequest(ctx, f, corpus), nil
	case queryTypeEvents:
		corpus, err := buildEventFilterCorpus(ctx, logger, f)
		if err != nil {
			return nil, err
		}
		return eventsRequest(ctx, f, p, corpus), nil
	default:
		// Unreachable: parseQueryTypes rejects anything else.
		return nil, fmt.Errorf("unknown query type %q", qtype)
	}
}

// ledgersRequest measures getLedgers' read: one ReadView.ScanLedgers over
// --ledgers-span ledgers from a random start in the fixture's range.
func ledgersRequest(f *queryFixture, p queryPlan) queryRequest {
	return func(rng *rand.Rand) (cellSample, error) {
		lo := f.pickStart(rng, p.LedgersSpan)
		hi := lo + p.LedgersSpan - 1
		return timed("", func() (int, error) {
			view, err := f.view()
			if err != nil {
				return 0, fmt.Errorf("acquire read view: %w", err)
			}
			defer view.Release()

			scan, err := view.ScanLedgers(lo, hi)
			if err != nil {
				return 0, fmt.Errorf("scan ledgers [%d, %d]: %w", lo, hi, err)
			}
			read := 0
			for entry, serr := range scan {
				if serr != nil {
					return 0, fmt.Errorf("scan ledgers [%d, %d]: %w", lo, hi, serr)
				}
				// Touch the borrowed bytes.
				if len(entry.Bytes) == 0 {
					return 0, fmt.Errorf("ledger %d decoded to zero bytes", entry.Seq)
				}
				read++
			}
			return read, nil
		})
	}
}

// txPageRequest measures getTransactions' read: walk --txpage-span ledgers and
// materialize each one's transactions, envelopes included, up to
// --txpage-limit. ScanLedgers lends its ledger bytes until the iterator steps;
// every byte field of a view aliases them.
func txPageRequest(f *queryFixture, p queryPlan) queryRequest {
	return func(rng *rand.Rand) (cellSample, error) {
		lo := f.pickStart(rng, p.TxPageSpan)
		hi := lo + p.TxPageSpan - 1
		return timed("", func() (int, error) {
			view, err := f.view()
			if err != nil {
				return 0, fmt.Errorf("acquire read view: %w", err)
			}
			defer view.Release()

			scan, err := view.ScanLedgers(lo, hi)
			if err != nil {
				return 0, fmt.Errorf("scan ledgers [%d, %d]: %w", lo, hi, err)
			}
			txs := 0
			for entry, serr := range scan {
				if serr != nil {
					return 0, fmt.Errorf("scan ledgers [%d, %d]: %w", lo, hi, serr)
				}
				remaining := p.TxPageLimit - txs
				if remaining <= 0 {
					break
				}
				views, verr := sdkingest.LedgerTransactionViewRange(
					xdr.LedgerCloseMetaView(entry.Bytes), 0, remaining, f.Passphrase)
				if verr != nil {
					return 0, fmt.Errorf("materialize transactions of ledger %d: %w", entry.Seq, verr)
				}
				txs += len(views)
			}
			return txs, nil
		})
	}
}

// txHashRequest measures getTransaction's read through
// adapters.TransactionReader: hot tx-hash indexes, then the cold window
// indexes, each candidate verified against its ledger. The reader is stateless;
// one serves every request.
func txHashRequest(ctx context.Context, f *queryFixture, corpus *txHashCorpus) queryRequest {
	reader := adapters.NewTransactionReader(f.Passphrase, nil)
	return func(rng *rand.Rand) (cellSample, error) {
		hash, wantFound := corpus.pick(rng)
		stage := txHashStageFound
		if !wantFound {
			stage = txHashStageMiss
		}
		return timed(stage, func() (int, error) {
			view, err := f.view()
			if err != nil {
				return 0, fmt.Errorf("acquire read view: %w", err)
			}
			defer view.Release()

			_, err = reader.GetTransaction(query.WithView(ctx, view), xdr.Hash(hash))
			found := err == nil
			if errors.Is(err, store.ErrNoTransaction) {
				err = nil
			}
			if err != nil {
				return 0, fmt.Errorf("look up transaction %x: %w", hash, err)
			}
			if found != wantFound {
				return 0, fmt.Errorf("transaction %x: found=%t, expected %t", hash, found, wantFound)
			}
			if found {
				return 1, nil
			}
			return 0, nil
		})
	}
}

// eventsRequest measures getEvents' read: one page of at most --events-limit
// events from a random start ledger to the end of the fixture's range, under a
// filter set from the corpus. An empty page is not an error.
func eventsRequest(
	ctx context.Context, f *queryFixture, p queryPlan, corpus *eventFilterCorpus,
) queryRequest {
	return func(rng *rand.Rand) (cellSample, error) {
		filters := corpus.pick(rng)
		lo := f.pickStart(rng, 1)
		hi := f.LastLedger
		cursor := query.EventCursor{Scope: query.EventScope{
			MinLedger: lo,
			MaxLedger: &hi,
			Dir:       query.Ascending,
			Filters:   filters,
		}}
		return timed("", func() (int, error) {
			view, err := f.view()
			if err != nil {
				return 0, fmt.Errorf("acquire read view: %w", err)
			}
			defer view.Release()

			page, err := view.QueryEvents(ctx, cursor, p.EventsLimit)
			if err != nil {
				return 0, fmt.Errorf("query events over [%d, %d]: %w", lo, hi, err)
			}
			return len(page.Events), nil
		})
	}
}

// pickStart returns a random first ledger for a span-long read inside the
// fixture's range. A span wider than the range returns the range's start.
func (f *queryFixture) pickStart(rng *rand.Rand, span uint32) uint32 {
	room := f.LastLedger - f.FirstLedger + 1
	if span >= room {
		return f.FirstLedger
	}
	return f.FirstLedger + uint32(rng.IntN(int(room-span+1))) //nolint:gosec // room fits a chunk range
}
