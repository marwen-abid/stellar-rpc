package bench

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/catalog"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/geometry"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/stores/hotchunk"
)

var errDatasetExists = errors.New("dataset already has a catalog")

// checkNoCatalog fails when layout already holds a catalog.
func checkNoCatalog(layout geometry.Layout) error {
	path := layout.CatalogPath()
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s (use a new dataset root)", errDatasetExists, path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return err
	}
}

// createDatasetCatalog opens the catalog at layout.CatalogPath() and pins the
// earliest ledger to start.FirstLedger(). The caller closes the catalog.
func createDatasetCatalog(
	layout geometry.Layout, start chunk.ID, logger *supportlog.Entry,
) (*catalog.Catalog, error) {
	txLayout, err := geometry.NewTxHashIndexLayout(geometry.ChunksPerTxhashIndex)
	if err != nil {
		return nil, err
	}
	cat, err := catalog.Open(layout.CatalogPath(), layout, txLayout, logger)
	if err != nil {
		return nil, fmt.Errorf("open dataset catalog: %w", err)
	}
	if err := cat.PinEarliestLedger(start.FirstLedger()); err != nil {
		_ = cat.Close()
		return nil, fmt.Errorf("pin earliest ledger: %w", err)
	}
	return cat, nil
}

// createFrontierChunk creates an empty, ready hot chunk database for c through
// the catalog's hot create bracket.
func createFrontierChunk(cat *catalog.Catalog, c chunk.ID, logger *supportlog.Entry) error {
	if err := cat.BeginHotCreate(c); err != nil {
		return err
	}
	db, err := hotchunk.Open(cat.Layout().HotChunkPath(c), c, logger)
	if err != nil {
		return fmt.Errorf("create frontier hot DB chunk %s: %w", c, err)
	}
	finishErr := cat.FinishHotCreate(c)
	closeErr := db.Close()
	if finishErr != nil {
		return finishErr
	}
	if closeErr != nil {
		return fmt.Errorf("close frontier hot DB chunk %s: %w", c, closeErr)
	}
	return nil
}
