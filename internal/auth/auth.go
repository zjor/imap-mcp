// Package auth checks the bearer token, IMAP credentials and IMAP host of
// each request (spec 7.1).
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/zjor/imap-mcp/internal/config"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
)

// Credentials are the IMAP login data of one request.
type Credentials = imapsafe.Credentials

type ctxKey struct{}

// CredentialsFrom returns the credentials Middleware stored in ctx.
func CredentialsFrom(ctx context.Context) (Credentials, bool) {
	c, ok := ctx.Value(ctxKey{}).(Credentials)
	return c, ok
}

// Middleware checks, in order: bearer token (401), IMAP username and
// password (401), IMAP host (403). With one configured host the
// X-IMAP-Host header is ignored. On success it stores Credentials in the
// context and adds user, token_id and imap_host to the context logger and
// to the http.request access line.
func Middleware(cfg config.Config) func(http.Handler) http.Handler {
	hashes := make([][32]byte, len(cfg.AccessTokens))
	for i, t := range cfg.AccessTokens {
		hashes[i] = sha256.Sum256([]byte(t))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := logging.From(r.Context())

			token, ok := bearer(r.Header.Get("Authorization"))
			if !ok || !tokenAllowed(hashes, token) {
				log.Warn("auth.rejected", "status", http.StatusUnauthorized, "reason", "bad or missing access token")
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			log = log.With("token_id", logging.TokenID(token))

			user := r.Header.Get("X-IMAP-Username")
			pass := r.Header.Get("X-IMAP-Password")
			if user == "" || pass == "" {
				log.Warn("auth.rejected", "status", http.StatusUnauthorized, "reason", "missing IMAP username or password")
				http.Error(w, "missing X-IMAP-Username or X-IMAP-Password", http.StatusUnauthorized)
				return
			}
			log = log.With("user", user)

			host, ok := pickHost(cfg.IMAPHosts, r.Header.Get("X-IMAP-Host"))
			if !ok {
				log.Warn("auth.rejected", "status", http.StatusForbidden, "reason", "IMAP host not allowed")
				http.Error(w, "IMAP host not allowed", http.StatusForbidden)
				return
			}
			log = log.With("imap_host", host)

			logging.SetAccessLogger(r.Context(), log)
			ctx := context.WithValue(r.Context(), ctxKey{}, Credentials{Username: user, Password: pass, Host: host})
			next.ServeHTTP(w, r.WithContext(logging.With(ctx, log)))
		})
	}
}

func bearer(h string) (string, bool) {
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// tokenAllowed compares SHA-256 hashes in constant time and checks every
// token, so timing reveals neither the match nor the token length.
func tokenAllowed(hashes [][32]byte, token string) bool {
	h := sha256.Sum256([]byte(token))
	match := 0
	for _, want := range hashes {
		match |= subtle.ConstantTimeCompare(h[:], want[:])
	}
	return match == 1
}

func pickHost(allowed []string, header string) (string, bool) {
	if len(allowed) == 1 {
		return allowed[0], true
	}
	header = strings.TrimSpace(header)
	for _, a := range allowed {
		if header != "" && strings.EqualFold(a, header) {
			return a, true
		}
	}
	return "", false
}
