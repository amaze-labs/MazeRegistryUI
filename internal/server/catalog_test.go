package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

func TestWalkCatalogFollowsThePagination(t *testing.T) {
	t.Parallel()

	pages := map[string]*registry.CatalogPage{
		"":  {Repositories: []registry.Repository{{Name: "a"}, {Name: "b"}}, NextLast: "b"},
		"b": {Repositories: []registry.Repository{{Name: "c"}}},
	}
	f := newFakeClient()
	f.CatalogFn = func(_ context.Context, _ int, last string) (*registry.CatalogPage, error) {
		p, ok := pages[last]
		if !ok {
			return nil, fmt.Errorf("unexpected cursor %q", last)
		}
		return p, nil
	}

	got, err := walkCatalog(context.Background(), f, 10)
	if err != nil {
		t.Fatalf("walkCatalog failed: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("walkCatalog = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walkCatalog = %v, want %v", got, want)
		}
	}
}

// A registry that keeps returning a full page with the same cursor must not
// spin the walker forever.
func TestWalkCatalogTerminatesOnAStuckCursor(t *testing.T) {
	t.Parallel()

	var calls int
	f := newFakeClient()
	f.CatalogFn = func(_ context.Context, _ int, _ string) (*registry.CatalogPage, error) {
		calls++
		if calls > 100 {
			return nil, errors.New("the walker never stopped")
		}
		return &registry.CatalogPage{
			Repositories: []registry.Repository{{Name: "a"}, {Name: "b"}},
			NextLast:     "b",
		}, nil
	}

	got, err := walkCatalog(context.Background(), f, 2)
	if err != nil {
		t.Fatalf("walkCatalog failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the walker made %d requests, want 2: the second page repeats the cursor and must stop it", calls)
	}
	if len(got) != 4 {
		t.Fatalf("walkCatalog collected %d names, want 4 (both pages before the repeat was noticed)", len(got))
	}
}

func TestWalkCatalogRespectsTheEntryCap(t *testing.T) {
	t.Parallel()

	// A registry with an unbounded catalog: every page is full and the cursor
	// keeps advancing.
	var n int
	f := newFakeClient()
	f.CatalogFn = func(_ context.Context, size int, _ string) (*registry.CatalogPage, error) {
		page := &registry.CatalogPage{}
		for range size {
			n++
			page.Repositories = append(page.Repositories, registry.Repository{Name: fmt.Sprintf("r%08d", n)})
		}
		page.NextLast = page.Repositories[len(page.Repositories)-1].Name
		return page, nil
	}

	got, err := walkCatalog(context.Background(), f, 1000)
	if err != nil {
		t.Fatalf("walkCatalog failed: %v", err)
	}
	if len(got) != maxCatalogEntries {
		t.Fatalf("walkCatalog collected %d names, want it capped at %d", len(got), maxCatalogEntries)
	}
}

func TestWalkCatalogDefaultsThePageSize(t *testing.T) {
	t.Parallel()

	var asked int
	f := newFakeClient()
	f.CatalogFn = func(_ context.Context, size int, _ string) (*registry.CatalogPage, error) {
		asked = size
		return &registry.CatalogPage{}, nil
	}
	if _, err := walkCatalog(context.Background(), f, 0); err != nil {
		t.Fatalf("walkCatalog failed: %v", err)
	}
	if asked != 100 {
		t.Fatalf("walkCatalog asked for a page of %d, want the default of 100", asked)
	}
}

func TestWalkCatalogPropagatesErrors(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.CatalogFn = func(context.Context, int, string) (*registry.CatalogPage, error) {
		return nil, registry.ErrUnauthorized
	}
	if _, err := walkCatalog(context.Background(), f, 10); !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("walkCatalog error = %v, want ErrUnauthorized", err)
	}
}

func TestCatalogCacheSortsAndCaches(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.Repos = []string{"z", "a", "m"}
	c := newCatalogCache()

	got, err := c.List(context.Background(), "local", f, 10, time.Minute)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "z" {
		t.Fatalf("List = %v, want it sorted", got)
	}

	if _, err := c.List(context.Background(), "local", f, 10, time.Minute); err != nil {
		t.Fatalf("second List failed: %v", err)
	}
	if f.CatalogCalls != 1 {
		t.Fatalf("the catalog was walked %d times, want 1 inside the TTL", f.CatalogCalls)
	}
}

func TestCatalogCacheRefreshesAfterTheTTL(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.Repos = []string{"a"}
	c := newCatalogCache()
	ctx := context.Background()

	if _, err := c.List(ctx, "local", f, 10, 0); err != nil {
		t.Fatalf("List failed: %v", err)
	}
	// A zero TTL means every call is a refresh.
	if _, err := c.List(ctx, "local", f, 10, 0); err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if f.CatalogCalls != 2 {
		t.Fatalf("the catalog was walked %d times, want 2 with a zero TTL", f.CatalogCalls)
	}
}

func TestCatalogCacheServesStaleOnFailure(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.Repos = []string{"a", "b"}
	c := newCatalogCache()
	ctx := context.Background()

	if _, err := c.List(ctx, "local", f, 10, time.Minute); err != nil {
		t.Fatalf("the warm-up List failed: %v", err)
	}
	f.CatalogFn = func(context.Context, int, string) (*registry.CatalogPage, error) {
		return nil, errors.New("registry unreachable")
	}

	got, err := c.List(ctx, "local", f, 10, 0)
	if err != nil {
		t.Fatalf("List returned an error instead of the stale list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %v, want the two cached names", got)
	}
}

func TestCatalogCacheReportsFailureWithNothingCached(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.CatalogFn = func(context.Context, int, string) (*registry.CatalogPage, error) {
		return nil, errors.New("registry unreachable")
	}
	c := newCatalogCache()
	if _, err := c.List(context.Background(), "local", f, 10, time.Minute); err == nil {
		t.Fatal("List succeeded with nothing cached and an unreachable registry")
	}
}

func TestCatalogCacheInvalidate(t *testing.T) {
	t.Parallel()

	f := newFakeClient()
	f.Repos = []string{"a"}
	c := newCatalogCache()
	ctx := context.Background()

	if _, err := c.List(ctx, "local", f, 10, time.Minute); err != nil {
		t.Fatalf("List failed: %v", err)
	}
	c.Invalidate("local")
	if _, err := c.List(ctx, "local", f, 10, time.Minute); err != nil {
		t.Fatalf("List after Invalidate failed: %v", err)
	}
	if f.CatalogCalls != 2 {
		t.Fatalf("the catalog was walked %d times, want 2 after an invalidation", f.CatalogCalls)
	}
}

func TestCatalogCacheKeepsRegistriesApart(t *testing.T) {
	t.Parallel()

	a, b := newFakeClient(), newFakeClient()
	a.Repos = []string{"from-a"}
	b.Repos = []string{"from-b"}
	c := newCatalogCache()
	ctx := context.Background()

	gotA, err := c.List(ctx, "a", a, 10, time.Minute)
	if err != nil {
		t.Fatalf("List(a) failed: %v", err)
	}
	gotB, err := c.List(ctx, "b", b, 10, time.Minute)
	if err != nil {
		t.Fatalf("List(b) failed: %v", err)
	}
	if gotA[0] != "from-a" || gotB[0] != "from-b" {
		t.Fatalf("the two registries share a cache entry: %v / %v", gotA, gotB)
	}
}

// A burst of concurrent requests must collapse into one walk, not one per
// caller.
func TestCatalogCacheCollapsesConcurrentLoads(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	var mu sync.Mutex
	calls := 0

	f := newFakeClient()
	f.CatalogFn = func(context.Context, int, string) (*registry.CatalogPage, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		once.Do(func() { close(started) })
		<-release
		return &registry.CatalogPage{Repositories: []registry.Repository{{Name: "a"}}}, nil
	}

	c := newCatalogCache()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.List(context.Background(), "local", f, 10, time.Minute)
		}()
	}
	<-started
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("the catalog was walked %d times for 8 concurrent readers, want 1", calls)
	}
}

func TestServerConcurrentRequests(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	f := fakes["local"]
	f.Repos = []string{"app", "team/api"}
	f.TagList["app"] = []string{"latest", "v1"}
	f.Imgs["app:latest"] = sampleImage("app", "latest")

	paths := []string{
		"/", "/healthz", "/r/local", "/r/local?q=app",
		"/r/local/repo/app", "/r/local/image/app?ref=latest",
		"/x/catalog/local", "/x/tags/local/app", "/x/tag/local/app?t=latest",
		"/x/repocount/local/app", "/x/health/local",
	}

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := get(t, s, paths[i%len(paths)])
			if rec.Code >= 500 {
				t.Errorf("GET %s = %d", paths[i%len(paths)], rec.Code)
			}
		}(i)
	}
	wg.Wait()
}

func TestAcquireHonoursCancellation(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)

	// Fill the per-registry limiter.
	releases := make([]func(), 0, perRegistryConcurrency)
	for range perRegistryConcurrency {
		release, ok := s.acquire(context.Background(), "local")
		if !ok {
			t.Fatal("acquire failed while the limiter still had room")
		}
		releases = append(releases, release)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := s.acquire(ctx, "local"); ok {
		t.Fatal("acquire succeeded on a cancelled context with the limiter full")
	}

	for _, release := range releases {
		release()
	}
	if _, ok := s.acquire(context.Background(), "local"); !ok {
		t.Fatal("acquire failed after every slot was released")
	}
}

func TestAcquireUnknownRegistryIsUnlimited(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	release, ok := s.acquire(context.Background(), "ghost")
	if !ok {
		t.Fatal("acquire on an unknown registry failed, want it to be a no-op")
	}
	release()
}

func TestHealthCacheExpiry(t *testing.T) {
	t.Parallel()

	h := newHealthCache()
	h.ttl = time.Millisecond
	h.set("local", healthState{State: "ok"})
	if _, ok := h.get("local"); !ok {
		t.Fatal("the entry is missing immediately after it was set")
	}

	// Age the entry instead of sleeping.
	h.mu.Lock()
	st := h.state["local"]
	st.At = time.Now().Add(-time.Hour)
	h.state["local"] = st
	h.mu.Unlock()

	if _, ok := h.get("local"); ok {
		t.Fatal("an entry older than the TTL was served")
	}
	if _, ok := h.get("unknown"); ok {
		t.Fatal("get on an unknown registry reported a hit")
	}
}

func TestRandomTokenAndAssetToken(t *testing.T) {
	t.Parallel()

	a, b := randomToken(), randomToken()
	if a == b {
		t.Fatal("randomToken returned the same value twice")
	}
	if len(a) != 32 {
		t.Fatalf("randomToken returned %d characters, want 32 hex characters", len(a))
	}
	if assetToken() == "" {
		t.Fatal("assetToken is empty; static assets would lose their cache-busting parameter")
	}
}

func TestNewNonceIsUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 64)
	for range 64 {
		n := newNonce()
		if n == "" {
			t.Fatal("newNonce returned an empty string")
		}
		if seen[n] {
			t.Fatalf("newNonce returned %q twice", n)
		}
		seen[n] = true
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	t.Parallel()

	raw := strings.Replace(testConfigYAML, `  addr: ":0"`, `  addr: "127.0.0.1:0"`, 1)
	s, _ := newTestServer(t, raw)

	// Listen ourselves so the port is known, then hand the address to Run.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port failed: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the port failed: %v", err)
	}
	s.cfg.Server.Addr = addr

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Poll until the listener is up rather than sleeping a fixed interval.
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/healthz")
		if err == nil {
			break
		}
	}
	if err != nil {
		cancel()
		t.Fatalf("the server never accepted a connection on %s: %v", addr, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after a clean shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of the context being cancelled")
	}
}

func TestRunReportsAListenFailure(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	s.cfg.Server.Addr = "127.0.0.1:not-a-port"

	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with an unusable listen address")
	}
}

func TestAccessLogRecordsTheStatus(t *testing.T) {
	t.Parallel()

	raw := strings.Replace(testConfigYAML, `  addr: ":0"`, "  addr: \":0\"\n  access_log: true", 1)
	s, _ := newTestServer(t, raw)

	var buf bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// An explicit WriteHeader, so the recorder has something to capture.
	assertStatus(t, get(t, s, "/r/ghost"), http.StatusNotFound)

	line := buf.String()
	for _, want := range []string{"status=404", "path=/r/ghost", "method=GET", "bytes="} {
		assertContains(t, line, want, "the access log line")
	}

	// Static assets are deliberately not logged.
	buf.Reset()
	get(t, s, "/static/app.css")
	if buf.Len() != 0 {
		t.Errorf("a static asset produced an access log line: %s", buf.String())
	}
}
