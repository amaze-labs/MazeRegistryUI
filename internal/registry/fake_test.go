package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one request the fake registry received.
type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
}

// fakeRegistry is an httptest.Server speaking enough of the Distribution API
// to drive the client. Routes are matched on the exact path; anything else
// falls through to the fallback handler, or to a 404 carrying a Distribution
// error document.
type fakeRegistry struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	routes   map[string]http.HandlerFunc
	fallback http.HandlerFunc
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{t: t, routes: make(map[string]http.HandlerFunc)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
	})
	h, ok := f.routes[r.URL.Path]
	fb := f.fallback
	f.mu.Unlock()

	switch {
	case ok:
		h(w, r)
	case fb != nil:
		fb(w, r)
	default:
		writeRegistryError(w, http.StatusNotFound, "NAME_UNKNOWN", "fake registry has no route for "+r.URL.Path)
	}
}

// handle registers a handler for one exact path.
func (f *fakeRegistry) handle(path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = h
}

// handleFallback registers the handler used when no exact route matches.
func (f *fakeRegistry) handleFallback(h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fallback = h
}

// ping installs the trivial /v2/ endpoint most tests need.
func (f *fakeRegistry) ping() {
	f.handle("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	})
}

func (f *fakeRegistry) URL() string { return f.server.URL }

// snapshot returns a copy of every request seen so far.
func (f *fakeRegistry) snapshot() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeRegistry) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// countPath counts requests whose path equals p.
func (f *fakeRegistry) countPath(p string) int {
	n := 0
	for _, r := range f.snapshot() {
		if r.Path == p {
			n++
		}
	}
	return n
}

// lastFor returns the most recent request for path p.
func (f *fakeRegistry) lastFor(t *testing.T, p string) recordedRequest {
	t.Helper()
	reqs := f.snapshot()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Path == p {
			return reqs[i]
		}
	}
	t.Fatalf("fake registry never received a request for %s; it saw %v", p, requestPaths(reqs))
	return recordedRequest{}
}

func requestPaths(reqs []recordedRequest) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// reset forgets every recorded request.
func (f *fakeRegistry) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func writeRegistryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}

// writeManifest serves raw manifest bytes with the media type and, unless
// suppressed, the Docker-Content-Digest header the registry would compute.
func writeManifest(w http.ResponseWriter, mediaType string, raw []byte, withDigest bool) {
	w.Header().Set("Content-Type", mediaType)
	if withDigest {
		w.Header().Set("Docker-Content-Digest", digestOf(raw))
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
	_, _ = w.Write(raw)
}

// --- client construction ----------------------------------------------------

// newTestClient builds a client against the fake registry. The concrete type
// is returned so tests can inspect the cache and other internals.
func newTestClient(t *testing.T, f *fakeRegistry, mutate ...func(*Options)) *client {
	t.Helper()
	opts := Options{
		Name:      "fake",
		BaseURL:   f.URL(),
		Timeout:   5 * time.Second,
		CacheTTL:  time.Minute,
		UserAgent: "MazeRegistryUI/test",
	}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New(%+v) returned an unexpected error: %v", opts, err)
	}
	impl, ok := c.(*client)
	if !ok {
		t.Fatalf("New returned %T, expected *client", c)
	}
	// Keep debug chatter out of the test output.
	impl.log = slog.New(slog.DiscardHandler)
	return impl
}

// --- manifest fixtures ------------------------------------------------------

// testDescriptor mirrors an OCI content descriptor for fixture building.
type testDescriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  *testPlatform     `json:"platform,omitempty"`
	Anns      map[string]string `json:"annotations,omitempty"`
}

type testPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	OSVersion    string `json:"os.version,omitempty"`
	Variant      string `json:"variant,omitempty"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling fixture %#v failed: %v", v, err)
	}
	return b
}

// fakeDigest builds a syntactically valid sha256 digest from a seed, so
// fixtures can reference blobs that are never actually served.
func fakeDigest(seed string) string {
	return digestOf([]byte(seed))
}

// imageManifest builds an OCI image manifest referencing the given config and
// layer descriptors.
func imageManifest(t *testing.T, mediaType string, cfg testDescriptor, layers ...testDescriptor) []byte {
	t.Helper()
	if layers == nil {
		layers = []testDescriptor{}
	}
	return mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaType,
		"config":        cfg,
		"layers":        layers,
	})
}

// indexManifest builds an OCI index or Docker manifest list.
func indexManifest(t *testing.T, mediaType string, children ...testDescriptor) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaType,
		"manifests":     children,
	})
}

// configBlob builds an image config document.
func configBlob(t *testing.T, os, arch string, created time.Time, history []map[string]any, extra map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"architecture": arch,
		"os":           os,
		"created":      created.UTC().Format(time.RFC3339Nano),
		"config":       extra,
		"history":      history,
	}
	return mustJSON(t, doc)
}

// serveManifest wires a manifest at both its tag and its digest, the way a
// real registry exposes it.
func (f *fakeRegistry) serveManifest(repo, tag, mediaType string, raw []byte) string {
	dgst := digestOf(raw)
	h := func(w http.ResponseWriter, _ *http.Request) {
		writeManifest(w, mediaType, raw, true)
	}
	if tag != "" {
		f.handle("/v2/"+repo+"/manifests/"+tag, h)
	}
	f.handle("/v2/"+repo+"/manifests/"+dgst, h)
	return dgst
}

// serveBlob wires a blob at its digest.
func (f *fakeRegistry) serveBlob(repo string, raw []byte) string {
	dgst := digestOf(raw)
	f.handle("/v2/"+repo+"/blobs/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		_, _ = w.Write(raw)
	})
	return dgst
}

// jsonList serves a JSON document at a path, optionally with a Link header.
func jsonHandler(body string, link string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if link != "" {
			w.Header().Set("Link", link)
		}
		_, _ = io.WriteString(w, body)
	}
}

// hasAll reports whether s contains every substring in want.
func hasAll(s string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
