//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
)

const testPassword = "testpass"

// imapHost is the Dovecot address from compose.yaml (implicit TLS).
func imapHost() string {
	if h := os.Getenv("IMAP_MCP_IT_HOST"); h != "" {
		return h
	}
	return "127.0.0.1:10993"
}

func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join("certs", "ca.pem"))
	if err != nil {
		t.Fatalf("read test CA (run gencerts.sh): %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("bad test CA")
	}
	return &tls.Config{RootCAs: pool}
}

func testDialer(t *testing.T) *imapsafe.Dialer {
	t.Helper()
	return &imapsafe.Dialer{
		TLSConfig: testTLS(t),
		Limiter:   imapsafe.NewLimiter(4),
		Timeout:   30 * time.Second,
	}
}

var userSeq atomic.Int64

// freshUser returns credentials for a new, empty mailbox.
func freshUser(t *testing.T) imapsafe.Credentials {
	t.Helper()
	name := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	user := fmt.Sprintf("%s-%d-%d", name, time.Now().UnixNano(), userSeq.Add(1))
	return imapsafe.Credentials{Username: user, Password: testPassword, Host: imapHost()}
}

// testWriter sends log output to t.Log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// testCtx returns a context with a debug logger. Its output goes to t.Log
// and to the returned buffer.
func testCtx(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l := logging.New(multiWriter{&buf, testWriter{t}}, slog.LevelDebug, "text")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return logging.With(ctx, l), &buf
}

type multiWriter struct {
	buf *bytes.Buffer
	tw  testWriter
}

func (m multiWriter) Write(p []byte) (int, error) {
	m.buf.Write(p)
	return m.tw.Write(p)
}

// seed appends .eml fixtures from testdata to folder with a raw client and
// returns their UIDs.
func seed(t *testing.T, c imapsafe.Credentials, folder string, emlFiles ...string) []uint32 {
	t.Helper()
	cl, err := imapclient.DialTLS(c.Host, &imapclient.Options{TLSConfig: testTLS(t)})
	if err != nil {
		t.Fatalf("seed dial: %v", err)
	}
	defer cl.Close()
	if err := cl.Login(c.Username, c.Password).Wait(); err != nil {
		t.Fatalf("seed login: %v", err)
	}
	var uids []uint32
	for _, f := range emlFiles {
		raw, err := os.ReadFile(filepath.Join("testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
		cmd := cl.Append(folder, int64(len(raw)), nil)
		if _, err := cmd.Write(raw); err != nil {
			t.Fatalf("seed write %s: %v", f, err)
		}
		if err := cmd.Close(); err != nil {
			t.Fatalf("seed close %s: %v", f, err)
		}
		data, err := cmd.Wait()
		if err != nil {
			t.Fatalf("seed append %s: %v", f, err)
		}
		uids = append(uids, uint32(data.UID))
	}
	if err := cl.Logout().Wait(); err != nil {
		t.Logf("seed logout: %v", err)
	}
	return uids
}
