package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amaze-labs/MazeRegistryUI/internal/config"
	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
	"github.com/amaze-labs/MazeRegistryUI/internal/version"
)

// tagCountLimit caps the tag lookup behind a catalog row's counter. Beyond it
// the row shows "N+" rather than walking a repository with thousands of tags.
const tagCountLimit = 1000

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"version":    version.Version,
		"commit":     version.Commit,
		"registries": len(s.cfg.Registries),
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, joinPath(s.cfg.Server.BasePath, "/r/"+s.cfg.Default().ID), http.StatusFound)
}

// --- catalog -----------------------------------------------------------

type repoRow struct {
	Name      string
	Namespace string
	Short     string
	Href      string
	CountURL  string
}

type catalogPage struct {
	Layout
	Repos   []repoRow
	Total   int
	Shown   int
	HasMore bool
	MoreURL string
	Err     string
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	page := s.buildCatalog(r, reg, client)
	page.Layout = s.layout(r, reg, reg.Name)
	page.Layout.Query = r.URL.Query().Get("q")
	s.render(w, r, "catalog", page)
}

// handleCatalogRows answers the search box and the "load more" button.
func (s *Server) handleCatalogRows(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	page := s.buildCatalog(r, reg, client)
	page.Layout = s.layout(r, reg, reg.Name)
	page.Layout.Query = r.URL.Query().Get("q")

	// Keep the address bar in step with the filter so the view is shareable.
	if q := r.URL.Query().Get("q"); r.Header.Get("HX-Request") != "" {
		target := joinPath(s.cfg.Server.BasePath, "/r/"+reg.ID)
		if q != "" {
			target += "?q=" + urlQueryEscape(q)
		}
		w.Header().Set("HX-Push-Url", target)
	}
	s.renderPartial(w, r, "catalog_rows", page)
}

func (s *Server) buildCatalog(r *http.Request, reg config.Registry, client registry.Client) *catalogPage {
	q := r.URL.Query().Get("q")
	offset := intParam(r, "o", 0)
	limit := s.cfg.UI.CatalogPageSize

	page := &catalogPage{}
	all, err := s.catalog.List(r.Context(), reg.ID, client, limit, reg.CacheTTL)
	if err != nil {
		page.Err = friendlyError(err)
		return page
	}

	matched := filterRepos(all, q)
	page.Total = len(matched)

	if offset > len(matched) {
		offset = len(matched)
	}
	end := min(offset+limit, len(matched))
	page.Shown = end
	page.HasMore = end < len(matched)

	base := joinPath(s.cfg.Server.BasePath, "/r/"+reg.ID)
	page.Repos = make([]repoRow, 0, end-offset)
	for _, name := range matched[offset:end] {
		ns, short := splitRepo(name)
		page.Repos = append(page.Repos, repoRow{
			Name:      name,
			Namespace: ns,
			Short:     short,
			Href:      base + "/repo/" + name,
			CountURL:  joinPath(s.cfg.Server.BasePath, "/x/repocount/"+reg.ID+"/"+name),
		})
	}

	if page.HasMore {
		page.MoreURL = fmt.Sprintf("%s/x/catalog/%s?o=%d&q=%s",
			s.cfg.Server.BasePath, reg.ID, end, urlQueryEscape(q))
	}
	return page
}

type repoCount struct {
	Count string
	Err   string
}

func (s *Server) handleRepoCount(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")

	release, acquired := s.acquire(r.Context(), reg.ID)
	if !acquired {
		return
	}
	defer release()

	tags, err := client.Tags(r.Context(), repo, tagCountLimit, "")
	if err != nil {
		s.renderPartial(w, r, "repo_count", repoCount{Err: friendlyError(err)})
		return
	}
	count := strconv.Itoa(len(tags.Tags))
	if tags.NextLast != "" {
		count += "+"
	}
	s.renderPartial(w, r, "repo_count", repoCount{Count: count})
}

// --- repository --------------------------------------------------------

type tagRowView struct {
	Tag        string
	Href       string
	SummaryURL string
	Sum        *registry.TagSummary
}

type repoPage struct {
	Layout
	Repo      string
	Namespace string
	Short     string
	Tags      []tagRowView
	TagCount  int
	HasMore   bool
	MoreURL   string
	Err       string
}

func (s *Server) handleRepository(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")
	ns, short := splitRepo(repo)

	page := &repoPage{Repo: repo, Namespace: ns, Short: short}
	page.Layout = s.layout(r, reg, repo)
	page.Crumbs = []Crumb{
		{Label: reg.Name, Href: joinPath(s.cfg.Server.BasePath, "/r/"+reg.ID)},
		{Label: repo},
	}

	tags, err := client.Tags(r.Context(), repo, s.cfg.UI.TagPageSize, r.URL.Query().Get("last"))
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			s.renderError(w, r, reg, http.StatusNotFound, "No such repository",
				fmt.Sprintf("%q does not exist on %s, or every tag has been removed.", repo, reg.Name))
			return
		}
		page.Err = friendlyError(err)
		s.render(w, r, "repository", page)
		return
	}

	s.fillTagRows(page, reg, repo, tags)
	s.render(w, r, "repository", page)
}

// handleTagRows appends the next page of tags.
func (s *Server) handleTagRows(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")

	tags, err := client.Tags(r.Context(), repo, s.cfg.UI.TagPageSize, r.URL.Query().Get("last"))
	if err != nil {
		http.Error(w, friendlyError(err), http.StatusBadGateway)
		return
	}
	page := &repoPage{Repo: repo}
	s.fillTagRows(page, reg, repo, tags)
	s.renderPartial(w, r, "tag_rows", page)
}

func (s *Server) fillTagRows(page *repoPage, reg config.Registry, repo string, tags *registry.TagPage) {
	bp := s.cfg.Server.BasePath
	page.TagCount = len(tags.Tags)
	page.Tags = make([]tagRowView, 0, len(tags.Tags))
	for _, tag := range tags.Tags {
		page.Tags = append(page.Tags, tagRowView{
			Tag:        tag,
			Href:       fmt.Sprintf("%s/r/%s/image/%s?ref=%s", bp, reg.ID, repo, urlQueryEscape(tag)),
			SummaryURL: fmt.Sprintf("%s/x/tag/%s/%s?t=%s", bp, reg.ID, repo, urlQueryEscape(tag)),
		})
	}
	if tags.NextLast != "" {
		page.HasMore = true
		page.MoreURL = fmt.Sprintf("%s/x/tags/%s/%s?last=%s", bp, reg.ID, repo, urlQueryEscape(tags.NextLast))
	}
}

// handleTagRow resolves one tag. Rows load lazily as they scroll into view, so
// a repository with hundreds of tags renders immediately and only pays for
// what is actually looked at.
func (s *Server) handleTagRow(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")
	tag := r.URL.Query().Get("t")

	release, acquired := s.acquire(r.Context(), reg.ID)
	if !acquired {
		return
	}
	defer release()

	sum := client.TagSummary(r.Context(), repo, tag)
	bp := s.cfg.Server.BasePath
	s.renderPartial(w, r, "tag_row", tagRowView{
		Tag:  tag,
		Href: fmt.Sprintf("%s/r/%s/image/%s?ref=%s", bp, reg.ID, repo, urlQueryEscape(tag)),
		Sum:  sum,
	})
}

// --- image -------------------------------------------------------------

type layerView struct {
	Index     int
	Digest    string
	MediaType string
	Command   string
	Size      int64
	Hot       bool
	Style     template.CSS
}

type childView struct {
	Platform    string
	Digest      string
	Size        int64
	Href        string
	Attestation bool
}

type imagePage struct {
	Layout
	Repo        string
	Namespace   string
	Short       string
	Ref         string
	Img         *registry.Image
	Cfg         *registry.ImageConfig
	Created     time.Time
	Platforms   []string
	Layers      []layerView
	LayerBytes  int64
	Children    []childView
	Env         []KV
	Labels      []KV
	RawManifest string
	PullCommand string
	ShowPull    bool
	CanDelete   bool
	CSRF        string
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	repo := r.PathValue("repo")
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "latest"
	}

	img, err := client.Image(r.Context(), repo, ref, true)
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrNotFound):
			s.renderError(w, r, reg, http.StatusNotFound, "No such image",
				fmt.Sprintf("%s:%s does not exist on %s.", repo, ref, reg.Name))
		case errors.Is(err, registry.ErrUnsupported):
			s.renderError(w, r, reg, http.StatusUnsupportedMediaType, "Unsupported manifest",
				"This image uses the legacy Docker schema 1 manifest format, which this UI does not read. Re-push it with a current build tool.")
		default:
			s.renderError(w, r, reg, http.StatusBadGateway, "Cannot read this image", friendlyError(err))
		}
		return
	}

	page := s.buildImagePage(reg, repo, ref, img)
	page.Layout = s.layout(r, reg, repo+":"+ref)
	page.Crumbs = []Crumb{
		{Label: reg.Name, Href: joinPath(s.cfg.Server.BasePath, "/r/"+reg.ID)},
		{Label: repo, Href: joinPath(s.cfg.Server.BasePath, "/r/"+reg.ID+"/repo/"+repo)},
		{Label: ref},
	}
	s.render(w, r, "image", page)
}

func (s *Server) buildImagePage(reg config.Registry, repo, ref string, img *registry.Image) *imagePage {
	ns, short := splitRepo(repo)
	page := &imagePage{
		Repo:      repo,
		Namespace: ns,
		Short:     short,
		Ref:       ref,
		Img:       img,
		Cfg:       img.Config,
		ShowPull:  s.cfg.UI.ShowPullCommand,
		CanDelete: reg.DeleteEnabled,
		CSRF:      s.csrf,
	}

	if img.Config != nil {
		page.Created = img.Config.Created
		page.Env = splitEnv(img.Config.Env)
		page.Labels = sortedKV(img.Config.Labels)
	}

	page.Layers = layerViews(img.Layers)
	for _, l := range img.Layers {
		page.LayerBytes += l.Size
	}

	bp := s.cfg.Server.BasePath
	seen := map[string]bool{}
	for _, c := range img.Children {
		plat := "unknown"
		attestation := false
		if c.Platform != nil {
			plat = c.Platform.String()
			attestation = c.Platform.OS == "unknown" || c.Platform.Architecture == "unknown"
		}
		size := c.SizeTotal
		if size == 0 {
			size = c.Size
		}
		page.Children = append(page.Children, childView{
			Platform:    plat,
			Digest:      c.Digest,
			Size:        size,
			Href:        fmt.Sprintf("%s/r/%s/image/%s?ref=%s", bp, reg.ID, repo, urlQueryEscape(c.Digest)),
			Attestation: attestation,
		})
		if !attestation && !seen[plat] {
			seen[plat] = true
			page.Platforms = append(page.Platforms, plat)
		}
		// An index carries no creation date of its own; borrow the first
		// resolved child's so the page can still show one.
		if page.Created.IsZero() && c.Resolved != nil && c.Resolved.Config != nil {
			page.Created = c.Resolved.Config.Created
		}
	}
	if len(page.Platforms) == 0 && img.Config != nil && img.Config.OS != "" {
		page.Platforms = []string{registry.Platform{
			OS: img.Config.OS, Architecture: img.Config.Architecture, Variant: img.Config.Variant,
		}.String()}
	}

	page.RawManifest = prettyJSON(img.RawManifest)
	page.PullCommand = pullCommand(reg.PullTarget(), repo, ref)
	return page
}

// layerViews turns layers into bar segments. Segments are sized by share of
// the total, and the three largest are highlighted because they are where any
// meaningful size reduction has to come from.
func layerViews(layers []registry.Layer) []layerView {
	if len(layers) == 0 {
		return nil
	}
	var total int64
	for _, l := range layers {
		total += l.Size
	}

	ranked := make([]int, len(layers))
	for i := range ranked {
		ranked[i] = i
	}
	sort.SliceStable(ranked, func(a, b int) bool { return layers[ranked[a]].Size > layers[ranked[b]].Size })
	hot := make(map[int]bool, 3)
	for i, idx := range ranked {
		if i >= 3 {
			break
		}
		if total > 0 && float64(layers[idx].Size)/float64(total) > 0.05 {
			hot[idx] = true
		}
	}

	out := make([]layerView, 0, len(layers))
	for i, l := range layers {
		grow := 1.0
		if total > 0 {
			grow = float64(l.Size) / float64(total) * 1000
		}
		out = append(out, layerView{
			Index:     i + 1,
			Digest:    l.Digest,
			MediaType: l.MediaType,
			Command:   l.Command,
			Size:      l.Size,
			Hot:       hot[i],
			Style:     template.CSS(fmt.Sprintf("flex-grow:%.4f", grow)),
		})
	}
	return out
}

// --- delete ------------------------------------------------------------

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if !reg.DeleteEnabled {
		s.renderError(w, r, reg, http.StatusForbidden, "Deletion is disabled",
			fmt.Sprintf("%s is configured read-only. Set delete_enabled on this registry to allow it.", reg.Name))
		return
	}
	if !sameOriginPost(r, s.csrf) {
		s.renderError(w, r, reg, http.StatusForbidden, "Request rejected",
			"This form did not come from the current page. Reload and try again.")
		return
	}

	repo := r.PostFormValue("repo")
	digest := r.PostFormValue("digest")
	if repo == "" || digest == "" {
		s.renderError(w, r, reg, http.StatusBadRequest, "Incomplete request",
			"The delete request was missing the repository or the digest.")
		return
	}

	if err := client.DeleteManifest(r.Context(), repo, digest); err != nil {
		s.renderError(w, r, reg, http.StatusBadGateway, "Delete failed", friendlyError(err))
		return
	}
	s.catalog.Invalidate(reg.ID)
	s.log.Info("manifest deleted", "registry", reg.ID, "repo", repo, "digest", digest)

	http.Redirect(w, r, joinPath(s.cfg.Server.BasePath,
		fmt.Sprintf("/r/%s/repo/%s", reg.ID, repo)), http.StatusSeeOther)
}

// --- small endpoints ---------------------------------------------------

func (s *Server) handleRegistryHealth(w http.ResponseWriter, r *http.Request) {
	reg, client, ok := s.lookup(w, r)
	if !ok {
		return
	}
	st, cached := s.health.get(reg.ID)
	if !cached {
		if err := client.Ping(r.Context()); err != nil {
			st = healthState{State: "down", Message: friendlyError(err)}
		} else {
			st = healthState{State: "ok", Message: "Reachable"}
		}
		s.health.set(reg.ID, st)
	}
	s.renderPartial(w, r, "health_dot", st)
}

func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	to := r.PostFormValue("to")
	if to != "light" && to != "dark" {
		to = "dark"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "mrui_theme",
		Value:    to,
		Path:     orSlash(s.cfg.Server.BasePath),
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: false, // the pre-paint script in the page reads it
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.safeReturn(r.PostFormValue("return")), http.StatusSeeOther)
}

// safeReturn keeps the theme toggle from being turned into an open redirect.
func (s *Server) safeReturn(p string) string {
	fallback := joinPath(s.cfg.Server.BasePath, "/r/"+s.cfg.Default().ID)
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return fallback
	}
	if bp := s.cfg.Server.BasePath; bp != "" && !strings.HasPrefix(p, bp+"/") {
		return fallback
	}
	return p
}

// --- shared helpers ----------------------------------------------------

type errorPage struct {
	Layout
	Code     int
	Headline string
	Detail   string
}

// lookup resolves the registry id in the path, writing an error page when it
// does not exist.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (config.Registry, registry.Client, bool) {
	rid := r.PathValue("rid")
	reg, found := s.cfg.Registry(rid)
	if !found {
		s.renderError(w, r, s.cfg.Default(), http.StatusNotFound, "No such registry",
			fmt.Sprintf("%q is not configured. Pick one from the registry menu.", rid))
		return config.Registry{}, nil, false
	}
	return reg, s.clients[rid], true
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, reg config.Registry, code int, headline, detail string) {
	page := &errorPage{Code: code, Headline: headline, Detail: detail}
	page.Layout = s.layout(r, reg, headline)
	w.WriteHeader(code)
	s.render(w, r, "error", page)
}

func (s *Server) layout(r *http.Request, current config.Registry, title string) Layout {
	l := Layout{
		Title:       title + " · " + s.cfg.UI.Title,
		UITitle:     s.cfg.UI.Title,
		BasePath:    s.cfg.Server.BasePath,
		Theme:       s.theme(r),
		Asset:       s.asset,
		Version:     version.Version,
		FooterNote:  s.cfg.UI.FooterNote,
		CurrentPath: joinPath(s.cfg.Server.BasePath, r.URL.RequestURI()),
	}
	if title == s.cfg.UI.Title {
		l.Title = s.cfg.UI.Title
	}

	l.Registries = make([]RegistryOption, 0, len(s.cfg.Registries))
	for i, reg := range s.cfg.Registries {
		opt := RegistryOption{
			ID:          reg.ID,
			Name:        reg.Name,
			Host:        reg.PullTarget(),
			Description: reg.Description,
			Delete:      reg.DeleteEnabled,
			Active:      reg.ID == current.ID,
			State:       "unknown",
			HealthDelay: i * 120,
		}
		if st, ok := s.health.get(reg.ID); ok {
			opt.State, opt.Message = st.State, st.Message
		}
		l.Registries = append(l.Registries, opt)
		if opt.Active {
			l.Current = opt
		}
	}
	return l
}

// theme resolves the server-side theme hint. The cookie wins; otherwise the
// configured default decides, and "auto" defers to the client script.
func (s *Server) theme(r *http.Request) string {
	if c, err := r.Cookie("mrui_theme"); err == nil {
		if c.Value == "dark" || c.Value == "light" {
			return c.Value
		}
	}
	if s.cfg.UI.Theme == "auto" {
		return "dark"
	}
	return s.cfg.UI.Theme
}

// friendlyError turns a client error into something an operator can act on,
// without leaking credentials or internal URLs.
func friendlyError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, registry.ErrUnauthorized):
		return "The registry rejected the configured credentials."
	case errors.Is(err, registry.ErrNotFound):
		return "The registry returned 404 for this request."
	case errors.Is(err, registry.ErrDeleteDenied):
		return "Deletion is not enabled for this registry."
	default:
		return err.Error()
	}
}

func splitRepo(name string) (namespace, short string) {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

func pullCommand(host, repo, ref string) string {
	if strings.HasPrefix(ref, "sha256:") {
		return fmt.Sprintf("docker pull %s/%s@%s", host, repo, ref)
	}
	return fmt.Sprintf("docker pull %s/%s:%s", host, repo, ref)
}

// splitEnv turns KEY=value strings into pairs, keeping registry order because
// later entries override earlier ones.
func splitEnv(env []string) []KV {
	out := make([]KV, 0, len(env))
	for _, e := range env {
		k, v, found := strings.Cut(e, "=")
		if !found {
			out = append(out, KV{Key: e})
			continue
		}
		out = append(out, KV{Key: k, Value: v})
	}
	return out
}

func sortedKV(m map[string]string) []KV {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, KV{Key: k, Value: m[k]})
	}
	return out
}

func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func orSlash(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// urlQueryEscape is url.QueryEscape with spaces as %20 rather than +, which
// reads better in a shared link.
func urlQueryEscape(s string) string {
	return strings.ReplaceAll(neturl.QueryEscape(s), "+", "%20")
}
