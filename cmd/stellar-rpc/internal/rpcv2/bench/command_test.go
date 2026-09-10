package bench

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/chunk"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/rpcv2/rpcv2test"
)

// TestNewCommand builds the full command tree, which executes every
// markRequired call, and checks each subcommand's required flags.
func TestNewCommand(t *testing.T) {
	cmd := NewCommand()
	require.Equal(t, "bench-ingest", cmd.Use)
	assertRequiredFlags(t, cmd, map[string][]string{
		"cold": {"start-chunk", "cold-out-dir"},
		"hot":  {"start-chunk", "hot-dir"},
	})
}

// TestMarkRequiredPanicsOnUnknownFlag pins the startup guard: main.go builds
// both command trees unconditionally, so a typo in a required flag name fails
// every invocation of the binary, not only the bench commands.
func TestMarkRequiredPanicsOnUnknownFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("known", "", "")
	require.NotPanics(t, func() { markRequired(cmd, "known") })
	require.Panics(t, func() { markRequired(cmd, "unknown") })
}

// assertRequiredFlags checks that each subcommand named in requiredBySubcommand
// exists under cmd and has every listed flag marked required.
func assertRequiredFlags(t *testing.T, cmd *cobra.Command, requiredBySubcommand map[string][]string) {
	t.Helper()
	subs := subcommandsByUse(cmd)
	for name, flags := range requiredBySubcommand {
		sub := subs[name]
		require.NotNil(t, sub, "subcommand %q missing", name)
		for _, fn := range flags {
			f := sub.Flags().Lookup(fn)
			require.NotNil(t, f, "%s: flag --%s missing", name, fn)
			require.Contains(t, f.Annotations, cobra.BashCompOneRequiredFlag,
				"%s: flag --%s not marked required", name, fn)
		}
	}
}

// subcommandsByUse indexes a command's children by Use.
func subcommandsByUse(cmd *cobra.Command) map[string]*cobra.Command {
	subs := make(map[string]*cobra.Command, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		subs[sub.Use] = sub
	}
	return subs
}

func TestQueryCommandRecordsCacheAndCorpus(t *testing.T) {
	const hotLedgers = 32
	packDir := writeLedgerPack(t, t.TempDir(), chunk.ID(0), hotLedgers, func(seq uint32) []byte {
		return rpcv2test.MultiTxLCMBytes(t, seq, 32)
	})
	hotRoot := ingestHotChunk(t, packDir, hotLedgers)
	// Cold opening needs a complete chunk. Only every hundredth ledger needs transactions.
	coldPack := writeLedgerPack(t, t.TempDir(), chunk.ID(0), chunk.LedgersPerChunk, func(seq uint32) []byte {
		if (seq-chunk.ID(0).FirstLedger())%eventEvery == 0 {
			return rpcv2test.MultiTxLCMBytes(t, seq, 32)
		}
		return rpcv2test.ZeroTxLCMBytes(t, seq)
	})
	coldRoot := t.TempDir()
	require.NoError(t, runCold(context.Background(), testLogger(), coldOptions{
		Source: sourceConfig{Kind: sourcePack, PackDir: coldPack}, StartChunk: 0, NumChunks: 1,
		Workers: 1, ColdRoot: coldRoot, OutDir: t.TempDir(),
	}))
	eviction := testEvictionUnsupported
	if evictSupported {
		eviction = testEvictionRequested
	}
	for _, tc := range []struct {
		name, tier, scenario, eviction string
		warmup                         int
		flags                          []string
	}{
		{"cold default", "cold", "cold-start", eviction, 0, nil},
		{"cold existing cache", "cold", "existing-cache", "off", 0, []string{"--evict-page-cache=false"}},
		{"cold warmup after eviction", "cold", "warm-run", eviction, 2, []string{"--warmup=2"}},
		{
			"cold warmup without eviction", "cold", "warm-run", "off", 2,
			[]string{"--warmup=2", "--evict-page-cache=false"},
		},
		{"hot default", "hot", "warm-run", "off", 20, nil},
		{"hot warmup", "hot", "warm-run", "off", 2, []string{"--warmup=2"}},
		{"hot existing cache", "hot", "existing-cache", "off", 0, []string{"--warmup=0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			args := []string{
				tc.tier, "--out=" + out, "--types=txhash", "--txhash-corpus-size=17",
				"--target-rps=100", "--duration=40ms", "--miss-fraction=0",
			}
			if tc.tier == "hot" {
				args = append(args, "--hot-dir="+hotRoot, "--chunk=0")
			} else {
				args = append(args, "--cold-dir="+coldRoot, "--start-chunk=0")
			}
			args = append(args, tc.flags...)
			cmd := NewQueryCommand()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute())

			data, err := os.ReadFile(filepath.Join(out, "invocation.json"))
			require.NoError(t, err)
			var record invocationRecord
			require.NoError(t, json.Unmarshal(data, &record))
			assert.Equal(t, "bench-query "+tc.tier, record.Command)
			assert.Empty(t, record.Error)
			assert.NotEmpty(t, record.FinishedAt)
			assert.Equal(t, strconv.Itoa(tc.warmup), record.Flags["warmup"])
			assert.Equal(t, "17", record.Flags["txhash-corpus-size"])
			assert.Equal(t, tc.scenario, record.Extra["cacheScenario"])
			assert.Equal(t, tc.eviction, record.Extra["pageCacheEviction"])
			assert.Equal(t, "17", record.Extra["txhashCorpusHashes"], "the requested cap is exact")
			assert.Equal(t, "2", record.Extra["txhashCorpusLedgers"], "16 hashes plus one from a second ledger")
			assertQueryReport(t, out, queryPlan{
				Types: []string{queryTypeTxHash}, TargetRPS: []float64{100}, Duration: 40 * time.Millisecond,
			})
		})
	}
}

func TestQueryCommandPersistsFailedMeasurements(t *testing.T) {
	packDir := writeLedgerPack(t, t.TempDir(), chunk.ID(0), 8, func(seq uint32) []byte {
		return rpcv2test.MultiTxLCMBytes(t, seq, 2)
	})
	hotRoot := ingestHotChunk(t, packDir, 8)
	out := t.TempDir()
	cmd := NewQueryCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"hot", "--hot-dir=" + hotRoot, "--chunk=0", "--out=" + out,
		"--types=events,ledgers,txpage", "--target-rps=100,200", "--duration=40ms", "--warmup=2",
		"--ledgers-span=1", "--txpage-span=1", "--network-passphrase=" + network.TestNetworkPassphrase,
	})
	err := cmd.Execute()
	require.ErrorContains(t, err, "query txpage at 100 rps: 4 of 4 requests failed")
	data, readErr := os.ReadFile(filepath.Join(out, "invocation.json"))
	require.NoError(t, readErr)
	var record invocationRecord
	require.NoError(t, json.Unmarshal(data, &record))
	assert.Equal(t, err.Error(), record.Error)
	assert.NotEmpty(t, record.FinishedAt)
	assert.Equal(t, network.TestNetworkPassphrase, record.Flags["network-passphrase"])
	assert.Equal(t, "warm-run", record.Extra["cacheScenario"])

	// Event corpus preparation and both earlier query types completed before txpage failed.
	assertQueryReport(t, out, queryPlan{
		Types: []string{queryTypeEvents, queryTypeLedgers}, TargetRPS: []float64{100, 200},
		Duration: 40 * time.Millisecond,
	})
	driver := readCSV(t, filepath.Join(out, "driver.csv"))
	accounting := readCSV(t, filepath.Join(out, fileQueryAccounting+".csv"))
	assertLegDriverRows(t, driver, accounting, queryTypeTxPage, 100, 4)
	assert.EqualValues(t, 4, accounting["txpage_r100_dispatched"]["n_items"])
	assert.EqualValues(t, 4, accounting["txpage_r100_failed"]["n_items"])
	assert.Zero(t, accounting["txpage_r100_successful"]["n_items"])
	assert.Zero(t, driver["txpage_r100_shed"]["n_items"])
	assert.Zero(t, driver["txpage_r100_millirps"]["total_ns"])
	assert.Zero(t, accounting["txpage_r100_completion_millirps"]["total_ns"])
	assert.NotContains(t, accounting, "txpage_r200_scheduled", "failure stops later legs")
	assert.NoFileExists(t, filepath.Join(out, "txpage.csv"), "failed requests have no latency samples")
}
