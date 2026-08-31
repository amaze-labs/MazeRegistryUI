package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Legacy Docker manifest schema 1 media types. They are listed only so that
// they can be recognised and rejected: schema 1 has a different shape, carries
// no config blob and is out of scope for this client.
const (
	mediaTypeDockerV1       = "application/vnd.docker.distribution.manifest.v1+json"
	mediaTypeDockerV1Signed = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// rawPlatform mirrors the OCI platform object. "os.version" is spelled with a
// dot in the wire format, which is why the tag cannot be inferred.
type rawPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	OSVersion    string `json:"os.version"`
	Variant      string `json:"variant"`
}

func (p *rawPlatform) platform() *Platform {
	if p == nil {
		return nil
	}
	return &Platform{
		OS:           p.OS,
		Architecture: p.Architecture,
		Variant:      p.Variant,
		OSVersion:    p.OSVersion,
	}
}

// rawDescriptor mirrors the OCI content descriptor.
type rawDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *rawPlatform      `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

func (d rawDescriptor) descriptor() Descriptor {
	return Descriptor{
		MediaType:   d.MediaType,
		Digest:      d.Digest,
		Size:        d.Size,
		Platform:    d.Platform.platform(),
		Annotations: d.Annotations,
	}
}

// manifestDoc is the union of the OCI image manifest and the OCI index (and
// their Docker v2 equivalents). The two documents overlap enough that a single
// struct can decode either; isIndex records which one it turned out to be.
type manifestDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        rawDescriptor     `json:"config"`
	Layers        []rawDescriptor   `json:"layers"`
	Manifests     []rawDescriptor   `json:"manifests"`
	Subject       *rawDescriptor    `json:"subject"`
	Annotations   map[string]string `json:"annotations"`

	// Schema 1 markers, decoded purely so the document can be rejected.
	FSLayers []json.RawMessage `json:"fsLayers"`

	isIndex bool
}

// parseManifest decodes a manifest document. headerMediaType is the value of
// the response Content-Type, used when the document itself omits mediaType,
// which older Docker v2 manifests and some proxies do.
func parseManifest(raw []byte, headerMediaType string) (*manifestDoc, error) {
	var doc manifestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("registry: decoding manifest: %w", err)
	}

	mt := normaliseMediaType(doc.MediaType)
	if mt == "" {
		mt = normaliseMediaType(headerMediaType)
	}

	switch mt {
	case mediaTypeDockerV1, mediaTypeDockerV1Signed:
		return nil, fmt.Errorf("registry: Docker manifest schema 1 (%s): %w", mt, ErrUnsupported)
	}
	if doc.SchemaVersion == 1 || len(doc.FSLayers) > 0 {
		return nil, fmt.Errorf("registry: Docker manifest schema 1: %w", ErrUnsupported)
	}

	switch mt {
	case MediaTypeOCIIndex, MediaTypeDockerList:
		doc.isIndex = true
	case MediaTypeOCIManifest, MediaTypeDockerV2:
		doc.isIndex = false
	default:
		// Either no media type at all or one we do not know. Both happen in
		// the wild, so fall back to the document's shape and only give up when
		// that is inconclusive too.
		switch {
		case len(doc.Manifests) > 0:
			doc.isIndex = true
			if mt == "" {
				mt = MediaTypeOCIIndex
			}
		case doc.Config.Digest != "" || len(doc.Layers) > 0:
			doc.isIndex = false
			if mt == "" {
				mt = MediaTypeOCIManifest
			}
		default:
			return nil, fmt.Errorf("registry: manifest media type %q is not an image manifest or index: %w", mt, ErrUnsupported)
		}
	}

	doc.MediaType = mt
	return &doc, nil
}

// normaliseMediaType strips parameters and case from a media type and treats
// the generic JSON types as "unknown", since registries hand those out when
// they have nothing better to say.
func normaliseMediaType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	switch s {
	case "application/json", "text/plain", "application/octet-stream":
		return ""
	}
	return s
}

// flexTime decodes an RFC 3339 timestamp but never fails: registries and old
// build tools emit empty strings, nulls and the occasional Go time.String()
// rendering. A timestamp we cannot read is worth less than the rest of the
// document, so it degrades to the zero time instead of failing the decode.
type flexTime struct {
	time.Time
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05 -0700 MST",
	"2006-01-02 15:04:05",
}

func (t *flexTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed.UTC()
			return nil
		}
	}
	return nil
}

// rawImageConfig is the subset of the OCI image configuration this client
// surfaces. Field names in the nested config object are capitalised on the
// wire, a Docker inheritance the OCI spec kept.
type rawImageConfig struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	Variant      string   `json:"variant"`
	Created      flexTime `json:"created"`
	Author       string   `json:"author"`

	Config struct {
		User         string                     `json:"User"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
		Env          []string                   `json:"Env"`
		Entrypoint   []string                   `json:"Entrypoint"`
		Cmd          []string                   `json:"Cmd"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		WorkingDir   string                     `json:"WorkingDir"`
		Labels       map[string]string          `json:"Labels"`
	} `json:"config"`

	History []struct {
		Created    flexTime `json:"created"`
		CreatedBy  string   `json:"created_by"`
		Comment    string   `json:"comment"`
		EmptyLayer bool     `json:"empty_layer"`
	} `json:"history"`
}

// parseImageConfig decodes a config blob into the display model.
func parseImageConfig(raw []byte) (*ImageConfig, []HistoryEntry, error) {
	var doc rawImageConfig
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("registry: decoding image config: %w", err)
	}

	cfg := &ImageConfig{
		Architecture: doc.Architecture,
		OS:           doc.OS,
		Variant:      doc.Variant,
		Created:      doc.Created.Time,
		Author:       doc.Author,
		Labels:       doc.Config.Labels,
		Env:          doc.Config.Env,
		Entrypoint:   doc.Config.Entrypoint,
		Cmd:          doc.Config.Cmd,
		WorkingDir:   doc.Config.WorkingDir,
		User:         doc.Config.User,
		ExposedPorts: sortedKeys(doc.Config.ExposedPorts),
		Volumes:      sortedKeys(doc.Config.Volumes),
	}

	history := make([]HistoryEntry, 0, len(doc.History))
	for _, h := range doc.History {
		history = append(history, HistoryEntry{
			Created:    h.Created.Time,
			CreatedBy:  h.CreatedBy,
			Comment:    h.Comment,
			EmptyLayer: h.EmptyLayer,
		})
	}
	return cfg, history, nil
}

// sortedKeys returns the keys of a set-shaped JSON object in a stable order.
func sortedKeys(m map[string]json.RawMessage) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cleanCommand turns a history created_by string into something readable.
// Classic `docker build` records a shell wrapper around every instruction; the
// #(nop) marker means the instruction produced no filesystem change and the
// wrapper is pure noise. BuildKit keeps the Dockerfile instruction but still
// writes the shell wrapper and stamps every entry with a trailing marker, so
// both writers need unwrapping before the command reads like the line someone
// actually wrote.
func cleanCommand(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// BuildKit stamps every record; the marker says nothing about the build.
	if rest, ok := strings.CutSuffix(s, "# buildkit"); ok {
		s = strings.TrimSpace(rest)
	}
	// BuildKit form: "RUN /bin/sh -c apk add curl" is just "RUN apk add curl".
	if rest, ok := strings.CutPrefix(s, "RUN /bin/sh -c "); ok {
		s = "RUN " + strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(s, "/bin/sh -c #(nop) "); ok {
		return strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(s, "/bin/sh -c #(nop)"); ok {
		return strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(s, "/bin/sh -c "); ok {
		return "RUN " + strings.TrimSpace(rest)
	}
	return s
}

// applyHistoryToLayers attaches each build step's command to the layer it
// produced, by walking the history and consuming one layer per non-empty step.
//
// The two lists are only meaningful together. If they disagree on how many
// layers there should be — a truncated history, a hand-written manifest — then
// every assignment after the first mismatch would be wrong, and a confidently
// wrong command is worse than none, so nothing is attached at all.
func applyHistoryToLayers(history []HistoryEntry, layers []Layer) {
	want := 0
	for _, h := range history {
		if !h.EmptyLayer {
			want++
		}
	}
	if want != len(layers) {
		return
	}
	i := 0
	for _, h := range history {
		if h.EmptyLayer {
			continue
		}
		layers[i].Command = cleanCommand(h.CreatedBy)
		i++
	}
}
