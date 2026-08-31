package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// minimal is a configuration that passes validation, for tests that only care
// about one field.
const minimal = `
registries:
  - name: Local
    url: http://localhost:5000
`

// parseOK parses raw and fails the test if it does not load cleanly.
func parseOK(t *testing.T, raw string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse failed for:\n%s\nerror: %v", raw, err)
	}
	return cfg
}

// parseErr parses raw expecting failure, and returns the error message.
func parseErr(t *testing.T, raw string) string {
	t.Helper()
	cfg, err := Parse([]byte(raw))
	if err == nil {
		t.Fatalf("Parse succeeded for:\n%s\ngot: %+v, want an error", raw, cfg)
	}
	return err.Error()
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg := parseOK(t, minimal)

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"server.addr", cfg.Server.Addr, DefaultAddr},
		{"server.base_path", cfg.Server.BasePath, ""},
		{"server.read_timeout", cfg.Server.ReadTimeout, 15 * time.Second},
		{"server.write_timeout", cfg.Server.WriteTimeout, 60 * time.Second},
		{"server.shutdown_timeout", cfg.Server.ShutdownTimeout, 10 * time.Second},
		{"server.log_level", cfg.Server.LogLevel, "info"},
		{"server.access_log", cfg.Server.AccessLog, false},
		{"ui.title", cfg.UI.Title, DefaultTitle},
		{"ui.theme", cfg.UI.Theme, "auto"},
		{"ui.catalog_page_size", cfg.UI.CatalogPageSize, DefaultCatalogPageSize},
		{"ui.tag_page_size", cfg.UI.TagPageSize, DefaultTagPageSize},
		{"ui.default_registry", cfg.UI.DefaultRegistry, "local"},
		{"registries[0].id", cfg.Registries[0].ID, "local"},
		{"registries[0].auth.type", cfg.Registries[0].Auth.Type, "none"},
		{"registries[0].timeout", cfg.Registries[0].Timeout, DefaultTimeout},
		{"registries[0].cache_ttl", cfg.Registries[0].CacheTTL, DefaultCacheTTL},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

func TestParseExplicitValuesWin(t *testing.T) {
	cfg := parseOK(t, `
server:
  addr: "127.0.0.1:9000"
  base_path: /registry
  read_timeout: 5s
  write_timeout: 6s
  shutdown_timeout: 7s
  access_log: true
  log_level: debug
ui:
  title: My Registries
  theme: light
  catalog_page_size: 10
  tag_page_size: 20
  show_pull_command: true
  footer_note: contact ops
registries:
  - id: prod
    name: Production
    url: https://reg.example.com/
    description: the main one
    pull_host: pull.example.com
    insecure: true
    delete_enabled: true
    timeout: 1s
    cache_ttl: 2s
`)

	if cfg.Server.Addr != "127.0.0.1:9000" || cfg.Server.BasePath != "/registry" {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Server.ReadTimeout != 5*time.Second || cfg.Server.WriteTimeout != 6*time.Second || cfg.Server.ShutdownTimeout != 7*time.Second {
		t.Errorf("timeouts = %v/%v/%v", cfg.Server.ReadTimeout, cfg.Server.WriteTimeout, cfg.Server.ShutdownTimeout)
	}
	if !cfg.Server.AccessLog || cfg.Server.LogLevel != "debug" {
		t.Errorf("access_log/log_level = %v/%q", cfg.Server.AccessLog, cfg.Server.LogLevel)
	}
	if cfg.UI.Title != "My Registries" || cfg.UI.Theme != "light" || !cfg.UI.ShowPullCommand || cfg.UI.FooterNote != "contact ops" {
		t.Errorf("ui = %+v", cfg.UI)
	}
	if cfg.UI.CatalogPageSize != 10 || cfg.UI.TagPageSize != 20 {
		t.Errorf("page sizes = %d/%d", cfg.UI.CatalogPageSize, cfg.UI.TagPageSize)
	}

	r := cfg.Registries[0]
	if r.ID != "prod" || r.Name != "Production" {
		t.Errorf("id/name = %q/%q, want prod/Production: an explicit id must not be overwritten", r.ID, r.Name)
	}
	if r.URL != "https://reg.example.com" {
		t.Errorf("url = %q, want the trailing slash trimmed", r.URL)
	}
	if !r.Insecure || !r.DeleteEnabled {
		t.Errorf("insecure/delete_enabled = %v/%v", r.Insecure, r.DeleteEnabled)
	}
	if r.Timeout != time.Second || r.CacheTTL != 2*time.Second {
		t.Errorf("timeout/cache_ttl = %v/%v", r.Timeout, r.CacheTTL)
	}
	if cfg.UI.DefaultRegistry != "prod" {
		t.Errorf("ui.default_registry = %q, want the first registry's id", cfg.UI.DefaultRegistry)
	}
}

func TestIDDerivation(t *testing.T) {
	tests := []struct {
		name    string
		regName string
		url     string
		wantID  string
	}{
		{name: "lowercased", regName: "Production", url: "https://a.example.com", wantID: "production"},
		{name: "spaces become dashes", regName: "My Registry", url: "https://a.example.com", wantID: "my-registry"},
		{name: "punctuation collapses", regName: "Team  //  Backend!!", url: "https://a.example.com", wantID: "team-backend"},
		{name: "leading and trailing separators trimmed", regName: "  -Prod-  ", url: "https://a.example.com", wantID: "prod"},
		{name: "digits are kept", regName: "reg2", url: "https://a.example.com", wantID: "reg2"},
		{name: "name defaults to the host", regName: "", url: "https://reg.example.com:5000", wantID: "reg-example-com-5000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := "registries:\n  - url: " + tc.url + "\n"
			if tc.regName != "" {
				raw += "    name: \"" + tc.regName + "\"\n"
			}
			cfg := parseOK(t, raw)
			if got := cfg.Registries[0].ID; got != tc.wantID {
				t.Fatalf("derived id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

func TestIDCannotBeDerived(t *testing.T) {
	// A name made entirely of separators slugs to the empty string.
	msg := parseErr(t, `
registries:
  - name: "---"
    url: https://a.example.com
`)
	if !strings.Contains(msg, "could not derive an id") {
		t.Fatalf("error %q does not explain that the id could not be derived", msg)
	}
}

func TestEnvExpansion(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		raw     string
		check   func(*testing.T, *Config)
		wantErr string
	}{
		{
			name: "simple reference",
			env:  map[string]string{"REG_URL": "https://from-env.example.com"},
			raw: `
registries:
  - name: r
    url: ${REG_URL}
`,
			check: func(t *testing.T, c *Config) {
				if c.Registries[0].URL != "https://from-env.example.com" {
					t.Fatalf("url = %q", c.Registries[0].URL)
				}
			},
		},
		{
			name: "fallback is used when the variable is unset",
			raw: `
registries:
  - name: r
    url: ${MRUI_TEST_UNSET_URL:-https://fallback.example.com}
`,
			check: func(t *testing.T, c *Config) {
				if c.Registries[0].URL != "https://fallback.example.com" {
					t.Fatalf("url = %q, want the fallback", c.Registries[0].URL)
				}
			},
		},
		{
			name: "a set variable beats the fallback",
			env:  map[string]string{"MRUI_TEST_URL": "https://real.example.com"},
			raw: `
registries:
  - name: r
    url: ${MRUI_TEST_URL:-https://fallback.example.com}
`,
			check: func(t *testing.T, c *Config) {
				if c.Registries[0].URL != "https://real.example.com" {
					t.Fatalf("url = %q, want the environment value", c.Registries[0].URL)
				}
			},
		},
		{
			name: "an empty fallback expands to nothing",
			raw: `
ui:
  footer_note: "x${MRUI_TEST_UNSET_NOTE:-}y"
registries:
  - name: r
    url: https://a.example.com
`,
			check: func(t *testing.T, c *Config) {
				if c.UI.FooterNote != "xy" {
					t.Fatalf("footer_note = %q, want xy", c.UI.FooterNote)
				}
			},
		},
		{
			name: "an empty variable is still set",
			env:  map[string]string{"MRUI_TEST_EMPTY": ""},
			raw: `
ui:
  footer_note: "[${MRUI_TEST_EMPTY:-fallback}]"
registries:
  - name: r
    url: https://a.example.com
`,
			check: func(t *testing.T, c *Config) {
				if c.UI.FooterNote != "[]" {
					t.Fatalf("footer_note = %q, want []: an empty variable is set, so the fallback must not apply", c.UI.FooterNote)
				}
			},
		},
		{
			name: "several references in one document",
			env:  map[string]string{"MRUI_TEST_A": "alpha", "MRUI_TEST_B": "beta"},
			raw: `
ui:
  title: ${MRUI_TEST_A}-${MRUI_TEST_B}
registries:
  - name: r
    url: https://a.example.com
`,
			check: func(t *testing.T, c *Config) {
				if c.UI.Title != "alpha-beta" {
					t.Fatalf("title = %q, want alpha-beta", c.UI.Title)
				}
			},
		},
		{
			name: "an unset variable without a fallback is a startup error",
			raw: `
registries:
  - name: r
    url: ${MRUI_TEST_DEFINITELY_UNSET}
`,
			wantErr: "unset environment variables: MRUI_TEST_DEFINITELY_UNSET",
		},
		{
			name: "every unset variable is reported",
			raw: `
ui:
  title: ${MRUI_TEST_MISSING_ONE}
registries:
  - name: r
    url: ${MRUI_TEST_MISSING_TWO}
`,
			wantErr: "MRUI_TEST_MISSING_ONE, MRUI_TEST_MISSING_TWO",
		},
		{
			name: "a dollar sign that is not a reference is left alone",
			raw: `
ui:
  footer_note: "costs $5, not ${}"
registries:
  - name: r
    url: https://a.example.com
`,
			check: func(t *testing.T, c *Config) {
				if c.UI.FooterNote != "costs $5, not ${}" {
					t.Fatalf("footer_note = %q, want it untouched", c.UI.FooterNote)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.wantErr != "" {
				if msg := parseErr(t, tc.raw); !strings.Contains(msg, tc.wantErr) {
					t.Fatalf("error %q does not contain %q", msg, tc.wantErr)
				}
				return
			}
			tc.check(t, parseOK(t, tc.raw))
		})
	}
}

func TestSecretResolution(t *testing.T) {
	t.Run("password_env overrides an inline password", func(t *testing.T) {
		t.Setenv("MRUI_TEST_PW", "from-env")
		cfg := parseOK(t, `
registries:
  - name: r
    url: https://a.example.com
    auth:
      type: basic
      username: alice
      password: inline
      password_env: MRUI_TEST_PW
`)
		if got := cfg.Registries[0].Auth.Password; got != "from-env" {
			t.Fatalf("password = %q, want the environment value to win", got)
		}
	})

	t.Run("token_env overrides an inline token", func(t *testing.T) {
		t.Setenv("MRUI_TEST_TOK", "tok-from-env")
		cfg := parseOK(t, `
registries:
  - name: r
    url: https://a.example.com
    auth:
      type: bearer
      token: inline
      token_env: MRUI_TEST_TOK
`)
		if got := cfg.Registries[0].Auth.Token; got != "tok-from-env" {
			t.Fatalf("token = %q, want the environment value to win", got)
		}
	})

	t.Run("an inline password is kept when no env var is named", func(t *testing.T) {
		cfg := parseOK(t, `
registries:
  - name: r
    url: https://a.example.com
    auth:
      type: basic
      username: alice
      password: inline
`)
		if got := cfg.Registries[0].Auth.Password; got != "inline" {
			t.Fatalf("password = %q, want inline", got)
		}
	})

	t.Run("an unset password_env is a startup error", func(t *testing.T) {
		msg := parseErr(t, `
registries:
  - name: r
    url: https://a.example.com
    auth:
      type: basic
      username: alice
      password_env: MRUI_TEST_NEVER_SET_PW
`)
		if !strings.Contains(msg, `password_env "MRUI_TEST_NEVER_SET_PW" is not set`) {
			t.Fatalf("error %q does not name the missing variable", msg)
		}
	})

	t.Run("an unset token_env is a startup error", func(t *testing.T) {
		msg := parseErr(t, `
registries:
  - name: r
    url: https://a.example.com
    auth:
      type: bearer
      token_env: MRUI_TEST_NEVER_SET_TOK
`)
		if !strings.Contains(msg, `token_env "MRUI_TEST_NEVER_SET_TOK" is not set`) {
			t.Fatalf("error %q does not name the missing variable", msg)
		}
	})

	t.Run("an empty password_env satisfies bearer validation", func(t *testing.T) {
		// An explicitly empty variable is "set": the operator asked for it.
		t.Setenv("MRUI_TEST_EMPTY_TOK", "")
		msg := parseErr(t, `
registries:
  - name: r
    url: https://a.example.com
    auth:
      type: bearer
      token_env: MRUI_TEST_EMPTY_TOK
`)
		if !strings.Contains(msg, "auth.token or auth.token_env is required") {
			t.Fatalf("error %q does not reject an empty bearer token", msg)
		}
	})
}

func TestUnknownKeysAreRejected(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{
			name: "top level",
			raw:  "registrys: []\n",
			want: "registrys",
		},
		{
			name: "inside server",
			raw:  "server:\n  addres: \":9000\"\nregistries:\n  - name: r\n    url: https://a.example.com\n",
			want: "addres",
		},
		{
			name: "inside a registry",
			raw:  "registries:\n  - name: r\n    url: https://a.example.com\n    delete: true\n",
			want: "delete",
		},
		{
			name: "inside auth",
			raw:  "registries:\n  - name: r\n    url: https://a.example.com\n    auth:\n      typ: basic\n",
			want: "typ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := parseErr(t, tc.raw)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not name the unknown key %q", msg, tc.want)
			}
			if !strings.Contains(msg, "parse config") {
				t.Fatalf("error %q is not reported as a parse failure", msg)
			}
		})
	}
}

func TestInvalidYAML(t *testing.T) {
	msg := parseErr(t, "registries: [oops\n")
	if !strings.Contains(msg, "parse config") {
		t.Fatalf("error %q is not reported as a parse failure", msg)
	}
}

func TestValidateRegistryURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{name: "https", url: "https://reg.example.com"},
		{name: "http", url: "http://localhost:5000"},
		{name: "with a path prefix", url: "https://reg.example.com/harbor"},
		{name: "missing", url: "", wantErr: "url is required"},
		{name: "no scheme", url: "reg.example.com", wantErr: "must use http or https"},
		{name: "wrong scheme", url: "ftp://reg.example.com", wantErr: "must use http or https"},
		{name: "no host", url: "https://", wantErr: "missing a host"},
		{name: "includes /v2", url: "https://reg.example.com/v2", wantErr: "must not include the /v2 API path"},
		{name: "includes /v2/", url: "https://reg.example.com/v2/", wantErr: "must not include the /v2 API path"},
		{name: "includes a nested /v2", url: "https://reg.example.com/harbor/v2", wantErr: "must not include the /v2 API path"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := "registries:\n  - id: r\n    name: r\n    url: \"" + tc.url + "\"\n"
			if tc.wantErr == "" {
				parseOK(t, raw)
				return
			}
			if msg := parseErr(t, raw); !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("error %q does not contain %q", msg, tc.wantErr)
			}
		})
	}
}

func TestValidateAuth(t *testing.T) {
	tests := []struct {
		name    string
		auth    string
		wantErr string
	}{
		{name: "none", auth: "      type: none\n"},
		{name: "default is none", auth: ""},
		{name: "basic with a username", auth: "      type: basic\n      username: alice\n      password: pw\n"},
		{name: "basic without a username", auth: "      type: basic\n      password: pw\n", wantErr: "auth.username is required for basic auth"},
		{name: "basic without a password is allowed", auth: "      type: basic\n      username: alice\n"},
		{name: "bearer with a token", auth: "      type: bearer\n      token: tok\n"},
		{name: "bearer without a token", auth: "      type: bearer\n", wantErr: "auth.token or auth.token_env is required for bearer auth"},
		{name: "unknown type", auth: "      type: oauth2\n", wantErr: `auth.type "oauth2" is not one of none, basic, bearer`},
		{name: "type is case-sensitive", auth: "      type: Basic\n      username: alice\n", wantErr: `auth.type "Basic" is not one of none, basic, bearer`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := "registries:\n  - id: r\n    name: r\n    url: https://a.example.com\n"
			if tc.auth != "" {
				raw += "    auth:\n" + tc.auth
			}
			if tc.wantErr == "" {
				parseOK(t, raw)
				return
			}
			if msg := parseErr(t, raw); !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("error %q does not contain %q", msg, tc.wantErr)
			}
		})
	}
}

func TestValidateSingleProblems(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{
			name: "no registries",
			raw:  "ui:\n  title: x\n",
			want: "no registries configured",
		},
		{
			name: "bad theme",
			raw:  "ui:\n  theme: neon\n" + minimal,
			want: `ui.theme "neon" is not one of auto, dark, light`,
		},
		{
			name: "bad log level",
			raw:  "server:\n  log_level: verbose\n" + minimal,
			want: `server.log_level "verbose" is not one of debug, info, warn, error`,
		},
		{
			name: "duplicate ids",
			raw: `
registries:
  - id: dup
    url: https://a.example.com
  - id: dup
    url: https://b.example.com
`,
			want: `registry "dup": duplicate id`,
		},
		{
			name: "duplicate ids derived from the same name",
			raw: `
registries:
  - name: My Reg
    url: https://a.example.com
  - name: my reg
    url: https://b.example.com
`,
			want: `registry "my-reg": duplicate id`,
		},
		{
			name: "default_registry points at nothing",
			raw: `
ui:
  default_registry: ghost
registries:
  - id: real
    url: https://a.example.com
`,
			want: `ui.default_registry "ghost" does not match any configured registry`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if msg := parseErr(t, tc.raw); !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not contain %q", msg, tc.want)
			}
		})
	}
}

// An operator fixing a config file should see everything that is wrong with
// it, not one problem per restart.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	msg := parseErr(t, `
server:
  log_level: chatty
ui:
  theme: neon
  default_registry: ghost
registries:
  - id: dup
    url: not-a-url
    auth:
      type: bearer
  - id: dup
    url: https://b.example.com/v2
    auth:
      type: basic
`)

	want := []string{
		`server.log_level "chatty"`,
		`ui.theme "neon"`,
		`ui.default_registry "ghost"`,
		`duplicate id`,
		`must use http or https`,
		`must not include the /v2 API path`,
		`auth.token or auth.token_env is required`,
		`auth.username is required`,
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("the combined error is missing %q.\nFull error:\n%s", w, msg)
		}
	}
	if got := strings.Count(msg, "\n") + 1; got < len(want) {
		t.Errorf("the combined error has %d lines, want at least %d:\n%s", got, len(want), msg)
	}
}

func TestValidateIndexInPositionWhenIDIsMissing(t *testing.T) {
	// With no id and no name there is nothing to label the entry with but its
	// position, so the message has to carry that.
	msg := parseErr(t, "registries:\n  - name: \"!!!\"\n    url: bad\n")
	if !strings.Contains(msg, "registries[0]") {
		t.Fatalf("error %q does not locate the faulty entry by index", msg)
	}
}

func TestNormalizeBasePath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{"   ", ""},
		{"registry", "/registry"},
		{"/registry", "/registry"},
		{"/registry/", "/registry"},
		{"registry/", "/registry"},
		{"  /registry/  ", "/registry"},
		{"/a/b/", "/a/b"},
		{"//", ""},
	}
	for _, tc := range tests {
		if got := normalizeBasePath(tc.in); got != tc.want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBasePathThroughParse(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{name: "no leading slash", in: "registry", want: "/registry"},
		{name: "both slashes", in: "/registry/", want: "/registry"},
		{name: "root", in: "/", want: ""},
		{name: "empty", in: `""`, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseOK(t, "server:\n  base_path: "+tc.in+"\n"+minimal)
			if cfg.Server.BasePath != tc.want {
				t.Fatalf("base_path %q normalised to %q, want %q", tc.in, cfg.Server.BasePath, tc.want)
			}
		})
	}
}

func TestRegistryLookup(t *testing.T) {
	cfg := parseOK(t, `
ui:
  default_registry: second
registries:
  - id: first
    url: https://a.example.com
  - id: second
    url: https://b.example.com
`)

	if r, ok := cfg.Registry("second"); !ok || r.URL != "https://b.example.com" {
		t.Errorf("Registry(second) = (%+v, %v)", r, ok)
	}
	if r, ok := cfg.Registry("ghost"); ok {
		t.Errorf("Registry(ghost) = (%+v, true), want not found", r)
	}
	if got := cfg.Default().ID; got != "second" {
		t.Errorf("Default().ID = %q, want second", got)
	}
}

func TestDefaultFallsBackToTheFirstRegistry(t *testing.T) {
	cfg := parseOK(t, minimal)
	// A default_registry that no longer resolves cannot happen through Parse,
	// so reach in to exercise the fallback.
	cfg.UI.DefaultRegistry = "ghost"
	if got := cfg.Default().ID; got != "local" {
		t.Fatalf("Default().ID = %q, want the first registry", got)
	}
}

func TestPullTarget(t *testing.T) {
	tests := []struct {
		name string
		reg  Registry
		want string
	}{
		{name: "pull_host wins", reg: Registry{URL: "https://internal:5000", PullHost: "reg.example.com"}, want: "reg.example.com"},
		{name: "host from the url", reg: Registry{URL: "https://reg.example.com:5000/x"}, want: "reg.example.com:5000"},
		{name: "unparseable url falls back to the raw value", reg: Registry{URL: "not a url"}, want: "not a url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.reg.PullTarget(); got != tc.want {
				t.Fatalf("PullTarget() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Run("reads a file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
			t.Fatalf("writing the fixture failed: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if len(cfg.Registries) != 1 || cfg.Registries[0].ID != "local" {
			t.Fatalf("Load returned %+v", cfg.Registries)
		}
	})

	t.Run("a missing file is an error", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil {
			t.Fatal("Load on a missing file succeeded")
		}
		if !strings.Contains(err.Error(), "read config") {
			t.Fatalf("error %q is not reported as a read failure", err)
		}
	})
}

// The shipped example must stay loadable, or the documentation is wrong.
func TestDeployConfigExampleLoads(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no example configuration at %s: %v", path, err)
	}
	// The example may reference secrets from the environment; supply anything
	// it asks for so the test exercises parsing, not the operator's shell.
	for _, name := range envRef.FindAllStringSubmatch(string(raw), -1) {
		if _, ok := os.LookupEnv(name[1]); !ok {
			t.Setenv(name[1], "placeholder")
		}
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("the shipped example configuration at %s does not load: %v", path, err)
	}
}
