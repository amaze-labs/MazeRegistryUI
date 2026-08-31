package server

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// humanBytes formats a byte count the way registries and image tooling do:
// binary multiples, at most one decimal, never a trailing ".0".
func humanBytes(n int64) string {
	if n < 0 {
		return "—"
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	var unit string
	for _, u := range units {
		value /= 1024
		unit = u
		if value < 1024 {
			break
		}
	}
	if value >= 100 || value == math.Trunc(value) {
		return fmt.Sprintf("%.0f %s", value, unit)
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}

// humanTime renders an absolute timestamp as a compact relative age. Anything
// older than a year falls back to the date, which is more useful than "14
// months ago" when comparing release tags.
func humanTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return t.UTC().Format("2006-01-02")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	default:
		return t.UTC().Format("2006-01-02")
	}
}

// fullTime is the tooltip companion to humanTime.
func fullTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// shortDigest trims a digest to the 12 hex characters humans actually compare,
// keeping the algorithm prefix off so it fits in a table cell.
func shortDigest(d string) string {
	if d == "" {
		return "—"
	}
	if _, hex, ok := strings.Cut(d, ":"); ok {
		if len(hex) > 12 {
			return hex[:12]
		}
		return hex
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
