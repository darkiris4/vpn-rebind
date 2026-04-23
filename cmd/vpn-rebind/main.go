package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/docker/docker/client"

	"github.com/mikechambers/vpn-rebind/internal/config"
	"github.com/mikechambers/vpn-rebind/internal/controller"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG_PATH", "/config/config.yaml"), "path to YAML config file (optional)")
	flag.Parse()

	// Bootstrap a temporary logger for startup errors before config is loaded.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// If the config file doesn't exist and no path was explicitly overridden,
	// treat it as "no file" rather than an error — env-only config is valid.
	if *configPath != "" {
		if _, err := os.Stat(*configPath); os.IsNotExist(err) {
			if os.Getenv("CONFIG_PATH") == "" && *configPath == "/config/config.yaml" {
				*configPath = "" // default path, file not present — env-only mode
			}
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Rebuild logger at the configured level.
	log = buildLogger(cfg.LogLevel)

	// Connect to Docker daemon. Respects DOCKER_HOST, DOCKER_CERT_PATH, etc.
	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Error("failed to connect to Docker daemon", "error", err)
		os.Exit(1)
	}
	defer dockerClient.Close()

	// Verify the daemon is reachable before entering the event loop.
	ctx := context.Background()
	if _, err := dockerClient.Ping(ctx); err != nil {
		log.Error("Docker daemon not reachable", "error", err,
			"hint", "mount the Docker socket: -v /var/run/docker.sock:/var/run/docker.sock")
		os.Exit(1)
	}

	ctrl := controller.New(dockerClient, cfg, log)

	// Honour SIGTERM and SIGINT for clean shutdown (container stop signals).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info("received signal — shutting down", "signal", sig)
		cancel()
	}()

	if err := ctrl.Run(runCtx); err != nil {
		log.Error("controller exited with error", "error", err)
		os.Exit(1)
	}
}

func buildLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
