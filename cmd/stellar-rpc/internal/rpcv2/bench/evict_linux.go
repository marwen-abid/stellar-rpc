//go:build linux

package bench

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// evictSupported reports that this platform can drop a file's pages from the
// OS page cache.
const evictSupported = true

// evictFile drops path's pages from the OS page cache via POSIX_FADV_DONTNEED.
// fadvise targets the inode's page cache, not one descriptor, so a reader
// already holding the file open is unaffected; offset 0 and length 0 mean the
// whole file. The hint is reliable for clean pages, which frozen artifacts are.
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
