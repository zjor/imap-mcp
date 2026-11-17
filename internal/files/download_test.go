package files

import "testing"

func TestContentDisposition(t *testing.T) {
	tests := []struct{ name, want string }{
		{"Faktura č.pdf", "attachment; filename*=UTF-8''Faktura%20%C4%8D.pdf"},
		{"a\u202egpj.exe", "attachment; filename*=UTF-8''agpj.exe"},
		{"../x';y\".txt", "attachment; filename*=UTF-8''x%27%3By.txt"},
		{"\u200b", "attachment; filename*=UTF-8''attachment"},
	}
	for _, tt := range tests {
		if got := contentDisposition(tt.name); got != tt.want {
			t.Errorf("contentDisposition(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestOriginalType(t *testing.T) {
	for in, want := range map[string]string{
		"text/html":                  "text/html",
		"text/html; charset=utf-8":   "text/html",
		"application/x-evil\r\nX: y": "",
		"":                           "",
	} {
		if got := originalType(in); got != want {
			t.Errorf("originalType(%q) = %q, want %q", in, got, want)
		}
	}
}
