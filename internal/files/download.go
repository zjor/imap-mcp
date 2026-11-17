// Package files serves the attachment file endpoints (spec 7.5). Bytes are
// streamed between IMAP and HTTP in small chunks and never stored.
package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/zjor/imap-mcp/internal/auth"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/mailparse"
	"github.com/zjor/imap-mcp/internal/tools"
)

const copyBufSize = 32 << 10

// DownloadHandler serves GET and HEAD /attachments/{id}. It reads the id
// from the "id" path value, so mount it on a pattern with {id}.
func DownloadHandler(d *imapsafe.Dialer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		download(d, w, r)
	})
}

func download(d *imapsafe.Dialer, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(ctx, w, http.StatusMethodNotAllowed, "bad_argument", "method not allowed")
		return
	}
	id, err := ids.DecodeAttachment(r.PathValue("id"))
	if err != nil {
		fail(ctx, w, "attachment", err)
		return
	}
	creds, ok := auth.CredentialsFrom(ctx)
	if !ok {
		writeError(ctx, w, http.StatusUnauthorized, "auth_failed", "missing credentials")
		return
	}

	start := time.Now()
	s, err := d.Open(ctx, creds)
	if err != nil {
		fail(ctx, w, "attachment", err, "message_uid", id.UID)
		return
	}
	defer s.Close()

	name, mediaType, err := s.PartInfo(ctx, id)
	if err != nil {
		fail(ctx, w, "attachment", err, "message_uid", id.UID)
		return
	}

	var body io.ReadCloser
	size := int64(-1)
	if r.Method == http.MethodGet {
		body, size, err = s.FetchPart(ctx, id)
		if err != nil {
			fail(ctx, w, "attachment", err, "message_uid", id.UID)
			return
		}
		// Runs before s.Close: the session is busy until the stream closes.
		defer body.Close()
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", contentDisposition(name))
	if ct := originalType(mediaType); ct != "" {
		h.Set("X-Original-Content-Type", ct)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if body == nil {
		return
	}

	n, err := io.CopyBuffer(ctxWriter{ctx, w}, body, make([]byte, copyBufSize))
	log := logging.From(ctx)
	if err != nil {
		log.Warn("attachment.aborted", "message_uid", id.UID, "part", partPath(id.Part), "size", n, "error", err.Error())
		// Headers are out. Abort the connection so the client sees an
		// error and not a short file that looks complete.
		panic(http.ErrAbortHandler)
	}
	log.Info("attachment.downloaded", "folder", id.Folder, "message_uid", id.UID, "part", partPath(id.Part),
		"filename", name, "size", n, "duration_ms", time.Since(start).Milliseconds())
}

// ctxWriter stops a copy when the request context ends.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

func partPath(part []int) string {
	s := make([]string, len(part))
	for i, p := range part {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ".")
}

// fail answers with the tool error shape and the matching HTTP status.
// It logs rejected requests (spec 7.8: 401, 403, 409, 413) as warn
// "<kind>.rejected" and unexpected errors as error "<kind>.failed", with
// attrs added.
func fail(ctx context.Context, w http.ResponseWriter, kind string, err error, attrs ...any) {
	if ctx.Err() != nil {
		return // client is gone
	}
	code, msg := classify(err)
	status := http.StatusBadGateway
	switch code {
	case "bad_id", "bad_argument":
		status = http.StatusBadRequest
	case "stale_id":
		status = http.StatusConflict
	case "not_found":
		status = http.StatusNotFound
	case "auth_failed":
		status = http.StatusUnauthorized
	case "not_editable":
		status = http.StatusForbidden
	case "too_large":
		status = http.StatusRequestEntityTooLarge
	}
	log := logging.From(ctx)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusRequestEntityTooLarge:
		log.Warn(kind+".rejected", append([]any{"status", status, "code", code}, attrs...)...)
	case http.StatusBadGateway:
		if !errors.Is(err, imapsafe.ErrUnsupported) {
			log.Error(kind+".failed", append([]any{"error", err.Error()}, attrs...)...)
		}
	}
	writeError(ctx, w, status, code, msg)
}

func writeError(ctx context.Context, w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(tools.ErrorJSON(ctx, code, msg))
}

// contentDisposition builds the attachment header with an RFC 5987
// filename*. Format characters (bidi marks and the like) are dropped first.
func contentDisposition(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, name)
	return "attachment; filename*=UTF-8''" + pctEncode(mailparse.CleanFilename(name))
}

// pctEncode percent-encodes s except the RFC 5987 attr-char set.
func pctEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%c%c", hex[c>>4], hex[c&15])
		}
	}
	return b.String()
}

// originalType re-formats the sender's type as type/subtype with no
// parameters. It returns "" when the value is not a valid media type.
func originalType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return mime.FormatMediaType(mt, nil)
}
