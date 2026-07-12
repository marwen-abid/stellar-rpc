//go:build linux

package bench

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// evictionWorks reports whether evictFromPageCache actually evicts on this
// platform; the cold query driver warns once per run when it does not.
const evictionWorks = true

// evictFromPageCache asks the kernel to drop path's clean pages from the OS
// page cache (posix_fadvise DONTNEED), so the cold-tier read that follows
// pays real device latency instead of hitting pages a previous iteration or
// the corpus scan left warm. The advice is inode-wide and advisory: dirty
// pages are unaffected and open readers on the same file keep working.
func evictFromPageCache(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("evict %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("evict %s: fadvise: %w", path, err)
	}
	return nil
}
