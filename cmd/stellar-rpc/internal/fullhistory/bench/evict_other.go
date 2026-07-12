//go:build !linux

package bench

// evictionWorks reports whether evictFromPageCache actually evicts on this
// platform; the cold query driver warns once per run when it does not.
const evictionWorks = false

// evictFromPageCache is a no-op on platforms without posix_fadvise (macOS
// offers no unprivileged equivalent): the page cache stays warm, so cold-tier
// latencies measured here reflect a warm cache. Cold measurement campaigns
// run on Linux.
func evictFromPageCache(string) error { return nil }
