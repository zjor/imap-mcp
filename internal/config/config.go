// Package config loads server configuration from environment variables.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config is the server configuration (spec 7.2).
type Config struct {
	Listen             string
	IMAPHosts          []string
	AccessTokens       []string
	PublicURL          string
	MaxUpload          int64
	DraftsFolder       string
	SentFolder         string
	TLSCert, TLSKey    string
	InsecureHTTP       bool
	MaxSessionsPerUser int
	LogLevel           slog.Level
	LogFormat          string
}

// Load builds a Config from getenv. It is pure so tests can pass a map.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var c Config
	c.Listen = get("IMAP_MCP_LISTEN", "127.0.0.1:8787")
	c.IMAPHosts = splitList(getenv("IMAP_MCP_IMAP_HOSTS"))
	if len(c.IMAPHosts) == 0 {
		return c, errors.New("IMAP_MCP_IMAP_HOSTS is required")
	}
	c.AccessTokens = splitList(getenv("IMAP_MCP_ACCESS_TOKENS"))
	if len(c.AccessTokens) == 0 {
		return c, errors.New("IMAP_MCP_ACCESS_TOKENS is required and must not be empty")
	}
	c.TLSCert = get("IMAP_MCP_TLS_CERT", "")
	c.TLSKey = get("IMAP_MCP_TLS_KEY", "")
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return c, errors.New("IMAP_MCP_TLS_CERT and IMAP_MCP_TLS_KEY must be set together")
	}
	var err error
	if c.InsecureHTTP, err = parseBool(get("IMAP_MCP_INSECURE_HTTP", "false")); err != nil {
		return c, fmt.Errorf("IMAP_MCP_INSECURE_HTTP: %w", err)
	}
	if c.TLSCert == "" && !c.InsecureHTTP && !isLoopback(c.Listen) {
		return c, fmt.Errorf("refusing to serve plain HTTP on non-loopback address %q: set IMAP_MCP_TLS_CERT and IMAP_MCP_TLS_KEY, or IMAP_MCP_INSECURE_HTTP=true behind a TLS proxy", c.Listen)
	}
	scheme := "http"
	if c.TLSCert != "" {
		scheme = "https"
	}
	c.PublicURL = get("IMAP_MCP_PUBLIC_URL", scheme+"://"+c.Listen)
	if c.MaxUpload, err = parseSize(get("IMAP_MCP_MAX_UPLOAD", "25MB")); err != nil {
		return c, fmt.Errorf("IMAP_MCP_MAX_UPLOAD: %w", err)
	}
	c.DraftsFolder = get("IMAP_MCP_DRAFTS_FOLDER", "")
	c.SentFolder = get("IMAP_MCP_SENT_FOLDER", "")
	c.MaxSessionsPerUser, err = strconv.Atoi(get("IMAP_MCP_MAX_SESSIONS_PER_USER", "4"))
	if err != nil || c.MaxSessionsPerUser < 1 {
		return c, errors.New("IMAP_MCP_MAX_SESSIONS_PER_USER must be a positive integer")
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("IMAP_MCP_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("IMAP_MCP_LOG_LEVEL: %w", err)
	}
	c.LogFormat = strings.ToLower(get("IMAP_MCP_LOG_FORMAT", "json"))
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return c, errors.New("IMAP_MCP_LOG_FORMAT must be json or text")
	}
	return c, nil
}

// LoadFromEnv reads .env.local when present, then the OS environment wins.
func LoadFromEnv() (Config, error) {
	file, err := readDotEnv(".env.local")
	if err != nil {
		return Config{}, err
	}
	return Load(func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return file[k]
	})
}

// Redacted returns a log-safe view. Tokens appear only as a count.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"listen":                c.Listen,
		"imap_hosts":            c.IMAPHosts,
		"access_tokens":         len(c.AccessTokens),
		"public_url":            c.PublicURL,
		"max_upload":            c.MaxUpload,
		"drafts_folder":         c.DraftsFolder,
		"sent_folder":           c.SentFolder,
		"tls":                   c.TLSCert != "",
		"insecure_http":         c.InsecureHTTP,
		"max_sessions_per_user": c.MaxSessionsPerUser,
		"log_level":             c.LogLevel.String(),
		"log_format":            c.LogFormat,
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no", "":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", s)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseSize accepts a byte count with an optional unit. KB/MB/GB and
// KiB/MiB/GiB all mean powers of 1024.
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1},
	}
	u := strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, x := range units {
		if strings.HasSuffix(u, x.suffix) {
			mult = x.mult
			u = strings.TrimSpace(strings.TrimSuffix(u, x.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// readDotEnv parses KEY=VALUE lines. A missing file is not an error.
func readDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}
