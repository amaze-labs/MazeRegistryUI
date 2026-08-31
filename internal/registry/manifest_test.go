package registry

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestNormaliseMediaType(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"application/vnd.oci.image.manifest.v1+json", MediaTypeOCIManifest},
		{"  APPLICATION/VND.OCI.IMAGE.INDEX.V1+JSON  ", MediaTypeOCIIndex},
		{"application/vnd.docker.distribution.manifest.v2+json; charset=utf-8", MediaTypeDockerV2},
		{"application/json", ""},
		{"text/plain; charset=utf-8", ""},
		{"application/octet-stream", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := normaliseMediaType(tc.in); got != tc.want {
			t.Errorf("normaliseMediaType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseManifest(t *testing.T) {
	t.Parallel()

	cfg := testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("cfg"), Size: 100}
	layer := testDescriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: fakeDigest("l1"), Size: 500}
	child := testDescriptor{
		MediaType: MediaTypeOCIManifest,
		Digest:    fakeDigest("child"),
		Size:      12,
		Platform:  &testPlatform{OS: "linux", Architecture: "amd64"},
	}

	tests := []struct {
		name        string
		raw         []byte
		headerMedia string
		wantIndex   bool
		wantMedia   string
		wantErr     error
		wantErrText string
	}{
		{
			name:      "oci image manifest",
			raw:       imageManifest(t, MediaTypeOCIManifest, cfg, layer),
			wantIndex: false,
			wantMedia: MediaTypeOCIManifest,
		},
		{
			name:      "oci index",
			raw:       indexManifest(t, MediaTypeOCIIndex, child),
			wantIndex: true,
			wantMedia: MediaTypeOCIIndex,
		},
		{
			name:      "docker manifest v2",
			raw:       imageManifest(t, MediaTypeDockerV2, cfg, layer),
			wantIndex: false,
			wantMedia: MediaTypeDockerV2,
		},
		{
			name:      "docker manifest list",
			raw:       indexManifest(t, MediaTypeDockerList, child),
			wantIndex: true,
			wantMedia: MediaTypeDockerList,
		},
		{
			name:        "media type only in the response header",
			raw:         mustJSON(t, map[string]any{"schemaVersion": 2, "config": cfg, "layers": []testDescriptor{layer}}),
			headerMedia: MediaTypeDockerV2,
			wantIndex:   false,
			wantMedia:   MediaTypeDockerV2,
		},
		{
			name:      "no media type at all, shape says index",
			raw:       mustJSON(t, map[string]any{"schemaVersion": 2, "manifests": []testDescriptor{child}}),
			wantIndex: true,
			wantMedia: MediaTypeOCIIndex,
		},
		{
			name:      "no media type at all, shape says image",
			raw:       mustJSON(t, map[string]any{"schemaVersion": 2, "config": cfg}),
			wantIndex: false,
			wantMedia: MediaTypeOCIManifest,
		},
		{
			name:        "docker schema 1 by media type",
			raw:         mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": mediaTypeDockerV1}),
			wantErr:     ErrUnsupported,
			wantErrText: "schema 1",
		},
		{
			name:        "docker schema 1 signed by media type",
			raw:         mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": mediaTypeDockerV1Signed}),
			wantErr:     ErrUnsupported,
			wantErrText: "schema 1",
		},
		{
			name:        "docker schema 1 by schemaVersion",
			raw:         mustJSON(t, map[string]any{"schemaVersion": 1, "name": "app", "tag": "latest"}),
			wantErr:     ErrUnsupported,
			wantErrText: "schema 1",
		},
		{
			name: "docker schema 1 by fsLayers",
			raw: mustJSON(t, map[string]any{
				"schemaVersion": 2,
				"fsLayers":      []map[string]string{{"blobSum": "sha256:deadbeef"}},
			}),
			wantErr:     ErrUnsupported,
			wantErrText: "schema 1",
		},
		{
			name:        "unrecognisable document",
			raw:         mustJSON(t, map[string]any{"schemaVersion": 2}),
			wantErr:     ErrUnsupported,
			wantErrText: "not an image manifest or index",
		},
		{
			name:        "invalid JSON",
			raw:         []byte("{not json"),
			wantErrText: "decoding manifest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := parseManifest(tc.raw, tc.headerMedia)
			switch {
			case tc.wantErr != nil || tc.wantErrText != "":
				if err == nil {
					t.Fatalf("parseManifest succeeded, want an error containing %q", tc.wantErrText)
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("parseManifest error %v does not wrap %v", err, tc.wantErr)
				}
				if tc.wantErrText != "" && !hasAll(err.Error(), tc.wantErrText) {
					t.Fatalf("parseManifest error %q does not contain %q", err, tc.wantErrText)
				}
			default:
				if err != nil {
					t.Fatalf("parseManifest failed: %v", err)
				}
				if doc.isIndex != tc.wantIndex {
					t.Errorf("isIndex = %v, want %v", doc.isIndex, tc.wantIndex)
				}
				if doc.MediaType != tc.wantMedia {
					t.Errorf("MediaType = %q, want %q", doc.MediaType, tc.wantMedia)
				}
			}
		})
	}
}

func TestParseManifestSubjectAndAnnotations(t *testing.T) {
	t.Parallel()

	raw := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     MediaTypeOCIManifest,
		"config":        testDescriptor{MediaType: MediaTypeOCIConfig, Digest: fakeDigest("c"), Size: 1},
		"layers":        []testDescriptor{},
		"annotations":   map[string]string{"org.opencontainers.image.source": "https://example.com/repo"},
		"subject": testDescriptor{
			MediaType: MediaTypeOCIManifest,
			Digest:    fakeDigest("subject"),
			Size:      42,
		},
	})

	doc, err := parseManifest(raw, "")
	if err != nil {
		t.Fatalf("parseManifest failed: %v", err)
	}
	if doc.Subject == nil {
		t.Fatal("Subject is nil, want the referrers subject descriptor")
	}
	if got, want := doc.Subject.Digest, fakeDigest("subject"); got != want {
		t.Errorf("Subject.Digest = %q, want %q", got, want)
	}
	if got := doc.Annotations["org.opencontainers.image.source"]; got != "https://example.com/repo" {
		t.Errorf("annotation lost: got %q", got)
	}
}

func TestFlexTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
		want    time.Time
	}{
		{name: "rfc3339 nano", in: `"2024-03-01T10:20:30.123456789Z"`, want: time.Date(2024, 3, 1, 10, 20, 30, 123456789, time.UTC)},
		{name: "rfc3339", in: `"2024-03-01T10:20:30Z"`, want: time.Date(2024, 3, 1, 10, 20, 30, 0, time.UTC)},
		{name: "offset without colon", in: `"2024-03-01T10:20:30+0100"`, want: time.Date(2024, 3, 1, 9, 20, 30, 0, time.UTC)},
		{name: "naive local", in: `"2024-03-01T10:20:30"`, want: time.Date(2024, 3, 1, 10, 20, 30, 0, time.UTC)},
		{name: "go time.String rendering", in: `"2024-03-01 10:20:30 +0000 UTC"`, want: time.Date(2024, 3, 1, 10, 20, 30, 0, time.UTC)},
		{name: "empty string is the zero time", in: `""`, want: time.Time{}},
		{name: "null is the zero time", in: `null`, want: time.Time{}},
		{name: "number is the zero time", in: `1700000000`, want: time.Time{}},
		{name: "garbage is the zero time", in: `"not a date"`, want: time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ft flexTime
			if err := json.Unmarshal([]byte(tc.in), &ft); err != nil {
				t.Fatalf("flexTime.UnmarshalJSON(%s) returned %v, it must never fail", tc.in, err)
			}
			if !ft.Time.Equal(tc.want) {
				t.Fatalf("flexTime.UnmarshalJSON(%s) = %v, want %v", tc.in, ft.Time, tc.want)
			}
		})
	}
}

func TestParseImageConfig(t *testing.T) {
	t.Parallel()

	raw := mustJSON(t, map[string]any{
		"architecture": "arm64",
		"os":           "linux",
		"variant":      "v8",
		"created":      "2024-01-02T03:04:05Z",
		"author":       "someone",
		"config": map[string]any{
			"User":         "1000:1000",
			"WorkingDir":   "/srv",
			"Env":          []string{"PATH=/usr/bin", "LANG=C"},
			"Entrypoint":   []string{"/entrypoint.sh"},
			"Cmd":          []string{"--serve"},
			"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}, "443/tcp": map[string]any{}},
			"Volumes":      map[string]any{"/data": map[string]any{}, "/cache": map[string]any{}},
			"Labels":       map[string]string{"maintainer": "team"},
		},
		"history": []map[string]any{
			{"created": "2024-01-02T03:04:00Z", "created_by": "/bin/sh -c #(nop) ADD file:abc in /", "empty_layer": false},
			{"created": "2024-01-02T03:04:05Z", "created_by": "/bin/sh -c #(nop)  CMD [\"sh\"]", "empty_layer": true},
		},
	})

	cfg, history, err := parseImageConfig(raw)
	if err != nil {
		t.Fatalf("parseImageConfig failed: %v", err)
	}
	if cfg.Architecture != "arm64" || cfg.OS != "linux" || cfg.Variant != "v8" {
		t.Errorf("platform fields = %q/%q/%q, want arm64/linux/v8", cfg.Architecture, cfg.OS, cfg.Variant)
	}
	if want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC); !cfg.Created.Equal(want) {
		t.Errorf("Created = %v, want %v", cfg.Created, want)
	}
	// Set-shaped objects come back as sorted keys, so the UI order is stable.
	if want := []string{"443/tcp", "8080/tcp"}; !reflect.DeepEqual(cfg.ExposedPorts, want) {
		t.Errorf("ExposedPorts = %v, want %v (sorted)", cfg.ExposedPorts, want)
	}
	if want := []string{"/cache", "/data"}; !reflect.DeepEqual(cfg.Volumes, want) {
		t.Errorf("Volumes = %v, want %v (sorted)", cfg.Volumes, want)
	}
	if want := []string{"PATH=/usr/bin", "LANG=C"}; !reflect.DeepEqual(cfg.Env, want) {
		t.Errorf("Env = %v, want %v (registry order preserved)", cfg.Env, want)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d entries, want 2", len(history))
	}
	if !history[1].EmptyLayer {
		t.Error("the second history entry should be marked empty_layer")
	}
}

func TestParseImageConfigEmptyDocument(t *testing.T) {
	t.Parallel()

	cfg, history, err := parseImageConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parseImageConfig({}) failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("parseImageConfig({}) returned a nil config")
	}
	if len(history) != 0 {
		t.Fatalf("history = %v, want empty", history)
	}
	if cfg.ExposedPorts != nil || cfg.Volumes != nil {
		t.Errorf("empty set-shaped objects should stay nil, got ports=%v volumes=%v", cfg.ExposedPorts, cfg.Volumes)
	}

	if _, _, err := parseImageConfig([]byte(`nope`)); err == nil {
		t.Fatal("parseImageConfig on invalid JSON succeeded, want an error")
	}
}

func TestCleanCommand(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, in, want string }{
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "   ", want: ""},
		{name: "nop with a space", in: `/bin/sh -c #(nop) ADD file:abc in /`, want: `ADD file:abc in /`},
		{name: "nop without a space", in: `/bin/sh -c #(nop)  CMD ["sh"]`, want: `CMD ["sh"]`},
		{name: "shell wrapper becomes RUN", in: `/bin/sh -c apk add --no-cache curl`, want: `RUN apk add --no-cache curl`},
		// The leading TrimSpace means a wrapper with nothing after it never
		// matches the "/bin/sh -c " prefix, so it passes through verbatim. The
		// `rest == ""` branch inside cleanCommand is therefore unreachable.
		{name: "bare shell wrapper passes through", in: `/bin/sh -c `, want: `/bin/sh -c`},
		{name: "buildkit RUN wrapper is unwrapped", in: `RUN /bin/sh -c apk add curl # buildkit`, want: `RUN apk add curl`},
		{name: "buildkit marker alone is stripped", in: `COPY app /app # buildkit`, want: `COPY app /app`},
		{name: "already legible passes through", in: `COPY /src /dst`, want: `COPY /src /dst`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cleanCommand(tc.in); got != tc.want {
				t.Fatalf("cleanCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplyHistoryToLayers(t *testing.T) {
	t.Parallel()

	t.Run("zips non-empty history entries onto layers", func(t *testing.T) {
		t.Parallel()
		history := []HistoryEntry{
			{CreatedBy: `/bin/sh -c #(nop) ADD file:a in /`},
			{CreatedBy: `/bin/sh -c #(nop)  ENV LANG=C`, EmptyLayer: true},
			{CreatedBy: `/bin/sh -c apk add curl`},
			{CreatedBy: `/bin/sh -c #(nop)  CMD ["sh"]`, EmptyLayer: true},
		}
		layers := []Layer{{Digest: fakeDigest("l1")}, {Digest: fakeDigest("l2")}}

		applyHistoryToLayers(history, layers)

		if layers[0].Command != `ADD file:a in /` {
			t.Errorf("layer 0 command = %q, want %q", layers[0].Command, `ADD file:a in /`)
		}
		if layers[1].Command != `RUN apk add curl` {
			t.Errorf("layer 1 command = %q, want %q", layers[1].Command, `RUN apk add curl`)
		}
	})

	t.Run("mismatched counts leave every command empty", func(t *testing.T) {
		t.Parallel()
		// Three non-empty steps, two layers: a truncated history. Assigning
		// anything would mis-label the layers.
		history := []HistoryEntry{
			{CreatedBy: "step one"},
			{CreatedBy: "step two"},
			{CreatedBy: "step three"},
		}
		layers := []Layer{{Digest: fakeDigest("l1")}, {Digest: fakeDigest("l2")}}

		applyHistoryToLayers(history, layers)

		for i, l := range layers {
			if l.Command != "" {
				t.Errorf("layer %d command = %q, want empty: the history and layer counts disagree", i, l.Command)
			}
		}
	})

	t.Run("no history leaves no commands", func(t *testing.T) {
		t.Parallel()
		layers := []Layer{{Digest: fakeDigest("l1")}}
		applyHistoryToLayers(nil, layers)
		if layers[0].Command != "" {
			t.Errorf("layer command = %q, want empty", layers[0].Command)
		}
	})

	t.Run("all-empty history and no layers is a no-op", func(t *testing.T) {
		t.Parallel()
		history := []HistoryEntry{{CreatedBy: "ENV X=1", EmptyLayer: true}}
		applyHistoryToLayers(history, nil) // must not panic
	})
}

func TestSortedKeys(t *testing.T) {
	t.Parallel()

	if got := sortedKeys(nil); got != nil {
		t.Errorf("sortedKeys(nil) = %v, want nil", got)
	}
	in := map[string]json.RawMessage{"b": nil, "a": nil, "c": nil}
	if got, want := sortedKeys(in), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sortedKeys = %v, want %v", got, want)
	}
}

func TestRawPlatformNil(t *testing.T) {
	t.Parallel()
	var p *rawPlatform
	if got := p.platform(); got != nil {
		t.Fatalf("(*rawPlatform)(nil).platform() = %v, want nil", got)
	}
}

func TestPlatformString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    Platform
		want string
	}{
		{name: "os and arch", p: Platform{OS: "linux", Architecture: "amd64"}, want: "linux/amd64"},
		{name: "with variant", p: Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, want: "linux/arm/v7"},
		{name: "empty", p: Platform{}, want: "unknown"},
		{name: "os only", p: Platform{OS: "linux"}, want: "linux/"},
		{name: "os version is not rendered", p: Platform{OS: "windows", Architecture: "amd64", OSVersion: "10.0.1"}, want: "windows/amd64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.String(); got != tc.want {
				t.Fatalf("Platform%+v.String() = %q, want %q", tc.p, got, tc.want)
			}
		})
	}
}
