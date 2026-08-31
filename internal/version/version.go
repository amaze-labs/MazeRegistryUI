// SPDX-License-Identifier: GPL-3.0-or-later

// Package version carries build metadata injected at link time.
package version

import "runtime/debug"

// Overridden with -ldflags -X at build time.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String renders the full version line shown in logs and the UI footer.
func String() string {
	v := Version
	if Commit != "unknown" && Commit != "" {
		v += " (" + Commit + ")"
	}
	return v
}

// UserAgent is sent on every outbound registry request.
func UserAgent() string {
	return "MazeRegistryUI/" + Version
}

func init() {
	// Fill in the commit from the embedded VCS stamp when the binary was built
	// with plain `go build`, which does not run our ldflags.
	if Commit != "unknown" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			Commit = s.Value[:7]
		}
	}
}
