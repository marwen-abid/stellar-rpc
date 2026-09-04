//go:build !linux

package bench

// evictSupported reports that this platform cannot drop a file's pages from the
// OS page cache: POSIX_FADV_DONTNEED is not portable, and campaigns run on
// Linux, so a run here records eviction as unsupported in invocation.json.
const evictSupported = false

// evictFile does nothing off Linux. A cold run on such a platform measures warm
// reads, which is why the run records that eviction did not happen.
func evictFile(string) error { return nil }
