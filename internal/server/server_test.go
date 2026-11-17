package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/auth"
	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/logging"
)

func testConfig() config.Config {
	return config.Config{
		IMAPHosts:    []string{"imap.example.com:993"},
		AccessTokens: []string{"tok-1"},
	}
}

type headerRT struct{ h http.Header }

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range rt.h {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestTools_SeeCredentials checks that a tool handler's context carries the
// credentials and request id that the HTTP middleware stored.
func TestTools_SeeCredentials(t *testing.T) {
	type probeOut struct {
		User, Host, RequestID string
	}
	h := newHandler(testConfig(), slog.New(slog.DiscardHandler), func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "probe"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, probeOut, error) {
			c, ok := auth.CredentialsFrom(ctx)
			if !ok {
				return nil, probeOut{}, nil
			}
			return nil, probeOut{User: c.Username, Host: c.Host, RequestID: logging.RequestID(ctx)}, nil
		})
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer tok-1")
	hdr.Set("X-IMAP-Username", "alice")
	hdr.Set("X-IMAP-Password", "secret")
	hdr.Set("X-Request-Id", "req-123")
	cl := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	ctx := context.Background()
	cs, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: headerRT{hdr}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "probe"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got, _ := res.StructuredContent.(map[string]any)
	if got["User"] != "alice" || got["Host"] != "imap.example.com:993" || got["RequestID"] != "req-123" {
		t.Fatalf("tool context = %v, want alice / imap.example.com:993 / req-123", res.StructuredContent)
	}
}

func TestHealthz_NoAuth(t *testing.T) {
	h := newHandler(testConfig(), slog.New(slog.DiscardHandler), func(*mcp.Server) {})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || !strings.Contains(string(body), "ok") {
		t.Fatalf("healthz = %d %q", rec.Code, body)
	}
}

func TestMCP_NeedsAuth(t *testing.T) {
	h := newHandler(testConfig(), slog.New(slog.DiscardHandler), func(*mcp.Server) {})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
