// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

func TestStripFromLayers(t *testing.T) {
	t.Parallel()

	if stripFromLayers(nil) != nil {
		t.Error("no layers must give no strip")
	}
	if stripFromLayers([]registry.Layer{{Size: 0}}) != nil {
		t.Error("layers without sizes must give no strip")
	}

	// base 10, thin 5, main 70, thin 10, thin 5: the two trailing thin
	// layers merge into one band.
	sv := stripFromLayers([]registry.Layer{{Size: 10}, {Size: 5}, {Size: 70}, {Size: 10}, {Size: 5}})
	if sv == nil {
		t.Fatal("expected a strip")
	}
	kinds := make([]string, 0, len(sv.Segs))
	for _, s := range sv.Segs {
		kinds = append(kinds, s.Kind)
	}
	if got := strings.Join(kinds, ","); got != "base,thin,main,thin" {
		t.Errorf("segment kinds = %s, want base,thin,main,thin", got)
	}
	if sv.BasePct != 10 || sv.MainPct != 70 || sv.Layers != 5 {
		t.Errorf("base/main/layers = %d/%d/%d, want 10/70/5", sv.BasePct, sv.MainPct, sv.Layers)
	}
	last := sv.Segs[len(sv.Segs)-1]
	if end := last.X + last.W; end < 99.99 || end > 100.01 {
		t.Errorf("the strip ends at %.3f, want 100", end)
	}

	// A single layer is all base.
	one := stripFromLayers([]registry.Layer{{Size: 42}})
	if len(one.Segs) != 1 || one.Segs[0].Kind != "base" || one.BasePct != 100 {
		t.Errorf("a one-layer image = %+v, want a single full base band", one.Segs)
	}
}

func TestTagKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"1":           "release",
		"1.2.3":       "release",
		"v1.2.3":      "release",
		"2026.11.11":  "release",
		"2.0.0-rc.3":  "pre",
		"1.29-alpine": "pre",
		"16-alpine":   "pre",
		"latest":      "moving",
		"main":        "moving",
		"sha-abc1234": "moving",
	}
	for tag, want := range cases {
		if got := tagKind(tag); got != want {
			t.Errorf("tagKind(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestPickLatest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tags []string
		want string
	}{
		{nil, ""},
		{[]string{"latest"}, "latest"},
		{[]string{"latest", "1.9.0", "1.8.0"}, "1.9.0"},
		{[]string{"2.0", "1.0"}, "2.0"},
		{[]string{"latest", "buildcache", "0.6.1", "0.6.0"}, "0.6.1"},
		{[]string{"main", "2.0.0-rc.1"}, "2.0.0-rc.1"},
		{[]string{"latest", "main", "edge"}, "main"},
	}
	for _, c := range cases {
		if got := pickLatest(c.tags); got != c.want {
			t.Errorf("pickLatest(%v) = %q, want %q", c.tags, got, c.want)
		}
	}
}

func TestNamespaceCounts(t *testing.T) {
	t.Parallel()
	got := namespaceCounts([]string{"a/x", "b/x", "b/y", "root", "c/x", "c/y"})
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	// Largest first, ties by name; the root namespace is "".
	if j := strings.Join(names, ","); j != "b,c,,a" {
		t.Errorf("namespace order = %q, want \"b,c,,a\"", j)
	}
}

func TestTintsAreDistinctAndStable(t *testing.T) {
	t.Parallel()
	repos := []string{"alpha/x", "bravo/x", "charlie/x", "delta/x", "echo/x", "plain"}
	tints := assignTints(repos)
	seen := map[string]string{}
	for ns, tint := range tints {
		if ns == "" {
			if tint != "tn" {
				t.Errorf("the root namespace got %q, want the neutral tn", tint)
			}
			continue
		}
		if other, dup := seen[tint]; dup {
			t.Errorf("%s and %s share %s", ns, other, tint)
		}
		seen[tint] = ns
	}

	// Before a catalog is known, the hash stands in, and it is stable.
	c := newCatalogCache()
	if a, b := c.Tint("reg", "team"), c.Tint("reg", "team"); a != b || !strings.HasPrefix(a, "t") {
		t.Errorf("fallback tint = %q then %q, want the same t<n>", a, b)
	}
}

// --- handlers ------------------------------------------------------------

func TestCatalogGroupsByNamespace(t *testing.T) {
	t.Parallel()

	t.Run("small registry: every group open with its rows", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].Repos = []string{"infra/redis", "team/api"}
		html := body(t, get(t, s, "/r/local"))
		assertContains(t, html, `class="group`, "the namespace groups")
		assertContains(t, html, ">infra<", "the infra group header")
		assertContains(t, html, ">team<", "the team group header")
		assertContains(t, html, "/x/reporow/local/team/api", "the lazy row of team/api")
		assertNotContains(t, html, `hx-trigger="toggle once"`, "a lazy group in a small registry")
	})

	t.Run("large registry: closed groups that load on first open", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML) // catalog_page_size: 3
		fakes["local"].Repos = []string{"a/1", "a/2", "b/1", "b/2"}
		html := body(t, get(t, s, "/r/local"))
		assertContains(t, html, `hx-trigger="toggle once"`, "the lazy group")
		assertContains(t, html, "/x/catalog/local?ns=a", "the group's rows URL")
		assertNotContains(t, html, "/x/reporow/local/a/1", "a row rendered inside a closed group")

		rows := body(t, get(t, s, "/x/catalog/local?ns=a"))
		assertContains(t, rows, "/x/reporow/local/a/1", "the rows of the opened group")
		assertNotContains(t, rows, "/x/reporow/local/b/1", "a row from another namespace")
	})

	t.Run("one namespace, a filter or the flat view: a plain list", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].Repos = []string{"infra/redis", "team/api"}
		for _, path := range []string{"/r/local?view=flat", "/r/local?q=api", "/r/local?ns=team"} {
			html := body(t, get(t, s, path))
			assertNotContains(t, html, `<details class="group`, path)
			assertContains(t, html, `id="repo-list"`, path)
		}
		ns := body(t, get(t, s, "/r/local?ns=team"))
		assertNotContains(t, ns, "/x/reporow/local/infra/redis", "a repository outside the namespace")
	})

	t.Run("the search box gets the whole body back", func(t *testing.T) {
		s, fakes := newTestServer(t, testConfigYAML)
		fakes["local"].Repos = []string{"infra/redis", "team/api"}
		req := httptest.NewRequest(http.MethodGet, "/x/catalog/local?part=body&q=", nil)
		req.Header.Set("HX-Request", "true")
		rec := do(t, s, req)
		assertContains(t, body(t, rec), `<details class="group`, "the regrouped body after clearing the filter")
		if got := rec.Header().Get("HX-Push-Url"); got != "/r/local" {
			t.Errorf("HX-Push-Url = %q, want /r/local", got)
		}
	})
}

func TestRepoRowDetails(t *testing.T) {
	t.Parallel()
	s, fakes := newTestServer(t, testConfigYAML)
	f := fakes["local"]
	f.TagList["team/api"] = []string{"latest", "1.2.0", "1.1.0"}
	f.SummaryFn = func(_ context.Context, _, tag string) *registry.TagSummary {
		return &registry.TagSummary{
			Name: tag, Digest: digestA, Size: 4096,
			Created:   time.Now().Add(-10 * time.Minute),
			Platforms: []registry.Platform{{OS: "linux", Architecture: "arm64"}},
		}
	}
	f.ImageFn = func(_ context.Context, _, ref string, _ bool) (*registry.Image, error) {
		if ref != digestA {
			t.Errorf("the strip resolved %q, want the summary's digest", ref)
		}
		return &registry.Image{Digest: digestA, Layers: []registry.Layer{{Size: 10}, {Size: 90}}}, nil
	}

	html := body(t, get(t, s, "/x/reporow/local/team/api"))
	assertContains(t, html, ">1.2.0<", "the newest version, preferred over latest")
	assertContains(t, html, "+2", "the other tags")
	assertContains(t, html, `class="strip"`, "the layer strip")
	assertContains(t, html, `class="strip__main"`, "the largest layer band")
	assertContains(t, html, "arm64", "the platform")
	assertContains(t, html, "is-fresh", "the push from the last hour")
	assertContains(t, html, `data-pull="reg.test/team/api:1.2.0"`, "the pull reference for the c key")
}

func TestTagRowCarriesKindAndStrip(t *testing.T) {
	t.Parallel()
	s, fakes := newTestServer(t, testConfigYAML)
	f := fakes["local"]
	f.SummaryFn = func(_ context.Context, _, tag string) *registry.TagSummary {
		return &registry.TagSummary{Name: tag, Digest: digestA, Size: 10}
	}
	f.ImageFn = func(context.Context, string, string, bool) (*registry.Image, error) {
		return &registry.Image{Digest: digestA, Layers: []registry.Layer{{Size: 3}, {Size: 7}}}, nil
	}
	html := body(t, get(t, s, "/x/tag/local/team/api?t=2.0.0-rc.1"))
	assertContains(t, html, `data-kind="pre"`, "the pre-release kind")
	assertContains(t, html, `data-digest="`+digestA+`"`, "the digest the alias marker matches on")
	assertContains(t, html, `class="strip"`, "the layer strip")
}

func TestRepositoryPageCountsKinds(t *testing.T) {
	t.Parallel()
	s, fakes := newTestServer(t, strings.Replace(testConfigYAML, "tag_page_size: 2", "tag_page_size: 10", 1))
	fakes["local"].TagList["team/api"] = []string{"latest", "1.0.0", "1.1.0-rc.1", "0.9.0"}
	html := body(t, get(t, s, "/r/local/repo/team/api"))
	assertContains(t, html, `data-kind="release">releases <small>2</small>`, "the release count")
	assertContains(t, html, `data-kind="pre">pre-releases <small>1</small>`, "the pre-release count")
	assertContains(t, html, `data-kind="moving">moving <small>1</small>`, "the moving count")
	assertContains(t, html, `data-pull-base="reg.test/team/api"`, "the pull bar")
}

func TestImagePagePullByDigest(t *testing.T) {
	t.Parallel()
	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Imgs["team/api:1.0"] = &registry.Image{
		Digest: digestA, MediaType: registry.MediaTypeOCIManifest,
		Layers: []registry.Layer{{Size: 1, Digest: digestA}, {Size: 9, Digest: digestA}},
	}
	html := body(t, get(t, s, "/r/local/image/team/api?ref=1.0"))
	assertContains(t, html, "docker pull reg.test/team/api:1.0", "the pull by tag")
	assertContains(t, html, "docker pull reg.test/team/api@"+digestA, "the pull by digest")
	assertContains(t, html, "layerbar__seg is-first", "the first layer band")
	assertContains(t, html, "is-top", "the largest layer")
	assertContains(t, html, `.share[data-layer="2"] .share__fill{width:90.00%}`, "the share bar width")
}

func TestMarkName(t *testing.T) {
	t.Parallel()
	got := markName("team/api-gateway", []string{"api", "team"})
	var b strings.Builder
	for _, seg := range got {
		switch {
		case seg.Hit:
			b.WriteString("[" + seg.Text + "]")
		case seg.NS:
			b.WriteString("(" + seg.Text + ")")
		default:
			b.WriteString(seg.Text)
		}
	}
	if s := b.String(); s != "[team](/)[api]-gateway" {
		t.Errorf("markName = %q, want \"[team](/)[api]-gateway\"", s)
	}
}

func TestLiveSearch(t *testing.T) {
	t.Parallel()
	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{"infra/redis", "team/api", "team/api-gateway", "team/web"}

	empty := body(t, get(t, s, "/x/jump/local?q="))
	assertNotContains(t, empty, "jump__item", "results for an empty query")

	html := body(t, get(t, s, "/x/jump/local?q=team+api"))
	assertContains(t, html, `href="/r/local/repo/team/api"`, "the first match")
	assertContains(t, html, `href="/r/local/repo/team/api-gateway"`, "the second match")
	assertNotContains(t, html, "team/web", "a repository missing one of the terms")
	assertContains(t, html, "<mark", "the highlighted match")
	assertContains(t, html, `href="/r/local?q=team%20api"`, "the link to the full filtered list")

	none := body(t, get(t, s, "/x/jump/local?q=nothing-here"))
	assertContains(t, none, "No repository matches", "the empty state")
}
