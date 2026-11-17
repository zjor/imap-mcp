package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zjor/imap-mcp/internal/mailparse"
)

func TestSpoolPrepare(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sp := spool{files: []spoolFile{
		{name: "ok.eml", contentType: "message/rfc822", path: write("ok", "Subject: a\r\n\r\nčau\r\n")},
		{name: "lf.eml", contentType: "message/rfc822", path: write("lf", "Subject: a\n\nb\n")},
		{name: "a.pdf", contentType: "application/pdf", path: write("pdf", "x\n")},
	}}
	if err := sp.prepare(); err != nil {
		t.Fatal(err)
	}
	want := []struct{ ct, enc string }{
		{"message/rfc822", "8bit"},
		{"application/octet-stream", ""},
		{"application/pdf", ""},
	}
	for i, w := range want {
		if f := sp.files[i]; f.contentType != w.ct || f.encoding != w.enc {
			t.Errorf("%s: type %q encoding %q, want %q %q", f.name, f.contentType, f.encoding, w.ct, w.enc)
		}
	}
}

func TestPartTypeAndName(t *testing.T) {
	if got := partType(mailparse.Attachment{ContentType: "text/plain", Charset: "windows-1250"}); got != "text/plain; charset=windows-1250" {
		t.Errorf("partType = %q", got)
	}
	if got := partType(mailparse.Attachment{ContentType: "application/pdf"}); got != "application/pdf" {
		t.Errorf("partType = %q", got)
	}
	if spoolName("", "message/rfc822") != "message.eml" || spoolName("", "image/png") != "attachment" || spoolName("a", "message/rfc822") != "a" {
		t.Error("spoolName")
	}
}
