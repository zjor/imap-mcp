//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/server"
)

// attachMail has a text body (part 1) and one attachment (part 2).
func attachMail(ctype, disposition, body string) []byte {
	return []byte("From: Alice <alice@example.com>\r\nTo: Bob <bob@example.com>\r\n" +
		"Subject: File\r\nDate: Mon, 16 Nov 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain\r\n\r\nSee file.\r\n" +
		"--b1\r\nContent-Type: " + ctype + "\r\nContent-Disposition: " + disposition + "\r\n\r\n" +
		body + "\r\n--b1--\r\n")
}

// attachmentID builds the id of part 2 of the message at uid in INBOX.
func attachmentID(t *testing.T, c imapsafe.Credentials, uid uint32) string {
	t.Helper()
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	uv, err := s.Examine(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	return ids.EncodeAttachment(ids.AttachmentID{
		MessageID: ids.MessageID{Folder: "INBOX", UIDValidity: uv, UID: uid},
		Part:      []int{2},
	})
}

func (m *mcpServer) download(t *testing.T, method, id string, hdr http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, m.URL+"/attachments/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestDownload_BytesAndHeaders(t *testing.T) {
	c := freshUser(t)
	uid := seedRaw(t, c, "INBOX", attachMail("application/pdf",
		"attachment; filename*=UTF-8''Faktura%20%C4%8D.%C5%99.pdf", "PDFDATA"))[0]
	srv := startServer(t)
	resp := srv.download(t, "GET", attachmentID(t, c, uid), authHeaders(c))
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 || string(body) != "PDFDATA" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	want := map[string]string{
		"Content-Type":            "application/octet-stream",
		"Content-Disposition":     "attachment; filename*=UTF-8''Faktura%20%C4%8D.%C5%99.pdf",
		"X-Original-Content-Type": "application/pdf",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Cache-Control":           "no-store",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	audit := logLine(srv.logs.String(), "attachment.downloaded")
	for _, want := range []string{"folder=INBOX", fmt.Sprintf("message_uid=%d", uid), "Faktura", "size=7"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit line %q lacks %q", audit, want)
		}
	}

	// HEAD: same headers, no body.
	head := srv.download(t, "HEAD", attachmentID(t, c, uid), authHeaders(c))
	if head.StatusCode != 200 || head.Header.Get("Content-Disposition") != want["Content-Disposition"] {
		t.Errorf("HEAD status %d, disposition %q", head.StatusCode, head.Header.Get("Content-Disposition"))
	}
	if b, _ := io.ReadAll(head.Body); len(b) != 0 {
		t.Errorf("HEAD body = %q", b)
	}
	if r := srv.download(t, "POST", attachmentID(t, c, uid), authHeaders(c)); r.StatusCode != 405 {
		t.Errorf("POST status = %d, want 405", r.StatusCode)
	}
}

func TestDownload_HTMLServedAsOctetStream(t *testing.T) {
	c := freshUser(t)
	uid := seedRaw(t, c, "INBOX", attachMail("text/html; charset=utf-8",
		"attachment; filename=\"../evil‮gpj.html\"", "<script>alert(1)</script>"))[0]
	resp := startServer(t).download(t, "GET", attachmentID(t, c, uid), authHeaders(c))
	body, _ := io.ReadAll(resp.Body)

	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Original-Content-Type") != "text/html" {
		t.Errorf("X-Original-Content-Type = %q, want text/html without params", resp.Header.Get("X-Original-Content-Type"))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("security headers missing: %v", resp.Header)
	}
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(cd, "%E2%80%AE") || strings.Contains(cd, "..") {
		t.Errorf("unclean Content-Disposition %q", cd)
	}
	if string(body) != "<script>alert(1)</script>" {
		t.Errorf("body = %q", body)
	}
}

func TestDownload_20MBFlatMemory(t *testing.T) {
	const size = 20 << 20
	c := freshUser(t)
	raw, want := bigAttachmentMail(size)
	uid := seedRaw(t, c, "INBOX", raw)[0]
	raw = nil
	id := attachmentID(t, c, uid)
	srv := startServer(t)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc

	resp := srv.download(t, "GET", id, authHeaders(c))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var n, nextSample int64
	for {
		k, err := resp.Body.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if n >= nextSample {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
			nextSample = n + 1<<20
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read after %d bytes: %v", n, err)
		}
	}
	if n != size {
		t.Errorf("read %d bytes, want %d", n, size)
	}
	if string(h.Sum(nil)) != string(want[:]) {
		t.Error("content differs")
	}
	if growth := int64(peak) - int64(base); growth >= 8<<20 {
		t.Errorf("heap grew by %d bytes while streaming, want < 8 MB", growth)
	}
}

func TestDownload_BadID400(t *testing.T) {
	c := freshUser(t)
	srv := startServer(t)
	resp := srv.download(t, "GET", "not-an-id", authHeaders(c))
	var e toolError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 || e.Error.Code != "bad_id" || e.Error.RequestID == "" {
		t.Fatalf("status %d, error %+v", resp.StatusCode, e.Error)
	}
}

func TestDownload_StaleAndNotFound(t *testing.T) {
	c := freshUser(t)
	uid := seedRaw(t, c, "INBOX", attachMail("application/pdf", "attachment; filename=a.pdf", "X"))[0]
	srv := startServer(t)

	stale := ids.EncodeAttachment(ids.AttachmentID{
		MessageID: ids.MessageID{Folder: "INBOX", UIDValidity: 1, UID: uid}, Part: []int{2}})
	if r := srv.download(t, "GET", stale, authHeaders(c)); r.StatusCode != 409 {
		t.Errorf("stale status = %d, want 409", r.StatusCode)
	}

	good, _ := ids.DecodeAttachment(attachmentID(t, c, uid))
	good.Part = []int{9}
	if r := srv.download(t, "GET", ids.EncodeAttachment(good), authHeaders(c)); r.StatusCode != 404 {
		t.Errorf("missing part status = %d, want 404", r.StatusCode)
	}
}

func TestDownload_NoAuth401(t *testing.T) {
	c := freshUser(t)
	uid := seedRaw(t, c, "INBOX", attachMail("application/pdf", "attachment; filename=a.pdf", "X"))[0]
	id := attachmentID(t, c, uid)
	srv := startServer(t)

	if r := srv.download(t, "GET", id, nil); r.StatusCode != 401 {
		t.Errorf("no headers: status = %d, want 401", r.StatusCode)
	}
	bad := authHeaders(c)
	bad.Set("X-IMAP-Password", "wrong")
	r := srv.download(t, "GET", id, bad)
	var e toolError
	_ = json.NewDecoder(r.Body).Decode(&e)
	if r.StatusCode != 401 || e.Error.Code != "auth_failed" {
		t.Errorf("wrong IMAP password: status %d, error %+v", r.StatusCode, e.Error)
	}
}

func TestDownload_DoesNotSetSeen(t *testing.T) {
	c := freshUser(t)
	uid := seedRaw(t, c, "INBOX", attachMail("application/pdf", "attachment; filename=a.pdf", "X"))[0]
	srv := startServer(t)
	resp := srv.download(t, "GET", attachmentID(t, c, uid), authHeaders(c))
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if flags := rawFlags(t, c, "INBOX", uid); slices.Contains(flags, imap.FlagSeen) {
		t.Fatalf("flags = %v, download set \\Seen", flags)
	}
}

// startServerOneSlot is startServer with a limiter of one session per user
// and the given IMAP address, so a leaked session blocks the next request.
func startServerOneSlot(t *testing.T, imapAddr string) *mcpServer {
	t.Helper()
	cfg := config.Config{
		Listen: "127.0.0.1:0", IMAPHosts: []string{imapAddr}, AccessTokens: []string{testToken},
		PublicURL: testPublicURL, MaxSessionsPerUser: 1,
	}
	logs := &syncBuffer{}
	d := testDialer(t)
	d.Limiter = imapsafe.NewLimiter(1)
	ts := httptest.NewServer(server.NewWithDialer(cfg, logging.New(logs, slog.LevelDebug, "text"), d))
	t.Cleanup(func() {
		ts.Close()
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	return &mcpServer{URL: ts.URL, logs: logs}
}

// cutProxy forwards TCP to imapHost() and closes both sides once limit
// bytes went from IMAP to the client. It returns the proxy address.
func cutProxy(t *testing.T, limit int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				out, err := net.Dial("tcp", imapHost())
				if err != nil {
					in.Close()
					return
				}
				go func() { io.Copy(out, in); out.Close() }()
				io.CopyN(in, out, limit)
				in.Close()
				out.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func waitLog(t *testing.T, srv *mcpServer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(srv.logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log never had %q:\n%s", want, srv.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// smallDownloadWorks fails unless a small attachment of c downloads
// within 10 s. With one slot per user it proves the slot was released.
func smallDownloadWorks(t *testing.T, srv *mcpServer, c imapsafe.Credentials) {
	t.Helper()
	uid := seedRaw(t, c, "INBOX", attachMail("application/pdf", "attachment; filename=a.pdf", "SMALL"))[0]
	req, _ := http.NewRequest("GET", srv.URL+"/attachments/"+attachmentID(t, c, uid), nil)
	req.Header = authHeaders(c)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("follow-up download: %v", err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(b) != "SMALL" {
		t.Fatalf("follow-up: status %d body %q", resp.StatusCode, b)
	}
}

func TestDownload_IMAPCutMidStream_ClientSeesBrokenTransfer(t *testing.T) {
	c := freshUser(t)
	raw, _ := bigAttachmentMail(20 << 20)
	uid := seedRaw(t, c, "INBOX", raw)[0]
	raw = nil
	id := attachmentID(t, c, uid)
	srv := startServerOneSlot(t, cutProxy(t, 4<<20))

	resp := srv.download(t, "GET", id, authHeaders(c))
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Fatalf("clean end after %d bytes; want a broken transfer", n)
	}
	t.Logf("client read %d bytes, error: %v", n, err)
	waitLog(t, srv, "attachment.aborted")
	waitLog(t, srv, "aborted=true")
	if strings.Contains(srv.logs.String(), "attachment.downloaded") {
		t.Error("aborted download logged as downloaded")
	}
	smallDownloadWorks(t, srv, c)
}

func TestDownload_ClientDisconnect_ReleasesSlot(t *testing.T) {
	c := freshUser(t)
	raw, _ := bigAttachmentMail(20 << 20)
	uid := seedRaw(t, c, "INBOX", raw)[0]
	raw = nil
	id := attachmentID(t, c, uid)
	srv := startServerOneSlot(t, imapHost())

	resp := srv.download(t, "GET", id, authHeaders(c))
	if _, err := io.ReadFull(resp.Body, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // closes the connection mid-stream
	waitLog(t, srv, "attachment.aborted")
	waitLog(t, srv, "http.request")
	smallDownloadWorks(t, srv, c)
}
