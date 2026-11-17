//go:build integration

package integration

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/server"
)

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// nonRequestEvents are the log events written outside any HTTP request.
// They are the only lines allowed without request_id.
var nonRequestEvents = map[string]bool{"server.start": true, "server.stop": true}

// TestLogs_NoSecrets runs every tool, a download, an upload and the main
// rejections with debug logs, then checks that no secret or mail content is
// in the log and that every request line has request_id (spec 7.8).
func TestLogs_NoSecrets(t *testing.T) {
	c := freshUser(t)

	// Secrets and content that must never be logged.
	marker := make([]byte, 32) // raw bytes inside the seeded attachment
	if _, err := rand.Read(marker); err != nil {
		t.Fatal(err)
	}
	uploadMarker := make([]byte, 32) // raw bytes inside the uploaded file
	if _, err := rand.Read(uploadMarker); err != nil {
		t.Fatal(err)
	}
	seededSubject := "Quarterly figures " + randHex(t, 8)
	seededBody := "Seeded body line " + randHex(t, 8)
	replySubject := "Re: " + seededSubject
	replyBody := "Reply body line " + randHex(t, 8)
	draftSubject := "Draft subject " + randHex(t, 8)
	draftBody := "Draft body line " + randHex(t, 8)
	updatedSubject := "Updated subject " + randHex(t, 8)
	wrongPassword := "wrong-pass-" + randHex(t, 8)
	wrongToken := "wrong-token-" + randHex(t, 8)

	attachment := append(append([]byte("head-"), marker...), []byte("-tail")...)
	b64 := base64.StdEncoding.EncodeToString(attachment)
	seedRaw(t, c, "INBOX",
		[]byte("From: Alice <alice@example.com>\r\nTo: Bob <bob@example.com>\r\n"+
			"Subject: "+seededSubject+"\r\nDate: Mon, 16 Nov 2026 12:00:00 +0000\r\n"+
			"Message-ID: <leak-1@example.com>\r\nMIME-Version: 1.0\r\n"+
			"Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n"+
			"--b1\r\nContent-Type: text/plain\r\n\r\n"+seededBody+"\r\n"+
			"--b1\r\nContent-Type: application/octet-stream\r\n"+
			"Content-Disposition: attachment; filename=data.bin\r\nContent-Transfer-Encoding: base64\r\n\r\n"+
			b64+"\r\n--b1--\r\n"),
		[]byte("From: Bob <bob@example.com>\r\nTo: Alice <alice@example.com>\r\n"+
			"Subject: "+replySubject+"\r\nDate: Mon, 16 Nov 2026 13:00:00 +0000\r\n"+
			"Message-ID: <leak-2@example.com>\r\nIn-Reply-To: <leak-1@example.com>\r\n"+
			"References: <leak-1@example.com>\r\n\r\n"+replyBody+"\r\n"))

	logs := &syncBuffer{}
	cfg := config.Config{
		Listen: "127.0.0.1:0", IMAPHosts: []string{imapHost()}, AccessTokens: []string{testToken},
		PublicURL: testPublicURL, MaxSessionsPerUser: 4, MaxUpload: 64 << 10,
	}
	ts := httptest.NewServer(server.NewWithDialer(cfg, logging.New(logs, slog.LevelDebug, "json"), testDialer(t)))
	t.Cleanup(func() {
		ts.Close()
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	srv := &mcpServer{URL: ts.URL, logs: logs}
	cs := srv.connect(t, c)

	// Every tool.
	mustCall(t, cs, "list_folders", nil, nil)
	var s searchOut
	mustCall(t, cs, "search_emails", map[string]any{"from": "alice@example.com"}, &s)
	if len(s.Messages) != 1 {
		t.Fatalf("search = %+v", s)
	}
	var e emailOut
	mustCall(t, cs, "read_email", map[string]any{"id": s.Messages[0].ID}, &e)
	if len(e.Attachments) != 1 {
		t.Fatalf("attachments = %+v", e.Attachments)
	}
	mustCall(t, cs, "read_email", map[string]any{"id": s.Messages[0].ID, "format": "raw_headers"}, nil)
	mustCall(t, cs, "get_thread", map[string]any{"id": s.Messages[0].ID}, nil)
	mustCall(t, cs, "search_contacts", map[string]any{"query": "example.com"}, nil)
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{
		"mode": "new", "to": []string{"Carol <carol@example.com>"}, "subject": draftSubject, "body_text": draftBody,
	}, &d)
	mustCall(t, cs, "create_draft", map[string]any{"mode": "reply", "in_reply_to": s.Messages[0].ID, "body_text": draftBody}, nil)
	mustCall(t, cs, "update_draft", map[string]any{"draft_id": d.DraftID, "subject": updatedSubject}, nil)
	mustCall(t, cs, "list_drafts", nil, nil)

	// A download.
	resp := srv.download(t, "GET", e.Attachments[0].ID, authHeaders(c))
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(got, attachment) {
		t.Fatalf("download: status %d, %d bytes", resp.StatusCode, len(got))
	}

	// An upload, then a 413.
	decodeUpload(t, srv.upload(t, c, d.DraftID, upPart{field: "file", filename: "up.bin",
		body: bytes.NewReader(append([]byte("x"), uploadMarker...))}))
	errorOf(t, srv.upload(t, c, d.DraftID, upPart{field: "file", filename: "big.bin",
		body: io.LimitReader(zeros{}, 128<<10)}), 413, "too_large")

	mustCall(t, cs, "delete_draft", map[string]any{"draft_id": d.DraftID}, nil)

	// A wrong IMAP password and a wrong access token, on both endpoints.
	badPass := authHeaders(c)
	badPass.Set("X-IMAP-Password", wrongPassword)
	if r := srv.download(t, "GET", e.Attachments[0].ID, badPass); r.StatusCode != 401 {
		t.Errorf("wrong password: status %d, want 401", r.StatusCode)
	}
	badToken := authHeaders(c)
	badToken.Set("Authorization", "Bearer "+wrongToken)
	if r := srv.download(t, "GET", e.Attachments[0].ID, badToken); r.StatusCode != 401 {
		t.Errorf("wrong token: status %d, want 401", r.StatusCode)
	}
	for _, h := range []http.Header{badPass, badToken} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_folders","arguments":{}}}`))
		req.Header = h.Clone()
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}

	out := logs.String()
	// Sanity: the run really logged at debug level and saw the rejections.
	for _, want := range []string{`"level":"DEBUG"`, `"msg":"attachment.downloaded"`, `"msg":"draft.attachments_added"`,
		`"msg":"upload.rejected"`, `"msg":"auth.rejected"`, `"msg":"draft.deleted"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s", want)
		}
	}

	forbidden := map[string]string{
		"IMAP password":        testPassword,
		"wrong IMAP password":  wrongPassword,
		"access token":         testToken,
		"wrong access token":   wrongToken,
		"seeded subject":       seededSubject,
		"seeded body":          seededBody,
		"reply subject":        replySubject,
		"reply body":           replyBody,
		"draft subject":        draftSubject,
		"draft body":           draftBody,
		"updated subject":      updatedSubject,
		"attachment (base64)":  b64,
		"attachment (hex)":     hex.EncodeToString(marker),
		"attachment (raw)":     string(marker),
		"upload (hex)":         hex.EncodeToString(uploadMarker),
		"upload (raw)":         string(uploadMarker),
		"attachment (escaped)": strings.Trim(jsonString(t, string(marker)), `"`),
		"upload (escaped)":     strings.Trim(jsonString(t, string(uploadMarker)), `"`),
	}
	for what, secret := range forbidden {
		if strings.Contains(out, secret) {
			t.Errorf("log contains the %s", what)
		}
	}

	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("not a JSON log line: %q", line)
			continue
		}
		lines++
		if msg, _ := rec["msg"].(string); nonRequestEvents[msg] {
			continue
		}
		if id, _ := rec["request_id"].(string); id == "" {
			t.Errorf("line without request_id: %s", line)
		}
	}
	if lines < 20 {
		t.Errorf("only %d log lines; the run did not log as expected", lines)
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
