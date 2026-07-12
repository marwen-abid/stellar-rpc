package bench

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/fullhistory/storage/stores/ledger"
)

// eventEvery: every eventEvery-th ledger of a fixture chunk carries one
// transaction with one contract event; the rest are zero-tx ledgers.
const eventEvery = 100

// writeFixturePack materializes chunkID's synthetic source ledger pack under
// the ledgers tree root packDir (the tree --pack-dir points at): numLedgers
// ledgers from the chunk's first sequence, every eventEvery-th one carrying a
// single-event transaction, the rest zero-tx. The same (chunk, numLedgers,
// seed) always produces the same ledgers — each transaction's source account
// is derived from (seed, ledger seq) — which is what lets CI benchmark two
// builds against one defined dataset. Returns the number of tx/event-bearing
// ledgers written.
func writeFixturePack(packDir string, chunkID chunk.ID, numLedgers uint32, seed uint64) (int, error) {
	packPath := geometry.LedgerPackPath(packDir, chunkID)
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir for %s: %w", packPath, err)
	}
	w, err := ledger.NewColdWriter(packPath, chunkID.FirstLedger(), ledger.ColdWriterOptions{})
	if err != nil {
		return 0, fmt.Errorf("open pack writer %s: %w", packPath, err)
	}
	defer func() { _ = w.Close() }()

	txLedgers := 0
	first := chunkID.FirstLedger()
	for seq := first; seq < first+numLedgers; seq++ {
		var raw []byte
		if (seq-first)%eventEvery == 0 {
			raw, err = eventLCMBytes(seq, seed)
			txLedgers++
		} else {
			raw, err = zeroTxLCMBytes(seq)
		}
		if err != nil {
			return 0, fmt.Errorf("build ledger %d: %w", seq, err)
		}
		if err := w.AppendLedger(seq, raw); err != nil {
			return 0, fmt.Errorf("append ledger %d: %w", seq, err)
		}
	}
	if err := w.Commit(); err != nil {
		return 0, fmt.Errorf("commit %s: %w", packPath, err)
	}
	return txLedgers, nil
}

// zeroTxLCMBytes returns the marshaled bytes of a minimal, zero-transaction
// LedgerCloseMeta (V2) for ledger seq.
func zeroTxLCMBytes(seq uint32) ([]byte, error) {
	lcm := xdr.LedgerCloseMeta{
		V: 2,
		V2: &xdr.LedgerCloseMetaV2{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(0)},
					LedgerSeq: xdr.Uint32(seq),
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{Phases: nil},
			},
			TxProcessing: nil,
		},
	}
	return lcm.MarshalBinary()
}

// eventLCMBytes returns the marshaled bytes of a single-transaction
// LedgerCloseMeta (V2) for ledger seq whose transaction carries one
// operation-level contract event. The transaction's source account is derived
// deterministically from (seed, seq), so every event ledger has a distinct
// but reproducible pubnet transaction hash. (These builders mirror the
// fhtest test fixtures, which stay test-only by design and draw random
// accounts instead.)
func eventLCMBytes(seq uint32, seed uint64) ([]byte, error) {
	rawSeed := sha256.Sum256(fmt.Appendf(nil, "bench-fixture:%d:%d", seed, seq))
	kp, err := keypair.FromRawSeed(rawSeed)
	if err != nil {
		return nil, fmt.Errorf("derive keypair: %w", err)
	}

	var contractID xdr.ContractId
	contractID[0] = 0xab
	sym := xdr.ScSymbol("fhtest")
	ev := xdr.ContractEvent{
		ContractId: &contractID,
		Type:       xdr.ContractEventTypeContract,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: []xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}},
				Data:   xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			},
		},
	}
	meta := xdr.TransactionMeta{
		V:  4,
		V4: &xdr.TransactionMetaV4{Operations: []xdr.OperationMetaV2{{Events: []xdr.ContractEvent{ev}}}},
	}

	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{
			Tx: xdr.Transaction{
				SourceAccount: xdr.MustMuxedAddress(kp.Address()),
				Ext: xdr.TransactionExt{
					V:           1,
					SorobanData: &xdr.SorobanTransactionData{},
				},
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, network.PublicNetworkPassphrase)
	if err != nil {
		return nil, fmt.Errorf("hash transaction: %w", err)
	}

	opResults := []xdr.OperationResult{}
	comp := []xdr.TxSetComponent{{
		Type: xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
		TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{
			Txs: []xdr.TransactionEnvelope{envelope},
		},
	}}
	lcm := xdr.LedgerCloseMeta{
		V: 2,
		V2: &xdr.LedgerCloseMetaV2{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(0)},
					LedgerSeq: xdr.Uint32(seq),
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{Phases: []xdr.TransactionPhase{{V: 0, V0Components: &comp}}},
			},
			TxProcessing: []xdr.TransactionResultMetaV1{{
				TxApplyProcessing: meta,
				Result: xdr.TransactionResultPair{
					TransactionHash: hash,
					Result: xdr.TransactionResult{
						FeeCharged: 100,
						Result: xdr.TransactionResultResult{
							Code:    xdr.TransactionResultCodeTxSuccess,
							Results: &opResults,
						},
					},
				},
			}},
		},
	}
	return lcm.MarshalBinary()
}
