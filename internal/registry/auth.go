package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// tokenSafetyMargin is subtracted from a token's advertised lifetime so a
	// token is never presented in the same instant it expires.
	tokenSafetyMargin = 10 * time.Second
	// defaultTokenLifetime is the fallback the Distribution token spec
	// prescribes when the response omits expires_in.
	defaultTokenLifetime = 60 * time.Second
	// maxTokenEntries bounds the token cache; one entry per scope in use.
	maxTokenEntries = 256
)

// authChallenge is one parsed entry of a WWW-Authenticate header.
type authChallenge struct {
	Scheme string
	Params map[string]string
}

func (c authChallenge) param(name string) string { return c.Params[name] }

// authError reports that the registry refused our credentials. It wraps both
// ErrUnauthorized, so callers can classify it, and the underlying cause, so
// operators can see what actually went wrong.
type authError struct {
	cause error
}

func (e *authError) Error() string {
	if e.cause == nil {
		return ErrUnauthorized.Error()
	}
	return e.cause.Error()
}

func (e *authError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnauthorized}
	}
	return []error{ErrUnauthorized, e.cause}
}

// applyStaticAuth attaches the credentials configured for this registry, if
// any. A bearer token from a challenge is applied separately, by do.
func (c *client) applyStaticAuth(req *http.Request) {
	switch strings.ToLower(strings.TrimSpace(c.opts.Auth.Type)) {
	case "basic":
		if c.opts.Auth.Username != "" || c.opts.Auth.Password != "" {
			req.SetBasicAuth(c.opts.Auth.Username, c.opts.Auth.Password)
		}
	case "bearer":
		if c.opts.Auth.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.opts.Auth.Token)
		}
	}
}

// bearerChallenge picks the Bearer challenge out of the WWW-Authenticate
// header values of a 401 response, if the registry offered one.
func bearerChallenge(values []string) (authChallenge, bool) {
	for _, v := range values {
		for _, ch := range parseChallenges(v) {
			if strings.EqualFold(ch.Scheme, "bearer") {
				return ch, true
			}
		}
	}
	return authChallenge{}, false
}

// parseChallenges parses a WWW-Authenticate header value into its challenges.
//
// The grammar (RFC 7235) is comma-separated in two places at once: challenges
// are separated by commas and so are the parameters within a challenge, and
// parameter values may be quoted strings that themselves contain commas — as
// they routinely do in a Distribution scope such as
// `scope="repository:a/b:pull,push"`. Splitting on commas is therefore always
// wrong; this is a small scanner instead. A bare token that is not followed by
// `=` marks the start of the next challenge.
func parseChallenges(header string) []authChallenge {
	s := header
	pos := 0

	skipSeparators := func() {
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == ',') {
			pos++
		}
	}
	skipSpace := func() {
		for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t') {
			pos++
		}
	}
	readToken := func() string {
		start := pos
		for pos < len(s) && isTokenChar(s[pos]) {
			pos++
		}
		return s[start:pos]
	}
	readQuoted := func() string {
		pos++ // opening quote
		var b strings.Builder
		for pos < len(s) {
			switch {
			case s[pos] == '\\' && pos+1 < len(s):
				b.WriteByte(s[pos+1])
				pos += 2
			case s[pos] == '"':
				pos++
				return b.String()
			default:
				b.WriteByte(s[pos])
				pos++
			}
		}
		return b.String()
	}

	var out []authChallenge
	for pos < len(s) {
		skipSeparators()
		if pos >= len(s) {
			break
		}
		scheme := readToken()
		if scheme == "" {
			pos++ // unparseable byte; step over it rather than spin
			continue
		}

		ch := authChallenge{Scheme: scheme, Params: make(map[string]string)}
		for {
			mark := pos
			skipSeparators()
			if pos >= len(s) {
				break
			}
			key := readToken()
			if key == "" {
				pos = mark
				break
			}
			skipSpace()
			if pos >= len(s) || s[pos] != '=' {
				// A bare token: this is the next challenge's scheme.
				pos = mark
				break
			}
			pos++ // '='
			skipSpace()
			var value string
			if pos < len(s) && s[pos] == '"' {
				value = readQuoted()
			} else {
				value = readToken()
			}
			ch.Params[strings.ToLower(key)] = value
		}
		out = append(out, ch)
	}
	return out
}

// isTokenChar reports whether b is valid in an RFC 7230 token.
func isTokenChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// tokenCache holds bearer tokens keyed by the challenge that produced them.
type tokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	value   string
	expires time.Time
}

func newTokenCache() *tokenCache {
	return &tokenCache{tokens: make(map[string]cachedToken)}
}

// token returns a valid bearer token for the challenge, fetching one if the
// cached token is missing or stale.
//
// The mutex is deliberately held across the network call. A page of tags
// fanning out into dozens of parallel manifest requests will hit the same 401
// at the same moment; serialising here turns that into one token request
// instead of one per goroutine, which is what registries rate-limit on.
func (t *tokenCache) token(ctx context.Context, c *client, ch authChallenge) (string, error) {
	key := ch.param("realm") + "\x00" + ch.param("service") + "\x00" + ch.param("scope")

	t.mu.Lock()
	defer t.mu.Unlock()

	if tok, ok := t.tokens[key]; ok && time.Now().Before(tok.expires) {
		return tok.value, nil
	}

	value, lifetime, err := c.requestToken(ctx, ch)
	if err != nil {
		return "", err
	}
	if lifetime > 0 {
		t.pruneLocked()
		t.tokens[key] = cachedToken{value: value, expires: time.Now().Add(lifetime)}
	}
	return value, nil
}

// pruneLocked keeps the token cache bounded. Tokens are short-lived, so
// dropping the expired ones is normally enough; the wholesale reset is a
// backstop for a pathological number of distinct scopes.
func (t *tokenCache) pruneLocked() {
	if len(t.tokens) < maxTokenEntries {
		return
	}
	now := time.Now()
	for k, tok := range t.tokens {
		if !now.Before(tok.expires) {
			delete(t.tokens, k)
		}
	}
	if len(t.tokens) >= maxTokenEntries {
		t.tokens = make(map[string]cachedToken)
	}
}

// tokenResponse covers both spellings of the token field. The Docker token
// spec says "token"; OAuth2-flavoured implementations say "access_token" and
// some registries send both.
type tokenResponse struct {
	Token       string   `json:"token"`
	AccessToken string   `json:"access_token"`
	ExpiresIn   int64    `json:"expires_in"`
	IssuedAt    flexTime `json:"issued_at"`
}

// requestToken performs the token-service handshake described by the
// challenge and returns the token together with how long it may be cached.
func (c *client) requestToken(ctx context.Context, ch authChallenge) (string, time.Duration, error) {
	realm := ch.param("realm")
	if realm == "" {
		return "", 0, &authError{cause: errors.New("registry: bearer challenge carried no realm")}
	}
	u, err := url.Parse(realm)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", 0, &authError{cause: fmt.Errorf("registry: bearer challenge realm %q is not a usable URL", realm)}
	}

	q := u.Query()
	if service := ch.param("service"); service != "" {
		q.Set("service", service)
	}
	if scope := ch.param("scope"); scope != "" {
		// A challenge may list several space-separated scopes; the token
		// endpoint expects one query parameter per scope.
		q.Del("scope")
		for _, one := range strings.Fields(scope) {
			q.Add("scope", one)
		}
	}
	u.RawQuery = q.Encode()
	endpoint := u.String()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", 0, fmt.Errorf("registry: building token request: %w", err)
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "application/json")

	switch {
	case c.opts.Auth.Username != "" || c.opts.Auth.Password != "":
		req.SetBasicAuth(c.opts.Auth.Username, c.opts.Auth.Password)
	case strings.EqualFold(c.opts.Auth.Type, "bearer") && c.opts.Auth.Token != "":
		// Registries that issue personal access tokens (GHCR, Gitea) accept
		// the same token on the token endpoint; anonymous would lose access to
		// private repositories the token can see.
		req.Header.Set("Authorization", "Bearer "+c.opts.Auth.Token)
	}

	c.log.Debug("registry: requesting bearer token", "registry", c.opts.Name, "endpoint", sanitiseURL(endpoint))

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("registry: token request to %s: %w", sanitiseURL(endpoint), err)
	}
	defer drain(resp)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		code, message := firstRegistryError(body)
		return "", 0, &authError{cause: &StatusError{
			Method:     http.MethodGet,
			URL:        sanitiseURL(endpoint),
			StatusCode: resp.StatusCode,
			Code:       code,
			Message:    message,
		}}
	}

	body, err := readLimited(resp.Body, maxTokenBytes)
	if err != nil {
		return "", 0, fmt.Errorf("registry: reading token response from %s: %w", sanitiseURL(endpoint), err)
	}
	var doc tokenResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", 0, fmt.Errorf("registry: decoding token response from %s: %w", sanitiseURL(endpoint), err)
	}

	value := doc.Token
	if value == "" {
		value = doc.AccessToken
	}
	if value == "" {
		return "", 0, &authError{cause: fmt.Errorf("registry: token response from %s carried no token", sanitiseURL(endpoint))}
	}

	lifetime := time.Duration(doc.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultTokenLifetime
	}
	// issued_at may predate this response; the token expires relative to it,
	// not to now.
	if !doc.IssuedAt.IsZero() {
		if remaining := time.Until(doc.IssuedAt.Time.Add(lifetime)); remaining < lifetime {
			lifetime = remaining
		}
	}
	lifetime -= tokenSafetyMargin
	if lifetime < 0 {
		lifetime = 0
	}
	return value, lifetime, nil
}
