package bench

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/network"
)

func validQueryFlags() queryFlags {
	return queryFlags{
		types:            queryTypeLedgers,
		targetRPS:        "1",
		duration:         time.Second,
		ledgersSpan:      defaultLedgersSpan,
		txPageSpan:       defaultTxPageSpan,
		txPageLimit:      defaultTxPageLimit,
		eventsLimit:      defaultEventsLimit,
		missFraction:     defaultMissFraction,
		txHashCorpusSize: corpusTargetHashes,
		passphrase:       network.PublicNetworkPassphrase,
		seed:             defaultSeed,
	}
}

func TestQueryPlanCorpusAndMissFraction(t *testing.T) {
	for _, size := range []int{-1, 0, 1, corpusTargetHashes, maxTxHashCorpusSize, maxTxHashCorpusSize + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := validQueryFlags()
			f.txHashCorpusSize = size
			p, err := f.plan()
			if size < 1 || size > maxTxHashCorpusSize {
				require.ErrorContains(t, err, "--txhash-corpus-size")
			} else {
				require.NoError(t, err)
				assert.Equal(t, size, p.TxHashCorpusSize)
			}
		})
	}
	for _, fraction := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 1.1} {
		f := validQueryFlags()
		f.missFraction = fraction
		_, err := f.plan()
		require.ErrorContains(t, err, "--miss-fraction")
	}
}

func TestQueryCacheControls(t *testing.T) {
	for _, cmd := range NewQueryCommand().Commands() {
		t.Run(cmd.Name(), func(t *testing.T) {
			warmup := cmd.Flags().Lookup("warmup")
			require.NotNil(t, warmup)
			want := "0"
			if cmd.Name() == "hot" {
				want = "20"
				assert.Nil(t, cmd.Flags().Lookup("evict-page-cache"))
			} else {
				eviction := cmd.Flags().Lookup("evict-page-cache")
				require.NotNil(t, eviction)
				assert.Equal(t, "true", eviction.DefValue)
				assert.Equal(t, "request OS page-cache eviction before each leg (Linux only)", eviction.Usage)
			}
			assert.Equal(t, want, warmup.DefValue)
			assert.Nil(t, cmd.Flags().Lookup("cache-scenario"))
			assert.Equal(t, "512", cmd.Flags().Lookup("txhash-corpus-size").DefValue)
		})
	}
	for _, tc := range []struct {
		warmup int
		evict  bool
		want   string
	}{
		{0, false, "existing-cache"},
		{0, true, "cold-start"},
		{20, false, "warm-run"},
		{20, true, "warm-run"},
	} {
		p := queryPlan{Warmup: tc.warmup, Evict: tc.evict}
		assert.Equal(t, tc.want, p.cacheScenario())
	}
	assert.Equal(t, "off", evictionState(false))
	want := "unsupported-on-this-platform"
	if evictSupported {
		want = "requested"
	}
	assert.Equal(t, want, evictionState(true))
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
