package drafts

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zjor/imap-mcp/internal/mailparse"
)

var fixedDate = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

func build(t *testing.T, in Input, atts []Part) string {
	t.Helper()
	in.Date, in.MessageID = fixedDate, "m1@example.com"
	var b bytes.Buffer
	if err := Build(&b, in, atts); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func base() Input {
	return Input{Mode: "new", From: "me@example.com", To: []string{"Ann <ann@example.com>"},
		Bcc: []string{"hidden@example.com"}, Subject: "Příliš žluťoučký", BodyText: "Ahoj světe\n", DraftID: "d-1"}
}

func att(name string, data []byte) Part {
	return Part{Filename: name, ContentType: "application/octet-stream",
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}

func TestBuild_NewPlainText(t *testing.T) {
	raw := build(t, base(), nil)
	if !strings.Contains(raw, "X-Imap-Mcp-Draft-Id: d-1") && !strings.Contains(raw, MarkerHeader+": d-1") {
		t.Errorf("no marker:\n%s", raw)
	}
	if !strings.Contains(raw, "=?utf-8?") && !strings.Contains(raw, "=?UTF-8?") {
		t.Errorf("subject not RFC 2047:\n%s", raw)
	}
	if strings.Contains(raw, "multipart") || !strings.Contains(raw, "text/plain") {
		t.Errorf("want only text/plain:\n%s", raw)
	}
	if !strings.Contains(raw, "Bcc:") {
		t.Error("Bcc dropped")
	}
	p, err := mailparse.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.Headers.Subject != "Příliš žluťoučký" || strings.TrimSpace(p.Text) != "Ahoj světe" {
		t.Errorf("got %q %q", p.Headers.Subject, p.Text)
	}
}

func TestBuild_TextAndHTML(t *testing.T) {
	in := base()
	in.BodyHTML = "<p>Ahoj</p>"
	raw := build(t, in, nil)
	if !strings.Contains(raw, "multipart/alternative") {
		t.Fatalf("not alternative:\n%s", raw)
	}
	p, _ := mailparse.Parse(strings.NewReader(raw))
	if !strings.Contains(p.HTML, "<p>Ahoj</p>") || !strings.Contains(p.Text, "Ahoj") {
		t.Errorf("got %q %q", p.Text, p.HTML)
	}
}

func TestBuild_WithAttachments(t *testing.T) {
	in := base()
	in.BodyHTML = "<p>x</p>"
	data := bytes.Repeat([]byte{0, 1, 2, 250, 251}, 5000)
	raw := build(t, in, []Part{att("příloha.bin", data)})
	if !strings.Contains(raw, "multipart/mixed") || !strings.Contains(raw, "multipart/alternative") {
		t.Fatalf("structure:\n%s", raw)
	}
	if !strings.Contains(raw, "filename*") {
		t.Errorf("filename not RFC 2231:\n%s", raw)
	}
	p, _ := mailparse.Parse(strings.NewReader(raw))
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "příloha.bin" || p.Attachments[0].Size != int64(len(data)) {
		t.Errorf("attachments: %+v", p.Attachments)
	}
}

func TestBuild_RejectsInjection(t *testing.T) {
	for name, mod := range map[string]func(*Input){
		"subject": func(i *Input) { i.Subject = "a\r\nBcc: x@y.z" },
		"to":      func(i *Input) { i.To = []string{"a@b.c\r\nBcc: x@y.z"} },
		"from":    func(i *Input) { i.From = "a@b.c\nX: y" },
		"badaddr": func(i *Input) { i.Cc = []string{"not an address"} },
	} {
		in := base()
		mod(&in)
		if err := Build(io.Discard, in, nil); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestReplyHeaders_Prefix(t *testing.T) {
	for in, want := range map[string]string{
		"x": "Re: x", "RE: x": "RE: x", "Re: Re: x": "Re: Re: x", "re: x": "re: x",
		"Fwd: x": "Fwd: x", "FWD: x": "FWD: x", "Fw: x": "Fw: x",
	} {
		_, _, got, _, _ := ReplyHeaders(mailparse.Headers{Subject: in}, false, "me@example.com")
		if got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestForwardSubject(t *testing.T) {
	for in, want := range map[string]string{
		"x": "Fwd: x", " x ": "Fwd: x", "Fwd: x": "Fwd: x", "FWD: x": "FWD: x", "Fw: x": "Fw: x",
		"Re: x": "Fwd: Re: x", "": "Fwd: ",
	} {
		if got := ForwardSubject(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestReplyHeaders_ReplyAll(t *testing.T) {
	h := mailparse.Headers{
		From:    []mailparse.Address{{Email: "ann@example.com"}},
		ReplyTo: []mailparse.Address{{Name: "Ann R", Email: "reply@example.com"}},
		To:      []mailparse.Address{{Email: "ME@example.com"}, {Email: "bob@example.com"}},
		Cc:      []mailparse.Address{{Email: "Bob@example.com"}, {Email: "carl@example.com"}, {Email: "reply@example.com"}},
	}
	to, cc, _, _, _ := ReplyHeaders(h, true, "me@example.com")
	if len(to) != 1 || !strings.Contains(to[0], "reply@example.com") {
		t.Errorf("to=%v", to)
	}
	if len(cc) != 2 || !strings.Contains(cc[0], "bob@example.com") || !strings.Contains(cc[1], "carl@example.com") {
		t.Errorf("cc=%v", cc)
	}
	h.ReplyTo = nil
	to, cc, _, _, _ = ReplyHeaders(h, false, "me@example.com")
	if len(to) != 1 || to[0] != "<ann@example.com>" || len(cc) != 0 {
		t.Errorf("to=%v cc=%v", to, cc)
	}
}

func TestReplyHeaders_References(t *testing.T) {
	h := mailparse.Headers{MessageID: "c@x", References: []string{"a@x", "b@x"}}
	_, _, _, irt, refs := ReplyHeaders(h, false, "me@example.com")
	if irt != "c@x" || strings.Join(refs, ",") != "a@x,b@x,c@x" {
		t.Errorf("irt=%q refs=%v", irt, refs)
	}
}

func TestBuild_ReplySetsThreading(t *testing.T) {
	in := base()
	in.Mode = "reply"
	in.Original = &mailparse.Parsed{Headers: mailparse.Headers{MessageID: "c@x", References: []string{"a@x"}}}
	raw := build(t, in, nil)
	if !strings.Contains(raw, "In-Reply-To: <c@x>") || !strings.Contains(raw, "References: <a@x> <c@x>") {
		t.Errorf("threading headers missing:\n%s", raw)
	}
}

func TestSize_MatchesBuild(t *testing.T) {
	in := base()
	in.BodyHTML = "<p>x</p>"
	in.Date, in.MessageID = fixedDate, "m1@example.com"
	atts := []Part{att("a.bin", bytes.Repeat([]byte("abc"), 10000)), att("příloha.txt", []byte("hi"))}
	var b bytes.Buffer
	if err := Build(&b, in, atts); err != nil {
		t.Fatal(err)
	}
	n, err := Size(in, atts)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(b.Len()) {
		t.Errorf("Size=%d len=%d", n, b.Len())
	}
}

func TestBuild_RequiredFields(t *testing.T) {
	in := base()
	in.MessageID = "m@x"
	if err := Build(io.Discard, in, nil); err == nil {
		t.Error("zero Date: want error")
	}
	if _, err := Size(in, nil); err == nil {
		t.Error("Size zero Date: want error")
	}
	in.Date = fixedDate
	for _, id := range []string{"", "<m@x>", "m @x", "m@x\r\nBcc: a@b.c"} {
		in.MessageID = id
		if err := Build(io.Discard, in, nil); err == nil {
			t.Errorf("message id %q: want error", id)
		}
	}
}

func TestBuild_AttachmentRejects(t *testing.T) {
	in := base()
	in.Date, in.MessageID = fixedDate, "m@x"
	for name, p := range map[string]Part{
		"badtype":   {Filename: "a", ContentType: "foo"},
		"spacetype": {Filename: "a", ContentType: "text/pl ain"},
		"crlftype":  {Filename: "a", ContentType: "text/plain\r\nBcc: a@b.c"},
		"crlfname":  {Filename: "a\r\nBcc: a@b.c", ContentType: "text/plain"},
	} {
		p.Open = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("x")), nil }
		if err := Build(io.Discard, in, []Part{p}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	p := Part{Filename: "a.txt", ContentType: "text/plain; charset=utf-8",
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("x")), nil }}
	if err := Build(io.Discard, in, []Part{p}); err != nil {
		t.Errorf("params should be allowed: %v", err)
	}
}

func TestNewMessageID(t *testing.T) {
	a, b := NewMessageID("Me <me@example.com>"), NewMessageID("me@example.com")
	if !strings.HasSuffix(a, "@example.com") || a == b {
		t.Errorf("%q %q", a, b)
	}
	if got := NewMessageID(""); !strings.HasSuffix(got, "@localhost") {
		t.Errorf("fallback: %q", got)
	}
}

func TestMessageEncoding(t *testing.T) {
	long := strings.Repeat("a", 998)
	for _, tc := range []struct{ in, want string }{
		{"Subject: x\r\n\r\nhi\r\n", "7bit"},
		{"Subject: x\r\n\r\nhi", "7bit"},
		{"Subject: x\r\n\r\nčau\r\n", "8bit"},
		{long + "\r\n", "7bit"},
		{long + "a\r\n", ""},
		{"Subject: x\n\nhi\n", ""},
		{"a\rb\r\n", ""},
		{"a\r", ""},
		{"a\x00b\r\n", ""},
		{"", "7bit"},
	} {
		got, err := MessageEncoding(strings.NewReader(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("MessageEncoding(%.20q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestBuild_MessagePartKeepsEncoding(t *testing.T) {
	inner := "From: x@example.com\r\nSubject: inner\r\n\r\nčau\r\n"
	p := att("fwd.eml", []byte(inner))
	p.ContentType, p.Encoding = "message/rfc822", "8bit"
	raw := build(t, base(), []Part{p})
	if !strings.Contains(raw, "Content-Transfer-Encoding: 8bit") || !strings.Contains(raw, inner) {
		t.Fatalf("message part not kept as is:\n%s", raw)
	}

	bad := att("x.bin", []byte("x"))
	bad.Encoding = "8bit"
	in := base()
	in.Date, in.MessageID = fixedDate, "m1@example.com"
	if err := Build(io.Discard, in, []Part{bad}); err == nil {
		t.Error("8bit accepted for a non-message part")
	}
}

func TestBuild_TextAttachmentKeepsCharset(t *testing.T) {
	p := att("a.txt", []byte("\xb1\xe8"))
	p.ContentType = "text/plain; charset=iso-8859-2"
	raw := build(t, base(), []Part{p})
	if !strings.Contains(raw, `charset=iso-8859-2`) {
		t.Fatalf("charset lost:\n%s", raw)
	}
}

func TestBuild_Base64LinesWrapped(t *testing.T) {
	in := base()
	in.BodyHTML = "<p>x</p>"
	raw := build(t, in, []Part{att("a.bin", bytes.Repeat([]byte{0xff}, 1000))})
	for _, l := range strings.Split(raw, "\r\n") {
		if len(l) > 76 && !strings.Contains(l, ":") {
			t.Fatalf("line of %d bytes: %q", len(l), l)
		}
	}
	if strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") {
		t.Error("bare LF in the message")
	}
	if strings.Count(strings.ToLower(raw), "mime-version") != 1 {
		t.Errorf("MIME-Version count:\n%s", raw)
	}
}
