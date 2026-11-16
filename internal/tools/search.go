package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/mailparse"
)

const snippetRunes = 200

type searchIn struct {
	Folder         string `json:"folder,omitempty" jsonschema:"folder name (default INBOX), or \"all\" for every folder except Trash and Junk"`
	From           string `json:"from,omitempty" jsonschema:"text in the From header"`
	To             string `json:"to,omitempty" jsonschema:"text in the To header"`
	Cc             string `json:"cc,omitempty" jsonschema:"text in the Cc header"`
	Subject        string `json:"subject,omitempty" jsonschema:"text in the subject"`
	Text           string `json:"text,omitempty" jsonschema:"text anywhere in the message"`
	Since          string `json:"since,omitempty" jsonschema:"received on or after this date (YYYY-MM-DD)"`
	Before         string `json:"before,omitempty" jsonschema:"received before this date (YYYY-MM-DD)"`
	Unread         *bool  `json:"unread,omitempty" jsonschema:"true: only unread, false: only read"`
	HasAttachments *bool  `json:"has_attachments,omitempty" jsonschema:"true: only with attachments, false: only without"`
	Limit          int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
	Cursor         string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type addrOut struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type summaryOut struct {
	ID               string    `json:"id"`
	Date             time.Time `json:"date"`
	From             []addrOut `json:"from"`
	To               []addrOut `json:"to"`
	UntrustedContent struct {
		Subject string `json:"subject"`
		Snippet string `json:"snippet"`
	} `json:"untrusted_content"`
	HasAttachments bool `json:"has_attachments"`
	Unread         bool `json:"unread"`
}

type searchOut struct {
	Messages   []summaryOut `json:"messages"`
	NextCursor *string      `json:"next_cursor"`
}

func registerSearch(s *mcp.Server, deps Deps) {
	addTool(s, deps, "search_emails",
		"Search messages, newest first. Filters are combined with AND. Returns id, date, from, to, subject, a short snippet, has_attachments and unread for each message. Pass next_cursor back as cursor for the next page; it stays correct when new mail arrives.",
		func(in searchIn) []any {
			return []any{"folder", folderArg(in), "limit", clampLimit(in.Limit), "filters", in.filterNames(), "cursor", in.Cursor != ""}
		},
		searchEmails)
}

func folderArg(in searchIn) string {
	if in.Folder == "" {
		return "INBOX"
	}
	return in.Folder
}

func (in searchIn) filterNames() []string {
	var names []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"from", in.From != ""}, {"to", in.To != ""}, {"cc", in.Cc != ""}, {"subject", in.Subject != ""},
		{"text", in.Text != ""}, {"since", in.Since != ""}, {"before", in.Before != ""},
		{"unread", in.Unread != nil}, {"has_attachments", in.HasAttachments != nil},
	} {
		if f.set {
			names = append(names, f.name)
		}
	}
	return names
}

func searchEmails(ctx context.Context, s *imapsafe.Session, in searchIn) (any, int, error) {
	debugFilters(ctx, "search_emails", "from", in.From, "to", in.To, "cc", in.Cc, "subject", in.Subject,
		"text", in.Text, "since", in.Since, "before", in.Before)
	crit := imapsafe.SearchCriteria{From: in.From, To: in.To, Cc: in.Cc, Subject: in.Subject, Text: in.Text, Unread: in.Unread}
	var err error
	if crit.Since, err = parseDate("since", in.Since); err != nil {
		return nil, 0, err
	}
	if crit.Before, err = parseDate("before", in.Before); err != nil {
		return nil, 0, err
	}
	limit := clampLimit(in.Limit)

	var sources []*source
	if in.Cursor != "" {
		cur, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, 0, err
		}
		for _, f := range cur.Folders {
			sources = append(sources, &source{folder: f.Name, uv: f.UIDValidity, before: f.Before})
		}
	} else {
		folders, err := searchFolders(ctx, s, folderArg(in))
		if err != nil {
			return nil, 0, err
		}
		for _, f := range folders {
			sources = append(sources, &source{folder: f})
		}
	}

	// Find the matching UIDs of each folder.
	live := sources[:0]
	for _, src := range sources {
		uv, err := s.Examine(ctx, src.folder)
		if err != nil {
			return nil, 0, err
		}
		if src.uv != 0 && src.uv != uv {
			return nil, 0, imapsafe.ErrStale
		}
		src.uv = uv
		src.uids, err = s.Search(ctx, src.folder, crit, src.before, 0)
		if err != nil {
			return nil, 0, err
		}
		if len(src.uids) == 0 {
			continue
		}
		if src.before == 0 {
			// Fix the upper bound now, so mail that arrives later never
			// shows up in later pages.
			src.before = src.uids[0] + 1
		}
		live = append(live, src)
	}
	sources = live

	// Merge the folders by date, newest first. Each folder is consumed in
	// UID order, so the cursor (lowest UID taken) leaves no gaps.
	out := searchOut{Messages: []summaryOut{}}
	for len(out.Messages) < limit {
		var best *source
		var bestDate time.Time
		for _, src := range sources {
			m, err := src.peek(ctx, s, limit, in.HasAttachments)
			if err != nil {
				return nil, 0, err
			}
			if m != nil && (best == nil || msgDate(*m).After(bestDate)) {
				best, bestDate = src, msgDate(*m)
			}
		}
		if best == nil {
			break
		}
		out.Messages = append(out.Messages, toSummaryOut(best.folder, best.uv, best.take()))
	}

	var next cursor
	for _, src := range sources {
		if !src.done() {
			next.Folders = append(next.Folders, cursorFolder{Name: src.folder, UIDValidity: src.uv, Before: src.before})
		}
	}
	if len(next.Folders) > 0 {
		c := next.encode()
		out.NextCursor = &c
	}
	return out, len(out.Messages), nil
}

// searchFolders resolves the folder argument. "all" means every folder
// except Trash and Junk.
func searchFolders(ctx context.Context, s *imapsafe.Session, folder string) ([]string, error) {
	if !strings.EqualFold(folder, "all") {
		return []string{folder}, nil
	}
	list, err := s.ListFolders(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range list {
		if f.Role != "trash" && f.Role != "junk" {
			names = append(names, f.Name)
		}
	}
	return names, nil
}

// source walks the matching messages of one folder, newest UID first.
type source struct {
	folder string
	uv     uint32
	before uint32             // only UIDs below this are left
	uids   []uint32           // matching, not fetched yet
	buf    []imapsafe.Summary // fetched, passed the filter, not taken yet
}

// peek returns the next message that passes the has_attachments filter,
// fetching batches of summaries as needed; nil when the folder is done.
func (src *source) peek(ctx context.Context, s *imapsafe.Session, batch int, hasAtt *bool) (*imapsafe.Summary, error) {
	for len(src.buf) == 0 && len(src.uids) > 0 {
		n := min(batch, len(src.uids))
		chunk := src.uids[:n]
		src.uids = src.uids[n:]
		sums, err := s.FetchSummaries(ctx, src.folder, chunk)
		if err != nil {
			return nil, err
		}
		for _, m := range sums {
			if hasAtt == nil || m.HasAttachments == *hasAtt {
				src.buf = append(src.buf, m)
			}
		}
	}
	if len(src.buf) == 0 {
		return nil, nil
	}
	return &src.buf[0], nil
}

// take removes the message peek returned and moves the cursor below it.
func (src *source) take() imapsafe.Summary {
	m := src.buf[0]
	src.buf = src.buf[1:]
	src.before = m.UID
	return m
}

func (src *source) done() bool { return len(src.buf) == 0 && len(src.uids) == 0 }

func msgDate(m imapsafe.Summary) time.Time {
	if !m.Date.IsZero() {
		return m.Date
	}
	return m.InternalDate
}

func toSummaryOut(folder string, uv uint32, m imapsafe.Summary) summaryOut {
	o := summaryOut{
		ID:             ids.EncodeMessage(ids.MessageID{Folder: folder, UIDValidity: uv, UID: m.UID}),
		Date:           msgDate(m),
		From:           toAddrOut(m.From),
		To:             toAddrOut(m.To),
		HasAttachments: m.HasAttachments,
		Unread:         m.Unread,
	}
	o.UntrustedContent.Subject = m.Subject
	o.UntrustedContent.Snippet = snippet(m)
	return o
}

func toAddrOut(in []imapsafe.Address) []addrOut {
	out := make([]addrOut, 0, len(in))
	for _, a := range in {
		out = append(out, addrOut{Name: a.Name, Email: a.Addr})
	}
	return out
}

// snippet decodes the raw start of the body best-effort: it puts the
// top-level Content-Type and transfer encoding from BODYSTRUCTURE in front
// of the fragment and parses that as a message. Anything that cannot be
// decoded gives an empty snippet.
func snippet(m imapsafe.Summary) string {
	if len(m.Snippet) == 0 {
		return ""
	}
	ct := m.ContentType
	if ct == "" {
		ct = "text/plain"
	}
	hdr := "Content-Type: " + ct + "\r\n"
	if m.Encoding == "base64" || m.Encoding == "quoted-printable" {
		hdr += "Content-Transfer-Encoding: " + m.Encoding + "\r\n"
	}
	p, err := mailparse.Parse(io.MultiReader(strings.NewReader(hdr+"\r\n"), bytes.NewReader(m.Snippet)))
	if err != nil {
		return ""
	}
	// The fragment may end inside a UTF-8 sequence.
	return strings.TrimRight(mailparse.Snippet(p.Text, snippetRunes), "�")
}

// cursor is the paging state: per folder, its UIDVALIDITY and the UID
// bound below which the next page starts. Folders that are done are left
// out.
type cursor struct {
	Folders []cursorFolder `json:"f"`
}

type cursorFolder struct {
	Name        string `json:"n"`
	UIDValidity uint32 `json:"v"`
	Before      uint32 `json:"b"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	bad := badArg("invalid cursor")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, bad
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || len(c.Folders) == 0 {
		return cursor{}, bad
	}
	for _, f := range c.Folders {
		if f.Name == "" || f.UIDValidity == 0 || f.Before == 0 {
			return cursor{}, bad
		}
	}
	return c, nil
}
