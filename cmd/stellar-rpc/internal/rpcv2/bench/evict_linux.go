//go:build linux

package bench

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// evictSupported reports whether this platform can drop a file's pages from
// the OS page cache.
const evictSupported = true

// evictFile drops path's pages from the OS page cache with POSIX_FADV_DONTNEED.
// The hint targets the inode's page cache, so a reader holding the file open is
// unaffected; offset 0 and length 0 mean the whole file. It is reliable only
// for clean pages.
func evictFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd()) //nolint:gosec // an open descriptor fits an int
	if err := unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("fadvise dontneed %s: %w", path, err)
	}
	return nil
}
