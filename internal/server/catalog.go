// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

// maxCatalogEntries bounds how much of a very large catalog is held in memory.
// Registries bigger than this are still browsable; the listing is simply
// truncated, which is preferable to an unbounded allocation.
const maxCatalogEntries = 50000

// catalogCache holds the full repository list per registry so filtering and
// pagination happen in memory. The Distribution API has no search endpoint, so
// the alternative is re-walking every catalog page on each keystroke.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]*catalogEntry
}

type catalogEntry struct {
	// load guards the fetch so a burst of requests triggers one walk, not many.
	load  sync.Mutex
	repos []string
	at    time.Time
	err   error
	// tints maps each namespace to its palette slot. Guarded by
	// catalogCache.mu rather than load, so a page can read a colour while a
	// refresh is walking the catalog.
	tints map[string]string
}

func newCatalogCache() *catalogCache {
	return &catalogCache{entries: make(map[string]*catalogEntry)}
}

func (c *catalogCache) entry(id string) *catalogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok {
		e = &catalogEntry{}
		c.entries[id] = e
	}
	return e
}

// List returns the cached repository list, refreshing it when older than ttl.
func (c *catalogCache) List(ctx context.Context, id string, client registry.Client, pageSize int, ttl time.Duration) ([]string, error) {
	e := c.entry(id)
	e.load.Lock()
	defer e.load.Unlock()

	if !e.at.IsZero() && time.Since(e.at) < ttl {
		return e.repos, e.err
	}

	repos, err := walkCatalog(ctx, client, pageSize)
	if err != nil {
		// Serve a stale list rather than an error page when we have one: a
		// momentarily unreachable registry should not blank the screen.
		if e.repos != nil {
			return e.repos, nil
		}
		e.at, e.err, e.repos = time.Now(), err, nil
		return nil, err
	}

	sort.Strings(repos)
	e.repos, e.err, e.at = repos, nil, time.Now()
	tints := assignTints(repos)
	c.mu.Lock()
	e.tints = tints
	c.mu.Unlock()
	return repos, nil
}

// Tint is the palette slot of a namespace on a registry. Slots are handed out
// in alphabetical namespace order, so up to eight namespaces never share a
// colour — hashing alone collides far too early (five namespaces in eight
// slots already give even odds of a clash). The price is that a new
// namespace can shift the colours after it, which is the lesser evil. Before
// the catalog has been listed once, the hash stands in.
func (c *catalogCache) Tint(id, ns string) string {
	c.mu.Lock()
	var t string
	if e := c.entries[id]; e != nil && e.tints != nil {
		t = e.tints[ns]
	}
	c.mu.Unlock()
	if t == "" {
		t = nsTint(ns)
	}
	return t
}

func assignTints(repos []string) map[string]string {
	out := map[string]string{}
	i := 0
	for _, r := range repos { // already sorted, so namespaces arrive in order
		ns, _ := splitRepo(r)
		if _, ok := out[ns]; ok {
			continue
		}
		if ns == "" {
			out[ns] = "tn"
			continue
		}
		out[ns] = "t" + string(rune('0'+i%tintCount))
		i++
	}
	return out
}

// Invalidate drops the cached list, used after a deletion.
func (c *catalogCache) Invalidate(id string) {
	e := c.entry(id)
	e.load.Lock()
	defer e.load.Unlock()
	e.at = time.Time{}
}

// walkCatalog follows the pagination cursor to the end of the catalog.
func walkCatalog(ctx context.Context, client registry.Client, pageSize int) ([]string, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	var (
		all  []string
		last string
	)
	for {
		page, err := client.Catalog(ctx, pageSize, last)
		if err != nil {
			return nil, err
		}
		for _, r := range page.Repositories {
			all = append(all, r.Name)
			if len(all) >= maxCatalogEntries {
				return all, nil
			}
		}
		if page.NextLast == "" || page.NextLast == last {
			return all, nil
		}
		last = page.NextLast
	}
}

// filterRepos narrows the list to those containing every whitespace-separated
// term, case-insensitively. Multiple terms behave as AND, which is how people
// actually narrow down "team backend api".
func filterRepos(repos []string, query string) []string {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return repos
	}
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		lower := strings.ToLower(r)
		match := true
		for _, t := range terms {
			if !strings.Contains(lower, t) {
				match = false
				break
			}
		}
		if match {
			out = append(out, r)
		}
	}
	return out
}
