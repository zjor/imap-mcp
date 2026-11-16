package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{
		"IMAP_MCP_IMAP_HOSTS":    "imap.example.com:993",
		"IMAP_MCP_ACCESS_TOKENS": "tok-secret-1,tok-secret-2",
	}
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8787" {
		t.Errorf("Listen=%q", c.Listen)
	}
	if c.MaxUpload != 25<<20 {
		t.Errorf("MaxUpload=%d", c.MaxUpload)
	}
	if c.MaxSessionsPerUser != 4 {
		t.Errorf("MaxSessionsPerUser=%d", c.MaxSessionsPerUser)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel=%v", c.LogLevel)
	}
	if c.LogFormat != "json" {
		t.Errorf("LogFormat=%q", c.LogFormat)
	}
	if c.PublicURL != "http://127.0.0.1:8787" {
		t.Errorf("PublicURL=%q", c.PublicURL)
	}
	if len(c.IMAPHosts) != 1 || len(c.AccessTokens) != 2 {
		t.Errorf("hosts=%v tokens=%v", c.IMAPHosts, c.AccessTokens)
	}
}

func TestLoad_RequiresTokens(t *testing.T) {
	m := base()
	m["IMAP_MCP_ACCESS_TOKENS"] = " , "
	_, err := Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "IMAP_MCP_ACCESS_TOKENS") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoad_RequiresHosts(t *testing.T) {
	m := base()
	delete(m, "IMAP_MCP_IMAP_HOSTS")
	_, err := Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "IMAP_MCP_IMAP_HOSTS") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoad_RejectsPlainHTTPOnPublicAddr(t *testing.T) {
	m := base()
	m["IMAP_MCP_LISTEN"] = "0.0.0.0:8787"
	_, err := Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "IMAP_MCP_INSECURE_HTTP") {
		t.Fatalf("err=%v", err)
	}

	m["IMAP_MCP_INSECURE_HTTP"] = "true"
	if _, err := Load(env(m)); err != nil {
		t.Errorf("insecure flag: %v", err)
	}

	m = base()
	m["IMAP_MCP_LISTEN"] = "0.0.0.0:8787"
	m["IMAP_MCP_TLS_CERT"] = "c.pem"
	m["IMAP_MCP_TLS_KEY"] = "k.pem"
	if _, err := Load(env(m)); err != nil {
		t.Errorf("tls: %v", err)
	}

	for _, l := range []string{"127.0.0.1:8787", "[::1]:8787"} {
		m = base()
		m["IMAP_MCP_LISTEN"] = l
		if _, err := Load(env(m)); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
}

func TestLoad_TLSNeedsBoth(t *testing.T) {
	m := base()
	m["IMAP_MCP_TLS_CERT"] = "c.pem"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error with only cert")
	}
	m = base()
	m["IMAP_MCP_TLS_KEY"] = "k.pem"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error with only key")
	}
}

func TestLoad_ParsesSizes(t *testing.T) {
	cases := map[string]int64{"25MB": 25 << 20, "10MiB": 10 << 20, "1048576": 1048576}
	for in, want := range cases {
		m := base()
		m["IMAP_MCP_MAX_UPLOAD"] = in
		c, err := Load(env(m))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if c.MaxUpload != want {
			t.Errorf("%s: got %d want %d", in, c.MaxUpload, want)
		}
	}
	m := base()
	m["IMAP_MCP_MAX_UPLOAD"] = "abc"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected error for abc")
	}
}

func TestRedacted_HidesTokens(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	out := c.Redacted()
	for k, v := range out {
		s := k + "=" + strings.TrimSpace(fmtAny(v))
		if strings.Contains(s, "tok-secret") {
			t.Errorf("leak in %s", s)
		}
	}
	if out["access_tokens"] != 2 {
		t.Errorf("access_tokens=%v", out["access_tokens"])
	}
}

func fmtAny(v any) string { return fmt.Sprintf("%v", v) }
