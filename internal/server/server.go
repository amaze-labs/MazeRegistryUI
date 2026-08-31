// Package server wires the HTTP layer: routing, template rendering and the
// per-registry clients. The browser never talks to a registry directly, so
// credentials stay on this side and CORS never enters the picture.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/config"
	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
	"github.com/amaze-labs/MazeRegistryUI/internal/version"
	"github.com/amaze-labs/MazeRegistryUI/web"
)

// perRegistryConcurrency bounds how many lazy row lookups run against one
// registry at a time. A page of fifty tags must not turn into fifty
// simultaneous manifest requests.
const perRegistryConcurrency = 6

// Server serves the UI.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	clients map[string]registry.Client
	sem     map[string]chan struct{}
	tmpl    map[string]*template.Template
	catalog *catalogCache
	health  *healthCache
	// asset is a cache-busting token for static assets, tied to the build.
	asset string
	// csrf is a per-process token embedded in destructive forms. There are no
	// sessions to protect, only the cross-site submission of a delete form.
	csrf    string
	handler http.Handler
}

// New builds a Server from a validated configuration.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	clients := make(map[string]registry.Client, len(cfg.Registries))
	sem := make(map[string]chan struct{}, len(cfg.Registries))
	for _, r := range cfg.Registries {
		client, err := registry.New(registry.Options{
			Name:    r.Name,
			BaseURL: r.URL,
			Auth: registry.AuthConfig{
				Type:     r.Auth.Type,
				Username: r.Auth.Username,
				Password: r.Auth.Password,
				Token:    r.Auth.Token,
			},
			Insecure:      r.Insecure,
			Timeout:       r.Timeout,
			CacheTTL:      r.CacheTTL,
			UserAgent:     version.UserAgent(),
			DeleteEnabled: r.DeleteEnabled,
		})
		if err != nil {
			return nil, fmt.Errorf("registry %q: %w", r.ID, err)
		}
		clients[r.ID] = client
		sem[r.ID] = make(chan struct{}, perRegistryConcurrency)
	}

	tmpl, err := parseTemplates(templateFuncs())
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		log:     log,
		clients: clients,
		sem:     sem,
		tmpl:    tmpl,
		catalog: newCatalogCache(),
		health:  newHealthCache(),
		asset:   assetToken(),
		csrf:    randomToken(),
	}
	s.handler = s.routes()
	return s, nil
}

// ServeHTTP makes Server the application handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /static/", s.staticHandler())

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /r/{rid}", s.handleCatalog)
	mux.HandleFunc("GET /r/{rid}/repo/{repo...}", s.handleRepository)
	mux.HandleFunc("GET /r/{rid}/image/{repo...}", s.handleImage)
	mux.HandleFunc("POST /r/{rid}/delete", s.handleDelete)

	// Fragment endpoints consumed by HTMX.
	mux.HandleFunc("GET /x/catalog/{rid}", s.handleCatalogRows)
	mux.HandleFunc("GET /x/repocount/{rid}/{repo...}", s.handleRepoCount)
	mux.HandleFunc("GET /x/tags/{rid}/{repo...}", s.handleTagRows)
	mux.HandleFunc("GET /x/tag/{rid}/{repo...}", s.handleTagRow)
	mux.HandleFunc("GET /x/health/{rid}", s.handleRegistryHealth)
	mux.HandleFunc("POST /x/theme", s.handleTheme)

	var h http.Handler = mux
	h = s.securityHeaders(h)
	if s.cfg.Server.AccessLog {
		h = s.accessLog(h)
	}
	h = s.recoverPanic(h)

	if bp := s.cfg.Server.BasePath; bp != "" {
		// Serving under a sub-path: strip it before matching, and redirect the
		// bare prefix so /registry and /registry/ both work.
		outer := http.NewServeMux()
		outer.Handle(bp+"/", http.StripPrefix(bp, h))
		outer.HandleFunc(bp, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, bp+"/", http.StatusMovedPermanently)
		})
		return outer
	}
	return h
}

// staticHandler serves the embedded assets. Every reference carries a build
// token in the query string, so the files themselves can be cached hard.
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		panic(fmt.Sprintf("embedded static assets are broken: %v", err))
	}
	files := http.FileServerFS(sub)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		files.ServeHTTP(w, r)
	}))
}

// Run starts the listener and blocks until ctx is cancelled, then drains.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Server.Addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       s.cfg.Server.ReadTimeout,
		WriteTimeout:      s.cfg.Server.WriteTimeout,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Info("listening",
			"addr", s.cfg.Server.Addr,
			"base_path", orDash(s.cfg.Server.BasePath),
			"registries", len(s.cfg.Registries),
			"version", version.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.cfg.Server.ShutdownTimeout)
	shutCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// healthCache remembers the last probe result per registry so the picker does
// not re-probe every endpoint on every page load.
type healthCache struct {
	mu    sync.Mutex
	state map[string]healthState
	ttl   time.Duration
}

type healthState struct {
	State   string // ok, down
	Message string
	At      time.Time
}

func newHealthCache() *healthCache {
	return &healthCache{state: make(map[string]healthState), ttl: 30 * time.Second}
}

func (h *healthCache) get(id string) (healthState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.state[id]
	if !ok || time.Since(st.At) > h.ttl {
		return healthState{}, false
	}
	return st, true
}

func (h *healthCache) set(id string, st healthState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st.At = time.Now()
	h.state[id] = st
}

// acquire takes a slot on the per-registry limiter, honouring cancellation.
func (s *Server) acquire(ctx context.Context, rid string) (release func(), ok bool) {
	sem, exists := s.sem[rid]
	if !exists {
		return func() {}, true
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a recoverable condition for a process
		// that is about to hand out anti-forgery tokens.
		panic("cannot read from the system random source: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// assetToken changes whenever the binary does, which is exactly when the
// embedded assets can have changed.
func assetToken() string {
	if version.Commit != "unknown" && version.Commit != "" {
		return version.Commit
	}
	return version.Version
}

func orDash(s string) string {
	if s == "" {
		return "/"
	}
	return s
}
