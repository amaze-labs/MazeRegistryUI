// SPDX-License-Identifier: GPL-3.0-or-later

package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseChallenges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   []authChallenge
	}{
		{
			name:   "single bearer challenge",
			header: `Bearer realm="https://auth.example.com/token",service="registry.example.com"`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm":   "https://auth.example.com/token",
				"service": "registry.example.com",
			}}},
		},
		{
			// The classic breakage: the scope value contains a comma, so any
			// naive strings.Split(header, ",") loses "push".
			name:   "scope value containing a comma",
			header: `Bearer realm="https://auth.example.com/token",service="reg",scope="repository:library/alpine:pull,push"`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm":   "https://auth.example.com/token",
				"service": "reg",
				"scope":   "repository:library/alpine:pull,push",
			}}},
		},
		{
			name:   "escaped quote inside a quoted value",
			header: `Bearer realm="https://auth.example.com/token",error="the \"token\" expired",service="reg"`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm":   "https://auth.example.com/token",
				"error":   `the "token" expired`,
				"service": "reg",
			}}},
		},
		{
			name:   "escaped backslash inside a quoted value",
			header: `Bearer realm="https://auth/token",note="a\\b"`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm": "https://auth/token",
				"note":  `a\b`,
			}}},
		},
		{
			name:   "two challenges, the second starting with a bare scheme token",
			header: `Basic realm="registry", Bearer realm="https://auth/token",service="reg"`,
			want: []authChallenge{
				{Scheme: "Basic", Params: map[string]string{"realm": "registry"}},
				{Scheme: "Bearer", Params: map[string]string{"realm": "https://auth/token", "service": "reg"}},
			},
		},
		{
			// An unquoted value is an RFC 7230 token, so it stops at the first
			// character outside that set. Registries always quote a realm URL;
			// this pins down what happens when one does not.
			name:   "unquoted token values",
			header: `Bearer service=reg,error=invalid_token`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"service": "reg",
				"error":   "invalid_token",
			}}},
		},
		{
			name:   "parameter names are lowercased",
			header: `Bearer REALM="https://auth/token",Service="reg"`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm":   "https://auth/token",
				"service": "reg",
			}}},
		},
		{
			name:   "scheme with no parameters",
			header: `Negotiate`,
			want:   []authChallenge{{Scheme: "Negotiate", Params: map[string]string{}}},
		},
		{
			name:   "empty header",
			header: "",
			want:   nil,
		},
		{
			name:   "unterminated quoted string does not hang",
			header: `Bearer realm="https://auth/token`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm": "https://auth/token",
			}}},
		},
		{
			name:   "stray punctuation is stepped over",
			header: `==== Bearer realm="https://auth/token"`,
			want: []authChallenge{{Scheme: "Bearer", Params: map[string]string{
				"realm": "https://auth/token",
			}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseChallenges(tc.header)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseChallenges(%q)\n got: %#v\nwant: %#v", tc.header, got, tc.want)
			}
		})
	}
}

func TestBearerChallenge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		values    []string
		wantFound bool
		wantRealm string
	}{
		{
			name:      "picks bearer out of several header values",
			values:    []string{`Basic realm="reg"`, `Bearer realm="https://auth/token"`},
			wantFound: true,
			wantRealm: "https://auth/token",
		},
		{
			name:      "scheme match is case-insensitive",
			values:    []string{`bEaReR realm="https://auth/token"`},
			wantFound: true,
			wantRealm: "https://auth/token",
		},
		{
			name:      "basic only",
			values:    []string{`Basic realm="reg"`},
			wantFound: false,
		},
		{
			name:      "no values at all",
			values:    nil,
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch, ok := bearerChallenge(tc.values)
			if ok != tc.wantFound {
				t.Fatalf("bearerChallenge(%q) found=%v, want %v", tc.values, ok, tc.wantFound)
			}
			if ok && ch.param("realm") != tc.wantRealm {
				t.Fatalf("bearerChallenge(%q) realm=%q, want %q", tc.values, ch.param("realm"), tc.wantRealm)
			}
		})
	}
}

// tokenServer is a minimal Distribution token endpoint.
type tokenServer struct {
	srv      *httptest.Server
	requests atomic.Int64
	// scopes records every scope query the endpoint was asked for.
	scopes chan []string
	// expiresIn is the advertised lifetime; 0 means the field is omitted.
	expiresIn int64
	token     string
	// authHeader captures the last Authorization header presented.
	authHeader atomic.Value
}

func newTokenServer(t *testing.T, token string, expiresIn int64) *tokenServer {
	t.Helper()
	ts := &tokenServer{token: token, expiresIn: expiresIn, scopes: make(chan []string, 64)}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requests.Add(1)
		ts.authHeader.Store(r.Header.Get("Authorization"))
		select {
		case ts.scopes <- r.URL.Query()["scope"]:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		if ts.expiresIn > 0 {
			fmt.Fprintf(w, `{"token":%q,"expires_in":%d,"issued_at":%q}`,
				ts.token, ts.expiresIn, time.Now().UTC().Format(time.RFC3339))
			return
		}
		fmt.Fprintf(w, `{"token":%q}`, ts.token)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) lastAuth() string {
	v, _ := ts.authHeader.Load().(string)
	return v
}

// challengeGate wraps a handler so the first request from each connection is
// answered with a 401 bearer challenge unless the right token is presented.
func bearerGate(realm, service, scope, wantToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm=%q,service=%q,scope=%q`, realm, service, scope))
			writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		next(w, r)
	}
}

func TestBearerHandshake(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	ts := newTokenServer(t, "tok-123", 300)

	f.handle("/v2/", bearerGate(ts.srv.URL, "fake", "registry:catalog:*", "tok-123",
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

	c := newTestClient(t, f)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after a bearer handshake failed: %v", err)
	}

	if got := ts.requests.Load(); got != 1 {
		t.Fatalf("token endpoint was called %d times, want exactly 1", got)
	}
	if got := f.countPath("/v2/"); got != 2 {
		t.Fatalf("/v2/ was requested %d times, want 2 (the challenge and the retry)", got)
	}
	scopes := <-ts.scopes
	if !reflect.DeepEqual(scopes, []string{"registry:catalog:*"}) {
		t.Fatalf("token endpoint received scope %q, want [registry:catalog:*]", scopes)
	}

	// A second call reuses both the cached challenge and the cached token, so
	// it costs exactly one request: no second token request, and no 401 spent
	// rediscovering a challenge already in hand.
	f.reset()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("second Ping failed: %v", err)
	}
	if got := ts.requests.Load(); got != 1 {
		t.Fatalf("token endpoint was called %d times after the token was cached, want 1", got)
	}
	if got := f.countPath("/v2/"); got != 1 {
		t.Fatalf("/v2/ was requested %d times on the cached path, want 1 (authenticated up front)", got)
	}
	if got := f.lastFor(t, "/v2/").Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("cached token was not presented on the retry: Authorization=%q, want %q", got, "Bearer tok-123")
	}
}

func TestBearerHandshakeOneTokenRequestPerScope(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	ts := newTokenServer(t, "tok-scope", 300)

	// Two resources under different scopes.
	f.handle("/v2/", bearerGate(ts.srv.URL, "fake", "registry:catalog:*", "tok-scope",
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))
	f.handle("/v2/_catalog", bearerGate(ts.srv.URL, "fake", "registry:catalog:*", "tok-scope",
		jsonHandler(`{"repositories":["a"]}`, "")))
	f.handle("/v2/app/tags/list", bearerGate(ts.srv.URL, "fake", "repository:app:pull", "tok-scope",
		jsonHandler(`{"name":"app","tags":["v1"]}`, "")))

	c := newTestClient(t, f)
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := c.Catalog(ctx, 10, ""); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if _, err := c.Tags(ctx, "app", 10, ""); err != nil {
		t.Fatalf("Tags: %v", err)
	}

	// Two distinct scopes were needed, so exactly two token requests.
	if got := ts.requests.Load(); got != 2 {
		t.Fatalf("token endpoint was called %d times, want 2 (one per distinct scope)", got)
	}
}

func TestBearerHandshakePersistent401(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	ts := newTokenServer(t, "wrong-token", 300)

	// The gate never accepts the token the endpoint hands out.
	f.handle("/v2/", bearerGate(ts.srv.URL, "fake", "registry:catalog:*", "the-right-one",
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

	c := newTestClient(t, f)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded against a registry that always answers 401, want an error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Ping error %v does not wrap ErrUnauthorized", err)
	}
	if got := f.countPath("/v2/"); got != 2 {
		t.Fatalf("/v2/ was requested %d times, want exactly 2 (the original and one retry)", got)
	}
	if got := ts.requests.Load(); got != 1 {
		t.Fatalf("token endpoint was called %d times, want 1", got)
	}
}

func TestBearerChallengeWithoutRealm(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer service="fake"`)
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "no realm here")
	})

	c := newTestClient(t, f)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded with a realm-less challenge, want an error")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error %v does not wrap ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "no realm") {
		t.Fatalf("error %q does not explain that the challenge carried no realm", err)
	}
}

func TestBearerChallengeUnusableRealm(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ftp://auth.example.com/token"`)
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad realm")
	})

	c := newTestClient(t, f)
	err := c.Ping(context.Background())
	if err == nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Ping returned %v, want an error wrapping ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "not a usable URL") {
		t.Fatalf("error %q does not mention the unusable realm URL", err)
	}
}

func TestNoBearerChallengeIsNotNegotiated(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "basic only")
	})

	c := newTestClient(t, f)
	err := c.Ping(context.Background())
	if err == nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Ping returned %v, want an error wrapping ErrUnauthorized", err)
	}
	if got := f.countPath("/v2/"); got != 1 {
		t.Fatalf("/v2/ was requested %d times, want 1: a Basic challenge is nothing to negotiate", got)
	}
}

func TestTokenResponseVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantToken string
		wantErr   string
	}{
		{name: "token field", body: `{"token":"aaa"}`, wantToken: "aaa"},
		{name: "access_token field", body: `{"access_token":"bbb"}`, wantToken: "bbb"},
		{name: "token wins over access_token", body: `{"token":"aaa","access_token":"bbb"}`, wantToken: "aaa"},
		{name: "empty response", body: `{}`, wantErr: "carried no token"},
		{name: "unparseable response", body: `not json`, wantErr: "decoding token response"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(tok.Close)

			f := newFakeRegistry(t)
			f.handle("/v2/", bearerGate(tok.URL, "fake", "registry:catalog:*", tc.wantToken,
				func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

			c := newTestClient(t, f)
			err := c.Ping(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Ping with body %s failed: %v", tc.body, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Ping with body %s succeeded, want an error mentioning %q", tc.body, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Ping error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestTokenEndpointFailureIsUnauthorized(t *testing.T) {
	t.Parallel()

	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRegistryError(w, http.StatusForbidden, "DENIED", "no token for you")
	}))
	t.Cleanup(tok.Close)

	f := newFakeRegistry(t)
	f.handle("/v2/", bearerGate(tok.URL, "fake", "registry:catalog:*", "never",
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

	c := newTestClient(t, f)
	err := c.Ping(context.Background())
	if err == nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Ping returned %v, want an error wrapping ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "no token for you") {
		t.Fatalf("error %q lost the registry's explanation", err)
	}
}

// A token whose whole advertised lifetime is inside the safety margin must not
// be cached: presenting it later would be presenting an expired token.
func TestShortLivedTokenIsNotCached(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	ts := newTokenServer(t, "tok-short", 5) // 5s - 10s safety margin => not cacheable

	f.handle("/v2/", bearerGate(ts.srv.URL, "fake", "registry:catalog:*", "tok-short",
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

	c := newTestClient(t, f)
	for i := range 2 {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("Ping %d failed: %v", i+1, err)
		}
	}
	if got := ts.requests.Load(); got != 2 {
		t.Fatalf("token endpoint was called %d times, want 2: a token expiring inside the safety margin must not be cached", got)
	}
}

func TestTokenCacheExpiry(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	c := newTestClient(t, f)
	ch := authChallenge{Scheme: "Bearer", Params: map[string]string{"realm": "https://auth/token"}}
	key := ch.param("realm") + "\x00\x00"

	c.tokens.tokens[key] = cachedToken{value: "stale", expires: time.Now().Add(-time.Second)}

	// A stale entry must not be served; with no reachable realm the fetch fails,
	// which is proof enough that the cache was bypassed.
	if _, err := c.tokens.token(context.Background(), c, ch); err == nil {
		t.Fatal("token() served an expired cache entry instead of refetching")
	}
}

func TestTokenCachePrune(t *testing.T) {
	t.Parallel()

	tc := newTokenCache()
	// Fill past the bound with entries that are already expired.
	for i := range maxTokenEntries {
		tc.tokens[fmt.Sprintf("k%d", i)] = cachedToken{value: "v", expires: time.Now().Add(-time.Minute)}
	}
	tc.pruneLocked()
	if len(tc.tokens) != 0 {
		t.Fatalf("pruneLocked left %d expired entries, want 0", len(tc.tokens))
	}

	// Live entries past the bound trigger the wholesale reset.
	for i := range maxTokenEntries {
		tc.tokens[fmt.Sprintf("k%d", i)] = cachedToken{value: "v", expires: time.Now().Add(time.Hour)}
	}
	tc.pruneLocked()
	if len(tc.tokens) != 0 {
		t.Fatalf("pruneLocked kept %d live entries past the bound, want a full reset to 0", len(tc.tokens))
	}
}

func TestStaticAuthModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		auth       AuthConfig
		wantHeader string
	}{
		{
			name:       "anonymous sends no Authorization",
			auth:       AuthConfig{},
			wantHeader: "",
		},
		{
			name:       "type none sends no Authorization",
			auth:       AuthConfig{Type: "none", Username: "u", Password: "p"},
			wantHeader: "",
		},
		{
			name:       "basic",
			auth:       AuthConfig{Type: "basic", Username: "alice", Password: "s3cr3t"},
			wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t")),
		},
		{
			name:       "basic is case-insensitive and tolerates whitespace",
			auth:       AuthConfig{Type: "  BASIC ", Username: "alice", Password: "s3cr3t"},
			wantHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t")),
		},
		{
			name:       "static bearer",
			auth:       AuthConfig{Type: "bearer", Token: "pat-xyz"},
			wantHeader: "Bearer pat-xyz",
		},
		{
			name:       "basic with empty credentials sends nothing",
			auth:       AuthConfig{Type: "basic"},
			wantHeader: "",
		},
		{
			name:       "bearer with empty token sends nothing",
			auth:       AuthConfig{Type: "bearer"},
			wantHeader: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeRegistry(t)
			f.ping()
			c := newTestClient(t, f, func(o *Options) { o.Auth = tc.auth })

			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("Ping failed: %v", err)
			}
			got := f.lastFor(t, "/v2/").Header.Get("Authorization")
			if got != tc.wantHeader {
				t.Fatalf("Authorization header = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

func TestTokenRequestCarriesCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth AuthConfig
		want string
	}{
		{
			name: "basic credentials are presented to the token endpoint",
			auth: AuthConfig{Type: "basic", Username: "alice", Password: "pw"},
			want: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:pw")),
		},
		{
			name: "a personal access token is presented to the token endpoint",
			auth: AuthConfig{Type: "bearer", Token: "pat-xyz"},
			want: "Bearer pat-xyz",
		},
		{
			name: "anonymous presents nothing",
			auth: AuthConfig{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeRegistry(t)
			ts := newTokenServer(t, "issued", 300)
			f.handle("/v2/", bearerGate(ts.srv.URL, "fake", "registry:catalog:*", "issued",
				func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

			c := newTestClient(t, f, func(o *Options) { o.Auth = tc.auth })
			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("Ping failed: %v", err)
			}
			if got := ts.lastAuth(); got != tc.want {
				t.Fatalf("token endpoint saw Authorization %q, want %q", got, tc.want)
			}
		})
	}
}

// A challenge listing several space-separated scopes must produce one scope
// query parameter each, not a single joined value.
func TestTokenRequestSplitsMultipleScopes(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	ts := newTokenServer(t, "multi", 300)
	f.handle("/v2/", bearerGate(ts.srv.URL, "fake", "repository:a:pull repository:b:pull", "multi",
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))

	c := newTestClient(t, f)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	got := <-ts.scopes
	want := []string{"repository:a:pull", "repository:b:pull"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("token endpoint received scopes %q, want %q", got, want)
	}
}

func TestAuthErrorUnwrap(t *testing.T) {
	t.Parallel()

	cause := errors.New("boom")
	e := &authError{cause: cause}
	if !errors.Is(e, ErrUnauthorized) {
		t.Error("authError does not wrap ErrUnauthorized")
	}
	if !errors.Is(e, cause) {
		t.Error("authError does not wrap its cause")
	}
	if e.Error() != "boom" {
		t.Errorf("authError.Error() = %q, want %q", e.Error(), "boom")
	}

	bare := &authError{}
	if !errors.Is(bare, ErrUnauthorized) {
		t.Error("a causeless authError does not wrap ErrUnauthorized")
	}
	if bare.Error() != ErrUnauthorized.Error() {
		t.Errorf("causeless authError.Error() = %q, want %q", bare.Error(), ErrUnauthorized.Error())
	}
}
