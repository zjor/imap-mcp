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

func TestEnvReport_SourcesAndRedaction(t *testing.T) {
	environ := []string{
		"PATH=/bin",
		"IMAP_MCP_LISTEN=0.0.0.0:9000",
		"IMAP_MCP_ACCESS_TOKENS=tok-secret-env",
		"IMAP_MCP_ACESS_TOKENS=tok-secret-typo",
	}
	file := map[string]string{
		"IMAP_MCP_LISTEN":     "127.0.0.1:1",
		"IMAP_MCP_IMAP_HOSTS": "imap.example.com:993",
		"IMAP_MCP_TOKEN":      "tok-secret-client",
		"IMAP_PASSWORD":       "pw-secret",
	}
	got := EnvReport(environ, file)

	want := map[string]EnvVar{
		"IMAP_MCP_LISTEN":        {Value: "0.0.0.0:9000", Source: "env"},
		"IMAP_MCP_ACCESS_TOKENS": {Value: redacted, Source: "env"},
		"IMAP_MCP_ACESS_TOKENS":  {Value: redacted, Source: "env"},
		"IMAP_MCP_IMAP_HOSTS":    {Value: "imap.example.com:993", Source: ".env.local"},
		"IMAP_MCP_TOKEN":         {Value: redacted, Source: ".env.local"},
		"IMAP_MCP_LOG_LEVEL":     {Source: "default"},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v want %+v", k, got[k], w)
		}
	}
	if _, ok := got["PATH"]; ok {
		t.Error("non-IMAP_MCP_ key from environ is logged")
	}
	if _, ok := got["IMAP_PASSWORD"]; ok {
		t.Error("non-IMAP_MCP_ key from file is logged")
	}
	for k := range envKeys {
		if _, ok := got[envKeys[k]]; !ok {
			t.Errorf("known key %s missing", envKeys[k])
		}
	}
	if s := fmtAny(got); strings.Contains(s, "secret") {
		t.Errorf("leak: %s", s)
	}
}
