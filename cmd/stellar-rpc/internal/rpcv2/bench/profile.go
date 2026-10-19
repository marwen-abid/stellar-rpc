package bench

import (
	"fmt"
	"os"
	"runtime/pprof"

	"github.com/spf13/cobra"

	supportlog "github.com/stellar/go-stellar-sdk/support/log"
)

// profileFlags is the optional Go-profiling flag pair shared by every bench
// command.
type profileFlags struct {
	cpuProfile string
	memProfile string
	// cpuPerScenario leaves --cpuprofile to the command, which writes one CPU
	// profile per scenario; around then writes none. Set it before bind.
	cpuPerScenario bool
}

func (p *profileFlags) bind(cmd *cobra.Command) {
	fs := cmd.Flags()
	cpuUsage := "write a Go CPU profile to PATH"
	if p.cpuPerScenario {
		cpuUsage = "write one Go CPU profile per scenario, covering its measured iterations, to PATH with " +
			"\".<type>.<rps>rps\" inserted before the extension (cpu.pprof -> cpu.ledgers.10rps.pprof)"
	}
	fs.StringVar(&p.cpuProfile, "cpuprofile", "", cpuUsage)
	fs.StringVar(&p.memProfile, "memprofile", "",
		"write a Go allocation profile to PATH (read it with `go tool pprof -alloc_space`)")
}

// around runs fn under the optional profiles: the CPU profile spans the whole
// run unless cpuPerScenario is set; the heap profile is written once fn
// returns (see writeHeapProfile).
func (p *profileFlags) around(logger *supportlog.Entry, fn func() error) error {
	if p.cpuProfile != "" && !p.cpuPerScenario {
		f, err := os.Create(p.cpuProfile)
		if err != nil {
			return fmt.Errorf("create %s: %w", p.cpuProfile, err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			return fmt.Errorf("start CPU profile: %w", err)
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = f.Close()
			logger.Infof("wrote CPU profile to %s", p.cpuProfile)
		}()
	}
	err := fn()
	if p.memProfile != "" {
		p.writeHeapProfile(logger)
	}
	return err
}

// writeHeapProfile writes the heap profile to --memprofile. Read it as an
// ALLOCATION profile (`go tool pprof -alloc_space`): that view is cumulative —
// every byte the run allocated, whether or not it was later freed — so writing
// it here, after the run's stores have closed, still captures the full picture.
// The default `-inuse_space` view is a live snapshot taken after teardown and
// under-reports, so it is not what this profile is for. No GC is forced:
// alloc_space is unaffected by collection, so there is nothing to gain by
// perturbing the run.
func (p *profileFlags) writeHeapProfile(logger *supportlog.Entry) {
	f, err := os.Create(p.memProfile)
	if err != nil {
		logger.Errorf("create %s: %v", p.memProfile, err)
		return
	}
	defer func() { _ = f.Close() }()
	if err := pprof.WriteHeapProfile(f); err != nil {
		logger.Errorf("write heap profile: %v", err)
		return
	}
	logger.Infof("wrote heap profile to %s", p.memProfile)
}
