// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// nonceKey carries the per-request style nonce down to the renderer.
type nonceKey struct{}

// nonceFrom returns the nonce generated for this request, if any.
func nonceFrom(ctx context.Context) string {
	n, _ := ctx.Value(nonceKey{}).(string)
	return n
}

// contentSecurityPolicy is deliberately strict: the UI loads nothing from the
// network at runtime, so everything but same-origin assets can be denied.
//
// Note that a nonce covers <style> elements but never style attributes, which
// CSP blocks outright without 'unsafe-inline'. That is why the layer bar puts
// its computed widths in a nonced <style> block instead of on the elements.
func contentSecurityPolicy(nonce string) string {
	return "default-src 'none'; " +
		"script-src 'self'; " +
		"style-src 'self' 'nonce-" + nonce + "'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"form-action 'self'; " +
		"base-uri 'none'; " +
		"frame-ancestors 'none'"
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newNonce()
		r = r.WithContext(context.WithValue(r.Context(), nonceKey{}, nonce))
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps the wrapper transparent: without it the gzip writer underneath
// cannot reach the real flusher once the access log is enabled.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets would drown out anything interesting.
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(start).Round(time.Millisecond).String())
	})
}

// recoverPanic keeps one broken request from taking the process down.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "value", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// sameOriginPost rejects a state-changing request that did not originate from
// this UI. Sec-Fetch-Site is sent by every current browser; the CSRF token
// covers the rest.
func sameOriginPost(r *http.Request, token string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	return r.PostFormValue("csrf") == token
}

// newNonce returns a fresh CSP nonce. A failure to read randomness is fatal
// for the request rather than silently producing a guessable value.
func newNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cannot read from the system random source: " + err.Error())
	}
	return base64.RawStdEncoding.EncodeToString(b)
}
