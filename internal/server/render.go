package server

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/web"
)

// Layout is the data every page needs to draw the shell around itself.
type Layout struct {
	Title       string
	UITitle     string
	BasePath    string
	Theme       string
	Asset       string
	Version     string
	FooterNote  string
	Registries  []RegistryOption
	Current     RegistryOption
	Crumbs      []Crumb
	Query       string
	CurrentPath string
}

// RegistryOption is one entry of the registry picker.
type RegistryOption struct {
	ID          string
	Name        string
	Host        string
	Description string
	Delete      bool
	Active      bool
	// State drives the status dot: unknown, ok or down.
	State string
	// HealthDelay staggers the background health probes so opening the picker
	// does not fire every request in the same millisecond.
	HealthDelay int
	Message     string
}

// Crumb is one breadcrumb entry. An empty Href marks the current page.
type Crumb struct {
	Label string
	Href  string
}

// KV is a sorted key/value pair rendered in the label and environment panels.
type KV struct {
	Key   string
	Value string
}

// templates parses one template set per page. Each page defines its own
// "content" block, so they cannot share a single set.
func parseTemplates(funcs template.FuncMap) (map[string]*template.Template, error) {
	pages, err := fs.Glob(web.Templates, "templates/*.html")
	if err != nil {
		return nil, err
	}

	out := make(map[string]*template.Template, len(pages)+1)
	for _, page := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(page, "templates/"), ".html")
		if name == "base" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(web.Templates,
			"templates/base.html", "templates/partials/*.html", page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		out[name] = t
	}

	// A set holding only the partials, for HTMX fragment responses.
	frag, err := template.New("partials").Funcs(funcs).ParseFS(web.Templates, "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse partials: %w", err)
	}
	out["_partials"] = frag

	return out, nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"url":   joinPath,
		"asset": joinPath,
		"bytes": humanBytes,
		"since": humanTime,
		"full":  fullTime,
		"short": shortDigest,
		"iso": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.UTC().Format(time.RFC3339)
		},
		"join": func(parts []string, sep string) string { return strings.Join(parts, sep) },
		"sub":  func(a, b int) int { return a - b },
		// dict builds an inline map so a partial can be called with ad-hoc
		// arguments, e.g. the banner partial.
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict expects an even number of arguments, got %d", len(kv))
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict key %d is not a string", i)
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
	}
}

// joinPath prefixes a root-relative path with the configured base path.
func joinPath(basePath, p string) string {
	if basePath == "" {
		return p
	}
	return basePath + p
}

// render writes a full page. It buffers first so a template error surfaces as
// a clean 500 rather than a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	t, ok := s.tmpl[page]
	if !ok {
		s.log.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		s.log.Error("render page", "page", page, "err", err, "path", r.URL.Path)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// renderPartial writes a single named template, for HTMX fragment responses.
func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl["_partials"].ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render partial", "partial", name, "err", err, "path", r.URL.Path)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
