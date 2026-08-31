// Command mazeregistryui serves a web UI for browsing OCI registries.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/amaze-labs/MazeRegistryUI/internal/config"
	"github.com/amaze-labs/MazeRegistryUI/internal/server"
	"github.com/amaze-labs/MazeRegistryUI/internal/version"
)

const defaultConfigPath = "/etc/mazeregistryui/config.yaml"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mazeregistryui: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", envOr("MRUI_CONFIG", defaultConfigPath), "path to the configuration file")
		checkOnly   = flag.Bool("check", false, "validate the configuration and exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		logFormat   = flag.String("log-format", envOr("MRUI_LOG_FORMAT", "text"), "log output format: text or json")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("mazeregistryui %s\nbuilt %s\n", version.String(), version.BuildDate)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	if *checkOnly {
		fmt.Printf("configuration at %s is valid: %d registries, listening on %s\n",
			*configPath, len(cfg.Registries), cfg.Server.Addr)
		return nil
	}

	log := newLogger(*logFormat, cfg.Server.LogLevel)
	slog.SetDefault(log)

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx)
}

func newLogger(format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
