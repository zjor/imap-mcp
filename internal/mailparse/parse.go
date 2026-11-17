// Package mailparse turns a raw RFC 5322 message into headers, a readable
// text body and a list of attachments. Attachment bytes are never kept.
package mailparse

import (
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 charsets
	gomail "github.com/emersion/go-message/mail"
	"github.com/k3a/html2text"
)

// maxBodyBytes limits the text and the HTML body each kept in memory.
const maxBodyBytes = 1 << 20

type Address struct{ Name, Email string }

type Headers struct {
	MessageID, InReplyTo  string
	References            []string
	From, To, Cc, ReplyTo []Address
	Bcc                   []Address // present in drafts only
	Subject               string
	Date                  time.Time
}

// Attachment describes one non-body leaf part. Part is the IMAP section
// number (RFC 3501), so it works in BODY.PEEK[<part>]. Size is the decoded
// size in bytes.
type Attachment struct {
	Part        []int
	Filename    string
	ContentType string
	// Charset is the charset parameter of a text/* part, else empty.
	Charset string
	Size    int64
	Inline  bool
}

type Parsed struct {
	Headers     Headers
	Text        string
	HTML        string
	Attachments []Attachment
	// Truncated is true when the text or HTML body was cut at maxBodyBytes.
	Truncated bool
}

// ErrUnparseable means the message cannot be parsed. Its text is fixed and
// never holds message content, so it is safe to log.
var ErrUnparseable = errors.New("mailparse: message cannot be parsed")

// errInternal is returned after a recovered panic. It wraps ErrUnparseable.
var errInternal = fmt.Errorf("%w: internal error", ErrUnparseable)

// Parse reads a message. It fails with ErrUnparseable only when the
// top-level header cannot be read. Broken MIME below that degrades to
// best-effort output.
//
// A panic inside a third-party parser is recovered: Parse then returns what
// was parsed so far together with an error that wraps ErrUnparseable.
func Parse(r io.Reader) (res Parsed, err error) {
	var p Parsed
	defer func() {
		if rec := recover(); rec != nil {
			res, err = p, errInternal
		}
	}()
	e, err := message.Read(r)
	if e == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		// The parser's error text can quote header lines; drop it.
		return Parsed{}, ErrUnparseable
	}
	p.Headers = readHeaders(e.Header)
	var text, html string
	var haveText, haveHTML bool
	w := walker{p: &p, text: &text, html: &html, haveText: &haveText, haveHTML: &haveHTML}
	if e.MultipartReader() != nil {
		w.walkMultipart(e, nil, 0)
	} else {
		w.leaf(e, []int{1})
	}
	switch {
	case haveText:
		p.Text = text
	case haveHTML:
		p.Text = strings.TrimSpace(html2text.HTML2Text(html))
	}
	p.HTML = html
	return p, nil
}

// maxDepth is the deepest multipart nesting that is walked. Deeper parts are
// listed as plain attachments and not entered.
const maxDepth = 20

type walker struct {
	p                  *Parsed
	text, html         *string
	haveText, haveHTML *bool
}

func (w *walker) walkMultipart(e *message.Entity, path []int, depth int) {
	mr := e.MultipartReader()
	if mr == nil {
		return
	}
	for i := 1; ; i++ {
		child, err := mr.NextPart()
		if child == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
			return // io.EOF or malformed: keep what we have
		}
		childPath := append(append([]int{}, path...), i)
		if depth < maxDepth && child.MultipartReader() != nil {
			w.walkMultipart(child, childPath, depth+1)
		} else {
			w.leaf(child, childPath)
		}
	}
}

func (w *walker) leaf(e *message.Entity, path []int) {
	mt, tparams, err := e.Header.ContentType()
	if err != nil || mt == "" {
		mt = "text/plain"
	}
	disp, dparams, _ := e.Header.ContentDisposition()
	filename := dparams["filename"]
	if filename == "" {
		filename = tparams["name"]
	}
	var charset string
	if strings.HasPrefix(mt, "text/") {
		charset = tparams["charset"]
	}
	isAttachment := disp == "attachment" || filename != ""
	if !isAttachment && (mt == "text/plain" || mt == "text/html") {
		if mt == "text/plain" && !*w.haveText {
			*w.haveText = true
			*w.text = w.readBody(e)
			return
		}
		if mt == "text/html" && !*w.haveHTML {
			*w.haveHTML = true
			*w.html = w.readBody(e)
			return
		}
	}
	n, _ := io.Copy(io.Discard, e.Body)
	w.p.Attachments = append(w.p.Attachments, Attachment{
		Part:        path,
		Filename:    CleanFilename(filename),
		ContentType: mt,
		Charset:     charset,
		Size:        n,
		Inline:      disp == "inline" || (disp == "" && e.Header.Get("Content-ID") != ""),
	})
}

func (w *walker) readBody(e *message.Entity) string {
	b, _ := io.ReadAll(io.LimitReader(e.Body, maxBodyBytes+1))
	if len(b) > maxBodyBytes {
		b = b[:maxBodyBytes]
		w.p.Truncated = true
	}
	s := strings.ToValidUTF8(string(b), "�")
	if w.p.Truncated && !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return s
}

func readHeaders(h message.Header) Headers {
	mh := gomail.Header{Header: h}
	var out Headers
	out.MessageID, _ = mh.MessageID()
	if ids, _ := mh.MsgIDList("In-Reply-To"); len(ids) > 0 {
		out.InReplyTo = ids[0]
	}
	out.References, _ = mh.MsgIDList("References")
	out.From = addrs(mh, "From")
	out.To = addrs(mh, "To")
	out.Cc = addrs(mh, "Cc")
	out.Bcc = addrs(mh, "Bcc")
	out.ReplyTo = addrs(mh, "Reply-To")
	if s, err := mh.Subject(); err == nil {
		out.Subject = s
	} else {
		out.Subject = h.Get("Subject")
	}
	if d, err := mh.Date(); err == nil {
		out.Date = d
	} else if d, err := mail.ParseDate(h.Get("Date")); err == nil {
		out.Date = d
	}
	return out
}

func addrs(h gomail.Header, key string) []Address {
	list, _ := h.AddressList(key)
	var out []Address
	for _, a := range list {
		out = append(out, Address{Name: a.Name, Email: a.Address})
	}
	return out
}

// Snippet returns a one-line preview: quoted lines (starting with ">")
// dropped, whitespace collapsed, cut at n runes.
func Snippet(text string, n int) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		kept = append(kept, line)
	}
	s := strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
