package logging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var buf bytes.Buffer
	base := New(&buf, slog.LevelInfo, "text")
	rec := httptest.NewRecorder()
	RequestMiddleware(base)(h).ServeHTTP(rec, req)
	return rec, buf.String()
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) })

func TestRequestMiddleware_UsesIncomingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-Id", "abc-1")
	rec, out := serve(t, ok, req)
	if got := rec.Header().Get("X-Request-Id"); got != "abc-1" {
		t.Fatalf("header = %q", got)
	}
	if !strings.Contains(out, "request_id=abc-1") {
		t.Fatalf("log = %s", out)
	}
}

func TestRequestMiddleware_RejectsBadIncomingID(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-Id", "<script>")
	rec, out := serve(t, ok, req)
	got := rec.Header().Get("X-Request-Id")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(got) {
		t.Fatalf("id = %q", got)
	}
	if strings.Contains(out, "script") || !strings.Contains(out, "request_id="+got) {
		t.Fatalf("log = %s", out)
	}
}

func TestRequestMiddleware_TraceID(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	_, out := serve(t, ok, req)
	if !strings.Contains(out, "trace_id=4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("log = %s", out)
	}
}

func TestRequestMiddleware_InvalidTraceparentIgnored(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("traceparent", "garbage")
	_, out := serve(t, ok, req)
	if strings.Contains(out, "trace_id") {
		t.Fatalf("log = %s", out)
	}
}

func TestRequestMiddleware_PathHasNoQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/mcp?token=hunter2&a=b", nil)
	_, out := serve(t, ok, req)
	if !strings.Contains(out, "path=/mcp") || strings.Contains(out, "hunter2") || strings.Contains(out, "?") {
		t.Fatalf("log = %s", out)
	}
}

func TestRequestMiddleware_LogsFields(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(201)
		w.Write([]byte("hello"))
	})
	req := httptest.NewRequest("POST", "/x", strings.NewReader("12345678"))
	_, out := serve(t, h, req)
	for _, want := range []string{"msg=http.request", "method=POST", "status=201", "bytes_in=8", "bytes_out=5", "duration_ms="} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestRequestMiddleware_DefaultStatus200AndFlusher(t *testing.T) {
	var flushed bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, isF := w.(http.Flusher)
		if !isF {
			t.Error("writer is not a Flusher")
			return
		}
		w.Write([]byte("a"))
		f.Flush()
		flushed = true
	})
	rec, out := serve(t, h, httptest.NewRequest("GET", "/x", nil))
	if !flushed || !rec.Flushed || !strings.Contains(out, "status=200") {
		t.Fatalf("flushed=%v rec.Flushed=%v log=%s", flushed, rec.Flushed, out)
	}
}

func TestContextLogger(t *testing.T) {
	var buf bytes.Buffer
	var gotID string
	var fromCtx *slog.Logger
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = RequestID(r.Context())
		fromCtx = From(r.Context())
		fromCtx.Info("inner")
	})
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Request-Id", "rid")
	RequestMiddleware(New(&buf, slog.LevelInfo, "text"))(h).ServeHTTP(httptest.NewRecorder(), req)
	if gotID != "rid" || !strings.Contains(buf.String(), "msg=inner request_id=rid") {
		t.Fatalf("id=%q log=%s", gotID, buf.String())
	}
	if RequestID(context.Background()) != "" {
		t.Fatal("expected empty id")
	}
}

func TestFrom_DefaultsWhenNone(t *testing.T) {
	if From(context.Background()) == nil {
		t.Fatal("nil logger")
	}
	l := New(io.Discard, slog.LevelInfo, "json")
	if From(With(context.Background(), l)) != l {
		t.Fatal("With/From mismatch")
	}
}

func TestNew_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "json").Info("hi")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Fatalf("out = %s", buf.String())
	}
}

func TestTokenID(t *testing.T) {
	sum := sha256.Sum256([]byte("secret"))
	want := hex.EncodeToString(sum[:])[:8]
	got := TokenID("secret")
	if got != want || strings.Contains(got, "secret") {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRequestMiddleware_LogsAbortedRequestAndRepanics(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		panic(http.ErrAbortHandler)
	})
	var buf bytes.Buffer
	base := New(&buf, slog.LevelInfo, "text")
	rec := httptest.NewRecorder()
	var got any
	func() {
		defer func() { got = recover() }()
		RequestMiddleware(base)(h).ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	}()
	if got != http.ErrAbortHandler {
		t.Fatalf("panic = %v, want http.ErrAbortHandler", got)
	}
	out := buf.String()
	for _, want := range []string{"msg=http.request", "aborted=true", "bytes_out=7"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}
