package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	origVersion, origCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = origVersion, origCommit })

	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{name: "version and commit", version: "1.2.3", commit: "abc1234", want: "1.2.3 (abc1234)"},
		{name: "unknown commit is omitted", version: "1.2.3", commit: "unknown", want: "1.2.3"},
		{name: "empty commit is omitted", version: "1.2.3", commit: "", want: "1.2.3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit = tc.version, tc.commit
			if got := String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUserAgent(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "9.9.9"
	if got, want := UserAgent(), "MazeRegistryUI/9.9.9"; got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(UserAgent(), "MazeRegistryUI/") {
		t.Fatal("the User-Agent no longer identifies the product")
	}
}

func TestDefaultsAreSet(t *testing.T) {
	if Version == "" {
		t.Error("Version is empty; the footer and User-Agent would render blank")
	}
	if BuildDate == "" {
		t.Error("BuildDate is empty")
	}
}
