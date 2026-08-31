package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/amaze-labs/MazeRegistryUI/internal/config"
	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

// fakeClient is an in-memory registry.Client. Every method delegates to an
// overridable function so a test can inject exactly the failure it needs.
type fakeClient struct {
	mu sync.Mutex

	// Data used by the default behaviour.
	Repos   []string
	TagList map[string][]string
	Imgs    map[string]*registry.Image

	// Overrides.
	PingFn       func(ctx context.Context) error
	CatalogFn    func(ctx context.Context, n int, last string) (*registry.CatalogPage, error)
	TagsFn       func(ctx context.Context, repo string, n int, last string) (*registry.TagPage, error)
	SummaryFn    func(ctx context.Context, repo, tag string) *registry.TagSummary
	ImageFn      func(ctx context.Context, repo, ref string, resolveChildren bool) (*registry.Image, error)
	DeleteFn     func(ctx context.Context, repo, digest string) error
	BlobFn       func(ctx context.Context, repo, digest string, limit int64) ([]byte, error)
	DeleteCalls  []string
	CatalogCalls int
}

var _ registry.Client = (*fakeClient)(nil)

func newFakeClient() *fakeClient {
	return &fakeClient{
		TagList: make(map[string][]string),
		Imgs:    make(map[string]*registry.Image),
	}
}

func (f *fakeClient) Ping(ctx context.Context) error {
	if f.PingFn != nil {
		return f.PingFn(ctx)
	}
	return nil
}

func (f *fakeClient) Catalog(ctx context.Context, n int, last string) (*registry.CatalogPage, error) {
	f.mu.Lock()
	f.CatalogCalls++
	f.mu.Unlock()
	if f.CatalogFn != nil {
		return f.CatalogFn(ctx, n, last)
	}
	page := &registry.CatalogPage{}
	for _, name := range f.Repos {
		page.Repositories = append(page.Repositories, registry.Repository{Name: name})
	}
	return page, nil
}

func (f *fakeClient) Tags(ctx context.Context, repo string, n int, last string) (*registry.TagPage, error) {
	if f.TagsFn != nil {
		return f.TagsFn(ctx, repo, n, last)
	}
	tags, ok := f.TagList[repo]
	if !ok {
		return nil, registry.ErrNotFound
	}
	return &registry.TagPage{Repository: repo, Tags: tags}, nil
}

func (f *fakeClient) TagSummary(ctx context.Context, repo, tag string) *registry.TagSummary {
	if f.SummaryFn != nil {
		return f.SummaryFn(ctx, repo, tag)
	}
	return &registry.TagSummary{Name: tag, Digest: "sha256:" + strings.Repeat("a", 64)}
}

func (f *fakeClient) Image(ctx context.Context, repo, ref string, resolveChildren bool) (*registry.Image, error) {
	if f.ImageFn != nil {
		return f.ImageFn(ctx, repo, ref, resolveChildren)
	}
	img, ok := f.Imgs[repo+":"+ref]
	if !ok {
		return nil, registry.ErrNotFound
	}
	return img, nil
}

func (f *fakeClient) DeleteManifest(ctx context.Context, repo, digest string) error {
	f.mu.Lock()
	f.DeleteCalls = append(f.DeleteCalls, repo+"@"+digest)
	f.mu.Unlock()
	if f.DeleteFn != nil {
		return f.DeleteFn(ctx, repo, digest)
	}
	return nil
}

func (f *fakeClient) Blob(ctx context.Context, repo, digest string, limit int64) ([]byte, error) {
	if f.BlobFn != nil {
		return f.BlobFn(ctx, repo, digest, limit)
	}
	return nil, registry.ErrNotFound
}

func (f *fakeClient) deletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.DeleteCalls...)
}

// --- server construction ----------------------------------------------------

const testConfigYAML = `
server:
  addr: ":0"
ui:
  title: Test UI
  catalog_page_size: 3
  tag_page_size: 2
  show_pull_command: true
registries:
  - id: local
    name: Local
    url: http://localhost:5000
    pull_host: reg.test
  - id: other
    name: Other
    url: http://localhost:5001
    delete_enabled: true
`

func mustConfig(t *testing.T, raw string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("the test configuration does not load: %v\n%s", err, raw)
	}
	return cfg
}

// newTestServer builds a Server whose registry clients are replaced by fakes.
// The returned map is keyed by registry id.
func newTestServer(t *testing.T, raw string) (*Server, map[string]*fakeClient) {
	t.Helper()
	cfg := mustConfig(t, raw)
	s, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}
	fakes := make(map[string]*fakeClient, len(cfg.Registries))
	for _, reg := range cfg.Registries {
		f := newFakeClient()
		fakes[reg.ID] = f
		s.clients[reg.ID] = f
	}
	return s, fakes
}

// do runs one request against the server and returns the recorder.
func do(t *testing.T, s *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// get is the common case: a GET with no headers.
func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s, httptest.NewRequest(http.MethodGet, path, nil))
}

// postForm builds a form POST, defaulting Sec-Fetch-Site to same-origin.
func postForm(t *testing.T, s *Server, path string, form map[string]string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var b strings.Builder
	first := true
	for k, v := range form {
		if !first {
			b.WriteByte('&')
		}
		first = false
		b.WriteString(k + "=" + urlQueryEscape(v))
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	return do(t, s, req)
}

func body(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("reading the response body failed: %v", err)
	}
	return string(b)
}

// assertStatus fails with the response body, which is where the reason lives.
func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d\nbody:\n%s", rec.Code, want, rec.Body.String())
	}
}

func assertContains(t *testing.T, haystack, needle, why string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: the response does not contain %q", why, needle)
	}
}

func assertNotContains(t *testing.T, haystack, needle, why string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: the response contains %q but must not", why, needle)
	}
}
