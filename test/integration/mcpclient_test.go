//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/server"
)

const (
	testToken     = "it-access-token"
	testPublicURL = "https://mail-mcp.example.test"
)

// mcpServer is the imap-mcp HTTP handler on a local test server.
type mcpServer struct {
	URL  string
	logs *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServer runs the real handler against Dovecot (or imapHosts when
// given). Logs are kept at debug level and shown on failure.
func startServer(t *testing.T, imapHosts ...string) *mcpServer {
	t.Helper()
	if len(imapHosts) == 0 {
		imapHosts = []string{imapHost()}
	}
	return startServerCfg(t, config.Config{
		Listen:             "127.0.0.1:0",
		IMAPHosts:          imapHosts,
		AccessTokens:       []string{testToken},
		PublicURL:          testPublicURL,
		MaxSessionsPerUser: 4,
		MaxUpload:          25 << 20,
	})
}

// startServerCfg runs the real handler with cfg against Dovecot.
func startServerCfg(t *testing.T, cfg config.Config) *mcpServer {
	t.Helper()
	logs := &syncBuffer{}
	logger := logging.New(logs, slog.LevelDebug, "text")
	ts := httptest.NewServer(server.NewWithDialer(cfg, logger, testDialer(t)))
	t.Cleanup(func() {
		ts.Close()
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	return &mcpServer{URL: ts.URL, logs: logs}
}

type headerRT struct{ h http.Header }

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range rt.h {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

// authHeaders are the headers a configured MCP client sends.
func authHeaders(c imapsafe.Credentials) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	h.Set("X-IMAP-Username", c.Username)
	h.Set("X-IMAP-Password", c.Password)
	return h
}

// connect opens an MCP client session over real HTTP with c's headers.
func (m *mcpServer) connect(t *testing.T, c imapsafe.Credentials) *mcp.ClientSession {
	t.Helper()
	cl := mcp.NewClient(&mcp.Implementation{Name: "it", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             m.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: headerRT{authHeaders(c)}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// toolError is the JSON error shape of a failed tool call.
type toolError struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// call runs a tool and decodes its JSON text into out. It returns the
// decoded error shape when the tool reports an error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *toolError {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s: content is %T", name, res.Content[0])
	}
	if res.IsError {
		var te toolError
		if err := json.Unmarshal([]byte(text.Text), &te); err != nil || te.Error.Code == "" {
			t.Fatalf("%s: error text is not the error shape: %q", name, text.Text)
		}
		return &te
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text.Text), out); err != nil {
			t.Fatalf("%s: decode %q: %v", name, text.Text, err)
		}
	}
	return nil
}

// mustCall is call that fails the test on a tool error.
func mustCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	if te := call(t, cs, name, args, out); te != nil {
		t.Fatalf("%s: tool error %+v", name, te.Error)
	}
}

// seedMsg is one message to append with a given internal date and flags.
type seedMsg struct {
	raw   []byte
	date  time.Time
	flags []imap.Flag
}

// seedMsgs appends msgs to folder and returns their UIDs.
func seedMsgs(t *testing.T, c imapsafe.Credentials, folder string, msgs ...seedMsg) []uint32 {
	t.Helper()
	cl := rawClient(t, c)
	defer cl.Close()
	var uids []uint32
	for i, m := range msgs {
		raw := bytes.ReplaceAll(bytes.ReplaceAll(m.raw, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
		cmd := cl.Append(folder, int64(len(raw)), &imap.AppendOptions{Time: m.date, Flags: m.flags})
		if _, err := cmd.Write(raw); err != nil {
			t.Fatalf("seed write #%d: %v", i, err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("seed close #%d: %v", i, err)
		}
		data, err := cmd.Wait()
		if err != nil {
			t.Fatalf("seed append #%d: %v", i, err)
		}
		uids = append(uids, uint32(data.UID))
	}
	if err := cl.Logout().Wait(); err != nil {
		t.Logf("seed logout: %v", err)
	}
	return uids
}
