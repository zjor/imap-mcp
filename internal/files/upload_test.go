package files

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjor/imap-mcp/internal/auth"
	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/tools"
)

func TestPartType(t *testing.T) {
	tests := []struct{ header, name, want string }{
		{"application/pdf; name=x", "a.bin", "application/pdf"},
		{"", "notes.txt", "text/plain; charset=utf-8"},
		{"not a type", "a.pdf", "application/pdf"},
		{"text/", "a.zzz", "application/octet-stream"},
		{"", "noext", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := partType(tt.header, tt.name); got != tt.want {
			t.Errorf("partType(%q, %q) = %q, want %q", tt.header, tt.name, got, tt.want)
		}
	}
}

// multipartReq builds a POST with one part per (field, filename, body).
func multipartReq(t *testing.T, max int64, parts ...[3]string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p[0]+`"; filename="`+p[1]+`"`)
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(p[2]))
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/drafts/x/attachments", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, max)
	return r
}

func removeAll(files []tools.UploadFile) {
	for _, f := range files {
		os.Remove(f.Path)
	}
}

func TestReceive(t *testing.T) {
	var files []tools.UploadFile
	err := receive(multipartReq(t, 1<<20, [3]string{"file", "../a.txt", "one"}, [3]string{"file", "b", "three"}), &files)
	defer removeAll(files)
	if err != nil || len(files) != 2 {
		t.Fatalf("files %+v, err %v", files, err)
	}
	if files[0].Filename != "a.txt" || files[0].Size != 3 || files[1].Size != 5 || files[1].ContentType != "application/octet-stream" {
		t.Errorf("files = %+v", files)
	}
	if b, _ := os.ReadFile(files[1].Path); string(b) != "three" {
		t.Errorf("temp file = %q", b)
	}

	bad := map[string]*http.Request{
		"wrong field": multipartReq(t, 1<<20, [3]string{"other", "a", "x"}),
		"no files":    multipartReq(t, 1<<20),
	}
	plain := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	plain.Header.Set("Content-Type", "text/plain")
	bad["not multipart"] = plain
	for name, r := range bad {
		var files []tools.UploadFile
		err := receive(r, &files)
		removeAll(files)
		var ae argError
		if !errors.As(err, &ae) {
			t.Errorf("%s: err = %v, want argError", name, err)
		}
	}

	var big []tools.UploadFile
	err = receive(multipartReq(t, 400, [3]string{"file", "a", strings.Repeat("x", 1000)}), &big)
	removeAll(big)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) || len(big) != 1 {
		t.Errorf("too large: err = %v, %d files, want MaxBytesError with the temp file listed", err, len(big))
	}
}

func TestFail_Status(t *testing.T) {
	tests := []struct {
		err    error
		status int
		log    string // expected level and event, "" for none
	}{
		{imapsafe.ErrNotEditable, http.StatusForbidden, "level=WARN msg=test.rejected"},
		{imapsafe.ErrTooLarge, http.StatusRequestEntityTooLarge, "level=WARN msg=test.rejected"},
		{imapsafe.ErrStale, http.StatusConflict, "level=WARN msg=test.rejected"},
		{errors.New("boom"), http.StatusBadGateway, "level=ERROR msg=test.failed"},
		{badArg("x"), http.StatusBadRequest, ""},
		{imapsafe.ErrUnsupported, http.StatusNotImplemented, ""},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		ctx := logging.With(context.Background(), logging.New(&buf, slog.LevelDebug, "text"))
		rec := httptest.NewRecorder()
		fail(ctx, rec, "test", tt.err, "draft_id", "d1")
		if rec.Code != tt.status {
			t.Errorf("%v: status %d, want %d", tt.err, rec.Code, tt.status)
		}
		logs := buf.String()
		if tt.log == "" && logs != "" {
			t.Errorf("%v: logged %q, want nothing", tt.err, logs)
		}
		if tt.log != "" && (!strings.Contains(logs, tt.log) || !strings.Contains(logs, "draft_id=d1")) {
			t.Errorf("%v: log %q, want %q with draft_id", tt.err, logs, tt.log)
		}
	}
}

// A client that stops sending is cut off after uploadIdle without data,
// and its temp files are removed.
func TestUpload_StalledBodyAborted(t *testing.T) {
	old := uploadIdle
	uploadIdle = 300 * time.Millisecond
	defer func() { uploadIdle = old }()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	var logs syncBuf
	cfg := config.Config{IMAPHosts: []string{"imap.example.com:993"}, AccessTokens: []string{"tok"}}
	mux := http.NewServeMux()
	mux.Handle("/drafts/{draft_id}/attachments", UploadHandler(nil, 1<<20))
	done := make(chan struct{})
	h := logging.RequestMiddleware(logging.New(&logs, slog.LevelDebug, "text"))(auth.Middleware(cfg)(mux))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	part := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\n\r\nfirst bytes"
	fmt.Fprintf(conn, "POST /drafts/%s/attachments HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\n"+
		"X-IMAP-Username: u\r\nX-IMAP-Password: p\r\nContent-Type: multipart/form-data; boundary=b\r\n"+
		"Content-Length: 100000\r\n\r\n%s", ids.NewDraftID(), part)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stalled upload was not aborted")
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout || !strings.Contains(string(body), `"code":"bad_argument"`) {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temp files left: %v", entries)
	}
	if !strings.Contains(logs.String(), "msg=upload.aborted") {
		t.Errorf("no upload.aborted log: %s", logs.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
