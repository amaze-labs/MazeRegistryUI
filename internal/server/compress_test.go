// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCompressible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want bool
	}{
		{"text/html; charset=utf-8", true},
		{"text/html", true},
		{"text/plain", true},
		{"text/css", true},
		{"application/json", true},
		{"application/javascript", true},
		{"text/javascript", true},
		{"image/svg+xml", true},
		{"application/xml", true},
		{"  text/html  ", true},
		{"font/woff2", false},
		{"image/png", false},
		{"application/octet-stream", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := compressible(tc.in); got != tc.want {
			t.Errorf("compressible(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// serveThroughCompress runs one handler behind the compression middleware.
func serveThroughCompress(t *testing.T, acceptEncoding string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	s.compress(h).ServeHTTP(rec, req)
	return rec
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("the response is not valid gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompressing the response failed: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("the gzip stream was not closed cleanly: %v", err)
	}
	return string(out)
}

func TestCompressHTML(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("<p>hello</p>", 200)
	rec := serveThroughCompress(t, "gzip, deflate", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, payload)
	})

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	// A stale Content-Length would truncate the compressed body.
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it removed", got)
	}
	if got := gunzip(t, rec.Body.Bytes()); got != payload {
		t.Fatalf("the decompressed body differs from what the handler wrote (%d vs %d bytes)", len(got), len(payload))
	}
	if rec.Body.Len() >= len(payload) {
		t.Errorf("the compressed body is %d bytes for a %d byte payload; compression achieved nothing", rec.Body.Len(), len(payload))
	}
}

func TestCompressSkippedWithoutAcceptEncoding(t *testing.T) {
	t.Parallel()

	payload := strings.Repeat("x", 2000)
	rec := serveThroughCompress(t, "", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, payload)
	})
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none when the client did not offer gzip", got)
	}
	if rec.Body.String() != payload {
		t.Fatal("the body was altered for a client that did not ask for gzip")
	}
}

func TestCompressPassthroughCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "an incompressible content type",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "font/woff2")
				_, _ = w.Write(make([]byte, 4096))
			},
		},
		{
			name: "a response the handler already encoded",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Content-Encoding", "br")
				_, _ = w.Write(make([]byte, 4096))
			},
		},
		{
			name: "a body too small to be worth compressing",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Content-Length", strconv.Itoa(gzipMinSize-1))
				_, _ = w.Write(make([]byte, gzipMinSize-1))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := serveThroughCompress(t, "gzip", tc.handler)
			if got := rec.Header().Get("Content-Encoding"); got == "gzip" {
				t.Fatalf("the response was compressed; %s must pass through", tc.name)
			}
			// Vary is set regardless, because the decision depends on the header.
			if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Errorf("Vary = %q, want Accept-Encoding", got)
			}
		})
	}
}

func TestCompressAtTheSizeThreshold(t *testing.T) {
	t.Parallel()

	rec := serveThroughCompress(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.Itoa(gzipMinSize))
		_, _ = w.Write([]byte(strings.Repeat("a", gzipMinSize)))
	})
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("a body of exactly %d bytes was not compressed (Content-Encoding %q)", gzipMinSize, got)
	}
}

func TestCompressDetectsTheContentType(t *testing.T) {
	t.Parallel()

	// The handler writes without setting a content type, exactly as net/http
	// allows; the wrapper has to sniff it before deciding.
	payload := "<!doctype html>" + strings.Repeat("<p>x</p>", 300)
	rec := serveThroughCompress(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip after sniffing text/html", got)
	}
	if got := gunzip(t, rec.Body.Bytes()); got != payload {
		t.Fatal("the sniffed-and-compressed body does not round-trip")
	}
}

func TestCompressStatusIsPreserved(t *testing.T) {
	t.Parallel()

	rec := serveThroughCompress(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, strings.Repeat("nope", 300))
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip on an error page too", got)
	}
}

func TestCompressIgnoresASecondWriteHeader(t *testing.T) {
	t.Parallel()

	rec := serveThroughCompress(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusTeapot) // a handler bug; must not corrupt the response
		_, _ = io.WriteString(w, strings.Repeat("x", 1000))
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the first WriteHeader to win", rec.Code)
	}
	if got := gunzip(t, rec.Body.Bytes()); len(got) != 1000 {
		t.Fatalf("the body decompressed to %d bytes, want 1000", len(got))
	}
}

func TestCompressFlush(t *testing.T) {
	t.Parallel()

	rec := serveThroughCompress(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, strings.Repeat("a", 1000))
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, strings.Repeat("b", 1000))
	})
	got := gunzip(t, rec.Body.Bytes())
	if got != strings.Repeat("a", 1000)+strings.Repeat("b", 1000) {
		t.Fatalf("a flushed stream lost data: %d bytes decompressed", len(got))
	}
}

func TestCompressEmptyResponse(t *testing.T) {
	t.Parallel()

	rec := serveThroughCompress(t, "gzip", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNoContent)
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

// End to end: a real client, a real server, and a page that is worth gzipping.
func TestCompressEndToEnd(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	repos := make([]string, 0, 40)
	for i := range 40 {
		repos = append(repos, "team/service-"+strconv.Itoa(i))
	}
	fakes["local"].Repos = repos

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	// DisableCompression keeps the transport from transparently decoding, so
	// the wire format can be inspected.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/r/local", nil)
	if err != nil {
		t.Fatalf("building the request failed: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body failed: %v", err)
	}
	html := gunzip(t, raw)
	if !strings.Contains(html, "team/service-0") {
		t.Fatal("the decompressed page does not contain the catalog")
	}
	if !strings.HasPrefix(strings.TrimSpace(html), "<!doctype html>") {
		t.Fatal("the decompressed body is not the full page")
	}
}

func TestCompressIsSkippedForAClientThatDoesNotAsk(t *testing.T) {
	t.Parallel()

	s, fakes := newTestServer(t, testConfigYAML)
	fakes["local"].Repos = []string{"app"}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Get(srv.URL + "/r/local")
	if err != nil {
		t.Fatalf("the request failed: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q for a client that did not offer gzip", got)
	}
	html, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(html), "<!doctype html>") {
		t.Fatal("the uncompressed page is not intact")
	}
}
