// Package server wires the HTTP endpoints: /mcp (MCP over Streamable HTTP,
// stateless) and /healthz. Every path except /healthz needs auth.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/auth"
	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/tools"
)

const (
	serverName      = "imap-mcp"
	serverVersion   = "0.1.0"
	shutdownTimeout = 10 * time.Second
)

// New builds the production handler. The IMAP dialer uses system roots.
func New(cfg config.Config, logger *slog.Logger) (http.Handler, error) {
	d := &imapsafe.Dialer{
		Limiter:        imapsafe.NewLimiter(cfg.MaxSessionsPerUser),
		DraftsOverride: cfg.DraftsFolder,
		SentOverride:   cfg.SentFolder,
	}
	return NewWithDialer(cfg, logger, d), nil
}

// NewWithDialer is New with a given IMAP dialer (tests use their own CA).
func NewWithDialer(cfg config.Config, logger *slog.Logger, d *imapsafe.Dialer) http.Handler {
	return newHandler(cfg, logger, func(s *mcp.Server) {
		tools.Register(s, tools.Deps{Dialer: d, PublicURL: cfg.PublicURL})
	})
}

// newHandler builds the chain logging → auth (not for /healthz) → mux.
// register adds the tools to the MCP server.
func newHandler(cfg config.Config, logger *slog.Logger, register func(*mcp.Server)) http.Handler {
	ms := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)
	register(ms)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, &mcp.StreamableHTTPOptions{
		Stateless: true,
		// Every request needs a bearer token, so DNS rebinding gains
		// nothing. The check would also reject a TLS proxy on the same
		// host that forwards the public Host header (spec 7.7).
		DisableLocalhostProtection: true,
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	root.Handle("/", auth.Middleware(cfg)(mux))
	return logging.RequestMiddleware(logger)(root)
}

// ListenAndServe serves h on cfg.Listen until ctx ends, then shuts down
// gracefully. It uses TLS when cfg.TLSCert and cfg.TLSKey are set. It logs
// with the logger in ctx.
func ListenAndServe(ctx context.Context, cfg config.Config, h http.Handler) error {
	logger := logging.From(ctx)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	logger.Info("server.start", "config", cfg.Redacted())

	errc := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()

	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		err = srv.Shutdown(sctx)
		cancel()
		if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
			err = serveErr
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	logger.Info("server.stop", "error", errString(err))
	return err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
