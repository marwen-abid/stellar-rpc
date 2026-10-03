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
		notFoundFraction: defaultNotFoundFraction,
		txHashPoolSize:   poolTargetHashes,
		passphrase:       network.PublicNetworkPassphrase,
		seed:             defaultSeed,
	}
}

func TestParseTargetRPSRateBounds(t *testing.T) {
	got, err := parseTargetRPS("0.0001, 2," + strconv.Itoa(maxTargetRPS))
	require.NoError(t, err)
	assert.Equal(t, []float64{0.0001, 2, maxTargetRPS}, got)
	for _, bad := range []string{"0", "-1", "NaN", "Inf"} {
		_, err = parseTargetRPS("1," + bad)
		require.ErrorContains(t, err, "--target-rps rates must be positive and finite", bad)
	}
	_, err = parseTargetRPS("1," + strconv.Itoa(maxTargetRPS+1))
	require.ErrorContains(t, err, "--target-rps rates must be <= 100000")
	_, err = parseTargetRPS("1,x")
	require.ErrorContains(t, err, "is not a list of numbers")
	_, err = parseTargetRPS("1,")
	require.ErrorContains(t, err, "empty entry")
}

func TestParseRejectsRepeats(t *testing.T) {
	_, err := parseTargetRPS("1,2,1")
	require.ErrorContains(t, err, "--target-rps repeats 1, which would duplicate its scenario rows")
	_, err = parseQueryTypes("ledgers,txhash,ledgers")
	require.ErrorContains(t, err, `--types repeats "ledgers", which would duplicate its scenario rows`)
	_, err = parseQueryTypes("ledgers,nope")
	require.ErrorContains(t, err, `unknown query type "nope"`)
	got, err := parseQueryTypes("txhash, ledgers")
	require.NoError(t, err)
	assert.Equal(t, []string{queryTypeTxHash, queryTypeLedgers}, got)
}

func TestQueryPlanPoolAndNotFoundFraction(t *testing.T) {
	for _, size := range []int{-1, 0, 1, poolTargetHashes, maxTxHashPoolSize, maxTxHashPoolSize + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			f := validQueryFlags()
			f.txHashPoolSize = size
			p, err := f.plan()
			if size < 1 || size > maxTxHashPoolSize {
				require.ErrorContains(t, err, "--txhash-pool-size")
			} else {
				require.NoError(t, err)
				assert.Equal(t, size, p.TxHashPoolSize)
			}
		})
	}
	for _, fraction := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 1.1} {
		f := validQueryFlags()
		f.notFoundFraction = fraction
		_, err := f.plan()
		require.ErrorContains(t, err, "--not-found-fraction")
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
				assert.Equal(t, "request OS page-cache eviction before each scenario (Linux only)", eviction.Usage)
			}
			assert.Equal(t, want, warmup.DefValue)
			assert.Nil(t, cmd.Flags().Lookup("cache-scenario"))
			assert.Equal(t, "512", cmd.Flags().Lookup("txhash-pool-size").DefValue)
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
	require.ErrorContains(t, err, "--ledgers-span")

	f = validQueryFlags()
	f.txPageSpan = maxReadSpan + 1
	_, err = f.plan()
	require.ErrorContains(t, err, "--txpage-span")
}
