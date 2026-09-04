package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/version"
)

// invocationRecord holds metadata about a benchmark invocation. The JSON
// keys are a versioned schema (schemaVersion) that downstream tooling
// consumes; treat key renames as breaking changes.
type invocationRecord struct {
	SchemaVersion int               `json:"schemaVersion"`
	Command       string            `json:"command"`
	Flags         map[string]string `json:"flags"`
	Binary        binaryInfo        `json:"binary"`
	Hostname      string            `json:"hostname"`
	StartedAt     string            `json:"startedAt"`
	// FinishedAt is absent in the record written at start, so a record without
	// it is a run that was killed before it finished.
	FinishedAt string `json:"finishedAt,omitempty"`
	// Extra carries facts the run resolved for itself rather than read off a
	// flag, such as whether page-cache eviction ran. Absent when none were
	// recorded.
	Extra map[string]string `json:"extra,omitempty"`
	// Error carries a failed run's error message; absent on a successful run.
	Error string `json:"error,omitempty"`
}

// binaryInfo holds build-time information about the binary.
type binaryInfo struct {
	Version        string `json:"version"`
	CommitHash     string `json:"commitHash"`
	BuildTimestamp string `json:"buildTimestamp"`
	Branch         string `json:"branch"`
}

// writeStartInvocationJSON writes the record of a run that has just started:
// command, flags and start time, with no finishedAt and no error. The write at
// the end of the run overwrites the same file.
func writeStartInvocationJSON(
	outDir string, cmd *cobra.Command, flags, extra map[string]string, startedAt time.Time,
) error {
	return writeInvocationJSON(outDir, cmd, flags, extra, startedAt, time.Time{}, nil)
}

// writeInvocationJSON writes an invocation record to outDir/invocation.json,
// replacing any existing file. A zero finishedAt leaves the field out, which is
// how a run in progress records itself; a non-nil runErr's message lands in the
// error field. Indented JSON with a trailing newline.
func writeInvocationJSON(
	outDir string,
	cmd *cobra.Command,
	flags, extra map[string]string,
	startedAt, finishedAt time.Time,
	runErr error,
) error {
	hostname, _ := os.Hostname() // empty string on error

	var errMsg string
	if runErr != nil {
		errMsg = runErr.Error()
	}

	var finished string
	if !finishedAt.IsZero() {
		finished = finishedAt.UTC().Format(time.RFC3339)
	}

	record := invocationRecord{
		SchemaVersion: 1,
		Command:       cmd.CommandPath(),
		Flags:         flags,
		Binary: binaryInfo{
			Version:        version.Version,
			CommitHash:     version.CommitHash,
			BuildTimestamp: version.BuildTimestamp,
			Branch:         version.Branch,
		},
		Hostname:   hostname,
		StartedAt:  startedAt.UTC().Format(time.RFC3339),
		FinishedAt: finished,
		Error:      errMsg,
	}
	if len(extra) > 0 {
		record.Extra = extra
	}

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal invocation record: %w", err)
	}

	path := filepath.Join(outDir, "invocation.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write invocation.json: %w", err)
	}
	return nil
}

// captureFlags extracts all flag values from a cobra command's flag set,
// returning them as a map of flag name to string value. Uses VisitAll to
// capture all flags (default and explicitly-set).
func captureFlags(cmd *cobra.Command) map[string]string {
	flags := make(map[string]string)
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		flags[f.Name] = f.Value.String()
	})
	return flags
}
