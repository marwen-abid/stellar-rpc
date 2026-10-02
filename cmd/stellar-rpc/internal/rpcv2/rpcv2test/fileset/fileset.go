// Package fileset records the file set of a directory tree so a test can
// prove that an operation changed no file in it.
package fileset

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Entry is the recorded state of one file or directory.
type Entry struct {
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
}

// Take walks root with filepath.WalkDir. Keys are paths relative to root.
// Directories are included, so a file created and then removed still shows as
// a change of its parent's mtime.
func Take(t testing.TB, root string) map[string]Entry {
	t.Helper()
	set := map[string]Entry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		set[rel] = Entry{Size: info.Size(), ModTime: info.ModTime(), Mode: info.Mode()}
		return nil
	})
	require.NoError(t, err, "take file set of %s", root)
	return set
}

// RequireUnchanged fails the test when the current file set of root differs from before.
func RequireUnchanged(t testing.TB, root string, before map[string]Entry) {
	t.Helper()
	after := Take(t, root)
	var diffs []string
	for name, b := range before {
		a, ok := after[name]
		switch {
		case !ok:
			diffs = append(diffs, "removed: "+name)
		case a.Size != b.Size || a.Mode != b.Mode || !a.ModTime.Equal(b.ModTime):
			diffs = append(diffs, fmt.Sprintf("changed: %s: %+v -> %+v", name, b, a))
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			diffs = append(diffs, "added: "+name)
		}
	}
	slices.Sort(diffs)
	require.Empty(t, diffs, "file set of %s changed", root)
}
