package catalog

import (
	"crypto/rand"
	"fmt"
)

// catalogSecretStoreKey holds the deployment's cold-index secret.
const catalogSecretStoreKey = "meta/catalog-secret"

// Secret returns a copy of the deployment's cold-index secret, minted once at
// Open and cached. Per-index secrets are derived from it, so an attacker who
// influences indexed keys cannot predict which block a key lands in. Returning
// a fixed-size array (not the internal slice) states the length and prevents a
// caller aliasing or mutating the cached value. Stable for the life of the
// catalog.
func (c *Catalog) Secret() [32]byte { return c.secret }

// ensureSecret loads the persisted cold-index secret, minting and persisting a
// fresh random one on first call. Open runs it single-threaded, after the
// census has already validated any persisted value's width, and caches the
// result; nothing else should call it (get-or-create is not atomic).
func (c *Catalog) ensureSecret() ([32]byte, error) {
	s, found, err := c.loadSecret()
	if err != nil || found {
		return s, err
	}
	if _, err := rand.Read(s[:]); err != nil {
		return s, err
	}
	if err := c.put(catalogSecretStoreKey, string(s[:])); err != nil {
		return s, err
	}
	return s, nil
}

// loadSecret reads the persisted cold-index secret; found is false when the
// catalog holds none. The census has already validated a persisted value's
// width.
func (c *Catalog) loadSecret() ([32]byte, bool, error) {
	var s [32]byte
	v, found, err := c.get(catalogSecretStoreKey)
	if err != nil || !found {
		return s, false, err
	}
	copy(s[:], v)
	return s, true, nil
}

// readSecret returns the cold-index secret that open caches, so Secret() reads
// are lock-free and cannot fail. A read-write open mints the secret when it is
// absent (get-or-create is not atomic; open runs it single-threaded). A
// read-only open refuses a catalog without it: every catalog that Open wrote
// holds one.
func (c *Catalog) readSecret(readOnly bool, path string) ([32]byte, error) {
	if !readOnly {
		secret, err := c.ensureSecret()
		if err != nil {
			return secret, fmt.Errorf("catalog: ensure cold-index secret: %w", err)
		}
		return secret, nil
	}
	secret, found, err := c.loadSecret()
	if err != nil {
		return secret, fmt.Errorf("catalog: load cold-index secret: %w", err)
	}
	if !found {
		return secret, fmt.Errorf("catalog: no cold-index secret at %s", path)
	}
	return secret, nil
}
