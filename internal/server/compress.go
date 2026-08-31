package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinSize is the point below which compression costs more than it saves:
// a small fragment ends up larger once the gzip header and trailer are added.
const gzipMinSize = 512

var gzipPool = sync.Pool{
	New: func() any {
		// BestSpeed, not BestCompression: these are small text responses
		// generated per request, and the last few percent are not worth the CPU.
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// compressible reports whether a content type benefits from gzip. Fonts and
// images in the asset bundle are already compressed, and re-compressing them
// only burns CPU.
func compressible(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	ct = strings.TrimSpace(ct)
	switch ct {
	case "application/json", "application/javascript", "text/javascript",
		"image/svg+xml", "application/xml":
		return true
	}
	return strings.HasPrefix(ct, "text/")
}

// compress gzips responses that are worth gzipping. Most deployments sit
// behind a proxy that would do this, but a single binary should not need one
// to serve a page economically.
func (s *Server) compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		// Vary regardless of whether this particular response is compressed:
		// caches must key on the header, not on what we happened to decide.
		w.Header().Add("Vary", "Accept-Encoding")

		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

// gzipResponseWriter defers the decision to compress until it has seen the
// content type and enough bytes to judge the size.
type gzipResponseWriter struct {
	http.ResponseWriter

	wroteHeader bool
	status      int
	gz          *gzip.Writer
	passing     bool // decided: write straight through
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = status

	h := g.Header()
	// A handler that already encoded, or a body that cannot be buffered
	// meaningfully, passes through untouched.
	if h.Get("Content-Encoding") != "" || !compressible(h.Get("Content-Type")) {
		g.passing = true
		g.ResponseWriter.WriteHeader(status)
		return
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < gzipMinSize {
		// Small enough to know up front that gzip would not pay for itself.
		g.passing = true
		g.ResponseWriter.WriteHeader(status)
		return
	}
	// The compressed length is unknown, and a stale Content-Length would
	// truncate the response.
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	g.ResponseWriter.WriteHeader(status)

	g.gz = gzipPool.Get().(*gzip.Writer)
	g.gz.Reset(g.ResponseWriter)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		// net/http infers the content type from the first write; do the same so
		// the decision above sees it.
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.passing {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

// Flush lets HTMX fragments and the access log behave as they would without
// the wrapper.
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) finish() {
	if g.gz == nil {
		return
	}
	_ = g.gz.Close()
	gzipPool.Put(g.gz)
	g.gz = nil
}
