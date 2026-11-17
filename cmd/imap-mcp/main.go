package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/server"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		// No valid config, so no log settings: use the defaults (JSON, info).
		attrs := []any{"error", err.Error()}
		if cfg.Env != nil {
			attrs = append(attrs, "env", cfg.Env)
		}
		logging.New(os.Stdout, slog.LevelInfo, "json").Error("config.invalid", attrs...)
		os.Exit(1)
	}
	logger := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	h, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("server.init_failed", "error", err.Error())
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.ListenAndServe(logging.With(ctx, logger), cfg, h); err != nil {
		logger.Error("server.failed", "error", err.Error())
		os.Exit(1)
	}
}
