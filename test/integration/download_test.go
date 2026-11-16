//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
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
	if !strings.Contains(srv.logs.String(), "attachment.downloaded") {
		t.Errorf("no audit line: %s", srv.logs.String())
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
