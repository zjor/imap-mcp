package mailparse

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zjor/imap-mcp/internal/ids"
)

func parseFixture(t *testing.T, name string) Parsed {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return p
}

func TestReadEmail_DecodesCzechHeadersAndFilenames(t *testing.T) {
	p := parseFixture(t, "czech.eml")
	if want := "Faktura č. 12 – říjen"; p.Headers.Subject != want {
		t.Errorf("subject = %q, want %q", p.Headers.Subject, want)
	}
	if len(p.Headers.From) != 1 || p.Headers.From[0].Name != "Jiří Novák" || p.Headers.From[0].Email != "jiri@example.cz" {
		t.Errorf("from = %+v", p.Headers.From)
	}
	if len(p.Headers.To) != 2 || p.Headers.To[1].Email != "bob@example.cz" {
		t.Errorf("to = %+v", p.Headers.To)
	}
	if len(p.Headers.Cc) != 1 || p.Headers.Cc[0].Name != "Evěna" {
		t.Errorf("cc = %+v", p.Headers.Cc)
	}
	if p.Headers.MessageID != "abc@example.cz" || p.Headers.InReplyTo != "prev@example.cz" {
		t.Errorf("ids = %q %q", p.Headers.MessageID, p.Headers.InReplyTo)
	}
	if !reflect.DeepEqual(p.Headers.References, []string{"r1@example.cz", "r2@example.cz"}) {
		t.Errorf("references = %v", p.Headers.References)
	}
	if p.Headers.Date.IsZero() || p.Headers.Date.Year() != 2026 {
		t.Errorf("date = %v", p.Headers.Date)
	}
	if !strings.Contains(p.Text, "Dobrý den") {
		t.Errorf("text = %q", p.Text)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	a := p.Attachments[0]
	if a.Filename != "faktura-říjen.pdf" {
		t.Errorf("filename = %q", a.Filename)
	}
	if a.ContentType != "application/pdf" || a.Inline || a.Size != 9 {
		t.Errorf("attachment = %+v", a)
	}
	if !reflect.DeepEqual(a.Part, []int{2}) {
		t.Errorf("part = %v", a.Part)
	}
}

func TestParse_OddShapes(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		check func(t *testing.T, p Parsed)
	}{
		{"html only", "html_only.eml", func(t *testing.T, p Parsed) {
			if !strings.Contains(p.Text, "Hello") || !strings.Contains(p.Text, "world") {
				t.Errorf("text = %q", p.Text)
			}
			if strings.ContainsAny(p.Text, "<>") || strings.Contains(p.Text, "alert") {
				t.Errorf("text has markup or script: %q", p.Text)
			}
			if p.HTML == "" {
				t.Error("HTML should be kept")
			}
			if len(p.Attachments) != 0 {
				t.Errorf("attachments = %+v", p.Attachments)
			}
		}},
		{"message/rfc822", "rfc822.eml", func(t *testing.T, p Parsed) {
			if len(p.Attachments) != 1 || p.Attachments[0].ContentType != "message/rfc822" {
				t.Fatalf("attachments = %+v", p.Attachments)
			}
			if !strings.Contains(p.Text, "See attached") || strings.Contains(p.Text, "inner body") {
				t.Errorf("text = %q", p.Text)
			}
			if !reflect.DeepEqual(p.Attachments[0].Part, []int{2}) {
				t.Errorf("part = %v", p.Attachments[0].Part)
			}
		}},
		{"inline image without filename", "inline_image.eml", func(t *testing.T, p Parsed) {
			if len(p.Attachments) != 1 {
				t.Fatalf("attachments = %+v", p.Attachments)
			}
			a := p.Attachments[0]
			if !a.Inline || a.Filename != "attachment" || a.ContentType != "image/png" {
				t.Errorf("attachment = %+v", a)
			}
			if !reflect.DeepEqual(a.Part, []int{2}) {
				t.Errorf("part = %v", a.Part)
			}
		}},
		{"no body", "no_body.eml", func(t *testing.T, p Parsed) {
			if p.Text != "" || len(p.Attachments) != 0 {
				t.Errorf("parsed = %+v", p)
			}
		}},
		{"simple text", "simple.eml", func(t *testing.T, p Parsed) {
			if strings.TrimSpace(p.Text) != "hello" {
				t.Errorf("text = %q", p.Text)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, parseFixture(t, tc.file)) })
	}
}

func TestParse_PartNumbers(t *testing.T) {
	p := parseFixture(t, "nested.eml")
	if len(p.Attachments) != 2 {
		t.Fatalf("attachments = %+v", p.Attachments)
	}
	if !reflect.DeepEqual(p.Attachments[0].Part, []int{2}) || p.Attachments[0].ContentType != "application/pdf" {
		t.Errorf("pdf = %+v", p.Attachments[0])
	}
	if !reflect.DeepEqual(p.Attachments[1].Part, []int{3}) || p.Attachments[1].ContentType != "message/rfc822" {
		t.Errorf("rfc822 = %+v", p.Attachments[1])
	}
	if strings.TrimSpace(p.Text) != "plain" {
		t.Errorf("text = %q", p.Text)
	}
}

func TestParse_BodyTruncated(t *testing.T) {
	big := strings.Repeat("a", maxBodyBytes+100)
	p, err := Parse(strings.NewReader("From: a@example.com\nContent-Type: text/plain\n\n" + big))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || len(p.Text) > maxBodyBytes {
		t.Errorf("truncated=%v len=%d", p.Truncated, len(p.Text))
	}
}

func TestParse_UnknownCharsetDoesNotFail(t *testing.T) {
	p, err := Parse(strings.NewReader("From: a@example.com\nContent-Type: text/plain; charset=x-bogus\n\nhello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Text, "hello") {
		t.Errorf("text = %q", p.Text)
	}
}

func TestParse_Latin2Charset(t *testing.T) {
	body := "From: a@example.com\nContent-Type: text/plain; charset=iso-8859-2\nContent-Transfer-Encoding: 8bit\n\n\xb1\xe8\n"
	p, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(p.Text) != "ąč" {
		t.Errorf("text = %q", p.Text)
	}
}

func TestCleanFilename(t *testing.T) {
	long := strings.Repeat("š", 150) + ".pdf" // 304 bytes
	tests := []struct{ in, want string }{
		{"../../etc/passwd", "passwd"},
		{`C:\dir\file.txt`, "file.txt"},
		{"a\"b\r\n.pdf", "ab.pdf"},
		{"", "attachment"},
		{"..", "attachment"},
		{" . name . ", "name"},
	}
	for _, tc := range tests {
		if got := CleanFilename(tc.in); got != tc.want {
			t.Errorf("CleanFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got := CleanFilename(long)
	if len(got) > 200 || !utf8.ValidString(got) || got == "attachment" {
		t.Errorf("long name: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestSnippet(t *testing.T) {
	in := "Hi   there\n> quoted line\n  > also quoted\n\nsecond\tline"
	if got, want := Snippet(in, 200), "Hi there second line"; got != want {
		t.Errorf("Snippet = %q, want %q", got, want)
	}
	long := strings.Repeat("ž", 500)
	got := Snippet(long, 200)
	if utf8.RuneCountInString(got) != 200 {
		t.Errorf("runes = %d", utf8.RuneCountInString(got))
	}
}

func TestParse_DeepNestingIsBounded(t *testing.T) {
	const levels = 1000
	var b strings.Builder
	b.WriteString("From: a@example.com\nContent-Type: multipart/mixed; boundary=b0\n\n")
	for i := 0; i < levels; i++ {
		b.WriteString("--b" + strconv.Itoa(i) + "\nContent-Type: multipart/mixed; boundary=b" + strconv.Itoa(i+1) + "\n\n")
	}
	b.WriteString("--b" + strconv.Itoa(levels) + "\nContent-Type: text/plain\n\ndeep\n")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	p, err := Parse(strings.NewReader(b.String()))
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("allocated %d bytes", grew)
	}
	if len(p.Attachments) != 1 || len(p.Attachments[0].Part) != ids.MaxPartDepth {
		t.Errorf("attachments = %+v", p.Attachments)
	}
	checkPartLimits(t, p)
}

// checkPartLimits fails when an attachment has no valid attachment id.
func checkPartLimits(t *testing.T, p Parsed) {
	t.Helper()
	if len(p.Attachments) > MaxAttachments {
		t.Errorf("%d attachments", len(p.Attachments))
	}
	for _, a := range p.Attachments {
		id := ids.EncodeAttachment(ids.AttachmentID{MessageID: ids.MessageID{Folder: "INBOX", UIDValidity: 1, UID: 1}, Part: a.Part})
		if _, err := ids.DecodeAttachment(id); err != nil {
			t.Fatalf("part %v has no valid id: %v", a.Part, err)
		}
	}
}

// manyParts is a message with groups multiparts of n attachments each.
func manyParts(groups, n int) string {
	var b strings.Builder
	b.WriteString("From: a@example.com\nContent-Type: multipart/mixed; boundary=top\n\n")
	for g := 0; g < groups; g++ {
		b.WriteString("--top\nContent-Type: multipart/mixed; boundary=g" + strconv.Itoa(g) + "\n\n")
		for i := 0; i < n; i++ {
			b.WriteString("--g" + strconv.Itoa(g) + "\nContent-Type: application/octet-stream\n\nx\n")
		}
		b.WriteString("--g" + strconv.Itoa(g) + "--\n")
	}
	b.WriteString("--top--\n")
	return b.String()
}

func TestParse_PartNumberLimit(t *testing.T) {
	p, err := Parse(strings.NewReader(manyParts(1, ids.MaxPartNumber+5)))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Attachments) != ids.MaxPartNumber || !p.AttachmentsTruncated {
		t.Errorf("%d attachments, truncated %v", len(p.Attachments), p.AttachmentsTruncated)
	}
	checkPartLimits(t, p)
}

func TestParse_AttachmentCap(t *testing.T) {
	p, err := Parse(strings.NewReader(manyParts(3, 400)))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Attachments) != MaxAttachments || !p.AttachmentsTruncated {
		t.Errorf("%d attachments, truncated %v", len(p.Attachments), p.AttachmentsTruncated)
	}
	checkPartLimits(t, p)

	p, _ = Parse(strings.NewReader(manyParts(2, 3)))
	if len(p.Attachments) != 6 || p.AttachmentsTruncated {
		t.Errorf("%d attachments, truncated %v", len(p.Attachments), p.AttachmentsTruncated)
	}
}

func FuzzParse(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.eml")
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(bytes.NewReader(data))
		if err != nil && strings.Contains(err.Error(), "internal error") {
			t.Fatalf("panic recovered: %v (%+v)", err, p)
		}
		if err != nil && !errors.Is(err, ErrUnparseable) {
			t.Fatalf("err = %v, want ErrUnparseable", err)
		}
		checkPartLimits(t, p)
	})
}

func TestParse_Bcc(t *testing.T) {
	p, err := Parse(strings.NewReader("From: a@example.com\r\nBcc: Eva <eva@example.com>, x@example.com\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Address{{"Eva", "eva@example.com"}, {"", "x@example.com"}}
	if !reflect.DeepEqual(p.Headers.Bcc, want) {
		t.Errorf("bcc = %+v", p.Headers.Bcc)
	}
}

func TestParse_MalformedHeaderIsUnparseable(t *testing.T) {
	raw := "From: a@example.com\r\nSubject: Secret quarterly\r\nnumbers inside\r\n\r\nbody\r\n"
	p, err := Parse(strings.NewReader(raw))
	if !errors.Is(err, ErrUnparseable) {
		t.Fatalf("err = %v, want ErrUnparseable", err)
	}
	if strings.Contains(err.Error(), "numbers inside") || strings.Contains(err.Error(), "Secret") {
		t.Errorf("error text holds input: %q", err)
	}
	if !reflect.DeepEqual(p, Parsed{}) {
		t.Errorf("parsed = %+v, want empty", p)
	}
}

func TestParse_AttachmentCharset(t *testing.T) {
	body := "From: a@example.com\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--b\r\nContent-Type: text/plain; charset=iso-8859-2\r\nContent-Disposition: attachment; filename=a.txt\r\n\r\n\xb1\r\n" +
		"--b\r\nContent-Type: application/pdf; charset=utf-8\r\nContent-Disposition: attachment; filename=a.pdf\r\n\r\nx\r\n--b--\r\n"
	p, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Attachments) != 2 || p.Attachments[0].Charset != "iso-8859-2" || p.Attachments[1].Charset != "" {
		t.Errorf("attachments = %+v", p.Attachments)
	}
}
