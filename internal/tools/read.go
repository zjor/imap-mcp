package tools

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/mailparse"
)

// maxRawHeaders caps the raw header block returned by format raw_headers.
const maxRawHeaders = 64 << 10

type readIn struct {
	ID     string `json:"id" jsonschema:"message id from search_emails or get_thread"`
	Format string `json:"format,omitempty" jsonschema:"text (default): body as plain text, HTML converted; html: the HTML body; raw_headers: text body plus the raw header block"`
}

type headersOut struct {
	From       []addrOut `json:"from"`
	To         []addrOut `json:"to"`
	Cc         []addrOut `json:"cc"`
	ReplyTo    []addrOut `json:"reply_to"`
	Date       time.Time `json:"date"`
	MessageID  string    `json:"message_id"`
	InReplyTo  string    `json:"in_reply_to"`
	References []string  `json:"references"`
}

type attachmentOut struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline"`
	DownloadURL string `json:"download_url"`
}

type emailOut struct {
	ID               string     `json:"id"`
	Headers          headersOut `json:"headers"`
	UntrustedContent struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
		// RawHeaders is set for format raw_headers only (spec 8).
		RawHeaders *string `json:"raw_headers,omitempty"`
	} `json:"untrusted_content"`
	// BodyTruncated is true when the body was cut to a size limit.
	BodyTruncated bool            `json:"body_truncated,omitempty"`
	Attachments   []attachmentOut `json:"attachments"`
	// AttachmentsTruncated is true when the message has more parts than
	// the list shows (mailparse.MaxAttachments and the part id limits).
	AttachmentsTruncated bool `json:"attachments_truncated,omitempty"`
}

func registerRead(s *mcp.Server, deps Deps) {
	addTool(s, deps, "read_email",
		"Read one message: headers, body and attachments. Each attachment has a download_url; fetch it with an HTTP GET that sends the same auth headers as this server.",
		func(in readIn) []any { return []any{"id", in.ID, "format", in.Format} },
		func(ctx context.Context, s mailbox, in readIn) (any, int, error) {
			switch in.Format {
			case "", "text", "html", "raw_headers":
			default:
				return nil, 0, badArg("format must be text, html or raw_headers")
			}
			id, err := ids.DecodeMessage(in.ID)
			if err != nil {
				return nil, 0, err
			}
			e, _, err := readMessage(ctx, s, deps, id, in.Format)
			if err != nil {
				return nil, 0, err
			}
			return e, 1, nil
		})
}

// readMessage fetches and parses one message. format is as for read_email.
// It also returns the parsed message for callers that need more of it.
func readMessage(ctx context.Context, s mailbox, deps Deps, id ids.MessageID, format string) (emailOut, mailparse.Parsed, error) {
	r, err := s.FetchRaw(ctx, id)
	if err != nil {
		return emailOut{}, mailparse.Parsed{}, err
	}
	var hc headerCapture
	var src io.Reader = r
	if format == "raw_headers" {
		src = io.TeeReader(r, &hc)
	}
	p, perr := mailparse.Parse(src)
	// Close drains the rest; it must run before the next command.
	if cerr := r.Close(); cerr != nil {
		return emailOut{}, mailparse.Parsed{}, cerr
	}
	if perr != nil && p.Headers.MessageID == "" && p.Text == "" && len(p.Attachments) == 0 {
		return emailOut{}, mailparse.Parsed{}, perr
	}

	e := emailOut{
		ID: ids.EncodeMessage(id),
		Headers: headersOut{
			From:       mailAddrs(p.Headers.From),
			To:         mailAddrs(p.Headers.To),
			Cc:         mailAddrs(p.Headers.Cc),
			ReplyTo:    mailAddrs(p.Headers.ReplyTo),
			Date:       p.Headers.Date,
			MessageID:  p.Headers.MessageID,
			InReplyTo:  p.Headers.InReplyTo,
			References: p.Headers.References,
		},
		Attachments: make([]attachmentOut, 0, len(p.Attachments)),
	}
	if e.Headers.References == nil {
		e.Headers.References = []string{}
	}
	e.UntrustedContent.Subject = p.Headers.Subject
	e.UntrustedContent.Body = p.Text
	e.BodyTruncated = p.Truncated
	e.AttachmentsTruncated = p.AttachmentsTruncated
	if format == "html" && p.HTML != "" {
		e.UntrustedContent.Body = p.HTML
	}
	if format == "raw_headers" {
		h := hc.headers()
		e.UntrustedContent.RawHeaders = &h
	}
	for _, a := range p.Attachments {
		aid := ids.EncodeAttachment(ids.AttachmentID{MessageID: id, Part: a.Part})
		e.Attachments = append(e.Attachments, attachmentOut{
			ID:          aid,
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Size:        a.Size,
			Inline:      a.Inline,
			DownloadURL: deps.PublicURL + "/attachments/" + aid,
		})
	}
	return e, p, nil
}

func mailAddrs(in []mailparse.Address) []addrOut {
	out := make([]addrOut, 0, len(in))
	for _, a := range in {
		out = append(out, addrOut{Name: a.Name, Email: a.Email})
	}
	return out
}

// headerCapture keeps the start of the message until the blank line that
// ends the header block (at most maxRawHeaders bytes).
type headerCapture struct {
	buf  bytes.Buffer
	done bool
}

func (h *headerCapture) Write(p []byte) (int, error) {
	if !h.done {
		room := maxRawHeaders - h.buf.Len()
		h.buf.Write(p[:min(len(p), room)])
		if bytes.Contains(h.buf.Bytes(), []byte("\r\n\r\n")) || bytes.Contains(h.buf.Bytes(), []byte("\n\n")) || h.buf.Len() >= maxRawHeaders {
			h.done = true
		}
	}
	return len(p), nil
}

func (h *headerCapture) headers() string {
	b := h.buf.Bytes()
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		return string(b[:i+2])
	}
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		return string(b[:i+1])
	}
	return string(b)
}
