// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int64
		want string
	}{
		{name: "zero", in: 0, want: "0 B"},
		{name: "one", in: 1, want: "1 B"},
		{name: "just under a kibibyte", in: 1023, want: "1023 B"},
		{name: "exactly a kibibyte", in: 1024, want: "1 KB"},
		{name: "one byte over still reads as a whole KB", in: 1025, want: "1 KB"},
		{name: "one and a half kibibytes", in: 1536, want: "1.5 KB"},
		{name: "no trailing point zero", in: 2048, want: "2 KB"},
		{name: "three digits drop the decimal", in: 102400, want: "100 KB"},
		{name: "just under a mebibyte", in: 1024*1024 - 1, want: "1024 KB"},
		{name: "exactly a mebibyte", in: 1024 * 1024, want: "1 MB"},
		{name: "gibibyte", in: 1024 * 1024 * 1024, want: "1 GB"},
		{name: "tebibyte", in: 1 << 40, want: "1 TB"},
		{name: "pebibyte", in: 1 << 50, want: "1 PB"},
		{name: "beyond the largest unit stays in PB", in: 1 << 60, want: "1024 PB"},
		{name: "negative is unknown", in: -1, want: "—"},
		{name: "very negative is unknown", in: -1 << 40, want: "—"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := humanBytes(tc.in); got != tc.want {
				t.Fatalf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// humanBytes promises "never a trailing .0", which has to hold for values that
// merely round to x.0, not just ones that are integral before rounding.
func TestHumanBytesNeverEndsInPointZero(t *testing.T) {
	t.Parallel()

	for _, n := range []int64{1025, 1024*1024 + 1, 1024*1024*1024 + 5} {
		if got := humanBytes(n); strings.HasSuffix(got, ".0 KB") || strings.HasSuffix(got, ".0 MB") || strings.HasSuffix(got, ".0 GB") {
			t.Errorf("humanBytes(%d) = %q, want no trailing .0", n, got)
		}
	}
}

func TestHumanTime(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero time", in: time.Time{}, want: "—"},
		{name: "seconds ago", in: now.Add(-30 * time.Second), want: "just now"},
		{name: "one minute boundary", in: now.Add(-time.Minute - time.Second), want: "1m ago"},
		{name: "minutes", in: now.Add(-45 * time.Minute), want: "45m ago"},
		{name: "hours", in: now.Add(-5 * time.Hour), want: "5h ago"},
		{name: "days", in: now.Add(-3 * 24 * time.Hour), want: "3d ago"},
		{name: "months", in: now.Add(-70 * 24 * time.Hour), want: "2mo ago"},
		{
			name: "older than a year falls back to the date",
			in:   time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC),
			want: "2020-06-01",
		},
		{
			name: "a future timestamp falls back to the date",
			in:   time.Date(2999, 1, 2, 3, 4, 5, 0, time.UTC),
			want: "2999-01-02",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := humanTime(tc.in); got != tc.want {
				t.Fatalf("humanTime(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFullTime(t *testing.T) {
	t.Parallel()

	if got := fullTime(time.Time{}); got != "unknown" {
		t.Errorf("fullTime(zero) = %q, want unknown", got)
	}
	in := time.Date(2024, 3, 4, 5, 6, 7, 0, time.FixedZone("CET", 3600))
	if got, want := fullTime(in), "2024-03-04 04:06:07 UTC"; got != want {
		t.Errorf("fullTime(%v) = %q, want %q (rendered in UTC)", in, got, want)
	}
}

func TestShortDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "—"},
		{name: "full sha256", in: "sha256:0123456789abcdef0123", want: "0123456789ab"},
		{name: "short hex after the algorithm", in: "sha256:abcdef", want: "abcdef"},
		{name: "exactly twelve hex characters", in: "sha256:0123456789ab", want: "0123456789ab"},
		{name: "no algorithm prefix", in: "0123456789abcdef", want: "0123456789ab"},
		{name: "short and unprefixed", in: "abc", want: "abc"},
		{name: "algorithm with nothing after it", in: "sha256:", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shortDigest(tc.in); got != tc.want {
				t.Fatalf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitRepo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		in           string
		wantNS, want string
	}{
		{name: "no namespace", in: "alpine", wantNS: "", want: "alpine"},
		{name: "one level", in: "library/alpine", wantNS: "library", want: "alpine"},
		{name: "several levels", in: "team/backend/api", wantNS: "team/backend", want: "api"},
		{name: "trailing slash", in: "library/", wantNS: "library", want: ""},
		{name: "leading slash", in: "/alpine", wantNS: "", want: "alpine"},
		{name: "empty", in: "", wantNS: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns, short := splitRepo(tc.in)
			if ns != tc.wantNS || short != tc.want {
				t.Fatalf("splitRepo(%q) = (%q, %q), want (%q, %q)", tc.in, ns, short, tc.wantNS, tc.want)
			}
		})
	}
}

func TestSplitEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []KV
	}{
		{name: "nil", in: nil, want: []KV{}},
		{name: "simple", in: []string{"PATH=/usr/bin"}, want: []KV{{Key: "PATH", Value: "/usr/bin"}}},
		{name: "value containing an equals sign", in: []string{"OPTS=a=b"}, want: []KV{{Key: "OPTS", Value: "a=b"}}},
		{name: "empty value", in: []string{"EMPTY="}, want: []KV{{Key: "EMPTY"}}},
		{name: "no equals sign at all", in: []string{"BARE"}, want: []KV{{Key: "BARE"}}},
		{name: "empty key", in: []string{"=orphan"}, want: []KV{{Value: "orphan"}}},
		{
			name: "order is preserved because later entries override earlier ones",
			in:   []string{"A=1", "B=2", "A=3"},
			want: []KV{{Key: "A", Value: "1"}, {Key: "B", Value: "2"}, {Key: "A", Value: "3"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := splitEnv(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("splitEnv(%v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSortedKV(t *testing.T) {
	t.Parallel()

	if got := sortedKV(nil); got != nil {
		t.Errorf("sortedKV(nil) = %v, want nil", got)
	}
	if got := sortedKV(map[string]string{}); got != nil {
		t.Errorf("sortedKV(empty) = %v, want nil", got)
	}
	got := sortedKV(map[string]string{"b": "2", "a": "1", "c": "3"})
	want := []KV{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sortedKV = %+v, want %+v", got, want)
	}
}

func TestPullCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		host, repo, ref string
		want            string
	}{
		{
			name: "tag",
			host: "reg.example.com", repo: "team/api", ref: "v1.2.3",
			want: "docker pull reg.example.com/team/api:v1.2.3",
		},
		{
			name: "digest uses an at sign",
			host: "reg.example.com", repo: "team/api", ref: "sha256:abc",
			want: "docker pull reg.example.com/team/api@sha256:abc",
		},
		{
			name: "host with a port",
			host: "localhost:5000", repo: "app", ref: "latest",
			want: "docker pull localhost:5000/app:latest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pullCommand(tc.host, tc.repo, tc.ref); got != tc.want {
				t.Fatalf("pullCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoinPath(t *testing.T) {
	t.Parallel()

	tests := []struct{ base, p, want string }{
		{"", "/r/local", "/r/local"},
		{"/registry", "/r/local", "/registry/r/local"},
		{"/a/b", "/static/app.css", "/a/b/static/app.css"},
	}
	for _, tc := range tests {
		if got := joinPath(tc.base, tc.p); got != tc.want {
			t.Errorf("joinPath(%q,%q) = %q, want %q", tc.base, tc.p, got, tc.want)
		}
	}
}

func TestOrSlashAndOrDash(t *testing.T) {
	t.Parallel()

	if got := orSlash(""); got != "/" {
		t.Errorf("orSlash(%q) = %q, want /", "", got)
	}
	if got := orSlash("/registry"); got != "/registry" {
		t.Errorf("orSlash(/registry) = %q", got)
	}
	if got := orDash(""); got != "/" {
		t.Errorf("orDash(%q) = %q, want /", "", got)
	}
	if got := orDash("/registry"); got != "/registry" {
		t.Errorf("orDash(/registry) = %q", got)
	}
}

func TestURLQueryEscape(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"latest", "latest"},
		{"a b", "a%20b"},
		{"a+b", "a%2Bb"},
		{`<script>`, "%3Cscript%3E"},
		{"sha256:abc", "sha256%3Aabc"},
		{"a&b=c", "a%26b%3Dc"},
	}
	for _, tc := range tests {
		if got := urlQueryEscape(tc.in); got != tc.want {
			t.Errorf("urlQueryEscape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
