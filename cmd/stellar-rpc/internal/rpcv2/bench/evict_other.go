//go:build !linux

package bench

// evictSupported reports whether this platform can drop a file's pages from
// the OS page cache.
const evictSupported = false

// evictFile does nothing off Linux.
func evictFile(string) error { return nil }
