package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults applied to a zero-valued Options.
const (
	defaultTimeout   = 20 * time.Second
	defaultCacheTTL  = 60 * time.Second
	defaultUserAgent = "MazeRegistryUI/dev"
)

// Response body limits. A registry is a remote system that can be slow,
// broken or hostile, so nothing is read without a ceiling.
const (
	maxManifestBytes = 8 << 20  // manifests and config blobs
	maxTokenBytes    = 1 << 20  // token service responses
	maxErrorBytes    = 64 << 10 // error documents
	maxDrainBytes    = 64 << 10 // read before closing, to reuse the connection
	defaultBlobLimit = 8 << 20  // used when Blob is called without a limit
)

const (
	// immutableTTL applies to digest-addressed content, which cannot change.
	immutableTTL = time.Hour
	// maxChildConcurrency bounds parallel child manifest fetches per index.
	// Six is enough to hide latency without looking like a scraper to a
	// rate-limited registry.
	maxChildConcurrency = 6
	// maxCatalogPageSize caps what we will ask a registry for in one page.
	maxCatalogPageSize = 1000
	// defaultPageSize is used when the caller passes a non-positive page size.
	defaultPageSize = 100
)

// acceptManifests is the Accept header sent with every manifest request. A
// registry serves whichever of these it holds; omitting one means the registry
// may fall back to schema 1, which this client cannot read.
var acceptManifests = strings.Join([]string{
	MediaTypeOCIIndex,
	MediaTypeOCIManifest,
	MediaTypeDockerList,
	MediaTypeDockerV2,
}, ", ")

// StatusError reports an unexpected HTTP status from the registry, enriched
// with the Distribution error document when the registry sent one.
type StatusError struct {
	Method     string
	URL        string
	StatusCode int
	// Code and Message come from the first entry of the registry's
	// {"errors":[...]} document; both are empty when there was no such body.
	Code    string
	Message string
}

func (e *StatusError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "registry: %s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Code != "" {
		fmt.Fprintf(&b, " [%s]", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

// client is the Client implementation. It is safe for concurrent use.
type client struct {
	opts   Options
	base   string // normalised base URL, no trailing slash, no /v2 suffix
	hc     *http.Client
	cache  *ttlCache
	tokens *tokenCache
	// challenges lets a repeat request authenticate without first being told
	// to, which halves the request count against a bearer-auth registry.
	challenges *challengeCache
	log        *slog.Logger
}

var _ Client = (*client)(nil)

// New builds a Client for one registry endpoint.
func New(opts Options) (Client, error) {
	base, err := normaliseBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(strings.TrimSpace(opts.Auth.Type)) {
	case "", "none", "basic", "bearer":
	default:
		return nil, fmt.Errorf("registry: unknown auth type %q", opts.Auth.Type)
	}

	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = defaultCacheTTL
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUserAgent
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: opts.Timeout,
	}
	if opts.Insecure {
		// Opt-in, for internal registries with self-signed certificates.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	return &client{
		opts:       opts,
		base:       base,
		cache:      newTTLCache(defaultCacheEntries),
		tokens:     newTokenCache(),
		challenges: newChallengeCache(),
		log:        slog.Default(),
		hc: &http.Client{
			Transport:     transport,
			Timeout:       opts.Timeout,
			CheckRedirect: stripAuthOnHostChange,
		},
	}, nil
}

// normaliseBaseURL validates the configured endpoint and reduces it to a
// scheme, host and optional path prefix that the /v2 routes can be appended to.
func normaliseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("registry: base URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("registry: invalid base URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("registry: base URL %q must use the http or https scheme", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("registry: base URL %q has no host", raw)
	}

	u.RawQuery = ""
	u.Fragment = ""
	u.RawPath = ""
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "/v2" || strings.HasSuffix(u.Path, "/v2") {
		return "", fmt.Errorf("registry: base URL %q must not include the /v2 API prefix", raw)
	}
	return u.String(), nil
}

// stripAuthOnHostChange keeps credentials from following a redirect to another
// host. Registries commonly redirect blob downloads to object storage, where a
// registry Authorization header is at best useless and at worst a credential
// handed to a third party.
func stripAuthOnHostChange(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("registry: stopped after 10 redirects")
	}
	if len(via) > 0 && req.URL.Host != via[len(via)-1].URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// --- path safety ------------------------------------------------------------

// Repository names go straight into a URL path, so they are validated against
// the OCI grammar rather than escaped: anything the grammar rejects has no
// business being there in the first place, and this leaves no room for `..`
// or a percent-encoded traversal to reach the registry.
var (
	repoComponentPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$`)
	tagPattern           = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	digestPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func validateRepository(repo string) error {
	if repo == "" {
		return errors.New("registry: repository name is empty")
	}
	if len(repo) > 255 {
		return fmt.Errorf("registry: repository name is %d characters, the limit is 255", len(repo))
	}
	for _, component := range strings.Split(repo, "/") {
		if !repoComponentPattern.MatchString(component) {
			return fmt.Errorf("registry: %q is not a valid repository name", repo)
		}
	}
	return nil
}

// isDigest reports whether s is a digest this client accepts. Only sha256 is
// recognised; it is the only algorithm Distribution registries emit.
func isDigest(s string) bool { return digestPattern.MatchString(s) }

func validateDigest(d string) error {
	if !isDigest(d) {
		return fmt.Errorf("registry: %q is not a valid sha256 digest", d)
	}
	return nil
}

// validateReference accepts a tag or a digest.
func validateReference(ref string) error {
	if ref == "" {
		return errors.New("registry: reference is empty")
	}
	if isDigest(ref) || tagPattern.MatchString(ref) {
		return nil
	}
	return fmt.Errorf("registry: %q is not a valid tag or digest", ref)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- HTTP plumbing ----------------------------------------------------------

// do performs a request, completing a bearer token handshake at most once if
// the registry answers 401 with a challenge. The response body is left open
// for the caller to read and drain.
func (c *client) do(ctx context.Context, method, rawURL, accept string) (*http.Response, error) {
	scopeKey := authScopeKey(rawURL)

	// If this resource has challenged us before, present a token up front.
	// Otherwise every request against a bearer registry pays a 401 round trip
	// to be told something we already know.
	if known, ok := c.challenges.get(scopeKey); ok {
		if token, err := c.tokens.token(ctx, c, known); err == nil {
			resp, err := c.send(ctx, method, rawURL, accept, token)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusUnauthorized {
				return resp, nil
			}
			// The remembered challenge no longer satisfies the registry —
			// scopes widen, tokens get revoked — so renegotiate from scratch.
			drain(resp)
		}
	}

	resp, err := c.send(ctx, method, rawURL, accept, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge, ok := bearerChallenge(resp.Header.Values("Www-Authenticate"))
	if !ok {
		// Basic challenge, or none at all: nothing to negotiate. The caller
		// turns the status into ErrUnauthorized.
		return resp, nil
	}
	drain(resp)
	c.challenges.put(scopeKey, challenge)

	token, err := c.tokens.token(ctx, c, challenge)
	if err != nil {
		return nil, err
	}

	// Exactly one retry. If it fails again the credentials are simply not
	// good enough for this resource, and retrying would only amplify load.
	return c.send(ctx, method, rawURL, accept, token)
}

// send issues a single request. A non-empty bearer overrides the statically
// configured credentials.
func (c *client) send(ctx context.Context, method, rawURL, accept, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: building %s %s: %w", method, sanitiseURL(rawURL), err)
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else {
		c.applyStaticAuth(req)
	}

	started := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: %s %s: %w", method, sanitiseURL(rawURL), err)
	}
	c.log.Debug("registry: request",
		"registry", c.opts.Name,
		"method", method,
		"url", sanitiseURL(rawURL),
		"status", resp.StatusCode,
		"elapsed", time.Since(started),
	)
	return resp, nil
}

// checkResponse maps a non-2xx response onto this package's errors. It reads
// the body to recover the registry's error document but does not close it;
// callers drain the response either way.
func (c *client) checkResponse(resp *http.Response, method, rawURL string) error {
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	code, message := firstRegistryError(body)
	status := &StatusError{
		Method:     method,
		URL:        sanitiseURL(rawURL),
		StatusCode: resp.StatusCode,
		Code:       code,
		Message:    message,
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%s: %w", status, ErrNotFound)
	case http.StatusUnauthorized, http.StatusForbidden:
		return &authError{cause: status}
	default:
		return status
	}
}

// firstRegistryError pulls the leading entry out of a Distribution error
// document. Anything else — HTML from a reverse proxy, an empty body — yields
// empty strings rather than an error, since the status code already carries
// the important part.
func firstRegistryError(body []byte) (code, message string) {
	var doc struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Errors) == 0 {
		return "", ""
	}
	return doc.Errors[0].Code, strings.TrimSpace(doc.Errors[0].Message)
}

// drain reads a bounded amount of the remaining body and closes it, so the
// connection can go back to the idle pool instead of being torn down.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
}

// readLimited reads at most limit bytes and reports an error if the source had
// more to give, rather than silently returning a truncated document.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response body exceeds the %d byte limit", limit)
	}
	return b, nil
}

// sanitiseURL removes any credentials before a URL reaches a log line or an
// error message.
func sanitiseURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// --- Client implementation --------------------------------------------------

func (c *client) Ping(ctx context.Context) error {
	endpoint := c.base + "/v2/"
	resp, err := c.do(ctx, http.MethodGet, endpoint, "")
	if err != nil {
		return err
	}
	defer drain(resp)
	return c.checkResponse(resp, http.MethodGet, endpoint)
}

func (c *client) Catalog(ctx context.Context, n int, last string) (*CatalogPage, error) {
	n = clampPageSize(n)
	key := fmt.Sprintf("catalog|%d|%s", n, last)
	if v, ok := c.cache.get(key); ok {
		if page, ok := v.(*CatalogPage); ok {
			return page.clone(), nil
		}
	}

	q := url.Values{}
	q.Set("n", strconv.Itoa(n))
	if last != "" {
		q.Set("last", last)
	}
	endpoint := c.base + "/v2/_catalog?" + q.Encode()

	resp, err := c.do(ctx, http.MethodGet, endpoint, "application/json")
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := c.checkResponse(resp, http.MethodGet, endpoint); err != nil {
		return nil, err
	}

	body, err := readLimited(resp.Body, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("registry: reading catalog: %w", err)
	}
	var doc struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("registry: decoding catalog: %w", err)
	}

	page := &CatalogPage{Repositories: make([]Repository, 0, len(doc.Repositories))}
	for _, name := range doc.Repositories {
		if name = strings.TrimSpace(name); name != "" {
			page.Repositories = append(page.Repositories, Repository{Name: name})
		}
	}
	page.NextLast = c.nextCursor(resp.Header.Get("Link"), lastOf(doc.Repositories), len(doc.Repositories), n)

	c.cache.set(key, cacheScopeGlobal, page, c.opts.CacheTTL)
	return page.clone(), nil
}

func (c *client) Tags(ctx context.Context, repo string, n int, last string) (*TagPage, error) {
	if err := validateRepository(repo); err != nil {
		return nil, err
	}
	n = clampPageSize(n)
	key := fmt.Sprintf("tags|%s|%d|%s", repo, n, last)
	if v, ok := c.cache.get(key); ok {
		if page, ok := v.(*TagPage); ok {
			return page.clone(), nil
		}
	}

	q := url.Values{}
	q.Set("n", strconv.Itoa(n))
	if last != "" {
		q.Set("last", last)
	}
	endpoint := c.base + "/v2/" + repo + "/tags/list?" + q.Encode()

	resp, err := c.do(ctx, http.MethodGet, endpoint, "application/json")
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := c.checkResponse(resp, http.MethodGet, endpoint); err != nil {
		return nil, err
	}

	body, err := readLimited(resp.Body, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("registry: reading tag list for %s: %w", repo, err)
	}
	// "tags": null is how registries report a repository with no tags left.
	var doc struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("registry: decoding tag list for %s: %w", repo, err)
	}

	page := &TagPage{Repository: repo, Tags: make([]string, 0, len(doc.Tags))}
	for _, tag := range doc.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			page.Tags = append(page.Tags, tag)
		}
	}
	// The cursor has to be taken from the registry's own ordering, before the
	// display sort rearranges the page.
	page.NextLast = c.nextCursor(resp.Header.Get("Link"), lastOf(doc.Tags), len(doc.Tags), n)
	sortTags(page.Tags)

	c.cache.set(key, repo, page, c.opts.CacheTTL)
	return page.clone(), nil
}

func (c *client) Image(ctx context.Context, repo, reference string, resolveChildren bool) (*Image, error) {
	if err := validateRepository(repo); err != nil {
		return nil, err
	}
	if err := validateReference(reference); err != nil {
		return nil, err
	}
	fetched, err := c.fetchManifest(ctx, repo, reference)
	if err != nil {
		return nil, err
	}
	return c.buildImage(ctx, repo, reference, fetched, resolveChildren)
}

func (c *client) TagSummary(ctx context.Context, repo, tag string) *TagSummary {
	summary := &TagSummary{Name: tag}

	// Children are resolved because that is what yields creation time and,
	// for indexes without platform descriptors, the platform list. Everything
	// it touches is digest-addressed and cached for an hour, so a page of tags
	// sharing base layers stays cheap after the first row.
	img, err := c.Image(ctx, repo, tag, true)
	if err != nil {
		summary.Err = err.Error()
		return summary
	}

	summary.Digest = img.Digest
	summary.MediaType = img.MediaType
	summary.Size = img.TotalSize
	summary.IsIndex = img.IsIndex
	if img.Config != nil {
		summary.Created = img.Config.Created
	}

	var platforms []Platform
	if img.IsIndex {
		for _, child := range img.Children {
			switch {
			case child.Platform != nil:
				platforms = append(platforms, *child.Platform)
			case child.Resolved != nil && child.Resolved.Config != nil:
				cfg := child.Resolved.Config
				platforms = append(platforms, Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant})
			}
			if summary.Created.IsZero() && child.Resolved != nil && child.Resolved.Config != nil {
				summary.Created = child.Resolved.Config.Created
			}
		}
	} else if img.Config != nil {
		platforms = append(platforms, Platform{
			OS:           img.Config.OS,
			Architecture: img.Config.Architecture,
			Variant:      img.Config.Variant,
		})
	}
	summary.Platforms = dedupePlatforms(platforms)
	return summary
}

func (c *client) DeleteManifest(ctx context.Context, repo, digest string) error {
	if !c.opts.DeleteEnabled {
		return ErrDeleteDenied
	}
	if err := validateRepository(repo); err != nil {
		return err
	}
	if err := validateDigest(digest); err != nil {
		return err
	}

	endpoint := c.base + "/v2/" + repo + "/manifests/" + digest
	resp, err := c.do(ctx, http.MethodDelete, endpoint, "")
	if err != nil {
		return err
	}
	defer drain(resp)

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK:
		c.cache.InvalidateRepo(repo)
		c.log.Debug("registry: manifest deleted", "registry", c.opts.Name, "repository", repo, "digest", digest)
		return nil
	case http.StatusMethodNotAllowed:
		// Deletion is enabled in our configuration but not in the registry's.
		// Say so plainly: the fix is on the registry, not here.
		return fmt.Errorf("registry: cannot delete %s@%s: %w — deletion is disabled on the registry itself (Distribution needs storage.delete.enabled, i.e. REGISTRY_STORAGE_DELETE_ENABLED=true, and a restart)", repo, digest, ErrDeleteDenied)
	default:
		return c.checkResponse(resp, http.MethodDelete, endpoint)
	}
}

func (c *client) Blob(ctx context.Context, repo, digest string, limit int64) ([]byte, error) {
	if err := validateRepository(repo); err != nil {
		return nil, err
	}
	if err := validateDigest(digest); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultBlobLimit
	}
	return c.fetchBlob(ctx, repo, digest, limit)
}

// --- manifests --------------------------------------------------------------

// manifestFetch is a manifest exactly as the registry served it, together with
// the two things only the response can tell us.
type manifestFetch struct {
	Raw       []byte
	MediaType string
	Digest    string
}

// fetchManifest retrieves a manifest, through the cache. Manifests requested
// by digest are immutable and cached for an hour; a tag can move, so it gets
// the configured TTL.
func (c *client) fetchManifest(ctx context.Context, repo, reference string) (*manifestFetch, error) {
	key := "manifest|" + repo + "|" + reference
	if v, ok := c.cache.get(key); ok {
		if m, ok := v.(*manifestFetch); ok {
			return m, nil
		}
	}

	endpoint := c.base + "/v2/" + repo + "/manifests/" + reference
	resp, err := c.do(ctx, http.MethodGet, endpoint, acceptManifests)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := c.checkResponse(resp, http.MethodGet, endpoint); err != nil {
		return nil, err
	}

	raw, err := readLimited(resp.Body, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("registry: reading manifest %s:%s: %w", repo, reference, err)
	}

	// The manifest bytes are the authority. Not every registry sends the
	// header, and one that sends a digest not matching what it served is
	// either broken or lying — in both cases the computed digest is the one
	// worth trusting, since it is what a client would pull by.
	computed := digestOf(raw)
	if advertised := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); isDigest(advertised) && advertised != computed {
		c.log.Warn("registry: advertised manifest digest does not match its bytes",
			"registry", c.opts.Name, "repository", repo,
			"advertised", advertised, "computed", computed)
	}
	digest := computed

	fetched := &manifestFetch{
		Raw:       raw,
		MediaType: normaliseMediaType(resp.Header.Get("Content-Type")),
		Digest:    digest,
	}

	if isDigest(reference) {
		c.cache.set(key, repo, fetched, immutableTTL)
	} else {
		c.cache.set(key, repo, fetched, c.opts.CacheTTL)
		// The same bytes are also reachable by digest, and that key never
		// goes stale; index children resolve through it.
		c.cache.set("manifest|"+repo+"|"+digest, repo, fetched, immutableTTL)
	}
	return fetched, nil
}

// buildImage turns fetched manifest bytes into the display model.
func (c *client) buildImage(ctx context.Context, repo, reference string, fetched *manifestFetch, resolveChildren bool) (*Image, error) {
	doc, err := parseManifest(fetched.Raw, fetched.MediaType)
	if err != nil {
		return nil, fmt.Errorf("%s:%s: %w", repo, reference, err)
	}

	img := &Image{
		Repository:   repo,
		Reference:    reference,
		Digest:       fetched.Digest,
		MediaType:    doc.MediaType,
		IsIndex:      doc.isIndex,
		ManifestSize: int64(len(fetched.Raw)),
		// The cached bytes are shared between callers; hand out a copy so a
		// caller writing into RawManifest cannot corrupt the cache.
		RawManifest: slices.Clone(fetched.Raw),
		Annotations: doc.Annotations,
	}
	if doc.Subject != nil {
		subject := doc.Subject.descriptor()
		img.Subject = &subject
	}

	if doc.isIndex {
		c.fillIndex(ctx, img, doc, resolveChildren)
		return img, nil
	}
	c.fillManifest(ctx, img, doc)
	return img, nil
}

// fillIndex populates the index fields of an image.
func (c *client) fillIndex(ctx context.Context, img *Image, doc *manifestDoc, resolveChildren bool) {
	img.Children = make([]IndexChild, 0, len(doc.Manifests))
	for _, m := range doc.Manifests {
		if !isDigest(m.Digest) {
			// A child we cannot address is a child we cannot show; skipping it
			// is better than putting an unusable digest in front of the user.
			c.log.Debug("registry: skipping index child with an unusable digest",
				"registry", c.opts.Name, "repository", img.Repository, "digest", m.Digest)
			continue
		}
		img.Children = append(img.Children, IndexChild{Descriptor: m.descriptor()})
	}
	if !resolveChildren || len(img.Children) == 0 {
		return
	}

	c.resolveChildren(ctx, img.Repository, img.Children)

	// Children of an index overwhelmingly share base layers. Counting a shared
	// blob once per platform would inflate the total several-fold, so size is
	// summed over distinct blob digests.
	counted := make(map[string]struct{})
	var total int64
	for i := range img.Children {
		if resolved := img.Children[i].Resolved; resolved != nil {
			total += distinctSize(resolved, counted)
		}
	}
	img.TotalSize = total
}

// resolveChildren fetches every child manifest, at most maxChildConcurrency at
// a time. Each goroutine writes only its own slice element, so no lock is
// needed around the results. A child that cannot be resolved is left with a
// nil Resolved rather than failing the whole index: one broken platform should
// not hide the others.
func (c *client) resolveChildren(ctx context.Context, repo string, children []IndexChild) {
	sem := make(chan struct{}, maxChildConcurrency)
	var wg sync.WaitGroup

	for i := range children {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			child, err := c.resolveChild(ctx, repo, children[i].Digest)
			if err != nil {
				c.log.Debug("registry: index child could not be resolved",
					"registry", c.opts.Name, "repository", repo,
					"digest", children[i].Digest, "error", err)
				return
			}
			children[i].Resolved = child
			children[i].SizeTotal = child.TotalSize
		}(i)
	}
	wg.Wait()
}

// resolveChild fetches one child manifest. Nested indexes are not expanded
// further: they are vanishingly rare and recursion here would be unbounded.
func (c *client) resolveChild(ctx context.Context, repo, digest string) (*Image, error) {
	fetched, err := c.fetchManifest(ctx, repo, digest)
	if err != nil {
		return nil, err
	}
	return c.buildImage(ctx, repo, digest, fetched, false)
}

// distinctSize adds the sizes of the blobs of img that counted has not seen
// yet, recording them as it goes.
func distinctSize(img *Image, counted map[string]struct{}) int64 {
	var total int64
	add := func(digest string, size int64) {
		if digest == "" {
			total += size
			return
		}
		if _, seen := counted[digest]; seen {
			return
		}
		counted[digest] = struct{}{}
		total += size
	}
	// Use the descriptor from the manifest rather than the parsed config: a
	// child whose config blob is unreadable still counted those bytes in its
	// own total, and an index that skipped them would report itself as smaller
	// than the sum of its platforms.
	add(img.ConfigRef.Digest, img.ConfigRef.Size)
	for _, layer := range img.Layers {
		add(layer.Digest, layer.Size)
	}
	return total
}

// fillManifest populates the single-platform fields of an image.
func (c *client) fillManifest(ctx context.Context, img *Image, doc *manifestDoc) {
	img.ConfigRef = Descriptor{
		MediaType: doc.Config.MediaType,
		Digest:    doc.Config.Digest,
		Size:      doc.Config.Size,
	}
	img.Layers = make([]Layer, 0, len(doc.Layers))
	total := doc.Config.Size
	for _, l := range doc.Layers {
		img.Layers = append(img.Layers, Layer{
			Digest:    l.Digest,
			MediaType: l.MediaType,
			Size:      l.Size,
		})
		total += l.Size
	}
	img.TotalSize = total

	if doc.Config.Digest == "" {
		return
	}
	// The config blob is extra detail, not the point of the request. An image
	// whose config is missing or unreadable is still worth showing.
	raw, err := c.fetchBlobCached(ctx, img.Repository, doc.Config.Digest, maxManifestBytes)
	if err != nil {
		c.log.Debug("registry: config blob unavailable",
			"registry", c.opts.Name, "repository", img.Repository,
			"digest", doc.Config.Digest, "error", err)
		return
	}
	cfg, history, err := parseImageConfig(raw)
	if err != nil {
		c.log.Debug("registry: config blob unreadable",
			"registry", c.opts.Name, "repository", img.Repository,
			"digest", doc.Config.Digest, "error", err)
		return
	}

	cfg.Digest = doc.Config.Digest
	cfg.Size = doc.Config.Size
	if cfg.Size == 0 {
		cfg.Size = int64(len(raw))
	}
	img.Config = cfg
	img.History = history
	applyHistoryToLayers(history, img.Layers)
}

// --- blobs ------------------------------------------------------------------

// fetchBlobCached retrieves a blob through the cache. Only small,
// digest-addressed blobs the client fetches for itself go through here; the
// public Blob method stays uncached so a large layer cannot evict everything.
func (c *client) fetchBlobCached(ctx context.Context, repo, digest string, limit int64) ([]byte, error) {
	key := "blob|" + repo + "|" + digest
	if v, ok := c.cache.get(key); ok {
		if b, ok := v.([]byte); ok {
			return b, nil
		}
	}
	b, err := c.fetchBlob(ctx, repo, digest, limit)
	if err != nil {
		return nil, err
	}
	c.cache.set(key, repo, b, immutableTTL)
	return b, nil
}

func (c *client) fetchBlob(ctx context.Context, repo, digest string, limit int64) ([]byte, error) {
	endpoint := c.base + "/v2/" + repo + "/blobs/" + digest
	resp, err := c.do(ctx, http.MethodGet, endpoint, "")
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := c.checkResponse(resp, http.MethodGet, endpoint); err != nil {
		return nil, err
	}

	// Refuse before reading when the size is declared, and enforce the same
	// limit while reading for chunked responses that declare nothing.
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("registry: blob %s in %s is %d bytes, over the %d byte limit",
			digest, repo, resp.ContentLength, limit)
	}
	data, err := readLimited(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: reading blob %s in %s: %w", digest, repo, err)
	}
	return data, nil
}

// --- pagination -------------------------------------------------------------

func clampPageSize(n int) int {
	switch {
	case n <= 0:
		return defaultPageSize
	case n > maxCatalogPageSize:
		return maxCatalogPageSize
	default:
		return n
	}
}

func lastOf(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1]
}

// nextCursor decides what `last` value the next page should ask for.
//
// The Link header is authoritative when the registry sends one. When it does
// not, a full page is taken as a hint that there is more and the last item
// becomes the cursor. A registry that ignored `n` entirely and returned
// everything gives us more items than we asked for, which is the signal that
// pagination is not supported at all and there is no next page.
func (c *client) nextCursor(linkHeader, lastItem string, returned, requested int) string {
	if cursor := parseLinkNextLast(linkHeader, c.base); cursor != "" {
		return cursor
	}
	if returned == requested && lastItem != "" {
		return lastItem
	}
	return ""
}

// parseLinkNextLast extracts the `last` query parameter from the rel="next"
// entry of an RFC 5988 Link header. The target may be relative, which is what
// Distribution sends.
func parseLinkNextLast(header, base string) string {
	for _, link := range splitLinkHeader(header) {
		target, params := parseLink(link)
		if target == "" || !hasRel(params["rel"], "next") {
			continue
		}
		u, err := url.Parse(target)
		if err != nil {
			continue
		}
		if !u.IsAbs() {
			if b, err := url.Parse(base); err == nil {
				u = b.ResolveReference(u)
			}
		}
		if last := u.Query().Get("last"); last != "" {
			return last
		}
	}
	return ""
}

// splitLinkHeader splits a Link header on the commas that separate links,
// ignoring those inside <> or a quoted string.
func splitLinkHeader(header string) []string {
	var (
		out      []string
		current  strings.Builder
		inAngle  bool
		inQuotes bool
	)
	for i := 0; i < len(header); i++ {
		ch := header[i]
		switch {
		case ch == '\\' && inQuotes && i+1 < len(header):
			current.WriteByte(ch)
			i++
			current.WriteByte(header[i])
			continue
		case ch == '"':
			inQuotes = !inQuotes
		case ch == '<' && !inQuotes:
			inAngle = true
		case ch == '>' && !inQuotes:
			inAngle = false
		case ch == ',' && !inAngle && !inQuotes:
			if s := strings.TrimSpace(current.String()); s != "" {
				out = append(out, s)
			}
			current.Reset()
			continue
		}
		current.WriteByte(ch)
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// parseLink splits one `<target>; key=value; key="value"` link into its parts.
func parseLink(link string) (target string, params map[string]string) {
	params = make(map[string]string)
	start := strings.IndexByte(link, '<')
	end := strings.IndexByte(link, '>')
	if start < 0 || end < start {
		return "", params
	}
	target = strings.TrimSpace(link[start+1 : end])

	for _, part := range strings.Split(link[end+1:], ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		params[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return target, params
}

// hasRel reports whether a rel parameter, which may list several space
// separated relations, contains want.
func hasRel(rel, want string) bool {
	for _, r := range strings.Fields(rel) {
		if strings.EqualFold(r, want) {
			return true
		}
	}
	return false
}

// --- ordering ---------------------------------------------------------------

// sortTags orders tags the way a human reads a version list: "latest" first,
// then newest version first. The ordering within a run of digits is natural —
// digits compare as numbers, so 1.10.0 is greater than 1.9.0 rather than
// sorting before it lexically — and the result is then reversed, because a
// repository with fifty tags is one where the current release must not be on
// the last page. Tags the natural rules consider equal fall back to reverse
// lexical order, which keeps the result deterministic for anything that is not
// version-shaped at all.
func sortTags(tags []string) {
	slices.SortStableFunc(tags, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == "latest":
			return -1
		case b == "latest":
			return 1
		}
		if c := naturalCompare(a, b); c != 0 {
			return -c
		}
		return strings.Compare(b, a)
	})
}

// naturalCompare compares two strings treating each run of digits as a single
// number, so "9" sorts before "10". Leading zeros are ignored for the value
// but not for the tie-break, so "1.0" and "1.00" stay distinguishable.
func naturalCompare(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ad, bd := isASCIIDigit(a[i]), isASCIIDigit(b[j])
		if ad && bd {
			startA, startB := i, j
			for i < len(a) && isASCIIDigit(a[i]) {
				i++
			}
			for j < len(b) && isASCIIDigit(b[j]) {
				j++
			}
			numA := strings.TrimLeft(a[startA:i], "0")
			numB := strings.TrimLeft(b[startB:j], "0")
			if len(numA) != len(numB) {
				if len(numA) < len(numB) {
					return -1
				}
				return 1
			}
			if numA != numB {
				return strings.Compare(numA, numB)
			}
			continue
		}
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case len(a)-i < len(b)-j:
		return -1
	case len(a)-i > len(b)-j:
		return 1
	}
	return 0
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// isAttestationPlatform reports whether a platform is the unknown/unknown
// placeholder BuildKit attaches to attestation manifests. Those children are
// kept in Children so the UI can show them, but they are not real platforms
// and do not belong in a platform list.
func isAttestationPlatform(p Platform) bool {
	return strings.EqualFold(p.OS, "unknown") && strings.EqualFold(p.Architecture, "unknown")
}

// dedupePlatforms removes duplicates, placeholders and empties, and returns
// the rest in a stable order.
func dedupePlatforms(in []Platform) []Platform {
	seen := make(map[string]struct{}, len(in))
	out := make([]Platform, 0, len(in))
	for _, p := range in {
		if p.OS == "" && p.Architecture == "" {
			continue
		}
		if isAttestationPlatform(p) {
			continue
		}
		key := p.String() + "|" + p.OSVersion
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Platform) int {
		if c := strings.Compare(a.OS, b.OS); c != 0 {
			return c
		}
		if c := strings.Compare(a.Architecture, b.Architecture); c != 0 {
			return c
		}
		if c := strings.Compare(a.Variant, b.Variant); c != 0 {
			return c
		}
		return strings.Compare(a.OSVersion, b.OSVersion)
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- page copies ------------------------------------------------------------

// Pages are cached, so callers get a copy: the UI is free to sort or filter
// what it receives without reaching into the cache.

func (p *CatalogPage) clone() *CatalogPage {
	return &CatalogPage{
		Repositories: slices.Clone(p.Repositories),
		NextLast:     p.NextLast,
	}
}

func (p *TagPage) clone() *TagPage {
	return &TagPage{
		Repository: p.Repository,
		Tags:       slices.Clone(p.Tags),
		NextLast:   p.NextLast,
	}
}
