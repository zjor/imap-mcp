package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
)

func TestFolderOut_UnknownCountsAreNull(t *testing.T) {
	b, _ := json.Marshal(toFolderOut(imapsafe.Folder{Name: "X", Delimiter: "/", Total: 0, Unread: 0}))
	if want := `{"name":"X","delimiter":"/","role":"","total":null,"unread":null}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
	b, _ = json.Marshal(toFolderOut(imapsafe.Folder{Name: "INBOX", Delimiter: "/", Role: "inbox", Total: 3, Unread: 0, CountsKnown: true}))
	if want := `{"name":"INBOX","delimiter":"/","role":"inbox","total":3,"unread":0}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestSnippet_Decodes(t *testing.T) {
	long := strings.Repeat("slovo ", 400)
	cases := []struct {
		name string
		sum  imapsafe.Summary
		want string
	}{
		{"quoted-printable", imapsafe.Summary{ContentType: "text/plain; charset=utf-8", Encoding: "quoted-printable",
			Snippet: []byte("Dobr=C3=BD den,\r\n> quoted\r\nsvet=\r\ne")}, "Dobrý den, svete"},
		{"base64 cut mid-quantum", imapsafe.Summary{ContentType: "text/plain; charset=utf-8", Encoding: "base64",
			Snippet: []byte("SGVsbG8gd29ybGQgYW5kIG1vcmU")}, "Hello world and mo"},
		{"latin-2 charset", imapsafe.Summary{ContentType: "text/plain; charset=iso-8859-2", Encoding: "quoted-printable",
			Snippet: []byte("=AElu=BBou=E8k=FD k=F9=F2")}, "Žluťoučký kůň"},
		{"multipart cut inside the text part", imapsafe.Summary{ContentType: `multipart/alternative; boundary="b"`,
			Snippet: []byte("preamble\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nAhoj sv=C4=9Bte, this is cut he")},
			"Ahoj světe, this is cut he"},
		{"html only", imapsafe.Summary{ContentType: "text/html", Snippet: []byte("<p>Hi <b>there</b></p>")}, "Hi there"},
		{"cut at 200 runes", imapsafe.Summary{Snippet: []byte(long)}, strings.TrimSpace(long[:200])},
		{"no BODYSTRUCTURE", imapsafe.Summary{Snippet: []byte("plain text")}, "plain text"},
		{"empty", imapsafe.Summary{}, ""},
	}
	for _, tc := range cases {
		if got := snippet(tc.sum); got != tc.want {
			t.Errorf("%s: snippet = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSnippet_GarbageNeverFails(t *testing.T) {
	for _, s := range []imapsafe.Summary{
		{ContentType: "multipart/mixed", Snippet: []byte("--\r\n--\r\n\x00\xff")},
		{ContentType: "text/plain; charset=nope", Encoding: "base64", Snippet: []byte("!!!!")},
		{ContentType: `multipart/mixed; boundary="x"`, Snippet: []byte("--x\r\nContent-Type: multipart/alternative; boundary=\"y\"\r\n\r\n--y\r\n")},
	} {
		_ = snippet(s) // must not panic
	}
}

func TestStripQuoted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Thanks.\n\nOn Mon, 11 Nov 2026 Carol wrote:\n> Note.\n> more\n", "Thanks."},
		{"Top\r\n> quoted\r\nBottom", "Top\nBottom"},
		{"Reply\n\n-----Original Message-----\nFrom: x\nold text", "Reply"},
		{"I said: On Monday he wrote:\nand then more text", "I said: On Monday he wrote:\nand then more text"},
		{"On Mon Bob wrote:\nnot quoted reply below\n> q", "On Mon Bob wrote:\nnot quoted reply below"},
	}
	for _, tc := range cases {
		if got := stripQuoted(tc.in); got != tc.want {
			t.Errorf("stripQuoted(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCursor_RoundTripAndBad(t *testing.T) {
	c := cursor{Folders: []cursorFolder{{Name: "Odeslaná pošta", UIDValidity: 7, Before: 42}}}
	got, err := decodeCursor(c.encode())
	if err != nil || len(got.Folders) != 1 || got.Folders[0] != c.Folders[0] {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	for _, s := range []string{"", "!!", "e30", cursor{Folders: []cursorFolder{{Name: "X"}}}.encode()} {
		if _, err := decodeCursor(s); err == nil {
			t.Errorf("decodeCursor(%q) accepted", s)
		}
	}
}

func TestClassify_NeverLeaksServerText(t *testing.T) {
	serverText := "NO [SERVERBUG] secret internal detail"
	cases := []struct {
		err  error
		code string
	}{
		{fmt.Errorf("x: %w", ids.ErrBadID), codeBadID},
		{imapsafe.ErrAuth, codeAuthFailed},
		{imapsafe.ErrStale, codeStaleID},
		{fmt.Errorf("x: %w", imapsafe.ErrNotFound), codeNotFound},
		{imapsafe.ErrUnsupported, codeUnsupported},
		{fmt.Errorf("imap examine: %w", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: serverText}), codeNotFound},
		{fmt.Errorf("imap fetch: %w", &imap.Error{Type: imap.StatusResponseTypeNo, Text: serverText}), codeIMAPError},
		{errors.New(serverText), codeIMAPError},
		{badArg("limit is bad"), codeBadArgument},
	}
	for _, tc := range cases {
		code, msg := classify(tc.err)
		if code != tc.code || msg == "" || strings.Contains(msg, "secret") {
			t.Errorf("classify(%v) = %q, %q; want code %q without server text", tc.err, code, msg, tc.code)
		}
	}
}

func TestErrorResult_Shape(t *testing.T) {
	res := errorResult(context.Background(), codeStaleID, "message id is stale, search again")
	if !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	var body map[string]map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &body); err != nil {
		t.Fatal(err)
	}
	e := body["error"]
	if e["code"] != "stale_id" || e["message"] == "" {
		t.Errorf("error = %v", e)
	}
	if _, ok := e["request_id"]; !ok {
		t.Error("request_id missing")
	}
}

func TestClampLimit(t *testing.T) {
	for in, want := range map[int]int{0: 20, -5: 20, 1: 1, 100: 100, 101: 100} {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d) = %d, want %d", in, got, want)
		}
	}
}
