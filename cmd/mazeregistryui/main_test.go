package main

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo},
	}
	for _, tc := range tests {
		if got := parseLevel(tc.in); got != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("MRUI_TEST_ENVOR", "value")
	if got := envOr("MRUI_TEST_ENVOR", "fallback"); got != "value" {
		t.Errorf("envOr = %q, want the environment value", got)
	}

	t.Setenv("MRUI_TEST_ENVOR_EMPTY", "")
	if got := envOr("MRUI_TEST_ENVOR_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("envOr with an empty variable = %q, want the fallback", got)
	}
	if got := envOr("MRUI_TEST_ENVOR_UNSET", "fallback"); got != "fallback" {
		t.Errorf("envOr with an unset variable = %q, want the fallback", got)
	}
}

func TestNewLogger(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"text", "json", "unknown"} {
		if got := newLogger(format, "debug"); got == nil {
			t.Errorf("newLogger(%q) returned nil", format)
		}
	}
	if !newLogger("text", "debug").Enabled(t.Context(), slog.LevelDebug) {
		t.Error("a debug logger does not emit debug records")
	}
	if newLogger("text", "error").Enabled(t.Context(), slog.LevelInfo) {
		t.Error("an error-level logger still emits info records")
	}
}
