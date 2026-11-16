//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
)

type uploadOut struct {
	DraftID     string `json:"draft_id"`
	Attachments []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	} `json:"attachments"`
	Warning string `json:"warning"`
}

// upPart is one multipart part. An empty contentType sends no header.
type upPart struct {
	field, filename, contentType string
	body                         io.Reader
}

func filePart(name, ctype, body string) upPart {
	return upPart{field: "file", filename: name, contentType: ctype, body: strings.NewReader(body)}
}

// writeParts writes parts as multipart/form-data to w and closes mw.
func writeParts(mw *multipart.Writer, parts []upPart) error {
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.field, p.filename))
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		pw, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		if _, err := io.Copy(pw, p.body); err != nil {
			return err
		}
	}
	return mw.Close()
}

// upload POSTs parts to the upload endpoint of draftID. The body is
// streamed, so big parts are never held in memory.
func (m *mcpServer) upload(t *testing.T, c imapsafe.Credentials, draftID string, parts ...upPart) *http.Response {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() { pw.CloseWithError(writeParts(mw, parts)) }()
	req, err := http.NewRequest("POST", m.URL+"/drafts/"+draftID+"/attachments", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = authHeaders(c)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeUpload(t *testing.T, resp *http.Response) uploadOut {
	t.Helper()
	var out uploadOut
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status %d: %s", resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// errorOf decodes the JSON error shape and checks status and code.
func errorOf(t *testing.T, resp *http.Response, status int, code string) {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	var e toolError
	if err := json.Unmarshal(b, &e); err != nil || resp.StatusCode != status || e.Error.Code != code || e.Error.RequestID == "" {
		t.Errorf("status %d body %s, want %d %s", resp.StatusCode, b, status, code)
	}
}

func uploadTemps(t *testing.T) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(os.TempDir(), "imap-mcp-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// waitTemps waits until the number of upload temp files is n.
func waitTemps(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for uploadTemps(t) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d upload temp files, want %d", uploadTemps(t), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUpload_TwoFilesOneRequest(t *testing.T) {
	c := freshUser(t)
	srv := startServer(t)
	cs := srv.connect(t, c)
	d := forwardDraft(t, c, cs) // has report.pdf
	oldUID, _ := ownDraftRaw(t, c, d.DraftID)

	resp := srv.upload(t, c, d.DraftID,
		filePart("Nabídka č.1.pdf", "application/pdf; name=x", "PDF-BYTES"),
		filePart("../notes.txt", "", "hello"),
		filePart("blob.zzz", "not a type", "\x00\x01"))
	out := decodeUpload(t, resp)

	type att struct {
		name, ctype string
		size        int64
	}
	var got []att
	for _, a := range out.Attachments {
		got = append(got, att{a.Filename, a.ContentType, a.Size})
	}
	want := []att{
		{"report.pdf", "application/pdf", int64(len(reportPDF))},
		{"Nabídka č.1.pdf", "application/pdf", 9},
		{"notes.txt", "text/plain; charset=utf-8", 5},
		{"blob.zzz", "application/octet-stream", 2},
	}
	if out.DraftID != d.DraftID || out.Warning != "" || !slices.Equal(got, want) {
		t.Errorf("result = %+v, want attachments %+v", out, want)
	}

	if uids := rawUIDs(t, c, draftsFolder); len(uids) != 1 || uids[0] == oldUID {
		t.Fatalf("Drafts UIDs = %v (old %d), want one new copy", uids, oldUID)
	}
	_, raw := ownDraftRaw(t, c, d.DraftID)
	files := draftAttachments(t, raw)
	if string(files["report.pdf"]) != reportPDF || string(files["Nabídka č.1.pdf"]) != "PDF-BYTES" ||
		string(files["notes.txt"]) != "hello" || string(files["blob.zzz"]) != "\x00\x01" || len(files) != 4 {
		t.Errorf("draft attachments = %q", files)
	}
	p := parseDraft(t, raw)
	if strings.TrimSpace(p.Text) != "FYI" || len(p.Headers.To) != 1 || p.Headers.To[0].Email != "carol@example.com" {
		t.Errorf("draft = %+v", p)
	}
	logs := srv.logs.String()
	if !strings.Contains(logs, "draft.attachments_added") || !strings.Contains(logs, "files=3") {
		t.Errorf("no audit line: %s", logs)
	}
	if strings.Contains(logs, "PDF-BYTES") {
		t.Error("file content in the log")
	}

	// Bad requests.
	errorOf(t, srv.upload(t, c, d.DraftID, upPart{field: "other", filename: "a", body: strings.NewReader("x")}), 400, "bad_argument")
	errorOf(t, srv.upload(t, c, d.DraftID), 400, "bad_argument")
	errorOf(t, srv.upload(t, c, "nope", filePart("a.txt", "", "x")), 400, "bad_id")
	req, _ := http.NewRequest("GET", srv.URL+"/drafts/"+d.DraftID+"/attachments", nil)
	req.Header = authHeaders(c)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	errorOf(t, r, 405, "bad_argument")
	if n := len(rawUIDs(t, c, draftsFolder)); n != 1 {
		t.Errorf("Drafts has %d messages after bad requests, want 1", n)
	}
}

func TestUpload_TooLarge413(t *testing.T) {
	c := freshUser(t)
	srv := startServerCfg(t, config.Config{
		Listen: "127.0.0.1:0", IMAPHosts: []string{imapHost()}, AccessTokens: []string{testToken},
		PublicURL: testPublicURL, MaxSessionsPerUser: 4, MaxUpload: 1 << 20,
	})
	cs := srv.connect(t, c)
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "body_text": "x"}, &d)
	oldUID, oldRaw := ownDraftRaw(t, c, d.DraftID)

	resp := srv.upload(t, c, d.DraftID, upPart{field: "file", filename: "big.bin", body: io.LimitReader(zeros{}, 2<<20)})
	errorOf(t, resp, 413, "too_large")

	uid, raw := ownDraftRaw(t, c, d.DraftID)
	if uid != oldUID || !bytes.Equal(raw, oldRaw) {
		t.Errorf("draft changed: uid %d (old %d)", uid, oldUID)
	}
	if uids := rawUIDs(t, c, draftsFolder); len(uids) != 1 {
		t.Errorf("Drafts UIDs = %v", uids)
	}
}

func TestUpload_ForeignDraft403(t *testing.T) {
	c := freshUser(t)
	srv := startServer(t)
	foreign := seedRaw(t, c, draftsFolder, []byte("From: me@example.com\nSubject: mine\n\nhand-written\n"))[0]

	errorOf(t, srv.upload(t, c, ids.NewDraftID(), filePart("a.txt", "", "x")), 403, "not_editable")
	if uids := rawUIDs(t, c, draftsFolder); !slices.Equal(uids, []uint32{foreign}) {
		t.Errorf("Drafts UIDs = %v, want only the foreign draft %d", uids, foreign)
	}
}

func TestUpload_NoTempFilesLeft(t *testing.T) {
	c := freshUser(t)
	srv := startServerCfg(t, config.Config{
		Listen: "127.0.0.1:0", IMAPHosts: []string{imapHost()}, AccessTokens: []string{testToken},
		PublicURL: testPublicURL, MaxSessionsPerUser: 4, MaxUpload: 4 << 20,
	})
	cs := srv.connect(t, c)
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "body_text": "x"}, &d)
	before := uploadTemps(t)

	decodeUpload(t, srv.upload(t, c, d.DraftID, filePart("a.txt", "", "one"), filePart("b.txt", "", "two")))
	waitTemps(t, before)

	errorOf(t, srv.upload(t, c, d.DraftID, filePart("a.txt", "", "ok"),
		upPart{field: "file", filename: "big.bin", body: io.LimitReader(zeros{}, 5<<20)}), 413, "too_large")
	waitTemps(t, before)

	// A client that sends part of the body and then disconnects.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var hdr bytes.Buffer
	fmt.Fprintf(&hdr, "POST /drafts/%s/attachments HTTP/1.1\r\nHost: x\r\n", d.DraftID)
	for k, v := range authHeaders(c) {
		fmt.Fprintf(&hdr, "%s: %s\r\n", k, v[0])
	}
	hdr.WriteString("Content-Type: multipart/form-data; boundary=BB\r\nContent-Length: 3000000\r\n\r\n")
	hdr.WriteString("--BB\r\nContent-Disposition: form-data; name=\"file\"; filename=\"cut.bin\"\r\n\r\n")
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	waitTemps(t, before+1) // the handler is spooling
	conn.Close()
	waitTemps(t, before)
	waitLog(t, srv, "upload.aborted")

	// The draft still has only the two files from the good upload.
	_, raw := ownDraftRaw(t, c, d.DraftID)
	if files := draftAttachments(t, raw); len(files) != 2 {
		t.Errorf("draft attachments = %q", files)
	}
}

func TestUpload_20MBFlatMemory(t *testing.T) {
	const size = 20 << 20
	c := freshUser(t)
	srv := startServer(t)
	cs := srv.connect(t, c)
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "body_text": "x"}, &d)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	peak.Store(base)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
		}
	}()

	h := sha256.New()
	body := io.TeeReader(io.LimitReader(pattern{}, size), h)
	out := decodeUpload(t, srv.upload(t, c, d.DraftID, upPart{field: "file", filename: "big.bin", body: body}))
	close(stop)
	<-done

	if len(out.Attachments) != 1 || out.Attachments[0].Size != size {
		t.Errorf("result = %+v", out)
	}
	if growth := int64(peak.Load()) - int64(base); growth >= 8<<20 {
		t.Errorf("heap grew by %d bytes during the upload, want < 8 MB", growth)
	}

	// The stored file has the uploaded bytes.
	_, raw := ownDraftRaw(t, c, d.DraftID)
	if got := draftAttachments(t, raw)["big.bin"]; sha256.Sum256(got) != [32]byte(h.Sum(nil)) {
		t.Errorf("stored file differs (%d bytes)", len(got))
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// pattern yields varied bytes, so lost or reordered bytes change the hash.
type pattern struct{}

func (p pattern) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return len(b), nil
}
