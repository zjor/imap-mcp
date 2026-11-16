package files

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"

	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/tools"
)

func TestPartType(t *testing.T) {
	tests := []struct{ header, name, want string }{
		{"application/pdf; name=x", "a.bin", "application/pdf"},
		{"", "notes.txt", "text/plain; charset=utf-8"},
		{"not a type", "a.pdf", "application/pdf"},
		{"text/", "a.zzz", "application/octet-stream"},
		{"", "noext", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := partType(tt.header, tt.name); got != tt.want {
			t.Errorf("partType(%q, %q) = %q, want %q", tt.header, tt.name, got, tt.want)
		}
	}
}

// multipartReq builds a POST with one part per (field, filename, body).
func multipartReq(t *testing.T, max int64, parts ...[3]string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p[0]+`"; filename="`+p[1]+`"`)
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(p[2]))
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/drafts/x/attachments", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, max)
	return r
}

func removeAll(files []tools.UploadFile) {
	for _, f := range files {
		os.Remove(f.Path)
	}
}

func TestReceive(t *testing.T) {
	var files []tools.UploadFile
	err := receive(multipartReq(t, 1<<20, [3]string{"file", "../a.txt", "one"}, [3]string{"file", "b", "three"}), &files)
	defer removeAll(files)
	if err != nil || len(files) != 2 {
		t.Fatalf("files %+v, err %v", files, err)
	}
	if files[0].Filename != "a.txt" || files[0].Size != 3 || files[1].Size != 5 || files[1].ContentType != "application/octet-stream" {
		t.Errorf("files = %+v", files)
	}
	if b, _ := os.ReadFile(files[1].Path); string(b) != "three" {
		t.Errorf("temp file = %q", b)
	}

	bad := map[string]*http.Request{
		"wrong field": multipartReq(t, 1<<20, [3]string{"other", "a", "x"}),
		"no files":    multipartReq(t, 1<<20),
	}
	plain := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	plain.Header.Set("Content-Type", "text/plain")
	bad["not multipart"] = plain
	for name, r := range bad {
		var files []tools.UploadFile
		err := receive(r, &files)
		removeAll(files)
		var ae argError
		if !errors.As(err, &ae) {
			t.Errorf("%s: err = %v, want argError", name, err)
		}
	}

	var big []tools.UploadFile
	err = receive(multipartReq(t, 400, [3]string{"file", "a", strings.Repeat("x", 1000)}), &big)
	removeAll(big)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) || len(big) != 1 {
		t.Errorf("too large: err = %v, %d files, want MaxBytesError with the temp file listed", err, len(big))
	}
}

func TestFail_Status(t *testing.T) {
	for err, want := range map[error]int{
		imapsafe.ErrNotEditable: http.StatusForbidden,
		imapsafe.ErrTooLarge:    http.StatusRequestEntityTooLarge,
		badArg("x"):             http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		fail(context.Background(), rec, "test.failed", err)
		if rec.Code != want {
			t.Errorf("%v: status %d, want %d", err, rec.Code, want)
		}
	}
}
