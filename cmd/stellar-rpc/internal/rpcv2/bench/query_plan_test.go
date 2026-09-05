package bench

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/network"
)

func validQueryFlags() queryFlags {
	return queryFlags{
		types:        queryTypeLedgers,
		targetRPS:    "1",
		duration:     time.Second,
		ledgersSpan:  defaultLedgersSpan,
		txPageSpan:   defaultTxPageSpan,
		txPageLimit:  defaultTxPageLimit,
		eventsLimit:  defaultEventsLimit,
		missFraction: defaultMissFraction,
		passphrase:   network.PublicNetworkPassphrase,
		seed:         defaultSeed,
	}
}

// A span above maxReadSpan could wrap start+span-1 below the start.
func TestPlanBoundsReadSpans(t *testing.T) {
	f := validQueryFlags()
	f.ledgersSpan = maxReadSpan
	f.txPageSpan = maxReadSpan
	_, err := f.plan()
	require.NoError(t, err)

	f = validQueryFlags()
	f.ledgersSpan = maxReadSpan + 1
	_, err = f.plan()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ledgers-span")

	f = validQueryFlags()
	f.txPageSpan = maxReadSpan + 1
	_, err = f.plan()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--txpage-span")
}
