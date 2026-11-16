package mailparse

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
