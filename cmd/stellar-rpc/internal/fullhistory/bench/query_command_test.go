package bench

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewQueryCommand builds the full bench-query tree — executing every
// markRequired call, whose panic on a bad flag name this test exists to
// catch (main.go calls NewQueryCommand unconditionally at startup) — and
// pins each subcommand's required flags.
func TestNewQueryCommand(t *testing.T) {
	cmd := NewQueryCommand()
	require.Equal(t, "bench-query", cmd.Use)

	requiredBySubcommand := map[string][]string{
		"cold": {"types", "chunk", "cold-dir"},
		"hot":  {"types", "chunk", "hot-dir"},
	}
	subs := make(map[string]*cobra.Command, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		subs[sub.Use] = sub
	}
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

func TestParseQueryTypes(t *testing.T) {
	types, err := parseQueryTypes("ledgers,txpage,events")
	require.NoError(t, err)
	assert.True(t, types.Ledgers)
	assert.True(t, types.TxPage)
	assert.False(t, types.Txhash)
	assert.True(t, types.Events)

	_, err = parseQueryTypes("ledgers,bogus")
	require.ErrorContains(t, err, "bogus")
}

func TestParseWorkersList(t *testing.T) {
	workers, err := parseWorkersList("1, 4,16")
	require.NoError(t, err)
	assert.Equal(t, []int{1, 4, 16}, workers)

	_, err = parseWorkersList("1,0")
	require.ErrorContains(t, err, "positive worker count")
	_, err = parseWorkersList("")
	require.ErrorContains(t, err, "at least one")
}
