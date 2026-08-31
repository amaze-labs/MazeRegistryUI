// SPDX-License-Identifier: GPL-3.0-or-later

package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     Options
		wantErr  string
		wantBase string
	}{
		{name: "empty base URL", opts: Options{}, wantErr: "base URL is required"},
		{name: "whitespace base URL", opts: Options{BaseURL: "   "}, wantErr: "base URL is required"},
		{name: "unsupported scheme", opts: Options{BaseURL: "ftp://reg.example.com"}, wantErr: "must use the http or https scheme"},
		{name: "no scheme", opts: Options{BaseURL: "reg.example.com"}, wantErr: "must use the http or https scheme"},
		{name: "no host", opts: Options{BaseURL: "https://"}, wantErr: "has no host"},
		{name: "v2 suffix", opts: Options{BaseURL: "https://reg.example.com/v2"}, wantErr: "must not include the /v2 API prefix"},
		{name: "v2 suffix with trailing slash", opts: Options{BaseURL: "https://reg.example.com/v2/"}, wantErr: "must not include the /v2 API prefix"},
		{name: "unknown auth type", opts: Options{BaseURL: "https://reg.example.com", Auth: AuthConfig{Type: "oauth"}}, wantErr: `unknown auth type "oauth"`},
		{name: "plain host", opts: Options{BaseURL: "https://reg.example.com"}, wantBase: "https://reg.example.com"},
		{name: "trailing slash trimmed", opts: Options{BaseURL: "https://reg.example.com/"}, wantBase: "https://reg.example.com"},
		{name: "path prefix kept", opts: Options{BaseURL: "https://reg.example.com/harbor/"}, wantBase: "https://reg.example.com/harbor"},
		{name: "query and fragment dropped", opts: Options{BaseURL: "https://reg.example.com/x?a=b#frag"}, wantBase: "https://reg.example.com/x"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := New(tc.opts)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("New(%+v) succeeded, want an error containing %q", tc.opts, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("New error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%+v) failed: %v", tc.opts, err)
			}
			if got := c.(*client).base; got != tc.wantBase {
				t.Fatalf("normalised base URL = %q, want %q", got, tc.wantBase)
			}
		})
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	c, err := New(Options{BaseURL: "https://reg.example.com"})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	impl := c.(*client)
	if impl.opts.Timeout != defaultTimeout {
		t.Errorf("Timeout = %v, want %v", impl.opts.Timeout, defaultTimeout)
	}
	if impl.opts.CacheTTL != defaultCacheTTL {
		t.Errorf("CacheTTL = %v, want %v", impl.opts.CacheTTL, defaultCacheTTL)
	}
	if impl.opts.UserAgent != defaultUserAgent {
		t.Errorf("UserAgent = %q, want %q", impl.opts.UserAgent, defaultUserAgent)
	}
}

func TestUserAgentIsSent(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.ping()
	c := newTestClient(t, f, func(o *Options) { o.UserAgent = "MazeRegistryUI/9.9" })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	if got := f.lastFor(t, "/v2/").Header.Get("User-Agent"); got != "MazeRegistryUI/9.9" {
		t.Fatalf("User-Agent = %q, want %q", got, "MazeRegistryUI/9.9")
	}
}

func TestPingStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "200 is healthy", status: http.StatusOK},
		{name: "404 maps to ErrNotFound", status: http.StatusNotFound, wantErr: ErrNotFound},
		{name: "401 maps to ErrUnauthorized", status: http.StatusUnauthorized, wantErr: ErrUnauthorized},
		{name: "403 maps to ErrUnauthorized", status: http.StatusForbidden, wantErr: ErrUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handle("/v2/", func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte("{}"))
					return
				}
				writeRegistryError(w, tc.status, "CODE", "message")
			})
			c := newTestClient(t, f)
			err := c.Ping(context.Background())
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Ping failed: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Ping error %v does not wrap %v", err, tc.wantErr)
			}
		})
	}
}

func TestStatusErrorMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  StatusError
		want string
	}{
		{
			name: "with code and message",
			err:  StatusError{Method: "GET", URL: "https://r/v2/", StatusCode: 404, Code: "NAME_UNKNOWN", Message: "no such repo"},
			want: "registry: GET https://r/v2/: 404 Not Found [NAME_UNKNOWN]: no such repo",
		},
		{
			name: "bare status",
			err:  StatusError{Method: "GET", URL: "https://r/v2/", StatusCode: 500},
			want: "registry: GET https://r/v2/: 500 Internal Server Error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("StatusError.Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFirstRegistryError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		body              string
		wantCode, wantMsg string
	}{
		{name: "distribution document", body: `{"errors":[{"code":"DENIED","message":"  nope  "}]}`, wantCode: "DENIED", wantMsg: "nope"},
		{name: "first entry wins", body: `{"errors":[{"code":"A"},{"code":"B"}]}`, wantCode: "A"},
		{name: "empty list", body: `{"errors":[]}`},
		{name: "html from a proxy", body: `<html>502</html>`},
		{name: "empty body", body: ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, msg := firstRegistryError([]byte(tc.body))
			if code != tc.wantCode || msg != tc.wantMsg {
				t.Fatalf("firstRegistryError(%q) = (%q, %q), want (%q, %q)", tc.body, code, msg, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func TestSanitiseURL(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"https://user:pass@reg.example.com/v2/", "https://reg.example.com/v2/"},
		{"https://reg.example.com/v2/", "https://reg.example.com/v2/"},
		{"://nonsense", "://nonsense"},
	}
	for _, tc := range tests {
		if got := sanitiseURL(tc.in); got != tc.want {
			t.Errorf("sanitiseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- catalog and tags -------------------------------------------------------

func TestCatalog(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/_catalog", jsonHandler(`{"repositories":["b","a"," ","c"]}`, ""))

	c := newTestClient(t, f)
	page, err := c.Catalog(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("Catalog failed: %v", err)
	}
	want := []Repository{{Name: "b"}, {Name: "a"}, {Name: "c"}}
	if !reflect.DeepEqual(page.Repositories, want) {
		t.Fatalf("Catalog repositories = %v, want %v (blank entries dropped, order preserved)", page.Repositories, want)
	}
	if page.NextLast != "" {
		t.Errorf("NextLast = %q, want empty: a short page is the last page", page.NextLast)
	}
	q := f.lastFor(t, "/v2/_catalog").Query
	if q.Get("n") != "10" {
		t.Errorf("catalog request n=%q, want 10", q.Get("n"))
	}
	if _, ok := q["last"]; ok {
		t.Errorf("catalog request carried a last parameter on the first page: %v", q)
	}
}

func TestCatalogPageSizeClamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int
		want string
	}{
		{name: "zero becomes the default", in: 0, want: strconv.Itoa(defaultPageSize)},
		{name: "negative becomes the default", in: -5, want: strconv.Itoa(defaultPageSize)},
		{name: "oversized is clamped", in: 99999, want: strconv.Itoa(maxCatalogPageSize)},
		{name: "in range is passed through", in: 25, want: "25"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handle("/v2/_catalog", jsonHandler(`{"repositories":[]}`, ""))
			c := newTestClient(t, f)
			if _, err := c.Catalog(context.Background(), tc.in, ""); err != nil {
				t.Fatalf("Catalog failed: %v", err)
			}
			if got := f.lastFor(t, "/v2/_catalog").Query.Get("n"); got != tc.want {
				t.Fatalf("Catalog(n=%d) requested n=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCatalogIsCachedAndCloned(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/_catalog", jsonHandler(`{"repositories":["a","b"]}`, ""))
	c := newTestClient(t, f)
	ctx := context.Background()

	first, err := c.Catalog(ctx, 10, "")
	if err != nil {
		t.Fatalf("Catalog failed: %v", err)
	}
	first.Repositories[0].Name = "mutated"

	second, err := c.Catalog(ctx, 10, "")
	if err != nil {
		t.Fatalf("second Catalog failed: %v", err)
	}
	if f.countPath("/v2/_catalog") != 1 {
		t.Errorf("the catalog was fetched %d times, want 1: the second call should hit the cache", f.countPath("/v2/_catalog"))
	}
	if second.Repositories[0].Name != "a" {
		t.Errorf("mutating a returned page corrupted the cache: got %q, want %q", second.Repositories[0].Name, "a")
	}
	if first == second {
		t.Error("Catalog returned the same pointer twice; callers must get their own copy")
	}
}

func TestCatalogErrors(t *testing.T) {
	t.Parallel()

	t.Run("undecodable body", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		f.handle("/v2/_catalog", jsonHandler(`{"repositories": nope}`, ""))
		c := newTestClient(t, f)
		if _, err := c.Catalog(context.Background(), 10, ""); err == nil || !strings.Contains(err.Error(), "decoding catalog") {
			t.Fatalf("Catalog error = %v, want one mentioning decoding", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		f.handle("/v2/_catalog", func(w http.ResponseWriter, _ *http.Request) {
			writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "boom")
		})
		c := newTestClient(t, f)
		_, err := c.Catalog(context.Background(), 10, "")
		var se *StatusError
		if !errors.As(err, &se) {
			t.Fatalf("Catalog error = %v, want a *StatusError", err)
		}
		if se.StatusCode != http.StatusInternalServerError || se.Message != "boom" {
			t.Fatalf("StatusError = %+v, want status 500 and message boom", se)
		}
	})
}

func TestTags(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/app/tags/list", jsonHandler(`{"name":"app","tags":["v1.9.0","latest"," ","v1.10.0","v1.2.0"]}`, ""))

	c := newTestClient(t, f)
	page, err := c.Tags(context.Background(), "app", 10, "")
	if err != nil {
		t.Fatalf("Tags failed: %v", err)
	}
	want := []string{"latest", "v1.10.0", "v1.9.0", "v1.2.0"}
	if !reflect.DeepEqual(page.Tags, want) {
		t.Fatalf("Tags = %v, want %v (latest first, then newest version first)", page.Tags, want)
	}
	if page.Repository != "app" {
		t.Errorf("Repository = %q, want app", page.Repository)
	}
}

func TestTagsNullIsEmptySlice(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/empty/tags/list", jsonHandler(`{"name":"empty","tags":null}`, ""))

	c := newTestClient(t, f)
	page, err := c.Tags(context.Background(), "empty", 10, "")
	if err != nil {
		t.Fatalf(`Tags with "tags": null failed: %v`, err)
	}
	if page.Tags == nil {
		t.Fatal(`Tags returned a nil slice for "tags": null, want an empty non-nil slice`)
	}
	if len(page.Tags) != 0 {
		t.Fatalf("Tags = %v, want empty", page.Tags)
	}
}

func TestSortTags(t *testing.T) {
	t.Parallel()

	// The display order is "latest" first, then newest version first, so the
	// current release is never pushed onto the last page of a long tag list.
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "latest first",
			in:   []string{"v2", "latest", "v1"},
			want: []string{"latest", "v2", "v1"},
		},
		{
			name: "numeric runs compare as numbers, newest first",
			in:   []string{"v1.2.0", "v1.10.0", "v1.9.0"},
			want: []string{"v1.10.0", "v1.9.0", "v1.2.0"},
		},
		{
			name: "leading zeros do not change the value",
			in:   []string{"v9", "v010"},
			want: []string{"v010", "v9"},
		},
		{
			name: "non version-shaped tags fall back to reverse lexical order",
			in:   []string{"edge", "beta", "stable"},
			want: []string{"stable", "edge", "beta"},
		},
		{
			name: "longer version sorts before its prefix",
			in:   []string{"v1.0", "v1.0.1"},
			want: []string{"v1.0.1", "v1.0"},
		},
		{
			name: "duplicate tags are stable",
			in:   []string{"v1", "v1", "v2"},
			want: []string{"v2", "v1", "v1"},
		},
		{
			name: "latest anywhere in the input still comes first",
			in:   []string{"v1", "v2", "latest"},
			want: []string{"latest", "v2", "v1"},
		},
		{
			name: "only latest",
			in:   []string{"latest"},
			want: []string{"latest"},
		},
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := append([]string(nil), tc.in...)
			sortTags(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sortTags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// naturalCompare is the ordering primitive: it must place v1.10.0 after
// v1.9.0, which is the whole point of comparing digit runs as numbers.
func TestNaturalCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{a: "v1.9.0", b: "v1.10.0", want: -1},
		{a: "v1.10.0", b: "v1.9.0", want: 1},
		{a: "v9", b: "v10", want: -1},
		{a: "v010", b: "v9", want: 1},
		{a: "v1.0", b: "v1.0", want: 0},
		{a: "v1.0", b: "v1.0.1", want: -1},
		{a: "abc", b: "abd", want: -1},
		{a: "", b: "a", want: -1},
		{a: "", b: "", want: 0},
		{a: "1", b: "01", want: 0}, // equal as numbers; the tie-break is elsewhere
	}
	for _, tc := range tests {
		if got := naturalCompare(tc.a, tc.b); got != tc.want {
			t.Errorf("naturalCompare(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNaturalCompareIsAntisymmetric(t *testing.T) {
	t.Parallel()

	samples := []string{"v1", "v1.0", "v1.10", "v1.9", "v01", "latest", "", "a1b2", "a1b10"}
	for _, a := range samples {
		for _, b := range samples {
			ab, ba := naturalCompare(a, b), naturalCompare(b, a)
			if ab != -ba {
				t.Errorf("naturalCompare(%q,%q)=%d but naturalCompare(%q,%q)=%d; the ordering is not antisymmetric", a, b, ab, b, a, ba)
			}
		}
	}
}

// --- pagination -------------------------------------------------------------

func TestLinkHeaderPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		link string
		body string
		n    int
		want string
	}{
		{
			name: "relative link, as Distribution sends it",
			link: `</v2/_catalog?n=2&last=b>; rel="next"`,
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "b",
		},
		{
			name: "absolute link",
			link: `<https://other.example.com/v2/_catalog?n=2&last=zz>; rel="next"`,
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "zz",
		},
		{
			name: "several links, only next counts",
			link: `</v2/_catalog?n=2&last=aa>; rel="prev", </v2/_catalog?n=2&last=cc>; rel="next"`,
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "cc",
		},
		{
			name: "rel listing several relations",
			link: `</v2/_catalog?n=2&last=dd>; rel="next noopener"`,
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "dd",
		},
		{
			name: "no link header, full page falls back to the last item",
			link: "",
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "b",
		},
		{
			name: "no link header, short page has no next",
			link: "",
			body: `{"repositories":["a"]}`,
			n:    2,
			want: "",
		},
		{
			name: "registry ignored n and returned more than asked: pagination is unsupported",
			link: "",
			body: `{"repositories":["a","b","c"]}`,
			n:    2,
			want: "",
		},
		{
			name: "link header without a last parameter",
			link: `</v2/_catalog?n=2>; rel="next"`,
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "b",
		},
		{
			name: "unparseable link header falls back",
			link: `garbage`,
			body: `{"repositories":["a","b"]}`,
			n:    2,
			want: "b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handle("/v2/_catalog", jsonHandler(tc.body, tc.link))
			c := newTestClient(t, f)
			page, err := c.Catalog(context.Background(), tc.n, "")
			if err != nil {
				t.Fatalf("Catalog failed: %v", err)
			}
			if page.NextLast != tc.want {
				t.Fatalf("NextLast = %q, want %q (Link: %q)", page.NextLast, tc.want, tc.link)
			}
		})
	}
}

func TestTagsLinkHeaderPagination(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("last") {
		case "":
			w.Header().Set("Link", `</v2/app/tags/list?n=2&last=v2>; rel="next"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"app","tags":["v1","v2"]}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"app","tags":["v3"]}`))
		}
	})

	c := newTestClient(t, f)
	ctx := context.Background()

	first, err := c.Tags(ctx, "app", 2, "")
	if err != nil {
		t.Fatalf("first Tags page failed: %v", err)
	}
	if first.NextLast != "v2" {
		t.Fatalf("first page NextLast = %q, want v2", first.NextLast)
	}

	second, err := c.Tags(ctx, "app", 2, first.NextLast)
	if err != nil {
		t.Fatalf("second Tags page failed: %v", err)
	}
	if !reflect.DeepEqual(second.Tags, []string{"v3"}) {
		t.Fatalf("second page tags = %v, want [v3]", second.Tags)
	}
	if second.NextLast != "" {
		t.Fatalf("second page NextLast = %q, want empty", second.NextLast)
	}
	if got := f.lastFor(t, "/v2/app/tags/list").Query.Get("last"); got != "v2" {
		t.Fatalf("second page requested last=%q, want v2", got)
	}
}

// A registry that keeps answering with a full page and the same cursor must
// not be able to spin a caller forever. Catalog itself fetches exactly one
// page, so the cursor it reports is the caller's termination signal: it must
// be one the caller can recognise as making no progress.
func TestPaginationTerminatesOnAnUnchangedCursor(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/_catalog", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `</v2/_catalog?n=2&last=b>; rel="next"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repositories":["a","b"]}`))
	})

	c := newTestClient(t, f)
	ctx := context.Background()

	var (
		last  string
		pages int
	)
	for pages < 50 { // bounded so a broken client fails instead of hanging
		page, err := c.Catalog(ctx, 2, last)
		if err != nil {
			t.Fatalf("Catalog failed on page %d: %v", pages, err)
		}
		pages++
		if page.NextLast == "" || page.NextLast == last {
			break
		}
		last = page.NextLast
	}
	if pages >= 50 {
		t.Fatal("the catalog cursor never stopped advancing against a registry that repeats the same page")
	}
	if pages != 2 {
		t.Fatalf("walked %d pages, want 2: the first page plus the one that proves the cursor is stuck", pages)
	}
}

func TestSplitLinkHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "single", in: `</a>; rel="next"`, want: []string{`</a>; rel="next"`}},
		{
			name: "comma inside angle brackets is not a separator",
			in:   `</a?x=1,2>; rel="next"`,
			want: []string{`</a?x=1,2>; rel="next"`},
		},
		{
			name: "comma inside a quoted string is not a separator",
			in:   `</a>; rel="next", </b>; title="x,y"`,
			want: []string{`</a>; rel="next"`, `</b>; title="x,y"`},
		},
		{
			name: "escaped quote inside a quoted string",
			in:   `</a>; title="a\"b", </b>; rel="next"`,
			want: []string{`</a>; title="a\"b"`, `</b>; rel="next"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := splitLinkHeader(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitLinkHeader(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseLink(t *testing.T) {
	t.Parallel()

	target, params := parseLink(`</v2/_catalog?n=2&last=b>; rel="next"; title=plain`)
	if target != "/v2/_catalog?n=2&last=b" {
		t.Errorf("target = %q", target)
	}
	if params["rel"] != "next" || params["title"] != "plain" {
		t.Errorf("params = %v", params)
	}

	if target, _ := parseLink(`no angle brackets`); target != "" {
		t.Errorf("parseLink on a malformed link returned target %q, want empty", target)
	}
}

func TestHasRel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		rel, want string
		expect    bool
	}{
		{"next", "next", true},
		{"NEXT", "next", true},
		{"prev next", "next", true},
		{"prev", "next", false},
		{"", "next", false},
	}
	for _, tc := range cases {
		if got := hasRel(tc.rel, tc.want); got != tc.expect {
			t.Errorf("hasRel(%q,%q) = %v, want %v", tc.rel, tc.want, got, tc.expect)
		}
	}
}

// --- path safety ------------------------------------------------------------

func TestPathSafetyRejectsBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	badRepos := []struct{ name, repo string }{
		{"parent traversal", "../etc/passwd"},
		{"traversal in the middle", "lib/../../etc"},
		{"percent-encoded traversal", "%2e%2e/secret"},
		{"double-encoded traversal", "%252e%252e/secret"},
		{"space", "my repo"},
		{"uppercase", "Library/Alpine"},
		{"leading slash", "/library/alpine"},
		{"trailing slash", "library/alpine/"},
		{"empty component", "library//alpine"},
		{"empty", ""},
		{"newline", "app\nX-Injected: 1"},
		{"query smuggling", "app?n=1"},
		{"fragment", "app#frag"},
		{"at sign", "user@host/app"},
		{"colon", "app:tag"},
		{"backslash", `app\..\etc`},
		{"absolute URL", "http://evil.example.com/x"},
	}

	for _, tc := range badRepos {
		t.Run("repository/"+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handleFallback(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("the registry was contacted at %s %s for repository %q; validation must happen first", r.Method, r.URL, tc.repo)
				w.WriteHeader(http.StatusOK)
			})
			c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
			ctx := context.Background()

			if _, err := c.Tags(ctx, tc.repo, 10, ""); err == nil {
				t.Errorf("Tags(%q) succeeded, want a validation error", tc.repo)
			}
			if _, err := c.Image(ctx, tc.repo, "latest", false); err == nil {
				t.Errorf("Image(%q) succeeded, want a validation error", tc.repo)
			}
			if _, err := c.Blob(ctx, tc.repo, fakeDigest("x"), 1024); err == nil {
				t.Errorf("Blob(%q) succeeded, want a validation error", tc.repo)
			}
			if err := c.DeleteManifest(ctx, tc.repo, fakeDigest("x")); err == nil {
				t.Errorf("DeleteManifest(%q) succeeded, want a validation error", tc.repo)
			}
			if got := f.total(); got != 0 {
				t.Fatalf("the fake registry received %d requests for repository %q, want 0: %v",
					got, tc.repo, requestPaths(f.snapshot()))
			}
		})
	}

	badRefs := []struct{ name, ref string }{
		{"parent traversal", "../../etc"},
		{"percent-encoded traversal", "%2e%2e"},
		{"space", "my tag"},
		{"leading slash", "/latest"},
		{"leading dot", ".latest"},
		{"leading dash", "-latest"},
		{"empty", ""},
		{"slash", "a/b"},
		{"too long", strings.Repeat("a", 129)},
		{"wrong digest algorithm", "sha512:" + strings.Repeat("a", 128)},
		{"short sha256", "sha256:abc"},
		{"uppercase digest", "sha256:" + strings.Repeat("A", 64)},
	}

	for _, tc := range badRefs {
		t.Run("reference/"+tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handleFallback(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("the registry was contacted at %s %s for reference %q; validation must happen first", r.Method, r.URL, tc.ref)
				w.WriteHeader(http.StatusOK)
			})
			c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
			ctx := context.Background()

			if _, err := c.Image(ctx, "app", tc.ref, false); err == nil {
				t.Errorf("Image(app, %q) succeeded, want a validation error", tc.ref)
			}
			if err := c.DeleteManifest(ctx, "app", tc.ref); err == nil {
				t.Errorf("DeleteManifest(app, %q) succeeded, want a validation error", tc.ref)
			}
			if got := f.total(); got != 0 {
				t.Fatalf("the fake registry received %d requests for reference %q, want 0: %v",
					got, tc.ref, requestPaths(f.snapshot()))
			}
		})
	}
}

func TestValidateRepositoryAccepts(t *testing.T) {
	t.Parallel()

	good := []string{
		"alpine",
		"library/alpine",
		"a/b/c/d",
		"my-repo",
		"my__repo",
		"my.repo",
		"repo-1.2_3",
		"0",
	}
	for _, repo := range good {
		if err := validateRepository(repo); err != nil {
			t.Errorf("validateRepository(%q) rejected a valid name: %v", repo, err)
		}
	}
	long := strings.Repeat("a", 256)
	if err := validateRepository(long); err == nil || !strings.Contains(err.Error(), "255") {
		t.Errorf("validateRepository on a 256-character name returned %v, want a length error", err)
	}
}

func TestValidateReferenceAccepts(t *testing.T) {
	t.Parallel()

	good := []string{"latest", "v1.2.3", "_underscore", "A", strings.Repeat("a", 128), "sha256:" + strings.Repeat("0", 64)}
	for _, ref := range good {
		if err := validateReference(ref); err != nil {
			t.Errorf("validateReference(%q) rejected a valid reference: %v", ref, err)
		}
	}
}

func TestDigestOf(t *testing.T) {
	t.Parallel()

	// The well-known sha256 of the empty string.
	if got, want := digestOf(nil), "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"; got != want {
		t.Fatalf("digestOf(nil) = %q, want %q", got, want)
	}
	if !isDigest(digestOf([]byte("hello"))) {
		t.Fatal("digestOf produced something isDigest rejects")
	}
}

// --- manifests over the wire ------------------------------------------------

func TestImageOCIManifest(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	cfgRaw := configBlob(t, "linux", "amd64", time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC),
		[]map[string]any{
			{"created_by": "/bin/sh -c #(nop) ADD file:a in /"},
			{"created_by": "/bin/sh -c apk add curl"},
		},
		map[string]any{"Labels": map[string]string{"a": "1"}, "Env": []string{"PATH=/bin"}})
	cfgDigest := f.serveBlob("app", cfgRaw)

	l1, l2 := fakeDigest("layer1"), fakeDigest("layer2")
	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: cfgDigest, Size: int64(len(cfgRaw))},
		testDescriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: l1, Size: 1000},
		testDescriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: l2, Size: 2000},
	)
	wantDigest := f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "latest", false)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}

	if img.IsIndex {
		t.Error("IsIndex is true for a single-platform manifest")
	}
	if img.Digest != wantDigest {
		t.Errorf("Digest = %q, want %q", img.Digest, wantDigest)
	}
	if img.MediaType != MediaTypeOCIManifest {
		t.Errorf("MediaType = %q, want %q", img.MediaType, MediaTypeOCIManifest)
	}
	if img.ManifestSize != int64(len(raw)) {
		t.Errorf("ManifestSize = %d, want %d", img.ManifestSize, len(raw))
	}
	if want := int64(len(cfgRaw)) + 3000; img.TotalSize != want {
		t.Errorf("TotalSize = %d, want %d (config plus both layers)", img.TotalSize, want)
	}
	if len(img.Layers) != 2 {
		t.Fatalf("Layers has %d entries, want 2", len(img.Layers))
	}
	if img.Layers[0].Command != "ADD file:a in /" || img.Layers[1].Command != "RUN apk add curl" {
		t.Errorf("layer commands = %q / %q, want the zipped history", img.Layers[0].Command, img.Layers[1].Command)
	}
	if img.Config == nil {
		t.Fatal("Config is nil, want the parsed config blob")
	}
	if img.Config.Digest != cfgDigest {
		t.Errorf("Config.Digest = %q, want %q", img.Config.Digest, cfgDigest)
	}

	// The Accept header must list everything the client can read; omitting one
	// invites the registry to fall back to schema 1.
	accept := f.lastFor(t, "/v2/app/manifests/latest").Header.Get("Accept")
	for _, mt := range []string{MediaTypeOCIIndex, MediaTypeOCIManifest, MediaTypeDockerList, MediaTypeDockerV2} {
		if !strings.Contains(accept, mt) {
			t.Errorf("Accept header %q does not offer %q", accept, mt)
		}
	}
}

func TestImageDockerV2Manifest(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	cfgRaw := configBlob(t, "linux", "amd64", time.Time{}, nil, nil)
	cfgDigest := f.serveBlob("app", cfgRaw)
	raw := imageManifest(t, MediaTypeDockerV2,
		testDescriptor{MediaType: MediaTypeDockerConfig, Digest: cfgDigest, Size: int64(len(cfgRaw))},
		testDescriptor{MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", Digest: fakeDigest("dl1"), Size: 7},
	)
	f.serveManifest("app", "v2", MediaTypeDockerV2, raw)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "v2", false)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	if img.IsIndex {
		t.Error("a Docker v2 manifest was classified as an index")
	}
	if img.MediaType != MediaTypeDockerV2 {
		t.Errorf("MediaType = %q, want %q", img.MediaType, MediaTypeDockerV2)
	}
}

func TestImageSchema1IsUnsupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mediaType string
		body      string
	}{
		{
			name:      "signed schema 1",
			mediaType: mediaTypeDockerV1Signed,
			body:      `{"schemaVersion":1,"name":"app","tag":"old","fsLayers":[{"blobSum":"sha256:x"}]}`,
		},
		{
			name:      "unsigned schema 1",
			mediaType: mediaTypeDockerV1,
			body:      `{"schemaVersion":2,"name":"app"}`,
		},
		{
			name:      "schema 1 shape with a generic content type",
			mediaType: "application/json",
			body:      `{"schemaVersion":1,"fsLayers":[]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handle("/v2/app/manifests/old", func(w http.ResponseWriter, _ *http.Request) {
				writeManifest(w, tc.mediaType, []byte(tc.body), true)
			})
			c := newTestClient(t, f)
			_, err := c.Image(context.Background(), "app", "old", false)
			if err == nil {
				t.Fatal("Image on a schema 1 manifest succeeded, want ErrUnsupported")
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Image error %v does not wrap ErrUnsupported", err)
			}
		})
	}
}

func TestManifestDigestHandling(t *testing.T) {
	t.Parallel()

	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("c"), Size: 1})
	computed := digestOf(raw)

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "header is trusted when present", header: computed, want: computed},
		{name: "absent header falls back to the computed digest", header: "", want: computed},
		{name: "malformed header is ignored", header: "not-a-digest", want: computed},
		{name: "wrong algorithm is ignored", header: "sha512:" + strings.Repeat("a", 128), want: computed},
		{name: "surrounding whitespace is trimmed", header: "  " + computed + "  ", want: computed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			f.handle("/v2/app/manifests/latest", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", MediaTypeOCIManifest)
				if tc.header != "" {
					w.Header().Set("Docker-Content-Digest", tc.header)
				}
				_, _ = w.Write(raw)
			})
			c := newTestClient(t, f)
			img, err := c.Image(context.Background(), "app", "latest", false)
			if err != nil {
				t.Fatalf("Image failed: %v", err)
			}
			if img.Digest != tc.want {
				t.Fatalf("Digest = %q, want %q", img.Digest, tc.want)
			}
		})
	}
}

// A registry advertising a digest that does not match what it served is broken
// or lying. The bytes are the authority — they are what a client would pull
// by — so the computed digest must win.
func TestManifestDigestHeaderIsVerifiedAgainstTheBytes(t *testing.T) {
	t.Parallel()

	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("c"), Size: 1})
	lie := fakeDigest("a different document entirely")

	f := newFakeRegistry(t)
	f.handle("/v2/app/manifests/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", MediaTypeOCIManifest)
		w.Header().Set("Docker-Content-Digest", lie)
		_, _ = w.Write(raw)
	})
	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "latest", false)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	if img.Digest == lie {
		t.Fatalf("Digest = %q: the advertised digest was trusted over the bytes it came with", img.Digest)
	}
	if want := digestOf(raw); img.Digest != want {
		t.Fatalf("Digest = %q, want %q computed from the manifest bytes", img.Digest, want)
	}
}

func TestManifestNotFound(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	c := newTestClient(t, f)
	_, err := c.Image(context.Background(), "app", "nope", false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Image error %v does not wrap ErrNotFound", err)
	}
}

func TestManifestIsCachedByTagAndDigest(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("c"), Size: 1})
	dgst := f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)

	c := newTestClient(t, f)
	ctx := context.Background()

	if _, err := c.Image(ctx, "app", "latest", false); err != nil {
		t.Fatalf("Image by tag failed: %v", err)
	}
	// Fetching the same content by digest must hit the cache the tag fetch
	// populated, not the network.
	if _, err := c.Image(ctx, "app", dgst, false); err != nil {
		t.Fatalf("Image by digest failed: %v", err)
	}
	if got := f.countPath("/v2/app/manifests/" + dgst); got != 0 {
		t.Fatalf("the digest route was hit %d times, want 0: the tag fetch should have cached it by digest too", got)
	}
	if got := f.countPath("/v2/app/manifests/latest"); got != 1 {
		t.Fatalf("the tag route was hit %d times, want 1", got)
	}
}

func TestRawManifestIsCopiedPerCaller(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("c"), Size: 1})
	f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)

	c := newTestClient(t, f)
	ctx := context.Background()

	first, err := c.Image(ctx, "app", "latest", false)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	first.RawManifest[0] = 'X'

	second, err := c.Image(ctx, "app", "latest", false)
	if err != nil {
		t.Fatalf("second Image failed: %v", err)
	}
	if second.RawManifest[0] == 'X' {
		t.Fatal("writing into a returned RawManifest corrupted the cached bytes")
	}
}

// --- indexes ----------------------------------------------------------------

// indexFixture wires an index with two real platforms sharing a layer, plus an
// attestation child, and returns the index digest.
func indexFixture(t *testing.T, f *fakeRegistry) (indexDigest string, childDigests []string) {
	t.Helper()

	sharedLayer := fakeDigest("shared-base-layer")
	amdCfg := configBlob(t, "linux", "amd64", time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC), nil, nil)
	armCfg := configBlob(t, "linux", "arm64", time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC), nil, nil)
	amdCfgDigest := f.serveBlob("app", amdCfg)
	armCfgDigest := f.serveBlob("app", armCfg)

	amdRaw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: amdCfgDigest, Size: int64(len(amdCfg))},
		testDescriptor{Digest: sharedLayer, Size: 1000},
		testDescriptor{Digest: fakeDigest("amd-only"), Size: 500},
	)
	armRaw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: armCfgDigest, Size: int64(len(armCfg))},
		testDescriptor{Digest: sharedLayer, Size: 1000},
		testDescriptor{Digest: fakeDigest("arm-only"), Size: 700},
	)
	attRaw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("att-cfg"), Size: 2},
		testDescriptor{Digest: fakeDigest("att-layer"), Size: 10},
	)

	amdDigest := f.serveManifest("app", "", MediaTypeOCIManifest, amdRaw)
	armDigest := f.serveManifest("app", "", MediaTypeOCIManifest, armRaw)
	attDigest := f.serveManifest("app", "", MediaTypeOCIManifest, attRaw)

	idx := indexManifest(t, MediaTypeOCIIndex,
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: amdDigest, Size: int64(len(amdRaw)),
			Platform: &testPlatform{OS: "linux", Architecture: "amd64"}},
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: armDigest, Size: int64(len(armRaw)),
			Platform: &testPlatform{OS: "linux", Architecture: "arm64"}},
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: attDigest, Size: int64(len(attRaw)),
			Platform: &testPlatform{OS: "unknown", Architecture: "unknown"}},
		// A child with an unusable digest must be dropped, not shown.
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: "not-a-digest", Size: 1},
	)
	indexDigest = f.serveManifest("app", "multi", MediaTypeOCIIndex, idx)
	return indexDigest, []string{amdDigest, armDigest, attDigest}
}

func TestImageIndexResolvesChildren(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	indexDigest, children := indexFixture(t, f)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "multi", true)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}

	if !img.IsIndex {
		t.Fatal("IsIndex is false for an OCI index")
	}
	if img.Digest != indexDigest {
		t.Errorf("Digest = %q, want %q", img.Digest, indexDigest)
	}
	if len(img.Children) != 3 {
		t.Fatalf("Children has %d entries, want 3: the child with an unusable digest must be dropped", len(img.Children))
	}
	for i, child := range img.Children {
		if child.Digest != children[i] {
			t.Errorf("child %d digest = %q, want %q", i, child.Digest, children[i])
		}
		if child.Resolved == nil {
			t.Errorf("child %d (%s) was not resolved", i, child.Digest)
			continue
		}
		if child.SizeTotal != child.Resolved.TotalSize {
			t.Errorf("child %d SizeTotal = %d, want %d", i, child.SizeTotal, child.Resolved.TotalSize)
		}
	}

	// The attestation child stays in Children so the UI can show it.
	att := img.Children[2]
	if att.Platform == nil || !isAttestationPlatform(*att.Platform) {
		t.Fatalf("the third child is %v, want the unknown/unknown attestation placeholder", att.Platform)
	}
}

func TestImageIndexTotalSizeDeduplicatesSharedBlobs(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	_, _ = indexFixture(t, f)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "multi", true)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}

	var naive int64
	for _, child := range img.Children {
		naive += child.SizeTotal
	}
	if img.TotalSize >= naive {
		t.Fatalf("TotalSize = %d but the naive per-child sum is %d; the shared base layer was counted more than once",
			img.TotalSize, naive)
	}
	// The shared 1000-byte layer must appear exactly once in the total, and
	// nothing else may go missing: sizes come from the manifest descriptors,
	// so a child whose config blob cannot be fetched still contributes its
	// declared config bytes.
	if want := naive - 1000; img.TotalSize != want {
		t.Fatalf("TotalSize = %d, want %d (the 1000-byte shared layer counted once)", img.TotalSize, want)
	}
}

// A child whose config blob cannot be fetched still declares a config size in
// its manifest, and its own TotalSize counts it. The index total must agree
// rather than silently reporting the image as smaller than its platforms.
func TestImageIndexTotalSizeIgnoresUnreadableConfigBlobs(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	// The config blob is declared in the manifest but never served.
	cfgDigest := fakeDigest("unserved-config")
	childRaw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: cfgDigest, Size: 500},
		testDescriptor{Digest: fakeDigest("only-layer"), Size: 1000})
	childDigest := f.serveManifest("app", "", MediaTypeOCIManifest, childRaw)
	f.serveManifest("app", "multi", MediaTypeOCIIndex, indexManifest(t, MediaTypeOCIIndex,
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: childDigest, Size: int64(len(childRaw)),
			Platform: &testPlatform{OS: "linux", Architecture: "amd64"}}))

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "multi", true)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	if img.TotalSize != img.Children[0].SizeTotal {
		t.Fatalf("index TotalSize = %d but its only child reports SizeTotal = %d; the declared config size was dropped",
			img.TotalSize, img.Children[0].SizeTotal)
	}
}

func TestDistinctSize(t *testing.T) {
	t.Parallel()

	counted := make(map[string]struct{})
	img := &Image{
		ConfigRef: Descriptor{Digest: "sha256:cfg", Size: 10},
		Layers: []Layer{
			{Digest: "sha256:a", Size: 100},
			{Digest: "sha256:a", Size: 100}, // the same blob twice in one manifest
			{Digest: "", Size: 7},           // no digest: always counted
		},
	}
	if got, want := distinctSize(img, counted), int64(117); got != want {
		t.Fatalf("distinctSize = %d, want %d", got, want)
	}
	// A second image sharing the blobs adds nothing but its undigested bytes.
	if got := distinctSize(img, counted); got != 7 {
		t.Fatalf("distinctSize on a second pass = %d, want 7", got)
	}
}

func TestImageIndexWithoutChildResolution(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	_, _ = indexFixture(t, f)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "multi", false)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	for i, child := range img.Children {
		if child.Resolved != nil {
			t.Errorf("child %d was resolved even though resolveChildren was false", i)
		}
	}
	if img.TotalSize != 0 {
		t.Errorf("TotalSize = %d, want 0: nothing was resolved to measure", img.TotalSize)
	}
	if n := f.countPath("/v2/app/manifests/" + img.Children[0].Digest); n != 0 {
		t.Errorf("a child manifest was fetched %d times with resolveChildren=false, want 0", n)
	}
}

func TestImageIndexBrokenChildDegradesGracefully(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	goodRaw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("gc"), Size: 5},
		testDescriptor{Digest: fakeDigest("gl"), Size: 100})
	goodDigest := f.serveManifest("app", "", MediaTypeOCIManifest, goodRaw)
	brokenDigest := fakeDigest("never served")

	idx := indexManifest(t, MediaTypeOCIIndex,
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: goodDigest, Size: int64(len(goodRaw)),
			Platform: &testPlatform{OS: "linux", Architecture: "amd64"}},
		testDescriptor{MediaType: MediaTypeOCIManifest, Digest: brokenDigest, Size: 1,
			Platform: &testPlatform{OS: "linux", Architecture: "arm64"}},
	)
	f.serveManifest("app", "multi", MediaTypeOCIIndex, idx)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "multi", true)
	if err != nil {
		t.Fatalf("Image failed even though only one child is broken: %v", err)
	}
	if img.Children[0].Resolved == nil {
		t.Error("the healthy child was not resolved")
	}
	if img.Children[1].Resolved != nil {
		t.Error("the broken child has a non-nil Resolved")
	}
	// The resolvable child's 100 layer bytes plus the 5 config bytes its
	// manifest declares. The config blob is never served, which must not make
	// the index look smaller than the child it contains.
	if img.TotalSize != 105 {
		t.Errorf("TotalSize = %d, want 105: the resolvable child's layers and its declared config", img.TotalSize)
	}
}

// Children of an index are fetched concurrently. With more children than the
// concurrency limit and a handler that blocks until several are in flight,
// a serial implementation would deadlock the test.
func TestImageIndexResolvesChildrenConcurrently(t *testing.T) {
	t.Parallel()

	const children = maxChildConcurrency
	f := newFakeRegistry(t)

	inFlight := make(chan struct{}, children)
	release := make(chan struct{})

	var descs []testDescriptor
	for i := range children {
		raw := imageManifest(t, MediaTypeOCIManifest,
			testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest(fmt.Sprintf("cfg%d", i)), Size: 1},
			testDescriptor{Digest: fakeDigest(fmt.Sprintf("layer%d", i)), Size: int64(10 * (i + 1))})
		dgst := digestOf(raw)
		f.handle("/v2/app/manifests/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
			inFlight <- struct{}{}
			<-release
			writeManifest(w, MediaTypeOCIManifest, raw, true)
		})
		descs = append(descs, testDescriptor{
			MediaType: MediaTypeOCIManifest, Digest: dgst, Size: int64(len(raw)),
			Platform: &testPlatform{OS: "linux", Architecture: fmt.Sprintf("arch%d", i)},
		})
	}
	f.serveManifest("app", "multi", MediaTypeOCIIndex, indexManifest(t, MediaTypeOCIIndex, descs...))

	// Unblock the handlers only once every child request has arrived.
	go func() {
		for range children {
			<-inFlight
		}
		close(release)
	}()

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "multi", true)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	for i, child := range img.Children {
		if child.Resolved == nil {
			t.Fatalf("child %d was not resolved", i)
		}
	}
}

func TestImageIndexCancelledContext(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	_, _ = indexFixture(t, f)
	c := newTestClient(t, f)

	// Fetch the index first so only the child resolution sees the cancellation.
	fetched, err := c.fetchManifest(context.Background(), "app", "multi")
	if err != nil {
		t.Fatalf("fetchManifest failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	img, err := c.buildImage(ctx, "app", "multi", fetched, true)
	if err != nil {
		t.Fatalf("buildImage on a cancelled context failed: %v", err)
	}
	if len(img.Children) == 0 {
		t.Fatal("Children is empty; the index descriptors should still be there")
	}
}

// --- graceful degradation ---------------------------------------------------

func TestImageWithUnreadableConfigStillHasLayers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "config blob returns 500",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "storage is down")
			},
		},
		{
			name: "config blob is missing",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeRegistryError(w, http.StatusNotFound, "BLOB_UNKNOWN", "gone")
			},
		},
		{
			name: "config blob is not JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("this is not a config"))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			cfgDigest := fakeDigest("broken-config")
			f.handle("/v2/app/blobs/"+cfgDigest, tc.handler)

			raw := imageManifest(t, MediaTypeOCIManifest,
				testDescriptor{MediaType: MediaTypeOCIConfig, Digest: cfgDigest, Size: 42},
				testDescriptor{Digest: fakeDigest("l1"), Size: 1000},
			)
			f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)

			c := newTestClient(t, f)
			img, err := c.Image(context.Background(), "app", "latest", false)
			if err != nil {
				t.Fatalf("Image failed even though only the config blob is broken: %v", err)
			}
			if len(img.Layers) != 1 {
				t.Fatalf("Layers has %d entries, want 1", len(img.Layers))
			}
			if img.Config != nil {
				t.Errorf("Config = %+v, want nil when the config blob cannot be read", img.Config)
			}
			if img.TotalSize != 1042 {
				t.Errorf("TotalSize = %d, want 1042: the declared config size still counts", img.TotalSize)
			}
		})
	}
}

func TestImageWithNoConfigDescriptor(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	raw := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaTypeOCIManifest,
		"layers":        []testDescriptor{{Digest: fakeDigest("l1"), Size: 5}},
	})
	f.serveManifest("app", "latest", MediaTypeOCIManifest, raw)

	c := newTestClient(t, f)
	img, err := c.Image(context.Background(), "app", "latest", false)
	if err != nil {
		t.Fatalf("Image failed: %v", err)
	}
	if img.Config != nil {
		t.Errorf("Config = %+v, want nil", img.Config)
	}
	if got := f.countPath("/v2/app/blobs/"); got != 0 {
		t.Errorf("a blob was requested for a manifest with no config descriptor")
	}
}

func TestConfigBlobIsCached(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	cfgRaw := configBlob(t, "linux", "amd64", time.Time{}, nil, nil)
	cfgDigest := f.serveBlob("app", cfgRaw)

	// Two manifests sharing one config blob.
	for _, tag := range []string{"a", "b"} {
		raw := imageManifest(t, MediaTypeOCIManifest,
			testDescriptor{MediaType: MediaTypeOCIConfig, Digest: cfgDigest, Size: int64(len(cfgRaw))},
			testDescriptor{Digest: fakeDigest("layer-" + tag), Size: 1})
		f.serveManifest("app", tag, MediaTypeOCIManifest, raw)
	}

	c := newTestClient(t, f)
	ctx := context.Background()
	for _, tag := range []string{"a", "b"} {
		if _, err := c.Image(ctx, "app", tag, false); err != nil {
			t.Fatalf("Image(%s) failed: %v", tag, err)
		}
	}
	if got := f.countPath("/v2/app/blobs/" + cfgDigest); got != 1 {
		t.Fatalf("the shared config blob was fetched %d times, want 1", got)
	}
}

// --- TagSummary -------------------------------------------------------------

func TestTagSummary(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	_, _ = indexFixture(t, f)

	c := newTestClient(t, f)
	sum := c.TagSummary(context.Background(), "app", "multi")
	if sum == nil {
		t.Fatal("TagSummary returned nil; it must always return a row")
	}
	if sum.Err != "" {
		t.Fatalf("TagSummary reported an error: %s", sum.Err)
	}
	if !sum.IsIndex {
		t.Error("IsIndex is false for an index")
	}
	if sum.Name != "multi" {
		t.Errorf("Name = %q, want multi", sum.Name)
	}
	want := []Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}
	if !reflect.DeepEqual(sum.Platforms, want) {
		t.Errorf("Platforms = %v, want %v: the unknown/unknown attestation child must be excluded", sum.Platforms, want)
	}
	if sum.Created.IsZero() {
		t.Error("Created is zero; an index should borrow a resolved child's creation time")
	}
	if sum.Size == 0 {
		t.Error("Size is zero; it should be the deduplicated total")
	}
}

func TestTagSummarySinglePlatform(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	created := time.Date(2023, 7, 7, 12, 0, 0, 0, time.UTC)
	cfgRaw := configBlob(t, "linux", "arm", created, nil, nil)
	cfgDigest := f.serveBlob("app", cfgRaw)
	raw := imageManifest(t, MediaTypeOCIManifest,
		testDescriptor{MediaType: MediaTypeOCIConfig, Digest: cfgDigest, Size: int64(len(cfgRaw))},
		testDescriptor{Digest: fakeDigest("l"), Size: 3})
	f.serveManifest("app", "solo", MediaTypeOCIManifest, raw)

	c := newTestClient(t, f)
	sum := c.TagSummary(context.Background(), "app", "solo")
	if sum.Err != "" {
		t.Fatalf("TagSummary reported an error: %s", sum.Err)
	}
	if sum.IsIndex {
		t.Error("IsIndex is true for a single-platform manifest")
	}
	if !sum.Created.Equal(created) {
		t.Errorf("Created = %v, want %v", sum.Created, created)
	}
	want := []Platform{{OS: "linux", Architecture: "arm"}}
	if !reflect.DeepEqual(sum.Platforms, want) {
		t.Errorf("Platforms = %v, want %v", sum.Platforms, want)
	}
}

func TestTagSummaryOnFailure(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, repo, tag string }{
		{name: "missing tag", repo: "app", tag: "nope"},
		{name: "invalid repository", repo: "../evil", tag: "latest"},
		{name: "invalid tag", repo: "app", tag: "not a tag"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRegistry(t)
			c := newTestClient(t, f)
			sum := c.TagSummary(context.Background(), tc.repo, tc.tag)
			if sum == nil {
				t.Fatal("TagSummary returned nil; it must always return a row")
			}
			if sum.Err == "" {
				t.Fatalf("TagSummary(%q,%q).Err is empty, want a message", tc.repo, tc.tag)
			}
			if sum.Name != tc.tag {
				t.Errorf("Name = %q, want %q even on failure", sum.Name, tc.tag)
			}
		})
	}
}

func TestDedupePlatforms(t *testing.T) {
	t.Parallel()

	in := []Platform{
		{OS: "linux", Architecture: "amd64"},
		{OS: "linux", Architecture: "amd64"},
		{OS: "unknown", Architecture: "unknown"},
		{OS: "UNKNOWN", Architecture: "Unknown"},
		{},
		{OS: "linux", Architecture: "arm", Variant: "v7"},
		{OS: "linux", Architecture: "arm", Variant: "v6"},
	}
	want := []Platform{
		{OS: "linux", Architecture: "amd64"},
		{OS: "linux", Architecture: "arm", Variant: "v6"},
		{OS: "linux", Architecture: "arm", Variant: "v7"},
	}
	if got := dedupePlatforms(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupePlatforms = %v, want %v", got, want)
	}
	if got := dedupePlatforms(nil); got != nil {
		t.Fatalf("dedupePlatforms(nil) = %v, want nil", got)
	}
	if got := dedupePlatforms([]Platform{{OS: "unknown", Architecture: "unknown"}}); got != nil {
		t.Fatalf("dedupePlatforms of only placeholders = %v, want nil", got)
	}
}

// --- blobs ------------------------------------------------------------------

func TestBlob(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	payload := []byte("hello blob")
	dgst := f.serveBlob("app", payload)

	c := newTestClient(t, f)
	got, err := c.Blob(context.Background(), "app", dgst, 1024)
	if err != nil {
		t.Fatalf("Blob failed: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("Blob = %q, want %q", got, payload)
	}
}

func TestBlobRefusesOversizedResponses(t *testing.T) {
	t.Parallel()

	t.Run("declared Content-Length over the limit", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		payload := make([]byte, 4096)
		dgst := f.serveBlob("app", payload)
		c := newTestClient(t, f)

		_, err := c.Blob(context.Background(), "app", dgst, 100)
		if err == nil {
			t.Fatal("Blob accepted a 4096-byte body under a 100-byte limit")
		}
		if !hasAll(err.Error(), "over the 100 byte limit") {
			t.Fatalf("Blob error %q does not name the limit it enforced", err)
		}
	})

	t.Run("undeclared length over the limit", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		dgst := fakeDigest("chunked")
		f.handle("/v2/app/blobs/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
			// No Content-Length: force a chunked response so only the read
			// limit can stop it.
			w.Header().Set("Transfer-Encoding", "chunked")
			flusher, _ := w.(http.Flusher)
			for range 8 {
				_, _ = w.Write(make([]byte, 256))
				if flusher != nil {
					flusher.Flush()
				}
			}
		})
		c := newTestClient(t, f)

		_, err := c.Blob(context.Background(), "app", dgst, 100)
		if err == nil {
			t.Fatal("Blob accepted an unbounded chunked body under a 100-byte limit")
		}
		if !hasAll(err.Error(), "exceeds the 100 byte limit") {
			t.Fatalf("Blob error %q does not report the read limit", err)
		}
	})

	t.Run("a non-positive limit falls back to the default", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		payload := []byte("small")
		dgst := f.serveBlob("app", payload)
		c := newTestClient(t, f)

		got, err := c.Blob(context.Background(), "app", dgst, 0)
		if err != nil {
			t.Fatalf("Blob with limit 0 failed: %v", err)
		}
		if string(got) != "small" {
			t.Fatalf("Blob = %q, want %q", got, payload)
		}
	})
}

func TestBlobNotFound(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	c := newTestClient(t, f)
	_, err := c.Blob(context.Background(), "app", fakeDigest("missing"), 1024)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Blob error %v does not wrap ErrNotFound", err)
	}
}

func TestBlobIsNotCached(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	payload := []byte("layer bytes")
	dgst := f.serveBlob("app", payload)
	c := newTestClient(t, f)
	ctx := context.Background()

	for range 2 {
		if _, err := c.Blob(ctx, "app", dgst, 1024); err != nil {
			t.Fatalf("Blob failed: %v", err)
		}
	}
	if got := f.countPath("/v2/app/blobs/" + dgst); got != 2 {
		t.Fatalf("the public Blob path was served %d times, want 2: it must not populate the cache", got)
	}
}

// A registry redirecting a blob to object storage must not hand the storage
// host the registry credentials.
func TestBlobDropsAuthorizationOnCrossHostRedirect(t *testing.T) {
	t.Parallel()

	type seen struct {
		auth string
		hit  bool
	}
	var got seen
	done := make(chan struct{})

	payload := []byte("stored elsewhere")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{auth: r.Header.Get("Authorization"), hit: true}
		close(done)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(storage.Close)

	f := newFakeRegistry(t)
	dgst := digestOf(payload)
	f.handle("/v2/app/blobs/"+dgst, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/objects/blob", http.StatusTemporaryRedirect)
	})

	c := newTestClient(t, f, func(o *Options) {
		o.Auth = AuthConfig{Type: "basic", Username: "alice", Password: "s3cr3t"}
	})
	body, err := c.Blob(context.Background(), "app", dgst, 1024)
	if err != nil {
		t.Fatalf("Blob failed: %v", err)
	}
	<-done

	if !got.hit {
		t.Fatal("the storage host was never reached")
	}
	if got.auth != "" {
		t.Fatalf("the storage host received Authorization %q; credentials must not follow a cross-host redirect", got.auth)
	}
	if string(body) != string(payload) {
		t.Fatalf("Blob = %q, want %q", body, payload)
	}
	// The registry itself did see the credentials.
	if a := f.lastFor(t, "/v2/app/blobs/"+dgst).Header.Get("Authorization"); a == "" {
		t.Fatal("the registry itself received no Authorization header")
	}
}

func TestBlobKeepsAuthorizationOnSameHostRedirect(t *testing.T) {
	t.Parallel()

	payload := []byte("same host")
	dgst := digestOf(payload)

	f := newFakeRegistry(t)
	f.handle("/v2/app/blobs/"+dgst, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/internal/blob", http.StatusTemporaryRedirect)
	})
	f.handle("/internal/blob", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	})

	c := newTestClient(t, f, func(o *Options) {
		o.Auth = AuthConfig{Type: "bearer", Token: "tok"}
	})
	if _, err := c.Blob(context.Background(), "app", dgst, 1024); err != nil {
		t.Fatalf("Blob failed: %v", err)
	}
	if got := f.lastFor(t, "/internal/blob").Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization on a same-host redirect = %q, want it preserved", got)
	}
}

func TestStripAuthOnHostChange(t *testing.T) {
	t.Parallel()

	mkReq := func(rawURL string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer x")
		return req
	}

	t.Run("different host strips", func(t *testing.T) {
		t.Parallel()
		req := mkReq("https://storage.example.com/blob")
		if err := stripAuthOnHostChange(req, []*http.Request{mkReq("https://reg.example.com/v2/")}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("Authorization survived a redirect to another host")
		}
	})

	t.Run("same host keeps", func(t *testing.T) {
		t.Parallel()
		req := mkReq("https://reg.example.com/other")
		if err := stripAuthOnHostChange(req, []*http.Request{mkReq("https://reg.example.com/v2/")}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if req.Header.Get("Authorization") == "" {
			t.Fatal("Authorization was stripped on a same-host redirect")
		}
	})

	t.Run("redirect chains are bounded", func(t *testing.T) {
		t.Parallel()
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = mkReq("https://reg.example.com/v2/")
		}
		if err := stripAuthOnHostChange(mkReq("https://reg.example.com/x"), via); err == nil {
			t.Fatal("an 11th redirect was allowed, want it refused")
		}
	})
}

func TestReadLimited(t *testing.T) {
	t.Parallel()

	got, err := readLimited(strings.NewReader("abc"), 3)
	if err != nil || string(got) != "abc" {
		t.Fatalf("readLimited at exactly the limit = (%q, %v), want (abc, nil)", got, err)
	}
	if _, err := readLimited(strings.NewReader("abcd"), 3); err == nil {
		t.Fatal("readLimited accepted a body one byte over the limit")
	}
}

// --- delete -----------------------------------------------------------------

func TestDeleteManifest(t *testing.T) {
	t.Parallel()

	t.Run("disabled in options", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		f.handleFallback(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the registry was contacted at %s %s even though deletion is disabled in the options", r.Method, r.URL)
		})
		c := newTestClient(t, f)
		err := c.DeleteManifest(context.Background(), "app", fakeDigest("m"))
		if !errors.Is(err, ErrDeleteDenied) {
			t.Fatalf("DeleteManifest error %v, want ErrDeleteDenied", err)
		}
		if f.total() != 0 {
			t.Fatalf("the registry received %d requests, want 0", f.total())
		}
	})

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		dgst := fakeDigest("m")
		f.handle("/v2/app/manifests/"+dgst, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s, want DELETE", r.Method)
			}
			w.WriteHeader(http.StatusAccepted)
		})
		c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
		if err := c.DeleteManifest(context.Background(), "app", dgst); err != nil {
			t.Fatalf("DeleteManifest failed: %v", err)
		}
	})

	t.Run("200 is also success", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		dgst := fakeDigest("m")
		f.handle("/v2/app/manifests/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
		if err := c.DeleteManifest(context.Background(), "app", dgst); err != nil {
			t.Fatalf("DeleteManifest failed: %v", err)
		}
	})

	t.Run("405 says deletion is disabled on the registry", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		dgst := fakeDigest("m")
		f.handle("/v2/app/manifests/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
			writeRegistryError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "not allowed")
		})
		c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
		err := c.DeleteManifest(context.Background(), "app", dgst)
		if err == nil {
			t.Fatal("DeleteManifest succeeded on a 405, want an error")
		}
		if !errors.Is(err, ErrDeleteDenied) {
			t.Errorf("error %v does not wrap ErrDeleteDenied", err)
		}
		if !hasAll(err.Error(), "deletion is disabled on the registry", "REGISTRY_STORAGE_DELETE_ENABLED") {
			t.Errorf("error %q does not explain that the fix is on the registry side", err)
		}
	})

	t.Run("other failures surface as a status error", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		dgst := fakeDigest("m")
		f.handle("/v2/app/manifests/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
			writeRegistryError(w, http.StatusInternalServerError, "UNKNOWN", "boom")
		})
		c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
		err := c.DeleteManifest(context.Background(), "app", dgst)
		var se *StatusError
		if !errors.As(err, &se) || se.StatusCode != http.StatusInternalServerError {
			t.Fatalf("DeleteManifest error = %v, want a *StatusError with status 500", err)
		}
	})

	t.Run("a tag is not an acceptable reference", func(t *testing.T) {
		t.Parallel()
		f := newFakeRegistry(t)
		f.handleFallback(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the registry was contacted at %s for a tag reference; only digests may be deleted", r.URL)
		})
		c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
		if err := c.DeleteManifest(context.Background(), "app", "latest"); err == nil {
			t.Fatal("DeleteManifest accepted a tag, want a digest validation error")
		}
	})
}

func TestDeleteInvalidatesTheCache(t *testing.T) {
	t.Parallel()

	f := newFakeRegistry(t)
	f.handle("/v2/app/tags/list", jsonHandler(`{"name":"app","tags":["v1"]}`, ""))
	f.handle("/v2/_catalog", jsonHandler(`{"repositories":["app"]}`, ""))
	dgst := fakeDigest("m")
	f.handle("/v2/app/manifests/"+dgst, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	c := newTestClient(t, f, func(o *Options) { o.DeleteEnabled = true })
	ctx := context.Background()

	if _, err := c.Tags(ctx, "app", 10, ""); err != nil {
		t.Fatalf("Tags failed: %v", err)
	}
	if _, err := c.Catalog(ctx, 10, ""); err != nil {
		t.Fatalf("Catalog failed: %v", err)
	}
	if err := c.DeleteManifest(ctx, "app", dgst); err != nil {
		t.Fatalf("DeleteManifest failed: %v", err)
	}

	if _, err := c.Tags(ctx, "app", 10, ""); err != nil {
		t.Fatalf("Tags after delete failed: %v", err)
	}
	if _, err := c.Catalog(ctx, 10, ""); err != nil {
		t.Fatalf("Catalog after delete failed: %v", err)
	}
	if got := f.countPath("/v2/app/tags/list"); got != 2 {
		t.Errorf("the tag list was fetched %d times, want 2: deleting must invalidate the repository's cache", got)
	}
	if got := f.countPath("/v2/_catalog"); got != 2 {
		t.Errorf("the catalog was fetched %d times, want 2: deleting must invalidate registry-wide entries too", got)
	}
}
