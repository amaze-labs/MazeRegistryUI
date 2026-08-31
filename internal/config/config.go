// Package config loads and validates the MazeRegistryUI configuration file.
//
// Configuration is file-only by design: registries cannot be added at runtime
// from the UI, so the file is the single source of truth for what an operator
// exposes. Secrets may be kept out of the file with ${ENV_VAR} references or
// the dedicated *_env fields.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of the configuration file.
type Config struct {
	Server     Server     `yaml:"server"`
	UI         UI         `yaml:"ui"`
	Registries []Registry `yaml:"registries"`
}

// Server holds the HTTP listener settings.
type Server struct {
	// Addr is the listen address, e.g. ":8080" or "127.0.0.1:8080".
	Addr string `yaml:"addr"`
	// BasePath lets the UI be served under a sub-path behind a reverse proxy,
	// e.g. "/registry". Always normalised to a leading slash and no trailing one.
	BasePath        string        `yaml:"base_path"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// AccessLog enables one structured log line per request.
	AccessLog bool `yaml:"access_log"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log_level"`
}

// UI holds presentation settings.
type UI struct {
	Title string `yaml:"title"`
	// Theme is the initial theme: auto, dark or light. Visitors can override it.
	Theme string `yaml:"theme"`
	// CatalogPageSize and TagPageSize bound how much is requested per page.
	CatalogPageSize int `yaml:"catalog_page_size"`
	TagPageSize     int `yaml:"tag_page_size"`
	// DefaultRegistry is the id selected on first visit. Defaults to the first.
	DefaultRegistry string `yaml:"default_registry"`
	// ShowPullCommand renders a copyable pull command on tag and image views.
	ShowPullCommand bool `yaml:"show_pull_command"`
	// FooterNote is optional free text shown in the footer, e.g. a support contact.
	FooterNote string `yaml:"footer_note"`
}

// Registry is one browsable registry endpoint.
type Registry struct {
	// ID is the stable identifier used in URLs. Derived from Name when omitted.
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// URL is the registry root, without the /v2/ suffix.
	URL string `yaml:"url"`
	// Description is shown in the registry picker.
	Description string `yaml:"description"`
	// PullHost overrides the hostname shown in pull commands, for registries
	// reachable at a different address from the one the UI uses.
	PullHost string `yaml:"pull_host"`
	Auth     Auth   `yaml:"auth"`
	// Insecure skips TLS certificate verification. Use only for internal
	// registries with self-signed certificates.
	Insecure bool `yaml:"insecure"`
	// DeleteEnabled exposes manifest deletion in the UI. The registry itself
	// must also be started with storage deletion enabled.
	DeleteEnabled bool          `yaml:"delete_enabled"`
	Timeout       time.Duration `yaml:"timeout"`
	CacheTTL      time.Duration `yaml:"cache_ttl"`
}

// Auth describes registry credentials.
type Auth struct {
	// Type is none, basic or bearer.
	Type     string `yaml:"type"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// PasswordEnv names an environment variable holding the password, so the
	// secret never has to appear in the file.
	PasswordEnv string `yaml:"password_env"`
	Token       string `yaml:"token"`
	TokenEnv    string `yaml:"token_env"`
}

// Defaults applied to any field left unset.
const (
	DefaultAddr            = ":8080"
	DefaultTitle           = "MazeRegistryUI"
	DefaultCatalogPageSize = 50
	DefaultTagPageSize     = 50
	DefaultTimeout         = 20 * time.Second
	DefaultCacheTTL        = 60 * time.Second
)

var idPattern = regexp.MustCompile(`[^a-z0-9]+`)

// Load reads, expands and validates the configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse builds a Config from YAML bytes. Exported so tests and callers that
// hold the document in memory do not have to touch the filesystem.
func Parse(raw []byte) (*Config, error) {
	// Expansion happens on the parsed document rather than the raw text. A
	// textual pass would also rewrite ${VAR} inside comments, and a password
	// containing a colon or a quote would corrupt the document it was
	// substituted into. Walking scalars avoids both.
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, errors.New("config file is empty")
	}

	var missing []string
	expandNode(&root, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %s",
			strings.Join(dedupe(missing), ", "))
	}

	// Round-trip so the decoder can reject unknown keys, which only a Decoder
	// can do; Node.Decode has no equivalent.
	expanded, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true) // a typo in a key is a configuration bug, not a default
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// envRef matches ${VAR} and ${VAR:-fallback}.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandNode substitutes ${VAR} references in every scalar of the document, so
// a secret can be injected into any field. Names of variables that are unset
// and carry no fallback are collected rather than substituted as empty
// strings: silently authenticating with an empty password is worse than
// refusing to start.
func expandNode(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode {
		expanded := expandString(n.Value, missing)
		if expanded != n.Value {
			n.Value = expanded
			// The original style may no longer be able to hold the value — an
			// expanded secret can contain anything — so let the emitter pick.
			n.Style = 0
			n.Tag = "!!str"
		}
		return
	}
	for _, child := range n.Content {
		expandNode(child, missing)
	}
}

func expandString(s string, missing *[]string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		g := envRef.FindStringSubmatch(m)
		name, fallback := g[1], g[2]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if strings.Contains(m, ":-") {
			return fallback
		}
		*missing = append(*missing, name)
		return ""
	})
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = DefaultAddr
	}
	c.Server.BasePath = normalizeBasePath(c.Server.BasePath)
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 15 * time.Second
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = 60 * time.Second
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 10 * time.Second
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.UI.Title == "" {
		c.UI.Title = DefaultTitle
	}
	if c.UI.Theme == "" {
		c.UI.Theme = "auto"
	}
	if c.UI.CatalogPageSize <= 0 {
		c.UI.CatalogPageSize = DefaultCatalogPageSize
	}
	if c.UI.TagPageSize <= 0 {
		c.UI.TagPageSize = DefaultTagPageSize
	}

	for i := range c.Registries {
		r := &c.Registries[i]
		if r.Name == "" {
			r.Name = hostOf(r.URL)
		}
		if r.ID == "" {
			r.ID = slug(r.Name)
		}
		if r.Auth.Type == "" {
			r.Auth.Type = "none"
		}
		if r.Timeout == 0 {
			r.Timeout = DefaultTimeout
		}
		if r.CacheTTL == 0 {
			r.CacheTTL = DefaultCacheTTL
		}
		r.URL = strings.TrimRight(r.URL, "/")
	}

	if c.UI.DefaultRegistry == "" && len(c.Registries) > 0 {
		c.UI.DefaultRegistry = c.Registries[0].ID
	}
}

// resolveSecrets pulls credentials out of the environment variables named by
// the *_env fields, which take precedence over an inline value.
func (c *Config) resolveSecrets() error {
	for i := range c.Registries {
		r := &c.Registries[i]
		if name := r.Auth.PasswordEnv; name != "" {
			v, ok := os.LookupEnv(name)
			if !ok {
				return fmt.Errorf("registry %q: password_env %q is not set", r.ID, name)
			}
			r.Auth.Password = v
		}
		if name := r.Auth.TokenEnv; name != "" {
			v, ok := os.LookupEnv(name)
			if !ok {
				return fmt.Errorf("registry %q: token_env %q is not set", r.ID, name)
			}
			r.Auth.Token = v
		}
	}
	return nil
}

// Validate reports every problem it can find at once, so an operator fixing a
// config file does not have to restart the process per mistake.
func (c *Config) Validate() error {
	var errs []error

	if len(c.Registries) == 0 {
		errs = append(errs, errors.New("no registries configured: add at least one entry under `registries`"))
	}
	switch c.UI.Theme {
	case "auto", "dark", "light":
	default:
		errs = append(errs, fmt.Errorf("ui.theme %q is not one of auto, dark, light", c.UI.Theme))
	}
	switch c.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("server.log_level %q is not one of debug, info, warn, error", c.Server.LogLevel))
	}

	seen := make(map[string]bool, len(c.Registries))
	for i, r := range c.Registries {
		where := fmt.Sprintf("registries[%d]", i)
		if r.ID != "" {
			where = fmt.Sprintf("registry %q", r.ID)
		}
		if r.ID == "" {
			errs = append(errs, fmt.Errorf("%s: could not derive an id, set `id` or `name`", where))
		} else if seen[r.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate id, ids must be unique", where))
		}
		seen[r.ID] = true

		if r.URL == "" {
			errs = append(errs, fmt.Errorf("%s: url is required", where))
		} else if err := validateURL(r.URL); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}

		switch r.Auth.Type {
		case "none":
		case "basic":
			if r.Auth.Username == "" {
				errs = append(errs, fmt.Errorf("%s: auth.username is required for basic auth", where))
			}
		case "bearer":
			if r.Auth.Token == "" {
				errs = append(errs, fmt.Errorf("%s: auth.token or auth.token_env is required for bearer auth", where))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: auth.type %q is not one of none, basic, bearer", where, r.Auth.Type))
		}
	}

	if c.UI.DefaultRegistry != "" && !seen[c.UI.DefaultRegistry] {
		errs = append(errs, fmt.Errorf("ui.default_registry %q does not match any configured registry", c.UI.DefaultRegistry))
	}

	return errors.Join(errs...)
}

// Registry returns the registry with the given id.
func (c *Config) Registry(id string) (Registry, bool) {
	for _, r := range c.Registries {
		if r.ID == id {
			return r, true
		}
	}
	return Registry{}, false
}

// Default returns the registry selected on first visit.
func (c *Config) Default() Registry {
	if r, ok := c.Registry(c.UI.DefaultRegistry); ok {
		return r
	}
	return c.Registries[0]
}

// PullTarget is the host used when rendering `docker pull` commands.
func (r Registry) PullTarget() string {
	if r.PullHost != "" {
		return r.PullHost
	}
	return hostOf(r.URL)
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q is missing a host", raw)
	}
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v2") {
		return fmt.Errorf("url %q must not include the /v2 API path", raw)
	}
	return nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func slug(s string) string {
	s = idPattern.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}
