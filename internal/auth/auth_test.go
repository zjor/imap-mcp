package auth

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/logging"
)

const (
	token    = "tok-secret-1"
	password = "hunter2-very-secret"
)

func oneHost() config.Config {
	return config.Config{
		IMAPHosts:    []string{"imap.example.com:993"},
		AccessTokens: []string{"other-token", token},
	}
}

func twoHosts() config.Config {
	c := oneHost()
	c.IMAPHosts = []string{"imap.example.com:993", "mail.example.org:143"}
	return c
}

func request(h map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-IMAP-Username", "alice@example.com")
	r.Header.Set("X-IMAP-Password", password)
	for k, v := range h {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	return r
}

type result struct {
	status int
	log    string
	called bool
	creds  Credentials
	ok     bool
	inner  string // log written by the next handler
}

func run(t *testing.T, cfg config.Config, r *http.Request) result {
	t.Helper()
	var buf bytes.Buffer
	base := logging.New(&buf, slog.LevelDebug, "text")
	var res result
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.called = true
		res.creds, res.ok = CredentialsFrom(r.Context())
		logging.From(r.Context()).Info("inner")
	})
	rec := httptest.NewRecorder()
	logging.RequestMiddleware(base)(Middleware(cfg)(next)).ServeHTTP(rec, r)
	res.status = rec.Code
	res.log = buf.String()
	for _, line := range strings.Split(res.log, "\n") {
		if strings.Contains(line, "msg=inner") {
			res.inner = line
		}
	}
	if strings.Contains(res.log, password) || strings.Contains(res.log, token) {
		t.Errorf("secret in log: %s", res.log)
	}
	return res
}

func expectReject(t *testing.T, res result, status int) {
	t.Helper()
	if res.status != status {
		t.Fatalf("status = %d, want %d", res.status, status)
	}
	if res.called {
		t.Fatal("next handler was called")
	}
	if !strings.Contains(res.log, "level=WARN") {
		t.Errorf("no warn log: %s", res.log)
	}
}

func TestMiddleware_MissingToken401(t *testing.T) {
	expectReject(t, run(t, oneHost(), request(map[string]string{"Authorization": ""})), 401)
}

func TestMiddleware_WrongToken401(t *testing.T) {
	expectReject(t, run(t, oneHost(), request(map[string]string{"Authorization": "Bearer nope"})), 401)
	expectReject(t, run(t, oneHost(), request(map[string]string{"Authorization": "Basic " + token})), 401)
}

func TestMiddleware_MissingPassword401(t *testing.T) {
	expectReject(t, run(t, oneHost(), request(map[string]string{"X-IMAP-Password": ""})), 401)
	expectReject(t, run(t, oneHost(), request(map[string]string{"X-IMAP-Username": ""})), 401)
}

func TestMiddleware_HostNotAllowed403(t *testing.T) {
	expectReject(t, run(t, twoHosts(), request(map[string]string{"X-IMAP-Host": "evil.example.net:993"})), 403)
}

func TestMiddleware_SingleHostIgnoresHeader(t *testing.T) {
	res := run(t, oneHost(), request(map[string]string{"X-IMAP-Host": "evil.example.net:993"}))
	if res.status != 200 || !res.called {
		t.Fatalf("status = %d called = %v", res.status, res.called)
	}
	if res.creds.Host != "imap.example.com:993" {
		t.Fatalf("host = %q", res.creds.Host)
	}
}

func TestMiddleware_MultiHostRequiresHeader(t *testing.T) {
	expectReject(t, run(t, twoHosts(), request(nil)), 403)

	res := run(t, twoHosts(), request(map[string]string{"x-imap-host": "mail.example.org:143"}))
	if res.status != 200 || res.creds.Host != "mail.example.org:143" {
		t.Fatalf("status = %d host = %q", res.status, res.creds.Host)
	}
}

func TestMiddleware_OK_PutsCredentials(t *testing.T) {
	r := request(map[string]string{"Authorization": "bearer " + token})
	res := run(t, oneHost(), r)
	if res.status != 200 || !res.called || !res.ok {
		t.Fatalf("status = %d called = %v ok = %v", res.status, res.called, res.ok)
	}
	want := Credentials{Username: "alice@example.com", Password: password, Host: "imap.example.com:993"}
	if res.creds != want {
		t.Fatalf("creds = %#v", res.creds)
	}
	for _, s := range []string{"user=alice@example.com", "token_id=" + logging.TokenID(token), "imap_host=imap.example.com:993", "request_id="} {
		if !strings.Contains(res.inner, s) {
			t.Errorf("inner log %q misses %q", res.inner, s)
		}
	}
}

func TestCredentialsFrom_Empty(t *testing.T) {
	if _, ok := CredentialsFrom(httptest.NewRequest("GET", "/", nil).Context()); ok {
		t.Fatal("ok on empty context")
	}
}

func TestCredentials_LogValueHasNoPassword(t *testing.T) {
	c := Credentials{Username: "alice", Password: password, Host: "h:993"}
	outs := []string{
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%+v", c),
		fmt.Sprintf("%#v", c),
		fmt.Sprintf("%s", c),
		fmt.Sprintf("%v", &c),
		fmt.Sprintf("%+v", &c),
		fmt.Sprintf("%#v", &c),
		fmt.Sprint(c),
		c.String(),
		c.GoString(),
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "creds", c, "ptr", &c)
	outs = append(outs, buf.String())
	for _, o := range outs {
		if strings.Contains(o, password) {
			t.Errorf("password in %q", o)
		}
		if !strings.Contains(o, "alice") {
			t.Errorf("username missing in %q", o)
		}
	}
}

func accessLine(t *testing.T, log string) string {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "msg=http.request") {
			return line
		}
	}
	t.Fatalf("no http.request line in %s", log)
	return ""
}

func TestMiddleware_AccessLineHasIdentity(t *testing.T) {
	line := accessLine(t, run(t, oneHost(), request(nil)).log)
	for _, s := range []string{"user=alice@example.com", "token_id=" + logging.TokenID(token), "imap_host=imap.example.com:993", "status=200"} {
		if !strings.Contains(line, s) {
			t.Errorf("access line %q misses %q", line, s)
		}
	}
}

func TestMiddleware_RejectedAccessLineHasNoIdentity(t *testing.T) {
	line := accessLine(t, run(t, oneHost(), request(map[string]string{"X-IMAP-Password": ""})).log)
	if !strings.Contains(line, "status=401") {
		t.Errorf("access line %q misses status=401", line)
	}
	for _, s := range []string{"user=", "imap_host="} {
		if strings.Contains(line, s) {
			t.Errorf("access line %q has %q", line, s)
		}
	}
}
