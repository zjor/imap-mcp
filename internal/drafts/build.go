// Package drafts builds RFC 5322 draft messages. It is pure: it does not
// talk to IMAP.
package drafts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/zjor/imap-mcp/internal/mailparse"
)

// MarkerHeader marks drafts created by this server.
const MarkerHeader = "X-IMAP-MCP-Draft-Id"

// Input describes one draft.
//
// Build is deterministic so that Size equals the real build:
//   - Date and MessageID are required inputs. The caller sets one fresh value
//     per build and uses the same Input for Size and Build.
//   - MIME boundaries are derived from a hash of DraftID.
type Input struct {
	// Mode is "new", "reply", "reply_all" or "forward". Reply modes set
	// In-Reply-To and References from Original.
	Mode     string
	Original *mailparse.Parsed
	From     string
	To       []string
	Cc       []string
	Bcc      []string // kept in the draft; the mail client handles it on send
	Subject  string
	BodyText string
	BodyHTML string
	DraftID  string
	Date     time.Time
	// MessageID is bare (no angle brackets), e.g. from NewMessageID.
	MessageID string
}

// Part is one attachment. Open is called once per build; the body is
// streamed and never held fully in memory.
type Part struct {
	Filename, ContentType string
	Open                  func() (io.ReadCloser, error)
}

// Build writes a full RFC 5322 message to w.
func Build(w io.Writer, in Input, attachments []Part) error {
	h, err := rootHeader(in)
	if err != nil {
		return err
	}
	hasHTML := in.BodyHTML != ""
	if len(attachments) == 0 {
		if !hasHTML {
			return writeText(w, h, in.BodyText)
		}
		return writeAlternative(w, h, in, boundary(in.DraftID, "alt"))
	}

	h.SetContentType("multipart/mixed", map[string]string{"boundary": boundary(in.DraftID, "mixed")})
	mw, err := message.CreateWriter(w, h.Header)
	if err != nil {
		return err
	}
	if err := writeBodyPart(mw, in); err != nil {
		return err
	}
	for _, a := range attachments {
		if err := writeAttachment(mw, a); err != nil {
			return err
		}
	}
	return mw.Close()
}

func writeBodyPart(mw *message.Writer, in Input) error {
	var ph message.Header
	if in.BodyHTML == "" {
		textHeader(&ph)
		pw, err := mw.CreatePart(ph)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(pw, in.BodyText); err != nil {
			return err
		}
		return pw.Close()
	}
	ph.SetContentType("multipart/alternative", map[string]string{"boundary": boundary(in.DraftID, "alt")})
	pw, err := mw.CreatePart(ph)
	if err != nil {
		return err
	}
	if err := altParts(pw, in); err != nil {
		return err
	}
	return pw.Close()
}

func writeText(w io.Writer, h gomail.Header, text string) error {
	textHeader(&h.Header)
	tw, err := message.CreateWriter(w, h.Header)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(tw, text); err != nil {
		return err
	}
	return tw.Close()
}

func writeAlternative(w io.Writer, h gomail.Header, in Input, b string) error {
	h.SetContentType("multipart/alternative", map[string]string{"boundary": b})
	mw, err := message.CreateWriter(w, h.Header)
	if err != nil {
		return err
	}
	if err := altParts(mw, in); err != nil {
		return err
	}
	return mw.Close()
}

func altParts(mw *message.Writer, in Input) error {
	var th, hh message.Header
	textHeader(&th)
	hh.SetContentType("text/html", map[string]string{"charset": "utf-8"})
	hh.Set("Content-Transfer-Encoding", "quoted-printable")
	for _, p := range []struct {
		h    message.Header
		body string
	}{{th, in.BodyText}, {hh, in.BodyHTML}} {
		pw, err := mw.CreatePart(p.h)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(pw, p.body); err != nil {
			return err
		}
		if err := pw.Close(); err != nil {
			return err
		}
	}
	return nil
}

func textHeader(h *message.Header) {
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	h.Set("Content-Transfer-Encoding", "quoted-printable")
}

func writeAttachment(mw *message.Writer, a Part) error {
	if a.Open == nil {
		return errors.New("attachment " + a.Filename + ": no Open function")
	}
	if hasCRLF(a.Filename) || hasCRLF(a.ContentType) {
		return errors.New("attachment: CR or LF in filename or content type")
	}
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return fmt.Errorf("attachment %q: bad content type: %w", a.Filename, err)
	}
	if i := strings.IndexByte(mt, '/'); i <= 0 || i == len(mt)-1 {
		return fmt.Errorf("attachment %q: content type needs type/subtype", a.Filename)
	}
	formatted := mime.FormatMediaType(mt, params)
	if formatted == "" || hasCRLF(formatted) {
		return fmt.Errorf("attachment %q: bad content type", a.Filename)
	}
	var ph message.Header
	ph.Set("Content-Type", formatted)
	// mime.FormatMediaType gives RFC 2231 (filename*=utf-8''...) for
	// non-ASCII names; go-message would use RFC 2047 here instead.
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	if disp == "" {
		return errors.New("attachment: bad filename " + a.Filename)
	}
	ph.Set("Content-Disposition", disp)
	ph.Set("Content-Transfer-Encoding", "base64")
	rc, err := a.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	pw, err := mw.CreatePart(ph)
	if err != nil {
		return err
	}
	if _, err := io.Copy(pw, rc); err != nil {
		return err
	}
	return pw.Close()
}

func rootHeader(in Input) (gomail.Header, error) {
	var h gomail.Header
	if in.DraftID == "" || hasCRLF(in.DraftID) || strings.ContainsAny(in.DraftID, " <>@\t") {
		return h, errors.New("invalid draft id")
	}
	if hasCRLF(in.Subject) {
		return h, errors.New("subject contains CR or LF")
	}
	h.Set("MIME-Version", "1.0")
	h.Set(MarkerHeader, in.DraftID)
	if in.Date.IsZero() {
		return h, errors.New("date is required")
	}
	if in.MessageID == "" || hasCRLF(in.MessageID) || strings.ContainsAny(in.MessageID, "<> \t") {
		return h, errors.New("invalid message id")
	}
	h.SetDate(in.Date)
	h.SetMessageID(in.MessageID)
	h.SetSubject(in.Subject)
	for _, f := range []struct {
		key  string
		vals []string
	}{{"From", nonEmpty(in.From)}, {"To", in.To}, {"Cc", in.Cc}, {"Bcc", in.Bcc}} {
		list, err := parseAddrs(f.key, f.vals)
		if err != nil {
			return h, err
		}
		if len(list) > 0 {
			h.SetAddressList(f.key, list)
		}
	}
	if o := in.Original; o != nil && strings.HasPrefix(in.Mode, "reply") && o.Headers.MessageID != "" {
		h.Set("In-Reply-To", "<"+o.Headers.MessageID+">")
		refs := append(append([]string{}, o.Headers.References...), o.Headers.MessageID)
		h.Set("References", "<"+strings.Join(refs, "> <")+">")
	}
	return h, nil
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func parseAddrs(field string, vals []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, v := range vals {
		if hasCRLF(v) {
			return nil, fmt.Errorf("%s: address contains CR or LF", field)
		}
		list, err := mail.ParseAddressList(v)
		if err != nil {
			return nil, fmt.Errorf("%s: bad address %q: %w", field, v, err)
		}
		out = append(out, list...)
	}
	return out, nil
}

func hasCRLF(s string) bool { return strings.ContainsAny(s, "\r\n") }

// boundary derives a stable MIME boundary from the draft id.
func boundary(draftID, role string) string {
	sum := sha256.Sum256([]byte(draftID + "/" + role))
	return "imap-mcp-" + role + "-" + hex.EncodeToString(sum[:12])
}

// NewMessageID returns a fresh bare Message-ID "uuid@domain", using the
// domain of from (fallback "localhost").
func NewMessageID(from string) string {
	domain := "localhost"
	if a, err := mail.ParseAddress(from); err == nil {
		if i := strings.LastIndex(a.Address, "@"); i >= 0 && i+1 < len(a.Address) {
			domain = a.Address[i+1:]
		}
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:] + "@" + domain
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// Size returns the exact byte length Build would write. Attachments are
// streamed through a counter, so call it with the same Input as Build.
func Size(in Input, attachments []Part) (int64, error) {
	var c countWriter
	if err := Build(&c, in, attachments); err != nil {
		return 0, err
	}
	return c.n, nil
}

// ReplyHeaders computes the headers of a reply. self is the login address;
// it is compared case-insensitively and removed from Cc on reply-all.
func ReplyHeaders(orig mailparse.Headers, all bool, self string) (to, cc []string, subject, inReplyTo string, refs []string) {
	subject = prefixed(orig.Subject, "Re:")
	if v := orig.MessageID; v != "" {
		inReplyTo = v
		refs = append(append(refs, orig.References...), v)
	} else {
		refs = append(refs, orig.References...)
	}
	sender := orig.ReplyTo
	if len(sender) == 0 {
		sender = orig.From
	}
	seen := map[string]bool{}
	add := func(dst *[]string, list []mailparse.Address) {
		for _, a := range list {
			k := strings.ToLower(a.Email)
			if a.Email == "" || seen[k] {
				continue
			}
			seen[k] = true
			*dst = append(*dst, (&mail.Address{Name: a.Name, Address: a.Email}).String())
		}
	}
	add(&to, sender) // kept even when it is self: a reply to own mail
	if all {
		seen[strings.ToLower(self)] = true
		add(&cc, orig.To)
		add(&cc, orig.Cc)
	}
	return
}

// prefixed adds prefix unless the subject already starts with Re:, Fwd: or
// Fw: (any case).
func prefixed(subject, prefix string) string {
	s := strings.TrimSpace(subject)
	l := strings.ToLower(s)
	for _, p := range []string{"re:", "fwd:", "fw:"} {
		if strings.HasPrefix(l, p) {
			return s
		}
	}
	return prefix + " " + s
}
