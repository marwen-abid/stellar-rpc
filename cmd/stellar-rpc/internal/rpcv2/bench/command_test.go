package bench

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
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
