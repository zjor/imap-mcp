// Package logging provides structured logging and per-request tracing.
package logging

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

var (
	requestIDRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	traceparentRe = regexp.MustCompile(`^[0-9a-f]{2}-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
)

// New builds a logger. format is "json" or "text"; anything else means json.
func New(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// With stores l in the context.
func With(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// From returns the logger in ctx, or the default logger when there is none.
func From(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RequestID returns the request id in ctx, or "" when there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// TokenID returns the first 8 hex chars of SHA-256(token). Safe to log.
func TokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.n += int64(n)
	return n, err
}

// Flush keeps streaming responses working.
func (s *statusWriter) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	http.NewResponseController(s.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// RequestMiddleware tags each request with request_id (and trace_id when a
// valid traceparent is sent) and logs one http.request line at the end.
// The logged path never includes the query string.
func RequestMiddleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := r.Header.Get("X-Request-Id")
			if !requestIDRe.MatchString(id) {
				id = newID()
			}
			l := base.With("request_id", id)
			if m := traceparentRe.FindStringSubmatch(r.Header.Get("traceparent")); m != nil {
				l = l.With("trace_id", m[1])
			}
			w.Header().Set("X-Request-Id", id)

			ctx := context.WithValue(With(r.Context(), l), requestIDKey, id)
			body := &countingBody{ReadCloser: r.Body}
			r = r.WithContext(ctx)
			r.Body = body
			sw := &statusWriter{ResponseWriter: w}

			next.ServeHTTP(sw, r)

			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			l.Info("http.request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes_in", body.n,
				"bytes_out", sw.n,
			)
		})
	}
}
