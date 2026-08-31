package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

const digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// sampleImage is a single-platform image with everything the image page draws.
func sampleImage(repo, ref string) *registry.Image {
	return &registry.Image{
		Repository:   repo,
		Reference:    ref,
		Digest:       digestA,
		MediaType:    registry.MediaTypeOCIManifest,
		ManifestSize: 512,
		TotalSize:    3500,
		RawManifest:  []byte(`{"schemaVersion":2}`),
		Config: &registry.ImageConfig{
			Architecture: "amd64",
			OS:           "linux",
			Created:      time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
			Author:       "team",
			User:         "1000",
			WorkingDir:   "/srv",
			Entrypoint:   []string{"/entrypoint.sh"},
			Cmd:          []string{"--serve"},
			ExposedPorts: []string{"8080/tcp"},
			Volumes:      []string{"/data"},
			Env:          []string{"PATH=/usr/bin"},
			Labels:       map[string]string{"maintainer": "team"},
			Digest:       digestB,
			Size:         500,
		},
		Layers: []registry.Layer{
			{Digest: digestA, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 3000, Command: "ADD file:a in /"},
			{Digest: digestB, MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 0},
		},
	}
}

// sampleIndex is a two-platform index plus an attestation child.
func sampleIndex(repo, ref string) *registry.Image {
	child := sampleImage(repo, digestA)
	return &registry.Image{
		Repository:  repo,
		Reference:   ref,
		Digest:      digestB,
		MediaType:   registry.MediaTypeOCIIndex,
		IsIndex:     true,
		TotalSize:   4000,
		RawManifest: []byte(`{"schemaVersion":2,"manifests":[]}`),
		Children: []registry.IndexChild{
			{
				Descriptor: registry.Descriptor{
					MediaType: registry.MediaTypeOCIManifest, Digest: digestA, Size: 100,
					Platform: &registry.Platform{OS: "linux", Architecture: "amd64"},
				},
				Resolved:  child,
				SizeTotal: 3500,
			},
			{
				Descriptor: registry.Descriptor{
					MediaType: registry.MediaTypeOCIManifest, Digest: digestB, Size: 50,
					Platform: &registry.Platform{OS: "unknown", Architecture: "unknown"},
				},
			},
		},
	}
}

// --- routing ----------------------------------------------------------------

func TestRoutes(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	f := fakes["local"]
	f.Repos = []string{"team/api", "team/web"}
	f.TagList["team/api"] = []string{"latest", "v1"}
	f.Imgs["team/api:latest"] = sampleImage("team/api", "latest")

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantType   string
	}{
		{name: "healthz", method: "GET", path: "/healthz", wantStatus: 200, wantType: "application/json"},
		{name: "index redirects", method: "GET", path: "/", wantStatus: http.StatusFound},
		{name: "catalog", method: "GET", path: "/r/local", wantStatus: 200, wantType: "text/html"},
		{name: "repository", method: "GET", path: "/r/local/repo/team/api", wantStatus: 200, wantType: "text/html"},
		{name: "image", method: "GET", path: "/r/local/image/team/api?ref=latest", wantStatus: 200, wantType: "text/html"},
		{name: "catalog rows fragment", method: "GET", path: "/x/catalog/local", wantStatus: 200, wantType: "text/html"},
		{name: "repo count fragment", method: "GET", path: "/x/repocount/local/team/api", wantStatus: 200, wantType: "text/html"},
		{name: "tag rows fragment", method: "GET", path: "/x/tags/local/team/api", wantStatus: 200, wantType: "text/html"},
		{name: "tag row fragment", method: "GET", path: "/x/tag/local/team/api?t=latest", wantStatus: 200, wantType: "text/html"},
		{name: "health fragment", method: "GET", path: "/x/health/local", wantStatus: 200, wantType: "text/html"},
		{name: "static asset", method: "GET", path: "/static/app.css", wantStatus: 200},
		{name: "unknown path", method: "GET", path: "/nope", wantStatus: 404},
		{name: "unknown static asset", method: "GET", path: "/static/nope.css", wantStatus: 404},
		{name: "index does not match a longer path", method: "GET", path: "/x", wantStatus: 404},
		{name: "GET on the delete route", method: "GET", path: "/r/local/delete", wantStatus: 405},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, httptest.NewRequest(tc.method, tc.path, nil))
			assertStatus(t, rec, tc.wantStatus)
			if tc.wantType != "" {
				if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.wantType) {
					t.Errorf("Content-Type = %q, want it to contain %q", got, tc.wantType)
				}
			}
		})
	}
}

func TestIndexRedirectsToTheDefaultRegistry(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := get(t, s, "/")
	assertStatus(t, rec, http.StatusFound)
	if got, want := rec.Header().Get("Location"), "/r/local"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestUnknownRegistryIs404(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	paths := []string{
		"/r/ghost",
		"/r/ghost/repo/app",
		"/r/ghost/image/app",
		"/x/catalog/ghost",
		"/x/tags/ghost/app",
		"/x/tag/ghost/app?t=latest",
		"/x/repocount/ghost/app",
		"/x/health/ghost",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rec := get(t, s, p)
			assertStatus(t, rec, http.StatusNotFound)
			assertContains(t, body(t, rec), "No such registry", "the 404 page")
		})
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := get(t, s, "/healthz")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var doc struct {
		Status     string `json:"status"`
		Version    string `json:"version"`
		Registries int    `json:"registries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the health document is not JSON: %v\n%s", err, rec.Body.String())
	}
	if doc.Status != "ok" {
		t.Errorf("status = %q, want ok", doc.Status)
	}
	if doc.Registries != 2 {
		t.Errorf("registries = %d, want 2", doc.Registries)
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := get(t, s, "/r/local")

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		"X-Frame-Options":        "DENY",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'none'", "script-src 'self'", "img-src 'self' data:",
		"form-action 'self'", "base-uri 'none'", "frame-ancestors 'none'",
	} {
		assertContains(t, csp, directive, "the Content-Security-Policy")
	}
	assertNotContains(t, csp, "unsafe-inline", "the Content-Security-Policy")
	if !strings.Contains(csp, "style-src 'self' 'nonce-") {
		t.Errorf("the CSP does not carry a style nonce: %q", csp)
	}
}

func TestNonceIsPerRequestAndMatchesThePage(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Imgs["app:latest"] = sampleImage("app", "latest")

	nonces := make(map[string]bool)
	for range 3 {
		rec := get(t, s, "/r/local/image/app?ref=latest")
		assertStatus(t, rec, http.StatusOK)
		csp := rec.Header().Get("Content-Security-Policy")
		_, rest, ok := strings.Cut(csp, "'nonce-")
		if !ok {
			t.Fatalf("no nonce in the CSP: %q", csp)
		}
		nonce, _, _ := strings.Cut(rest, "'")
		if nonce == "" {
			t.Fatal("the CSP nonce is empty")
		}
		if nonces[nonce] {
			t.Fatalf("nonce %q was reused across requests", nonce)
		}
		nonces[nonce] = true

		// The attribute goes through HTML escaping, so compare the decoded
		// value: that is what the browser's CSP check sees.
		html := body(t, rec)
		_, after, ok := strings.Cut(html, `<style nonce="`)
		if !ok {
			t.Fatal("the rendered page has no nonced <style> element")
		}
		attr, _, _ := strings.Cut(after, `"`)
		if got := htmlpkg.UnescapeString(attr); got != nonce {
			t.Fatalf("the page carries nonce %q but the CSP advertises %q", got, nonce)
		}
	}
}

// --- catalog ----------------------------------------------------------------

func TestCatalogSearchFilter(t *testing.T) {
	t.Parallel()

	repos := []string{"team/backend-api", "team/frontend", "other/backend", "TEAM/Backend-Worker"}

	tests := []struct {
		name  string
		query string
		want  []string
		gone  []string
	}{
		{name: "no filter shows everything", query: "", want: repos},
		{
			name:  "single term",
			query: "backend",
			want:  []string{"team/backend-api", "other/backend", "TEAM/Backend-Worker"},
			gone:  []string{"team/frontend"},
		},
		{
			name:  "case-insensitive",
			query: "BACKEND",
			want:  []string{"other/backend", "TEAM/Backend-Worker"},
		},
		{
			name:  "two terms behave as AND",
			query: "team backend",
			want:  []string{"team/backend-api", "TEAM/Backend-Worker"},
			gone:  []string{"other/backend", "team/frontend"},
		},
		{
			name:  "a term that matches nothing",
			query: "team nothingmatches",
			gone:  repos,
		},
		{name: "whitespace only is no filter", query: "   ", want: repos},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, fakes := newTestServer(t, strings.Replace(testConfigYAML, "catalog_page_size: 3", "catalog_page_size: 50", 1))
			fakes["local"].Repos = repos

			rec := get(t, s, "/r/local?q="+urlQueryEscape(tc.query))
			assertStatus(t, rec, http.StatusOK)
			html := body(t, rec)
			for _, want := range tc.want {
				assertContains(t, html, want, "the filtered catalog")
			}
			for _, gone := range tc.gone {
				assertNotContains(t, html, gone, "the filtered catalog")
			}
		})
	}
}

func TestFilterRepos(t *testing.T) {
	t.Parallel()

	repos := []string{"a/one", "a/two", "b/one"}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "empty query returns the input slice", query: "", want: repos},
		{name: "one term", query: "one", want: []string{"a/one", "b/one"}},
		{name: "two terms", query: "a one", want: []string{"a/one"}},
		{name: "no match", query: "zzz", want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := filterRepos(repos, tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("filterRepos(%q) = %v, want %v", tc.query, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("filterRepos(%q) = %v, want %v", tc.query, got, tc.want)
				}
			}
		})
	}
}

func TestCatalogPagination(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML) // catalog_page_size: 3
	fakes["local"].Repos = []string{"r1", "r2", "r3", "r4", "r5"}

	first := get(t, s, "/r/local")
	assertStatus(t, first, http.StatusOK)
	html := body(t, first)
	for _, want := range []string{"r1", "r2", "r3"} {
		assertContains(t, html, ">"+want+"<", "the first catalog page")
	}
	assertNotContains(t, html, ">r4<", "the first catalog page")
	assertContains(t, html, "Load more", "the first catalog page")
	assertContains(t, html, "o=3", "the load-more URL")

	second := get(t, s, "/x/catalog/local?o=3")
	assertStatus(t, second, http.StatusOK)
	html = body(t, second)
	for _, want := range []string{"r4", "r5"} {
		assertContains(t, html, ">"+want+"<", "the second catalog page")
	}
	assertNotContains(t, html, ">r1<", "the second catalog page")
	assertNotContains(t, html, "Load more", "the last catalog page")
}

func TestCatalogOffsetBeyondTheEnd(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{"r1", "r2"}

	for _, path := range []string{"/x/catalog/local?o=99", "/x/catalog/local?o=-1", "/x/catalog/local?o=abc"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, s, path)
			assertStatus(t, rec, http.StatusOK)
		})
	}
}

func TestIntParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		def   int
		want  int
	}{
		{name: "absent", query: "", def: 7, want: 7},
		{name: "valid", query: "?o=12", def: 7, want: 12},
		{name: "zero", query: "?o=0", def: 7, want: 0},
		{name: "negative falls back", query: "?o=-1", def: 7, want: 7},
		{name: "not a number falls back", query: "?o=x", def: 7, want: 7},
		{name: "empty value falls back", query: "?o=", def: 7, want: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/"+tc.query, nil)
			if got := intParam(r, "o", tc.def); got != tc.want {
				t.Fatalf("intParam = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCatalogRowsPushesTheFilterIntoTheURL(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{"api"}

	req := httptest.NewRequest(http.MethodGet, "/x/catalog/local?q=api", nil)
	req.Header.Set("HX-Request", "true")
	rec := do(t, s, req)
	assertStatus(t, rec, http.StatusOK)
	if got, want := rec.Header().Get("HX-Push-Url"), "/r/local?q=api"; got != want {
		t.Fatalf("HX-Push-Url = %q, want %q", got, want)
	}

	// Without the HTMX header there is nothing to push.
	rec = get(t, s, "/x/catalog/local?q=api")
	if got := rec.Header().Get("HX-Push-Url"); got != "" {
		t.Fatalf("HX-Push-Url = %q on a non-HTMX request, want it absent", got)
	}
}

func TestCatalogUnreachableRegistryShowsABanner(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].CatalogFn = func(context.Context, int, string) (*registry.CatalogPage, error) {
		return nil, fmt.Errorf("dial tcp: %w", registry.ErrUnauthorized)
	}

	rec := get(t, s, "/r/local")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)
	assertContains(t, html, "Cannot reach this registry", "the catalog page")
	assertContains(t, html, "The registry rejected the configured credentials.", "the catalog page")
}

// --- fragments --------------------------------------------------------------

func TestFragmentsAreNotFullPages(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	f := fakes["local"]
	f.Repos = []string{"app"}
	f.TagList["app"] = []string{"latest"}

	fragments := []string{
		"/x/catalog/local",
		"/x/repocount/local/app",
		"/x/tags/local/app",
		"/x/tag/local/app?t=latest",
		"/x/health/local",
	}
	for _, path := range fragments {
		t.Run(path, func(t *testing.T) {
			rec := get(t, s, path)
			assertStatus(t, rec, http.StatusOK)
			html := body(t, rec)
			for _, marker := range []string{"<!doctype html>", "<html", "<head>", "</body>"} {
				assertNotContains(t, html, marker, "a fragment response")
			}
			if strings.TrimSpace(html) == "" {
				t.Error("the fragment is empty")
			}
		})
	}
}

func TestRepoCountFragment(t *testing.T) {
	t.Parallel()

	t.Run("plain count", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].TagList["app"] = []string{"a", "b", "c"}
		rec := get(t, s, "/x/repocount/local/app")
		assertStatus(t, rec, http.StatusOK)
		assertContains(t, body(t, rec), ">3<", "the repo count fragment")
	})

	t.Run("more pages get a plus", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].TagsFn = func(_ context.Context, repo string, _ int, _ string) (*registry.TagPage, error) {
			return &registry.TagPage{Repository: repo, Tags: []string{"a"}, NextLast: "a"}, nil
		}
		rec := get(t, s, "/x/repocount/local/app")
		// html/template escapes the plus as a numeric entity.
		assertContains(t, body(t, rec), "1&#43;", "the repo count fragment")
	})

	t.Run("failure shows an error chip", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].TagsFn = func(context.Context, string, int, string) (*registry.TagPage, error) {
			return nil, registry.ErrNotFound
		}
		rec := get(t, s, "/x/repocount/local/app")
		assertStatus(t, rec, http.StatusOK)
		assertContains(t, body(t, rec), "error", "the repo count fragment")
	})
}

func TestTagRowFragment(t *testing.T) {
	t.Parallel()

	t.Run("resolved row", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].SummaryFn = func(_ context.Context, _, tag string) *registry.TagSummary {
			return &registry.TagSummary{
				Name:      tag,
				Digest:    digestA,
				Size:      2048,
				IsIndex:   true,
				Created:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				Platforms: []registry.Platform{{OS: "linux", Architecture: "amd64"}},
			}
		}
		rec := get(t, s, "/x/tag/local/app?t=latest")
		assertStatus(t, rec, http.StatusOK)
		html := body(t, rec)
		assertContains(t, html, "linux/amd64", "the tag row")
		assertContains(t, html, "2 KB", "the tag row")
		assertContains(t, html, "index", "the tag row")
	})

	t.Run("degraded row", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].SummaryFn = func(_ context.Context, _, tag string) *registry.TagSummary {
			return &registry.TagSummary{Name: tag, Err: "manifest unreadable"}
		}
		rec := get(t, s, "/x/tag/local/app?t=broken")
		assertStatus(t, rec, http.StatusOK)
		html := body(t, rec)
		assertContains(t, html, "unreadable", "the degraded tag row")
		assertContains(t, html, "is-degraded", "the degraded tag row")
	})

	t.Run("a summary with no platforms still renders", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].SummaryFn = func(_ context.Context, _, tag string) *registry.TagSummary {
			return &registry.TagSummary{Name: tag}
		}
		rec := get(t, s, "/x/tag/local/app?t=bare")
		assertStatus(t, rec, http.StatusOK)
	})
}

func TestTagRowsFragmentPagination(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].TagsFn = func(_ context.Context, repo string, _ int, last string) (*registry.TagPage, error) {
		if last == "" {
			return &registry.TagPage{Repository: repo, Tags: []string{"v1", "v2"}, NextLast: "v2"}, nil
		}
		return &registry.TagPage{Repository: repo, Tags: []string{"v3"}}, nil
	}

	rec := get(t, s, "/r/local/repo/app")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)
	assertContains(t, html, "/x/tags/local/app?last=v2", "the load-more URL")

	rec = get(t, s, "/x/tags/local/app?last=v2")
	assertStatus(t, rec, http.StatusOK)
	html = body(t, rec)
	assertContains(t, html, "v3", "the second tag page")
	assertNotContains(t, html, "Load more", "the last tag page")
}

func TestTagRowsFragmentFailure(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].TagsFn = func(context.Context, string, int, string) (*registry.TagPage, error) {
		return nil, errors.New("upstream exploded")
	}
	rec := get(t, s, "/x/tags/local/app")
	assertStatus(t, rec, http.StatusBadGateway)
	assertContains(t, body(t, rec), "upstream exploded", "the fragment error")
}

func TestRepositoryNotFound(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := get(t, s, "/r/local/repo/ghost")
	assertStatus(t, rec, http.StatusNotFound)
	assertContains(t, body(t, rec), "No such repository", "the repository 404 page")
}

func TestRepositoryOtherFailureShowsABanner(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].TagsFn = func(context.Context, string, int, string) (*registry.TagPage, error) {
		return nil, errors.New("registry is on fire")
	}
	rec := get(t, s, "/r/local/repo/app")
	assertStatus(t, rec, http.StatusOK)
	assertContains(t, body(t, rec), "Cannot list tags", "the repository page")
}

func TestRegistryHealthFragment(t *testing.T) {
	t.Parallel()

	t.Run("reachable", func(t *testing.T) {
		s, _ := newTestServer(t, testConfigYAML)
		rec := get(t, s, "/x/health/local")
		assertStatus(t, rec, http.StatusOK)
		assertContains(t, body(t, rec), "dot--ok", "the health dot")
	})

	t.Run("unreachable", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].PingFn = func(context.Context) error { return registry.ErrUnauthorized }
		rec := get(t, s, "/x/health/local")
		assertStatus(t, rec, http.StatusOK)
		html := body(t, rec)
		assertContains(t, html, "dot--down", "the health dot")
		assertContains(t, html, "rejected the configured credentials", "the health dot title")
	})

	t.Run("the result is cached", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		var pings int
		fakes["local"].PingFn = func(context.Context) error { pings++; return nil }
		for range 3 {
			get(t, s, "/x/health/local")
		}
		if pings != 1 {
			t.Fatalf("the registry was pinged %d times, want 1: the result is cached", pings)
		}
	})
}

// --- image page -------------------------------------------------------------

func TestImagePage(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Imgs["team/api:v1"] = sampleImage("team/api", "v1")

	rec := get(t, s, "/r/local/image/team/api?ref=v1")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)

	assertContains(t, html, "docker pull reg.test/team/api:v1", "the pull command")
	assertContains(t, html, "linux/amd64", "the platform row")
	assertContains(t, html, "ADD file:a in /", "the layer command")
	assertContains(t, html, "maintainer", "the labels panel")
	assertContains(t, html, "PATH", "the environment panel")
	assertContains(t, html, "8080/tcp", "the configuration panel")
	// The registry is read-only, so no delete form.
	assertNotContains(t, html, "Delete manifest", "a read-only registry's image page")
}

func TestImagePageDefaultsToLatest(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	var asked string
	fakes["local"].ImageFn = func(_ context.Context, repo, ref string, _ bool) (*registry.Image, error) {
		asked = ref
		return sampleImage(repo, ref), nil
	}
	rec := get(t, s, "/r/local/image/app")
	assertStatus(t, rec, http.StatusOK)
	if asked != "latest" {
		t.Fatalf("the handler asked for reference %q, want latest", asked)
	}
}

func TestImagePageIndex(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Imgs["app:multi"] = sampleIndex("app", "multi")

	rec := get(t, s, "/r/local/image/app?ref=multi")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)

	assertContains(t, html, "Image index", "the index page")
	assertContains(t, html, "Platform manifests", "the index page")
	assertContains(t, html, "attestation", "the attestation child")
	assertContains(t, html, "linux/amd64", "the platform list")
	// The layer breakdown is borrowed from a representative child.
	assertContains(t, html, "ADD file:a in /", "the borrowed layer list")
}

func TestImagePageErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantText   string
	}{
		{name: "not found", err: registry.ErrNotFound, wantStatus: http.StatusNotFound, wantText: "No such image"},
		{name: "schema 1", err: registry.ErrUnsupported, wantStatus: http.StatusUnsupportedMediaType, wantText: "Unsupported manifest"},
		{name: "anything else", err: errors.New("boom"), wantStatus: http.StatusBadGateway, wantText: "Cannot read this image"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, fakes := newTestServer(t, testConfigYAML)
			fakes["local"].ImageFn = func(context.Context, string, string, bool) (*registry.Image, error) {
				return nil, tc.err
			}
			rec := get(t, s, "/r/local/image/app?ref=x")
			assertStatus(t, rec, tc.wantStatus)
			assertContains(t, body(t, rec), tc.wantText, "the error page")
		})
	}
}

func TestLayerViews(t *testing.T) {
	t.Parallel()

	t.Run("no layers", func(t *testing.T) {
		t.Parallel()
		if got := layerViews(nil); got != nil {
			t.Fatalf("layerViews(nil) = %v, want nil", got)
		}
		if got := layerCSS(nil); got != "" {
			t.Fatalf("layerCSS(nil) = %q, want empty", got)
		}
	})

	t.Run("the three largest layers are highlighted", func(t *testing.T) {
		t.Parallel()
		layers := []registry.Layer{
			{Size: 10}, {Size: 500}, {Size: 300}, {Size: 200}, {Size: 5},
		}
		views := layerViews(layers)
		if len(views) != len(layers) {
			t.Fatalf("layerViews returned %d views for %d layers", len(views), len(layers))
		}
		var hot []int
		for _, v := range views {
			if v.Hot {
				hot = append(hot, v.Index)
			}
		}
		// Indexes are 1-based: 500, 300 and 200 are entries 2, 3 and 4.
		if len(hot) != 3 || hot[0] != 2 || hot[1] != 3 || hot[2] != 4 {
			t.Fatalf("highlighted layers = %v, want [2 3 4]", hot)
		}
	})

	t.Run("a tiny layer is never highlighted", func(t *testing.T) {
		t.Parallel()
		layers := []registry.Layer{{Size: 1000}, {Size: 1}, {Size: 1}}
		for _, v := range layerViews(layers) {
			if v.Index > 1 && v.Hot {
				t.Errorf("layer %d is %d bytes of %d and should not be highlighted", v.Index, v.Size, 1002)
			}
		}
	})

	t.Run("all-zero sizes do not divide by zero", func(t *testing.T) {
		t.Parallel()
		views := layerViews([]registry.Layer{{Size: 0}, {Size: 0}})
		for _, v := range views {
			if v.Grow != 1.0 {
				t.Errorf("layer %d grow = %v, want the 1.0 fallback", v.Index, v.Grow)
			}
		}
	})

	t.Run("the stylesheet only contains formatted numbers", func(t *testing.T) {
		t.Parallel()
		css := string(layerCSS(layerViews([]registry.Layer{{Size: 1}, {Size: 3}})))
		assertContains(t, css, `.layerbar__seg[data-layer="1"]{flex-grow:`, "the layer stylesheet")
		assertContains(t, css, `.layerbar__seg[data-layer="2"]{flex-grow:`, "the layer stylesheet")
		for _, bad := range []string{"<", ">", "\"}", "expression("} {
			assertNotContains(t, css, bad, "the layer stylesheet")
		}
	})
}

func TestRepresentativeChild(t *testing.T) {
	t.Parallel()

	withLayers := func(os, arch string, resolved bool) registry.IndexChild {
		c := registry.IndexChild{Descriptor: registry.Descriptor{
			Digest:   digestA,
			Platform: &registry.Platform{OS: os, Architecture: arch},
		}}
		if resolved {
			c.Resolved = &registry.Image{Layers: []registry.Layer{{Size: 1}}}
		}
		return c
	}

	t.Run("prefers linux/amd64", func(t *testing.T) {
		t.Parallel()
		img := &registry.Image{Children: []registry.IndexChild{
			withLayers("linux", "arm64", true),
			withLayers("linux", "amd64", true),
		}}
		got := representativeChild(img)
		if got == nil || got.Platform.Architecture != "amd64" {
			t.Fatalf("representativeChild = %+v, want the linux/amd64 child", got)
		}
	})

	t.Run("falls back to the first usable child", func(t *testing.T) {
		t.Parallel()
		img := &registry.Image{Children: []registry.IndexChild{
			withLayers("unknown", "unknown", true),
			withLayers("linux", "arm64", true),
		}}
		got := representativeChild(img)
		if got == nil || got.Platform.Architecture != "arm64" {
			t.Fatalf("representativeChild = %+v, want the linux/arm64 child", got)
		}
	})

	t.Run("nothing usable", func(t *testing.T) {
		t.Parallel()
		img := &registry.Image{Children: []registry.IndexChild{
			withLayers("linux", "amd64", false),
			{Descriptor: registry.Descriptor{Digest: digestA}, Resolved: &registry.Image{}},
		}}
		if got := representativeChild(img); got != nil {
			t.Fatalf("representativeChild = %+v, want nil", got)
		}
	})

	t.Run("no children", func(t *testing.T) {
		t.Parallel()
		if got := representativeChild(&registry.Image{}); got != nil {
			t.Fatalf("representativeChild = %+v, want nil", got)
		}
	})
}

func TestPrettyJSON(t *testing.T) {
	t.Parallel()

	if got := prettyJSON(nil); got != "" {
		t.Errorf("prettyJSON(nil) = %q, want empty", got)
	}
	if got := prettyJSON([]byte(`{"a":1}`)); got != "{\n  \"a\": 1\n}" {
		t.Errorf("prettyJSON = %q", got)
	}
	// Anything that is not JSON is shown as-is rather than swallowed.
	if got := prettyJSON([]byte(`not json`)); got != "not json" {
		t.Errorf("prettyJSON(non-JSON) = %q, want it passed through", got)
	}
}

func TestFriendlyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "unauthorized", err: fmt.Errorf("wrapped: %w", registry.ErrUnauthorized), want: "The registry rejected the configured credentials."},
		{name: "not found", err: registry.ErrNotFound, want: "The registry returned 404 for this request."},
		{name: "delete denied", err: registry.ErrDeleteDenied, want: "Deletion is not enabled for this registry."},
		{name: "anything else is passed through", err: errors.New("dial tcp: refused"), want: "dial tcp: refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := friendlyError(tc.err); got != tc.want {
				t.Fatalf("friendlyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// --- delete -----------------------------------------------------------------

func TestDeleteEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("rejected on a read-only registry", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/local/delete",
			map[string]string{"csrf": s.csrf, "repo": "app", "digest": digestA}, nil)
		assertStatus(t, rec, http.StatusForbidden)
		assertContains(t, body(t, rec), "Deletion is disabled", "the rejection page")
		if got := fakes["local"].deletes(); len(got) != 0 {
			t.Fatalf("the client was asked to delete %v on a read-only registry", got)
		}
	})

	t.Run("rejected without a CSRF token", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"repo": "app", "digest": digestA}, nil)
		assertStatus(t, rec, http.StatusForbidden)
		assertContains(t, body(t, rec), "Request rejected", "the rejection page")
		if got := fakes["other"].deletes(); len(got) != 0 {
			t.Fatalf("the client was asked to delete %v without a CSRF token", got)
		}
	})

	t.Run("rejected with the wrong CSRF token", func(t *testing.T) {
		s, _ := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": "not-the-token", "repo": "app", "digest": digestA}, nil)
		assertStatus(t, rec, http.StatusForbidden)
	})

	t.Run("rejected cross-site", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": s.csrf, "repo": "app", "digest": digestA},
			map[string]string{"Sec-Fetch-Site": "cross-site"})
		assertStatus(t, rec, http.StatusForbidden)
		if got := fakes["other"].deletes(); len(got) != 0 {
			t.Fatalf("a cross-site request deleted %v", got)
		}
	})

	t.Run("rejected same-site but not same-origin", func(t *testing.T) {
		s, _ := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": s.csrf, "repo": "app", "digest": digestA},
			map[string]string{"Sec-Fetch-Site": "same-site"})
		assertStatus(t, rec, http.StatusForbidden)
	})

	t.Run("accepted when Sec-Fetch-Site is absent", func(t *testing.T) {
		// Not every client sends the header; the CSRF token covers those.
		s, fakes := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": s.csrf, "repo": "app", "digest": digestA},
			map[string]string{"Sec-Fetch-Site": ""})
		assertStatus(t, rec, http.StatusSeeOther)
		if got := fakes["other"].deletes(); len(got) != 1 {
			t.Fatalf("deletes = %v, want exactly one", got)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": s.csrf, "repo": "team/app", "digest": digestA}, nil)
		assertStatus(t, rec, http.StatusSeeOther)
		if got, want := rec.Header().Get("Location"), "/r/other/repo/team/app"; got != want {
			t.Errorf("Location = %q, want %q", got, want)
		}
		want := []string{"team/app@" + digestA}
		got := fakes["other"].deletes()
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("deletes = %v, want %v", got, want)
		}
	})

	t.Run("incomplete form", func(t *testing.T) {
		s, _ := newTestServer(t, testConfigYAML)
		for _, form := range []map[string]string{
			{"csrf": s.csrf, "digest": digestA},
			{"csrf": s.csrf, "repo": "app"},
			{"csrf": s.csrf},
		} {
			rec := postForm(t, s, "/r/other/delete", form, nil)
			assertStatus(t, rec, http.StatusBadRequest)
			assertContains(t, body(t, rec), "Incomplete request", "the rejection page")
		}
	})

	t.Run("the registry refuses", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["other"].DeleteFn = func(context.Context, string, string) error {
			return registry.ErrDeleteDenied
		}
		rec := postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": s.csrf, "repo": "app", "digest": digestA}, nil)
		assertStatus(t, rec, http.StatusBadGateway)
		html := body(t, rec)
		assertContains(t, html, "Delete failed", "the failure page")
		assertContains(t, html, "Deletion is not enabled for this registry.", "the failure page")
	})

	t.Run("deleting invalidates the catalog cache", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		f := fakes["other"]
		f.Repos = []string{"app"}

		get(t, s, "/r/other")
		get(t, s, "/r/other")
		if f.CatalogCalls != 1 {
			t.Fatalf("the catalog was walked %d times before the delete, want 1", f.CatalogCalls)
		}
		postForm(t, s, "/r/other/delete",
			map[string]string{"csrf": s.csrf, "repo": "app", "digest": digestA}, nil)
		get(t, s, "/r/other")
		if f.CatalogCalls != 2 {
			t.Fatalf("the catalog was walked %d times in total, want 2: the delete must invalidate it", f.CatalogCalls)
		}
	})
}

func TestDeleteFormIsRenderedOnAWritableRegistry(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["other"].Imgs["app:latest"] = sampleImage("app", "latest")

	rec := get(t, s, "/r/other/image/app?ref=latest")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)
	assertContains(t, html, "Delete manifest", "a writable registry's image page")
	assertContains(t, html, `name="csrf" value="`+s.csrf+`"`, "the delete form")
}

func TestSameOriginPost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		site  string
		token string
		want  bool
	}{
		{name: "same-origin with the right token", site: "same-origin", token: "tok", want: true},
		{name: "no Sec-Fetch-Site header", site: "", token: "tok", want: true},
		{name: "cross-site", site: "cross-site", token: "tok", want: false},
		{name: "same-site", site: "same-site", token: "tok", want: false},
		{name: "none", site: "none", token: "tok", want: false},
		{name: "wrong token", site: "same-origin", token: "nope", want: false},
		{name: "no token", site: "same-origin", token: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("csrf="+tc.token))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if got := sameOriginPost(req, "tok"); got != tc.want {
				t.Fatalf("sameOriginPost = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- theme ------------------------------------------------------------------

func TestThemeToggle(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := postForm(t, s, "/x/theme", map[string]string{"to": "light", "return": "/r/local"}, nil)
	assertStatus(t, rec, http.StatusSeeOther)
	if got, want := rec.Header().Get("Location"), "/r/local"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("the response set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != "mrui_theme" || c.Value != "light" {
		t.Errorf("cookie = %s=%s, want mrui_theme=light", c.Name, c.Value)
	}
	if c.Path != "/" {
		t.Errorf("cookie path = %q, want /", c.Path)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", c.SameSite)
	}
}

func TestThemeToggleInvalidValueDefaultsToDark(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	for _, to := range []string{"", "neon", "AUTO", "<script>"} {
		rec := postForm(t, s, "/x/theme", map[string]string{"to": to}, nil)
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Value != "dark" {
			t.Fatalf("to=%q set the cookie to %v, want dark", to, cookies)
		}
	}
}

func TestThemeReturnIsNotAnOpenRedirect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		ret  string
		want string
	}{
		{name: "empty", base: "", ret: "", want: "/r/local"},
		{name: "protocol relative", base: "", ret: "//evil.com", want: "/r/local"},
		{name: "protocol relative with a path", base: "", ret: "//evil.com/x", want: "/r/local"},
		{name: "absolute https", base: "", ret: "https://evil.com", want: "/r/local"},
		{name: "absolute http", base: "", ret: "http://evil.com/x", want: "/r/local"},
		{name: "scheme relative with credentials", base: "", ret: "//user@evil.com", want: "/r/local"},
		{name: "javascript scheme", base: "", ret: "javascript:alert(1)", want: "/r/local"},
		{name: "relative without a leading slash", base: "", ret: "r/local", want: "/r/local"},
		{name: "a same-origin path is kept", base: "", ret: "/r/other/repo/app", want: "/r/other/repo/app"},
		{name: "outside the base path", base: "/registry", ret: "/elsewhere", want: "/registry/r/local"},
		{name: "the bare base path is not inside it", base: "/registry", ret: "/registry", want: "/registry/r/local"},
		{name: "a path inside the base path is kept", base: "/registry", ret: "/registry/r/local", want: "/registry/r/local"},
		{name: "a lookalike prefix is rejected", base: "/registry", ret: "/registryevil/x", want: "/registry/r/local"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := testConfigYAML
			if tc.base != "" {
				raw = strings.Replace(raw, `  addr: ":0"`, "  addr: \":0\"\n  base_path: "+tc.base, 1)
			}
			s, _ := newTestServer(t, raw)
			if got := s.safeReturn(tc.ret); got != tc.want {
				t.Fatalf("safeReturn(%q) with base %q = %q, want %q", tc.ret, tc.base, got, tc.want)
			}
		})
	}
}

// Browsers following the WHATWG URL rules treat a backslash as a slash, so a
// Location of "/\evil.com" is parsed as "//evil.com" and leaves the origin.
func TestThemeReturnBackslashIsNotAnOpenRedirect(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	for _, ret := range []string{`/\evil.com`, `/\/evil.com`, "/\t/evil.com"} {
		if got := s.safeReturn(ret); got != "/r/local" {
			t.Errorf("safeReturn(%q) = %q, want the fallback /r/local", ret, got)
		}
	}
}

func TestThemeCookieDrivesTheRenderedTheme(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cookie     string
		configured string
		want       string
	}{
		{name: "no cookie, auto config", cookie: "", configured: "auto", want: "dark"},
		{name: "no cookie, light config", cookie: "", configured: "light", want: "light"},
		{name: "cookie wins over config", cookie: "light", configured: "dark", want: "light"},
		{name: "dark cookie", cookie: "dark", configured: "light", want: "dark"},
		{name: "a nonsense cookie is ignored", cookie: "neon", configured: "light", want: "light"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(testConfigYAML, "  title: Test UI", "  title: Test UI\n  theme: "+tc.configured, 1)
			s, _ := newTestServer(t, raw)
			req := httptest.NewRequest(http.MethodGet, "/r/local", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "mrui_theme", Value: tc.cookie})
			}
			rec := do(t, s, req)
			assertStatus(t, rec, http.StatusOK)
			assertContains(t, body(t, rec), `data-theme="`+tc.want+`"`, "the rendered page")
		})
	}
}

// --- base path --------------------------------------------------------------

func TestBasePathRouting(t *testing.T) {
	t.Parallel()

	raw := strings.Replace(testConfigYAML, `  addr: ":0"`, "  addr: \":0\"\n  base_path: /registry", 1)
	s, fakes := newTestServer(t, raw)
	fakes["local"].Repos = []string{"app"}

	t.Run("the bare prefix redirects", func(t *testing.T) {
		rec := get(t, s, "/registry")
		assertStatus(t, rec, http.StatusMovedPermanently)
		if got := rec.Header().Get("Location"); got != "/registry/" {
			t.Fatalf("Location = %q, want /registry/", got)
		}
	})

	t.Run("the index under the prefix redirects to the default registry", func(t *testing.T) {
		rec := get(t, s, "/registry/")
		assertStatus(t, rec, http.StatusFound)
		if got := rec.Header().Get("Location"); got != "/registry/r/local" {
			t.Fatalf("Location = %q, want /registry/r/local", got)
		}
	})

	t.Run("pages render under the prefix", func(t *testing.T) {
		rec := get(t, s, "/registry/r/local")
		assertStatus(t, rec, http.StatusOK)
		html := body(t, rec)
		assertContains(t, html, `href="/registry/static/app.css`, "asset URLs")
		assertContains(t, html, `/registry/r/local/repo/app`, "repository links")
	})

	t.Run("the unprefixed path is not served", func(t *testing.T) {
		rec := get(t, s, "/r/local")
		assertStatus(t, rec, http.StatusNotFound)
	})

	t.Run("healthz lives under the prefix too", func(t *testing.T) {
		assertStatus(t, get(t, s, "/registry/healthz"), http.StatusOK)
	})
}

// --- middleware -------------------------------------------------------------

func TestRecoverPanic(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].CatalogFn = func(context.Context, int, string) (*registry.CatalogPage, error) {
		panic("deliberate test panic")
	}
	rec := get(t, s, "/r/local")
	assertStatus(t, rec, http.StatusInternalServerError)
	assertContains(t, body(t, rec), "internal error", "the recovered response")
}

func TestAccessLogPassesRequestsThrough(t *testing.T) {
	t.Parallel()

	raw := strings.Replace(testConfigYAML, `  addr: ":0"`, "  addr: \":0\"\n  access_log: true", 1)
	s, _ := newTestServer(t, raw)
	assertStatus(t, get(t, s, "/r/local"), http.StatusOK)
	assertStatus(t, get(t, s, "/static/app.css"), http.StatusOK)
}

func TestStaticAssetCaching(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)

	rec := get(t, s, "/static/app.css?v=abc123")
	assertStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("versioned asset Cache-Control = %q, want the immutable policy", got)
	}

	rec = get(t, s, "/static/app.css")
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("unversioned asset Cache-Control = %q, want the short policy", got)
	}
}

func TestNewRejectsABadRegistryURL(t *testing.T) {
	t.Parallel()

	// Validation cannot produce this, so build the Config directly.
	cfg := mustConfig(t, testConfigYAML)
	cfg.Registries[0].URL = "ftp://nope"
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("server.New accepted a registry URL the client rejects")
	}
}
