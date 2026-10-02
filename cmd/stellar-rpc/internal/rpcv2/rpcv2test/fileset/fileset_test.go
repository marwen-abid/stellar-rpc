package fileset

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failRecorder is a testing.TB that records a failure and lets the caller
// continue, so a test can assert that RequireUnchanged fails.
type failRecorder struct {
	testing.TB

	failed bool
}

func (r *failRecorder) Errorf(string, ...any) { r.failed = true }
func (r *failRecorder) FailNow()              { r.failed = true }

func TestTakeDetectsChanges(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"new file": func(t *testing.T, dir string) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c"), []byte("c"), 0o644))
		},
		"removed file": func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, "a")))
		},
		"size change": func(t *testing.T, dir string) {
			// Restore the mtime so only the size differs.
			path := filepath.Join(dir, "a")
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("longer"), 0o644))
			require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
		},
		"mtime change": func(t *testing.T, dir string) {
			later := time.Now().Add(time.Hour)
			require.NoError(t, os.Chtimes(filepath.Join(dir, "sub", "b"), later, later))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644))
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b"), []byte("b"), 0o644))
			before := Take(t, dir)
			require.Len(t, before, 4, "root, a, sub and sub/b")

			RequireUnchanged(t, dir, before)

			mutate(t, dir)
			rec := &failRecorder{TB: t}
			RequireUnchanged(rec, dir, before)
			require.True(t, rec.failed, "RequireUnchanged must fail after a %s", name)
		})
	}
}
