// Package registry implements a client for the OCI Distribution Specification
// v1.1 as served by Distribution 3.x registries. Legacy Docker manifest
// schema 1 is intentionally not supported.
package registry

import (
	"context"
	"errors"
	"time"
)

// Media types understood by this client.
const (
	MediaTypeOCIIndex     = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIConfig    = "application/vnd.oci.image.config.v1+json"
	MediaTypeDockerList   = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerV2     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerConfig = "application/vnd.docker.container.image.v1+json"
)

// Errors returned by the client. Handlers map these onto HTTP status codes.
var (
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
	ErrUnsupported  = errors.New("unsupported manifest type")
	ErrDeleteDenied = errors.New("delete is not enabled for this registry")
)

// Repository is one entry of the registry catalog.
type Repository struct {
	Name string
}

// CatalogPage is a page of the catalog, plus the cursor for the next one.
type CatalogPage struct {
	Repositories []Repository
	NextLast     string // value for the `last` query parameter; empty when exhausted
}

// TagPage is a page of tags for a repository.
type TagPage struct {
	Repository string
	Tags       []string
	NextLast   string
}

// Platform identifies the target of an image within an index.
type Platform struct {
	OS           string
	Architecture string
	Variant      string
	OSVersion    string
}

// Descriptor is a content-addressable pointer to a manifest or blob.
type Descriptor struct {
	MediaType   string
	Digest      string
	Size        int64
	Platform    *Platform
	Annotations map[string]string
}

// Layer is one filesystem layer of an image.
type Layer struct {
	Digest    string
	MediaType string
	Size      int64
	// Command is the build instruction that produced this layer, recovered by
	// zipping the config history against the layer list. Empty when unknown.
	Command string
}

// HistoryEntry is one step of the image build history.
type HistoryEntry struct {
	Created    time.Time
	CreatedBy  string
	Comment    string
	EmptyLayer bool
}

// ImageConfig is the interesting subset of the OCI image configuration.
type ImageConfig struct {
	Architecture string
	OS           string
	Variant      string
	Created      time.Time
	Author       string
	Labels       map[string]string
	Env          []string
	Entrypoint   []string
	Cmd          []string
	WorkingDir   string
	User         string
	ExposedPorts []string
	Volumes      []string
	Digest       string
	Size         int64
}

// Image is a fully resolved manifest: either a single-platform image or an
// index. For an index, Children holds the per-platform manifests and the
// image fields describe the platform selected for display (if any).
type Image struct {
	Repository string
	Reference  string // tag or digest as requested
	Digest     string // digest of the manifest itself
	MediaType  string
	IsIndex    bool

	// Single-platform fields, also populated for the selected child of an index.
	//
	// ConfigRef is the config blob's descriptor as the manifest declares it. It
	// is set even when the blob itself could not be fetched, so size accounting
	// never depends on whether Config was resolved.
	ConfigRef    Descriptor
	Config       *ImageConfig
	Layers       []Layer
	History      []HistoryEntry
	ManifestSize int64 // size of the manifest document
	TotalSize    int64 // config blob + layers
	Subject      *Descriptor

	// Index fields.
	Children []IndexChild

	Annotations map[string]string
	RawManifest []byte
}

// IndexChild is one platform-specific manifest referenced by an index.
type IndexChild struct {
	Descriptor
	// Resolved is filled in when the child manifest was fetched.
	Resolved  *Image
	SizeTotal int64
}

// TagSummary is the lightweight view of a tag used in listings.
type TagSummary struct {
	Name      string
	Digest    string
	MediaType string
	Size      int64
	Created   time.Time
	Platforms []Platform
	IsIndex   bool
	// Err is set when this tag could not be inspected; the row is still shown.
	Err string
}

// Client talks to a single registry.
type Client interface {
	// Ping verifies the /v2/ endpoint answers and credentials are accepted.
	Ping(ctx context.Context) error
	// Catalog lists repositories. n is the page size, last is the cursor.
	Catalog(ctx context.Context, n int, last string) (*CatalogPage, error)
	// Tags lists tags of a repository.
	Tags(ctx context.Context, repo string, n int, last string) (*TagPage, error)
	// TagSummary resolves a tag to the data needed for a listing row.
	TagSummary(ctx context.Context, repo, tag string) *TagSummary
	// Image resolves a manifest by tag or digest. When resolveChildren is set
	// and the manifest is an index, each child manifest is fetched too.
	Image(ctx context.Context, repo, reference string, resolveChildren bool) (*Image, error)
	// DeleteManifest deletes a manifest by digest.
	DeleteManifest(ctx context.Context, repo, digest string) error
	// Blob fetches a blob, refusing anything larger than limit bytes.
	Blob(ctx context.Context, repo, digest string, limit int64) ([]byte, error)
}

// Platform renders as the familiar os/arch/variant string.
func (p Platform) String() string {
	if p.OS == "" && p.Architecture == "" {
		return "unknown"
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// AuthConfig describes how to authenticate against a registry.
type AuthConfig struct {
	// Type is "none", "basic" or "bearer". Empty means "none".
	Type     string
	Username string
	Password string
	// Token is a pre-issued bearer token, used when Type is "bearer".
	Token string
}

// Options configures a Client.
type Options struct {
	Name          string
	BaseURL       string // e.g. https://registry.example.com
	Auth          AuthConfig
	Insecure      bool // skip TLS verification
	Timeout       time.Duration
	CacheTTL      time.Duration
	UserAgent     string
	DeleteEnabled bool
}
