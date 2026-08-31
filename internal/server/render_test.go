// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

// xss is the payload every user-controlled string in these tests carries. If
// it reaches the browser unescaped, the UI executes whatever a registry (or
// whoever can push to one) put in a repository name, tag, label or env var.
const xss = `<script>alert(1)</script>"'`

// assertEscaped fails when the raw payload survives into the response, and
// checks the escaped form is there instead so the test cannot pass simply
// because the value was dropped.
func assertEscaped(t *testing.T, html, why string) {
	t.Helper()
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Errorf("%s: the raw payload <script>alert(1)</script> reached the response", why)
	}
	if !strings.Contains(html, "alert(1)") {
		t.Errorf("%s: the payload is absent entirely, so this test proves nothing", why)
	}
	if !strings.Contains(html, "&lt;script&gt;") && !strings.Contains(html, "%3Cscript%3E") {
		t.Errorf("%s: no escaped form of the payload found", why)
	}
}

func TestEscapingRepositoryNames(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{"team/" + xss}

	for _, path := range []string{"/r/local", "/x/catalog/local"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, s, path)
			assertStatus(t, rec, http.StatusOK)
			assertEscaped(t, body(t, rec), "the catalog listing")
		})
	}
}

func TestEscapingSearchQuery(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{"app"}

	rec := get(t, s, "/r/local?q="+urlQueryEscape(xss))
	assertStatus(t, rec, http.StatusOK)
	assertEscaped(t, body(t, rec), "the search box and the empty-state message")
}

func TestEscapingTagNames(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].TagList["app"] = []string{xss}

	t.Run("tag list", func(t *testing.T) {
		rec := get(t, s, "/r/local/repo/app")
		assertStatus(t, rec, http.StatusOK)
		assertEscaped(t, body(t, rec), "the tag list")
	})

	t.Run("lazy tag row", func(t *testing.T) {
		rec := get(t, s, "/x/tag/local/app?t="+urlQueryEscape(xss))
		assertStatus(t, rec, http.StatusOK)
		assertEscaped(t, body(t, rec), "the lazy tag row")
	})
}

func TestEscapingImagePageValues(t *testing.T) {
	t.Parallel()

	img := sampleImage("team/app", xss)
	img.Config.Labels = map[string]string{"org.label": xss}
	img.Config.Env = []string{"EVIL=" + xss}
	img.Config.Author = xss
	img.Config.WorkingDir = xss
	img.Config.Entrypoint = []string{xss}
	img.Layers[0].Command = "RUN " + xss
	img.RawManifest = []byte(`{"note":"` + "<script>alert(1)</script>" + `"}`)

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].ImageFn = func(context.Context, string, string, bool) (*registry.Image, error) {
		return img, nil
	}

	rec := get(t, s, "/r/local/image/team/app?ref="+urlQueryEscape(xss))
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)

	assertEscaped(t, html, "the image page")
	// Each panel individually, so a single escaped occurrence cannot mask an
	// unescaped one elsewhere.
	for _, marker := range []string{"EVIL", "org.label"} {
		assertContains(t, html, marker, "the image page")
	}
	if strings.Count(html, "<script>alert(1)</script>") != 0 {
		t.Error("an unescaped payload survived somewhere on the image page")
	}
}

func TestEscapingErrorMessages(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := get(t, s, "/r/"+urlQueryEscape(xss))
	assertStatus(t, rec, http.StatusNotFound)
	assertEscaped(t, body(t, rec), "the unknown-registry error page")
}

func TestEscapingConfiguredStrings(t *testing.T) {
	t.Parallel()

	raw := `
server:
  addr: ":0"
ui:
  title: "T<script>alert(1)</script>"
  footer_note: "F<script>alert(1)</script>"
registries:
  - id: local
    name: "N<script>alert(1)</script>"
    description: "D<script>alert(1)</script>"
    url: http://localhost:5000
`
	s, _ := newTestServer(t, raw)
	rec := get(t, s, "/r/local")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)
	assertEscaped(t, html, "the page shell")
	for _, marker := range []string{"T&lt;script&gt;", "F&lt;script&gt;", "N&lt;script&gt;", "D&lt;script&gt;"} {
		assertContains(t, html, marker, "the page shell")
	}
}

// A repository name is interpolated straight into an href, so the URL context
// escaper has to handle it too.
func TestEscapingInHrefContext(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{`app"onmouseover="alert(1)`}

	rec := get(t, s, "/r/local")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)
	if strings.Contains(html, `onmouseover="alert(1)"`) {
		t.Error("a repository name broke out of the href attribute")
	}
	assertContains(t, html, "&#34;", "the escaped repository name")
}

// --- template health --------------------------------------------------------

func TestAllTemplatesParse(t *testing.T) {
	t.Parallel()

	tmpl, err := parseTemplates(templateFuncs())
	if err != nil {
		t.Fatalf("parseTemplates failed: %v", err)
	}
	for _, name := range []string{"catalog", "repository", "image", "error", "_partials"} {
		if _, ok := tmpl[name]; !ok {
			t.Errorf("template set %q is missing", name)
		}
	}
	if _, ok := tmpl["base"]; ok {
		t.Error("base is registered as a page; it is only a layout")
	}
	for _, partial := range []string{"catalog_rows", "repo_count", "tag_rows", "tag_row", "banner", "health_dot"} {
		if tmpl["_partials"].Lookup(partial) == nil {
			t.Errorf("partial %q is not defined in the fragment set", partial)
		}
	}
}

// Empty data is where template rendering usually blows up into a blank 500, so
// every page is rendered against the emptiest input it can legally receive.
func TestPagesRenderWithEmptyData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*fakeClient)
		path  string
	}{
		{
			name:  "catalog with no repositories",
			setup: func(f *fakeClient) { f.Repos = nil },
			path:  "/r/local",
		},
		{
			name:  "catalog rows with no repositories",
			setup: func(f *fakeClient) { f.Repos = nil },
			path:  "/x/catalog/local",
		},
		{
			name:  "repository with no tags",
			setup: func(f *fakeClient) { f.TagList["app"] = nil },
			path:  "/r/local/repo/app",
		},
		{
			name:  "tag rows with no tags",
			setup: func(f *fakeClient) { f.TagList["app"] = []string{} },
			path:  "/x/tags/local/app",
		},
		{
			name: "image with nothing but a digest",
			setup: func(f *fakeClient) {
				f.Imgs["app:latest"] = &registry.Image{Repository: "app", Reference: "latest", Digest: digestA}
			},
			path: "/r/local/image/app?ref=latest",
		},
		{
			name: "image with no layers and no config",
			setup: func(f *fakeClient) {
				f.Imgs["app:latest"] = &registry.Image{
					Repository: "app", Reference: "latest", Digest: digestA,
					MediaType: registry.MediaTypeOCIManifest, RawManifest: []byte(`{}`),
				}
			},
			path: "/r/local/image/app?ref=latest",
		},
		{
			name: "an index with no children",
			setup: func(f *fakeClient) {
				f.Imgs["app:multi"] = &registry.Image{
					Repository: "app", Reference: "multi", Digest: digestA,
					MediaType: registry.MediaTypeOCIIndex, IsIndex: true,
				}
			},
			path: "/r/local/image/app?ref=multi",
		},
		{
			name: "an index whose children never resolved",
			setup: func(f *fakeClient) {
				f.Imgs["app:multi"] = &registry.Image{
					Repository: "app", Reference: "multi", Digest: digestA,
					MediaType: registry.MediaTypeOCIIndex, IsIndex: true,
					Children: []registry.IndexChild{
						{Descriptor: registry.Descriptor{Digest: digestB}},
					},
				}
			},
			path: "/r/local/image/app?ref=multi",
		},
		{
			name: "an index child with no platform",
			setup: func(f *fakeClient) {
				f.Imgs["app:multi"] = &registry.Image{
					Repository: "app", Reference: "multi", Digest: digestA,
					MediaType: registry.MediaTypeOCIIndex, IsIndex: true,
					Children: []registry.IndexChild{{
						Descriptor: registry.Descriptor{Digest: digestB, Size: 10},
						Resolved:   &registry.Image{Digest: digestB},
					}},
				}
			},
			path: "/r/local/image/app?ref=multi",
		},
		{
			name: "an image with an empty config",
			setup: func(f *fakeClient) {
				f.Imgs["app:latest"] = &registry.Image{
					Repository: "app", Reference: "latest", Digest: digestA,
					MediaType: registry.MediaTypeOCIManifest,
					Config:    &registry.ImageConfig{},
					Layers:    []registry.Layer{{}},
				}
			},
			path: "/r/local/image/app?ref=latest",
		},
		{
			name: "a tag summary with nothing filled in",
			setup: func(f *fakeClient) {
				f.SummaryFn = func(_ context.Context, _, tag string) *registry.TagSummary {
					return &registry.TagSummary{Name: tag}
				}
			},
			path: "/x/tag/local/app?t=latest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, fakes := newTestServer(t, testConfigYAML)
			tc.setup(fakes["local"])
			rec := get(t, s, tc.path)
			assertStatus(t, rec, http.StatusOK)
			if strings.Contains(rec.Body.String(), "internal error") {
				t.Fatalf("the page rendered as a 500 body:\n%s", rec.Body.String())
			}
			if strings.TrimSpace(rec.Body.String()) == "" {
				t.Fatal("the page rendered empty")
			}
		})
	}
}

// A configuration with a single registry and every optional field omitted must
// still render, since that is what a first-run config looks like.
func TestMinimalConfigRenders(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, "registries:\n  - name: Local\n    url: http://localhost:5000\n")
	for _, path := range []string{"/", "/r/local", "/healthz"} {
		rec := get(t, s, path)
		if rec.Code >= 500 {
			t.Fatalf("GET %s = %d\n%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRenderUnknownTemplateIs500(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), "no-such-page", nil)
	assertStatus(t, rec, http.StatusInternalServerError)
}

// A failing template must produce a clean 500, not a half-written page.
func TestRenderBuffersBeforeWriting(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	// catalog expects a *catalogPage; a value with no such fields fails while
	// executing, after the shell has already been written to the buffer.
	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), "catalog", struct{}{})
	assertStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatal("a partially rendered page was written before the error was noticed")
	}
}

func TestRenderPartialFailureIs500(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, testConfigYAML)
	rec := httptest.NewRecorder()
	s.renderPartial(rec, httptest.NewRequest(http.MethodGet, "/", nil), "tag_row", struct{}{})
	assertStatus(t, rec, http.StatusInternalServerError)
}

func TestTemplateFuncDict(t *testing.T) {
	t.Parallel()

	dict, ok := templateFuncs()["dict"].(func(...any) (map[string]any, error))
	if !ok {
		t.Fatal("the dict helper has an unexpected signature")
	}

	got, err := dict("a", 1, "b", "two")
	if err != nil {
		t.Fatalf("dict failed: %v", err)
	}
	if got["a"] != 1 || got["b"] != "two" {
		t.Errorf("dict = %v", got)
	}
	if _, err := dict("a"); err == nil {
		t.Error("dict accepted an odd number of arguments")
	}
	if _, err := dict(1, "a"); err == nil {
		t.Error("dict accepted a non-string key")
	}
}

func TestTemplateFuncISO(t *testing.T) {
	t.Parallel()

	iso, ok := templateFuncs()["iso"].(func(time.Time) string)
	if !ok {
		t.Fatal("the iso helper has an unexpected signature")
	}
	if got := iso(time.Time{}); got != "" {
		t.Errorf("iso(zero) = %q, want empty so the datetime attribute stays absent", got)
	}
	if got, want := iso(time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)), "2024-05-06T07:08:09Z"; got != want {
		t.Errorf("iso = %q, want %q", got, want)
	}
}

func TestTemplateFuncsAreAllUsable(t *testing.T) {
	t.Parallel()

	funcs := templateFuncs()
	for _, name := range []string{"url", "asset", "bytes", "since", "full", "short", "iso", "join", "sub", "dict"} {
		if funcs[name] == nil {
			t.Errorf("template function %q is missing", name)
		}
	}
	// Every function has to survive a real parse and execution.
	tmpl := template.Must(template.New("t").Funcs(funcs).Parse(
		`{{url "" "/x"}}{{asset "" "/y"}}{{bytes 1024}}{{short "sha256:abcdef012345678"}}{{join .Parts ","}}{{sub 3 1}}{{(dict "k" "v").k}}`))
	var b strings.Builder
	if err := tmpl.Execute(&b, map[string]any{"Parts": []string{"a", "b"}}); err != nil {
		t.Fatalf("executing a template using every helper failed: %v", err)
	}
	if b.String() == "" {
		t.Fatal("the template produced nothing")
	}
}

// When a registry happens to be named like the UI itself, the title is not
// doubled up into "Local · Local".
func TestLayoutTitleIsNotDuplicated(t *testing.T) {
	t.Parallel()

	raw := `
server:
  addr: ":0"
ui:
  title: Local
registries:
  - id: local
    name: Local
    url: http://localhost:5000
`
	s, _ := newTestServer(t, raw)
	rec := get(t, s, "/r/local")
	assertStatus(t, rec, http.StatusOK)
	html := body(t, rec)
	assertContains(t, html, "<title>Local</title>", "the page title")
	assertNotContains(t, html, "Local · Local", "the page title")
}
