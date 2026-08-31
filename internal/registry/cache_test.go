package registry

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable time source for the cache's `now` seam.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestTTLCacheGetSet(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	c := newTTLCache(10)
	c.now = clock.Now

	c.set("k", "repo", "value", time.Minute)
	got, ok := c.get("k")
	if !ok || got != "value" {
		t.Fatalf("get(k) = (%v, %v), want (value, true)", got, ok)
	}

	if _, ok := c.get("missing"); ok {
		t.Fatal("get on an absent key reported a hit")
	}
}

func TestTTLCacheExpiry(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	c := newTTLCache(10)
	c.now = clock.Now
	c.set("k", "repo", "value", time.Minute)

	clock.Advance(time.Minute - time.Nanosecond)
	if _, ok := c.get("k"); !ok {
		t.Fatal("the entry expired one nanosecond early")
	}

	// Expiry is exclusive: at exactly the expiry instant the entry is gone.
	clock.Advance(time.Nanosecond)
	if _, ok := c.get("k"); ok {
		t.Fatal("the entry survived its expiry instant")
	}
	if c.len() != 0 {
		t.Fatalf("the expired entry was not dropped on read: %d entries remain", c.len())
	}
}

func TestTTLCacheNonPositiveTTLStoresNothing(t *testing.T) {
	t.Parallel()

	c := newTTLCache(10)
	for _, ttl := range []time.Duration{0, -time.Second} {
		c.set("k", "repo", "value", ttl)
		if _, ok := c.get("k"); ok {
			t.Fatalf("set with ttl %v stored an entry, want it dropped", ttl)
		}
	}
}

func TestTTLCacheEviction(t *testing.T) {
	t.Parallel()

	const max = 20
	c := newTTLCache(max)

	for i := range max + 1 {
		c.set(fmt.Sprintf("k%02d", i), "repo", i, time.Hour)
	}

	if got := c.len(); got > max {
		t.Fatalf("the cache holds %d entries with a bound of %d", got, max)
	}
	// Eviction trims below the bound rather than to it, so the cost is
	// amortised over subsequent writes.
	if want := max * 9 / 10; c.len() != want {
		t.Fatalf("after eviction the cache holds %d entries, want %d", c.len(), want)
	}
	// The oldest keys are the ones that went.
	if _, ok := c.get("k00"); ok {
		t.Error("the least recently used entry survived eviction")
	}
	if _, ok := c.get(fmt.Sprintf("k%02d", max)); !ok {
		t.Error("the most recently written entry was evicted")
	}
}

func TestTTLCacheEvictionPrefersExpiredEntries(t *testing.T) {
	t.Parallel()

	const max = 10
	clock := newFakeClock()
	c := newTTLCache(max)
	c.now = clock.Now

	// Short-lived entries written first, then long-lived ones.
	for i := range max {
		c.set(fmt.Sprintf("short%d", i), "repo", i, time.Minute)
	}
	clock.Advance(2 * time.Minute)
	for i := range max {
		c.set(fmt.Sprintf("long%d", i), "repo", i, time.Hour)
	}

	// Every short entry is expired, so nothing live should have been dropped.
	for i := range max {
		if _, ok := c.get(fmt.Sprintf("long%d", i)); !ok {
			t.Fatalf("live entry long%d was evicted while expired entries were still present", i)
		}
	}
	for i := range max {
		if _, ok := c.get(fmt.Sprintf("short%d", i)); ok {
			t.Fatalf("expired entry short%d is still readable", i)
		}
	}
}

func TestTTLCacheLRUOrdering(t *testing.T) {
	t.Parallel()

	const max = 20
	c := newTTLCache(max)
	for i := range max {
		c.set(fmt.Sprintf("k%02d", i), "repo", i, time.Hour)
	}
	// Touch the oldest entry so it is no longer the least recently used.
	if _, ok := c.get("k00"); !ok {
		t.Fatal("k00 is missing before the eviction round")
	}
	c.set("trigger", "repo", 0, time.Hour)

	if _, ok := c.get("k00"); !ok {
		t.Error("a recently read entry was evicted; the LRU ordering ignores reads")
	}
	if _, ok := c.get("k01"); ok {
		t.Error("k01 survived; it was the least recently used entry")
	}
}

func TestTTLCacheInvalidateRepo(t *testing.T) {
	t.Parallel()

	c := newTTLCache(100)
	c.set("tags|app", "app", 1, time.Hour)
	c.set("manifest|app", "app", 2, time.Hour)
	c.set("tags|other", "other", 3, time.Hour)
	c.set("catalog", cacheScopeGlobal, 4, time.Hour)

	c.InvalidateRepo("app")

	for _, key := range []string{"tags|app", "manifest|app"} {
		if _, ok := c.get(key); ok {
			t.Errorf("%s survived InvalidateRepo(app)", key)
		}
	}
	// Deleting the last tag of a repository can make it vanish from the
	// catalog, so registry-wide entries go too.
	if _, ok := c.get("catalog"); ok {
		t.Error("the registry-wide catalog entry survived a repository invalidation")
	}
	if _, ok := c.get("tags|other"); !ok {
		t.Error("another repository's entry was dropped by InvalidateRepo(app)")
	}
}

func TestTTLCachePurge(t *testing.T) {
	t.Parallel()

	c := newTTLCache(10)
	c.set("a", "repo", 1, time.Hour)
	c.set("b", cacheScopeGlobal, 2, time.Hour)
	c.Purge()
	if c.len() != 0 {
		t.Fatalf("Purge left %d entries", c.len())
	}
}

func TestNewTTLCacheDefaultBound(t *testing.T) {
	t.Parallel()

	for _, max := range []int{0, -1} {
		if got := newTTLCache(max).max; got != defaultCacheEntries {
			t.Errorf("newTTLCache(%d).max = %d, want %d", max, got, defaultCacheEntries)
		}
	}
}

// The client's own TTLs must be honoured: a tag manifest expires with the
// configured TTL, a digest-addressed one is treated as immutable.
func TestClientCacheTTLs(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("c"), Size: 1})
	dgst := f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)

	clock := newFakeClock()
	c := newTestClient(t, f, func(o *Options) { o.CacheTTL = 30 * time.Second })
	c.cache.now = clock.Now
	ctx := context.Background()

	if _, err := c.Image(ctx, "app", "latest", false); err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	clock.Advance(31 * time.Second)
	if _, err := c.Image(ctx, "app", "latest", false); err != nil {
		t.Fatalf("second Image failed: %v", err)
	}
	if got := f.countPath("/v2/app/manifests/latest"); got != 2 {
		t.Errorf("the tag manifest was fetched %d times across a TTL boundary, want 2", got)
	}

	// The digest entry has the immutable TTL and is still warm.
	f.reset()
	clock.Advance(immutableTTL - time.Minute)
	if _, err := c.Image(ctx, "app", dgst, false); err != nil {
		t.Fatalf("Image by digest failed: %v", err)
	}
	if got := f.countPath("/v2/app/manifests/" + dgst); got != 0 {
		t.Errorf("the digest-addressed manifest was refetched %d times inside the immutable TTL, want 0", got)
	}
}

func TestTTLCacheConcurrentAccess(t *testing.T) {
	t.Parallel()

	c := newTTLCache(64)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 200 {
				key := fmt.Sprintf("k%d", i%80)
				c.set(key, fmt.Sprintf("repo%d", g%4), i, time.Minute)
				c.get(key)
				if i%50 == 0 {
					c.InvalidateRepo(fmt.Sprintf("repo%d", g%4))
				}
				c.len()
			}
		}(g)
	}
	wg.Wait()
}

// One client driven hard from many goroutines must stay race-free.
func TestClientConcurrentUse(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.ping()
	f.handle("/v2/_catalog", jsonHandler(`{"repositories":["app","other"]}`, ""))
	f.handle("/v2/app/tags/list", jsonHandler(`{"name":"app","tags":["latest","v1"]}`, ""))

	cfgRaw := configBlob(t, "linux", "amd64", time.Now().UTC().Truncate(time.Second), nil, nil)
	cfgDigest := f.serveBlob("app", cfgRaw)
	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: cfgDigest, Size: int64(len(cfgRaw))},
		testDescriptor{Digest: fakeDigest("l1"), Size: 10})
	f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)
	f.handle("/v2/app/manifests/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeRegistryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "no such tag")
	})

	_, _ = indexFixture(t, f)

	c := newTestClient(t, f)
	ctx := context.Background()

	const goroutines = 24
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*8)

	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for range 8 {
				switch g % 6 {
				case 0:
					if err := c.Ping(ctx); err != nil {
						errs <- fmt.Errorf("Ping: %w", err)
					}
				case 1:
					if _, err := c.Catalog(ctx, 10, ""); err != nil {
						errs <- fmt.Errorf("Catalog: %w", err)
					}
				case 2:
					if _, err := c.Tags(ctx, "app", 10, ""); err != nil {
						errs <- fmt.Errorf("Tags: %w", err)
					}
				case 3:
					img, err := c.Image(ctx, "app", "latest", false)
					if err != nil {
						errs <- fmt.Errorf("Image: %w", err)
						continue
					}
					// Writing into the returned copy must not affect anyone else.
					if len(img.RawManifest) > 0 {
						img.RawManifest[0] = 'Z'
					}
				case 4:
					if _, err := c.Image(ctx, "app", "multi", true); err != nil {
						errs <- fmt.Errorf("Image(index): %w", err)
					}
				case 5:
					if sum := c.TagSummary(ctx, "app", "v1"); sum == nil {
						errs <- fmt.Errorf("TagSummary returned nil")
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent client use failed: %v", err)
	}
}
