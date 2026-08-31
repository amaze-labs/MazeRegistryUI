// SPDX-License-Identifier: GPL-3.0-or-later

package registry

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// defaultCacheEntries bounds the cache. Registry responses are small (a
// manifest is kilobytes, a config blob rarely more), so a few thousand entries
// is a modest memory footprint for a very useful hit rate.
const defaultCacheEntries = 2048

// cacheScopeGlobal marks entries that are not tied to a single repository,
// such as catalog pages. They are dropped by any repository invalidation
// because deleting the last tag of a repository can make it disappear from the
// catalog altogether.
const cacheScopeGlobal = ""

// ttlCache is a bounded, concurrency-safe cache with per-entry expiry.
//
// Values are handed to every caller by reference, so anything stored here must
// be treated as immutable by readers. The client only ever stores raw response
// bytes and page structs that it clones before returning to callers.
//
// Expiry is enforced lazily on read and on write. No background goroutine is
// started, so a client can be constructed and discarded freely without leaking
// one.
type ttlCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	max     int
	// tick is a monotonically increasing counter standing in for a timestamp
	// in the LRU ordering; it avoids depending on clock resolution.
	tick uint64
	// now is a seam for tests. Production always uses time.Now.
	now func() time.Time
}

type cacheEntry struct {
	value   any
	repo    string
	expires time.Time
	used    uint64
}

func newTTLCache(max int) *ttlCache {
	if max <= 0 {
		max = defaultCacheEntries
	}
	return &ttlCache{
		entries: make(map[string]*cacheEntry, 64),
		max:     max,
		now:     time.Now,
	}
}

// get returns the live value for key, if any.
func (c *ttlCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	c.tick++
	e.used = c.tick
	return e.value, true
}

// set stores a value under key for ttl. repo associates the entry with a
// repository so InvalidateRepo can find it; pass cacheScopeGlobal for entries
// that belong to the registry as a whole. A non-positive ttl stores nothing.
func (c *ttlCache) set(key, repo string, value any, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tick++
	c.entries[key] = &cacheEntry{
		value:   value,
		repo:    repo,
		expires: c.now().Add(ttl),
		used:    c.tick,
	}
	c.evictLocked()
}

// evictLocked brings the cache back under its bound: expired entries go first,
// then the least recently used ones. It trims below the limit rather than to
// it, so the O(n) sort is amortised over many subsequent writes.
func (c *ttlCache) evictLocked() {
	if len(c.entries) <= c.max {
		return
	}

	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) <= c.max {
		return
	}

	type ref struct {
		key  string
		used uint64
	}
	refs := make([]ref, 0, len(c.entries))
	for k, e := range c.entries {
		refs = append(refs, ref{key: k, used: e.used})
	}
	slices.SortFunc(refs, func(a, b ref) int { return cmp.Compare(a.used, b.used) })

	target := c.max * 9 / 10
	for i := 0; i < len(refs)-target; i++ {
		delete(c.entries, refs[i].key)
	}
}

// InvalidateRepo drops every entry belonging to a repository, plus the
// registry-wide entries that a change to that repository could invalidate.
func (c *ttlCache) InvalidateRepo(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, e := range c.entries {
		if e.repo == repo || e.repo == cacheScopeGlobal {
			delete(c.entries, k)
		}
	}
}

// Purge empties the cache.
func (c *ttlCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry, 64)
}

// len reports the number of entries, expired ones included. Used by tests.
func (c *ttlCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
