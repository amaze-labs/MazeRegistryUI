// Package web holds the embedded frontend assets: HTML templates, stylesheet,
// fonts and the HTMX runtime. Everything ships inside the binary so a
// deployment is a single file with no external CDN dependency.
package web

import "embed"

// Templates contains the HTML template tree.
//
//go:embed templates
var Templates embed.FS

// Static contains files served verbatim under /static/.
//
//go:embed static
var Static embed.FS
