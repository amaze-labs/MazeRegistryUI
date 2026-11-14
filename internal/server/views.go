// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"hash/fnv"
	"regexp"
	"sort"
	"strings"

	"github.com/amaze-labs/MazeRegistryUI/internal/registry"
)

// tintCount is the size of the namespace palette in app.css (.t0 … .t7).
const tintCount = 8

// nsTint maps a namespace to a palette slot by hashing its name. It is the
// fallback of catalogCache.Tint, used before a registry's catalog is known.
// Repositories without a namespace get the neutral slot.
func nsTint(ns string) string {
	if ns == "" {
		return "tn"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(ns))
	return "t" + string(rune('0'+h.Sum32()%tintCount))
}

// stripSeg is one band of a layer strip, in percent of the strip's width.
type stripSeg struct {
	X, W float64
	// Kind is "base" (the first layer — usually the base image's root
	// filesystem), "main" (the largest of the remaining layers) or "thin".
	Kind string
}

// stripView is the compact composition bar drawn on catalog and tag rows. It
// is rendered as SVG geometry rather than CSS widths: the rows arrive as HTMX
// fragments, which cannot carry the page's style nonce, and the
// Content-Security-Policy forbids style attributes.
type stripView struct {
	Segs     []stripSeg
	BasePct  int
	MainPct  int
	Layers   int
	TotalLen int64
}

// stripFromLayers builds the strip for a single-platform image's layers. It
// returns nil for an image with no layers (or no known sizes), so templates
// can fall back to a dash.
func stripFromLayers(layers []registry.Layer) *stripView {
	var total int64
	for _, l := range layers {
		total += l.Size
	}
	if len(layers) == 0 || total <= 0 {
		return nil
	}

	main := -1
	for i := 1; i < len(layers); i++ {
		if main < 0 || layers[i].Size > layers[main].Size {
			main = i
		}
	}

	sv := &stripView{Layers: len(layers), TotalLen: total}
	x := 0.0
	for i, l := range layers {
		w := float64(l.Size) / float64(total) * 100
		kind := "thin"
		switch i {
		case 0:
			kind = "base"
			sv.BasePct = int(w + 0.5)
		case main:
			kind = "main"
			sv.MainPct = int(w + 0.5)
		}
		// Adjacent thin layers merge into one band: dozens of 1 % slivers
		// say nothing a single dark band does not.
		if n := len(sv.Segs); kind == "thin" && n > 0 && sv.Segs[n-1].Kind == "thin" {
			sv.Segs[n-1].W += w
		} else {
			sv.Segs = append(sv.Segs, stripSeg{X: x, W: w, Kind: kind})
		}
		x += w
	}
	return sv
}

// imageLayers returns the layers that describe an image: its own, or for an
// index those of its representative platform.
func imageLayers(img *registry.Image) []registry.Layer {
	if img == nil {
		return nil
	}
	if len(img.Layers) > 0 || !img.IsIndex {
		return img.Layers
	}
	if child := representativeChild(img); child != nil {
		return child.Resolved.Layers
	}
	return nil
}

// tag kinds drive the filter chips on the repository page.
var (
	// releaseTag is a plain version: 1, 1.2, v1.2.3, 2026.11.11.
	releaseTag = regexp.MustCompile(`^v?\d+(\.\d+)*$`)
	// preTag is a version with a suffix: 2.0.0-rc.3, 1.29-alpine, 3.4.1-1.
	preTag = regexp.MustCompile(`^v?\d+(\.\d+)*[-+._].+$`)
)

// tagKind classifies a tag name for the repository page's filter chips.
// "moving" is anything that is not version-shaped (latest, main, edge): a
// pointer that is expected to move, which is exactly why it is worth telling
// apart.
func tagKind(tag string) string {
	switch {
	case releaseTag.MatchString(tag):
		return "release"
	case preTag.MatchString(tag):
		return "pre"
	default:
		return "moving"
	}
}

// pickLatest chooses the tag a catalog row describes. Tags arrive sorted
// newest version first with "latest" on top, but a natural sort also puts
// words such as "buildcache" or "main" ahead of every number. So the first
// plain version wins, then the first pre-release, and only then a moving
// tag — preferring anything over "latest", which says the least.
func pickLatest(tags []string) string {
	var pre, moving string
	for _, t := range tags {
		switch tagKind(t) {
		case "release":
			return t
		case "pre":
			if pre == "" {
				pre = t
			}
		default:
			if moving == "" && t != "latest" {
				moving = t
			}
		}
	}
	switch {
	case pre != "":
		return pre
	case moving != "":
		return moving
	case len(tags) > 0:
		return tags[0]
	}
	return ""
}

// nsCount is one namespace of a registry with the number of repositories in it.
type nsCount struct {
	Name  string
	Count int
}

// namespaceCounts groups repository names by namespace, largest first, ties
// by name. The empty namespace (repositories at the root) is included.
func namespaceCounts(repos []string) []nsCount {
	counts := map[string]int{}
	for _, r := range repos {
		ns, _ := splitRepo(r)
		counts[ns]++
	}
	out := make([]nsCount, 0, len(counts))
	for ns, n := range counts {
		out = append(out, nsCount{Name: ns, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// nameSeg is a run of a repository name that shares the same styling: inside
// the namespace prefix or not, and matching the query or not.
type nameSeg struct {
	Text string
	NS   bool
	Hit  bool
}

// markName splits a repository name into runs for the live search results:
// every case-insensitive occurrence of every term is a hit, and the
// "namespace/" prefix is told apart so it can be drawn dimmer.
func markName(name string, terms []string) []nameSeg {
	lower := strings.ToLower(name)
	hit := make([]bool, len(name))
	for _, t := range terms {
		if t == "" {
			continue
		}
		for from := 0; from <= len(lower)-len(t); {
			i := strings.Index(lower[from:], t)
			if i < 0 {
				break
			}
			for k := from + i; k < from+i+len(t); k++ {
				hit[k] = true
			}
			from += i + len(t)
		}
	}
	nsEnd := strings.LastIndex(name, "/") + 1
	var out []nameSeg
	for i := 0; i < len(name); i++ {
		ns, h := i < nsEnd, hit[i]
		if n := len(out); n > 0 && out[n-1].NS == ns && out[n-1].Hit == h {
			out[n-1].Text += name[i : i+1]
			continue
		}
		out = append(out, nameSeg{Text: name[i : i+1], NS: ns, Hit: h})
	}
	return out
}
