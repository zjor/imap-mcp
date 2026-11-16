package imapsafe

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/logging"
)

// Read operations. They use only EXAMINE, STATUS, LIST, UID SEARCH,
// UID THREAD and UID FETCH with PEEK items (spec 7.3). None of them changes
// flags or selects a folder read-write.
//
// A Session runs one command at a time and is not safe for concurrent use.

var (
	// ErrStale means the folder's UIDVALIDITY differs from the one in the id.
	ErrStale = errors.New("message id is stale, search again")
	// ErrNotFound means the message or MIME part does not exist (any more).
	ErrNotFound = errors.New("message or part not found")
	// ErrUnsupported means the server lacks the extension the call needs.
	ErrUnsupported = errors.New("not supported on this server")

	errBusy = errors.New("imap session busy: close the open reader first")
)

const snippetBytes = 2048

// Folder is one selectable mailbox. Role is "inbox", "drafts", "sent",
// "trash", "junk", "archive", "all", "flagged" or "".
//
// CountsKnown is false when the server refused STATUS for this folder (for
// example no read rights, or the folder vanished after LIST); Total and
// Unread are then 0 and mean "unknown".
type Folder struct {
	Name, Delimiter, Role string
	Total, Unread         uint32
	CountsKnown           bool
}

// SearchCriteria are ANDed. Zero fields are ignored. Since and Before use
// the date only (IMAP internal date). Unread nil means any.
type SearchCriteria struct {
	From, To, Cc, Subject, Text string
	Since, Before               time.Time
	Unread                      *bool
}

// Address is one mailbox from an envelope. Addr is "local@domain".
type Address struct {
	Name, Addr string
}

// Summary is the list view of one message.
type Summary struct {
	UID          uint32
	Date         time.Time // envelope Date header; zero when missing
	InternalDate time.Time // when the server received the message
	From, To, Cc []Address
	Subject      string   // decoded to UTF-8
	MessageID    string   // without angle brackets
	InReplyTo    []string // without angle brackets
	Unread       bool     // no \Seen flag
	// HasAttachments is true when BODYSTRUCTURE has a part with
	// disposition "attachment", a filename without disposition "inline",
	// or a message/rfc822 part.
	HasAttachments bool
	// Snippet is the first 2048 bytes of BODY[TEXT], exactly as stored:
	// still transfer-encoded and, for multipart mail, with MIME boundaries
	// and part headers. Callers decode it with ContentType and Encoding.
	Snippet []byte
	// ContentType is the top-level media type with its parameters (charset,
	// boundary), formatted as a Content-Type header value. Encoding is the
	// top-level Content-Transfer-Encoding (lower case). Both come from
	// BODYSTRUCTURE and are empty when it is missing.
	ContentType, Encoding string
}

// ListFolders runs LIST and then one STATUS (MESSAGES UNSEEN) per
// selectable folder (no STATUS for \Noselect or \NonExistent; those are
// left out). The STATUS commands are pipelined. A folder whose STATUS the
// server refuses is kept with CountsKnown false.
func (s *Session) ListFolders(ctx context.Context) ([]Folder, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var opts *imap.ListOptions
	if s.c.Caps().Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	start := time.Now()
	list, err := s.c.List("", "*", opts).Collect()
	logCmd(logging.From(ctx), "LIST", "", 0, start, err)
	if err != nil {
		return nil, fmt.Errorf("imap list: %w", err)
	}

	type pending struct {
		mb    *imap.ListData
		cmd   *imapclient.StatusCommand
		start time.Time
	}
	var cmds []pending
	for _, mb := range list {
		if slices.Contains(mb.Attrs, imap.MailboxAttrNoSelect) || slices.Contains(mb.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		cmd := s.c.Status(mb.Mailbox, &imap.StatusOptions{NumMessages: true, NumUnseen: true})
		cmds = append(cmds, pending{mb, cmd, time.Now()})
	}

	// A refused STATUS (tagged NO/BAD) affects only that folder: it stays
	// in the list with CountsKnown false. Other errors (closed connection,
	// timeout) fail the call.
	log := logging.From(ctx)
	var folders []Folder
	var firstErr error
	for _, p := range cmds {
		data, err := p.cmd.Wait()
		logCmd(log, "STATUS", p.mb.Mailbox, 0, p.start, err)
		f := Folder{Name: p.mb.Mailbox, Role: s.role(p.mb)}
		if p.mb.Delim != 0 {
			f.Delimiter = string(p.mb.Delim)
		}
		var ie *imap.Error
		switch {
		case err == nil:
			f.CountsKnown = true
			if data.NumMessages != nil {
				f.Total = *data.NumMessages
			}
			if data.NumUnseen != nil {
				f.Unread = *data.NumUnseen
			}
		case errors.As(err, &ie):
			log.Warn("imap.status_refused", "folder", p.mb.Mailbox, "status", string(ie.Type), "code", string(ie.Code))
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("imap status: %w", err)
			}
			continue
		}
		folders = append(folders, f)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return folders, nil
}

func (s *Session) role(mb *imap.ListData) string {
	switch mb.Mailbox {
	case "":
	case s.folders.Drafts:
		return "drafts"
	case s.folders.Sent:
		return "sent"
	case s.folders.Trash:
		return "trash"
	case s.folders.Junk:
		return "junk"
	}
	if strings.EqualFold(mb.Mailbox, "INBOX") {
		return "inbox"
	}
	for _, a := range mb.Attrs {
		switch a {
		case imap.MailboxAttrArchive:
			return "archive"
		case imap.MailboxAttrAll:
			return "all"
		case imap.MailboxAttrFlagged:
			return "flagged"
		}
	}
	return ""
}

// Examine opens folder read-only and returns its UIDVALIDITY. It sends
// EXAMINE only when folder is not already the open folder of this session.
func (s *Session) Examine(ctx context.Context, folder string) (uidValidity uint32, err error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	return s.examine(ctx, folder)
}

func (s *Session) examine(ctx context.Context, folder string) (uint32, error) {
	if s.examined == folder && s.examined != "" {
		return s.uidValidity, nil
	}
	s.examined = ""
	start := time.Now()
	uv, err := s.w.selectFolder(folder, true)
	logCmd(logging.From(ctx), "EXAMINE", folder, 0, start, err)
	if err != nil {
		return 0, fmt.Errorf("imap examine %q: %w", folder, err)
	}
	s.examined, s.uidValidity = folder, uv
	return uv, nil
}

// Search runs UID SEARCH in folder. It returns UIDs newest first (highest
// UID first). beforeUID > 0 keeps only UIDs below it. limit > 0 caps the
// result.
func (s *Session) Search(ctx context.Context, folder string, c SearchCriteria, beforeUID uint32, limit int) (uids []uint32, err error) {
	crit := toCriteria(c)
	if beforeUID == 1 {
		return nil, nil
	}
	if beforeUID > 1 {
		crit.UID = []imap.UIDSet{{{Start: 1, Stop: imap.UID(beforeUID - 1)}}}
	}
	uids, err = s.uidSearch(ctx, folder, crit)
	if err != nil {
		return nil, err
	}
	slices.Reverse(uids)
	if limit > 0 && len(uids) > limit {
		uids = uids[:limit]
	}
	return uids, nil
}

// SearchHeader returns the UIDs (ascending) in folder whose header contains
// value (UID SEARCH HEADER).
func (s *Session) SearchHeader(ctx context.Context, folder, header, value string) ([]uint32, error) {
	return s.uidSearch(ctx, folder, &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: header, Value: value}},
	})
}

// uidSearch returns matching UIDs in ascending order.
func (s *Session) uidSearch(ctx context.Context, folder string, crit *imap.SearchCriteria) ([]uint32, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if _, err := s.examine(ctx, folder); err != nil {
		return nil, err
	}
	start := time.Now()
	data, err := s.c.UIDSearch(crit, nil).Wait()
	var found []imap.UID
	if err == nil {
		found = data.AllUIDs()
	}
	logCmd(logging.From(ctx), "UID SEARCH", folder, len(found), start, err)
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}
	uids := make([]uint32, len(found))
	for i, u := range found {
		uids[i] = uint32(u)
	}
	slices.Sort(uids)
	return uids, nil
}

func toCriteria(c SearchCriteria) *imap.SearchCriteria {
	crit := &imap.SearchCriteria{Since: c.Since, Before: c.Before}
	for _, h := range []struct{ key, value string }{
		{"From", c.From}, {"To", c.To}, {"Cc", c.Cc}, {"Subject", c.Subject},
	} {
		if h.value != "" {
			crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: h.key, Value: h.value})
		}
	}
	if c.Text != "" {
		crit.Text = []string{c.Text}
	}
	if c.Unread != nil {
		if *c.Unread {
			crit.NotFlag = []imap.Flag{imap.FlagSeen}
		} else {
			crit.Flag = []imap.Flag{imap.FlagSeen}
		}
	}
	return crit
}

// HasAttachments fetches BODYSTRUCTURE only for uids in folder and reports,
// for each UID that still exists, whether the message has attachments (same
// rule as Summary.HasAttachments).
func (s *Session) HasAttachments(ctx context.Context, folder string, uids []uint32) (map[uint32]bool, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return map[uint32]bool{}, nil
	}
	if _, err := s.examine(ctx, folder); err != nil {
		return nil, err
	}
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	start := time.Now()
	msgs, err := s.c.Fetch(set, &imap.FetchOptions{
		UID:           true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	logCmd(logging.From(ctx), "UID FETCH", folder, len(uids), start, err)
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}
	out := make(map[uint32]bool, len(msgs))
	for _, m := range msgs {
		out[uint32(m.UID)] = m.BodyStructure != nil && hasAttachments(m.BodyStructure)
	}
	return out, nil
}

// FetchSummaries fetches ENVELOPE, FLAGS, INTERNALDATE, BODYSTRUCTURE and
// BODY.PEEK[TEXT]<0.2048> for uids in folder. The result follows the order
// of uids; UIDs that no longer exist are left out.
func (s *Session) FetchSummaries(ctx context.Context, folder string, uids []uint32) ([]Summary, error) {
	return s.fetchSummaries(ctx, folder, uids, true)
}

// FetchEnvelopes is FetchSummaries with ENVELOPE and INTERNALDATE only:
// Unread, HasAttachments, Snippet, ContentType and Encoding stay empty.
func (s *Session) FetchEnvelopes(ctx context.Context, folder string, uids []uint32) ([]Summary, error) {
	return s.fetchSummaries(ctx, folder, uids, false)
}

func (s *Session) fetchSummaries(ctx context.Context, folder string, uids []uint32, full bool) ([]Summary, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return nil, nil
	}
	if _, err := s.examine(ctx, folder); err != nil {
		return nil, err
	}
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	snippet := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierText,
		Peek:      true,
		Partial:   &imap.SectionPartial{Offset: 0, Size: snippetBytes},
	}
	opts := &imap.FetchOptions{UID: true, Envelope: true, InternalDate: true}
	if full {
		opts.Flags = true
		opts.BodyStructure = &imap.FetchItemBodyStructure{Extended: true}
		opts.BodySection = []*imap.FetchItemBodySection{snippet}
	}
	start := time.Now()
	msgs, err := s.c.Fetch(set, opts).Collect()
	logCmd(logging.From(ctx), "UID FETCH", folder, len(uids), start, err)
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}

	byUID := make(map[uint32]*imapclient.FetchMessageBuffer, len(msgs))
	for _, m := range msgs {
		byUID[uint32(m.UID)] = m
	}
	out := make([]Summary, 0, len(msgs))
	for _, u := range uids {
		m, ok := byUID[u]
		if !ok {
			continue
		}
		sum := Summary{UID: u, InternalDate: m.InternalDate}
		if full {
			sum.Unread = !slices.Contains(m.Flags, imap.FlagSeen)
			sum.Snippet = m.FindBodySection(snippet)
			if m.BodyStructure != nil {
				sum.HasAttachments = hasAttachments(m.BodyStructure)
				sum.ContentType, sum.Encoding = topType(m.BodyStructure)
			}
		}
		if e := m.Envelope; e != nil {
			sum.Date = e.Date
			sum.Subject = e.Subject
			sum.MessageID = e.MessageID
			sum.InReplyTo = e.InReplyTo
			sum.From, sum.To, sum.Cc = toAddrs(e.From), toAddrs(e.To), toAddrs(e.Cc)
		}
		out = append(out, sum)
	}
	return out, nil
}

func toAddrs(in []imap.Address) []Address {
	var out []Address
	for _, a := range in {
		if a.IsGroupStart() || a.IsGroupEnd() {
			continue
		}
		out = append(out, Address{Name: a.Name, Addr: a.Addr()})
	}
	return out
}

// topType returns the top-level Content-Type value (with charset or
// boundary) and transfer encoding from BODYSTRUCTURE.
func topType(bs imap.BodyStructure) (contentType, encoding string) {
	var params map[string]string
	switch b := bs.(type) {
	case *imap.BodyStructureSinglePart:
		params, encoding = b.Params, strings.ToLower(b.Encoding)
	case *imap.BodyStructureMultiPart:
		if b.Extended != nil {
			params = b.Extended.Params
		}
	}
	return mime.FormatMediaType(bs.MediaType(), params), encoding
}

func hasAttachments(bs imap.BodyStructure) bool {
	found := false
	bs.Walk(func(_ []int, part imap.BodyStructure) bool {
		sp, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			return !found
		}
		disp := ""
		if d := sp.Disposition(); d != nil {
			disp = strings.ToLower(d.Value)
		}
		if disp == "attachment" || (sp.Filename() != "" && disp != "inline") || sp.MediaType() == "message/rfc822" {
			found = true
		}
		return !found
	})
	return found
}

// FetchRaw streams the full message (UID FETCH BODY.PEEK[]). It returns
// ErrStale when the folder's UIDVALIDITY differs from id.UIDValidity and
// ErrNotFound when the UID does not exist.
//
// The reader keeps the session busy: the caller must Close it before the
// next call on this Session. Close drains any unread rest of the message.
func (s *Session) FetchRaw(ctx context.Context, id ids.MessageID) (io.ReadCloser, error) {
	if err := s.open(ctx, id); err != nil {
		return nil, err
	}
	r, _, err := s.fetchLiteral(ctx, id.Folder, id.UID, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	})
	return r, err
}

// FetchPart streams one MIME part, decoded from its transfer encoding.
// With the BINARY capability it uses UID FETCH BINARY.PEEK[part] and size is
// the decoded size. Without it, it reads the part's encoding from
// BODYSTRUCTURE, fetches BODY.PEEK[part] and decodes base64 or
// quoted-printable while streaming; size is then -1 (unknown).
//
// Errors and the busy rule are as for FetchRaw.
func (s *Session) FetchPart(ctx context.Context, id ids.AttachmentID) (r io.ReadCloser, size int64, err error) {
	if err := s.open(ctx, id.MessageID); err != nil {
		return nil, 0, err
	}
	if s.caps.Binary {
		r, size, err = s.fetchLiteral(ctx, id.Folder, id.UID, &imap.FetchOptions{
			BinarySection: []*imap.FetchItemBinarySection{{Part: id.Part, Peek: true}},
		})
		var ie *imap.Error
		if !errors.As(err, &ie) || ie.Code != imap.ResponseCodeUnknownCTE {
			return r, size, err
		}
		// The server cannot decode this part; fall back to BODY.PEEK.
	}

	enc, err := s.partEncoding(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	r, _, err = s.fetchLiteral(ctx, id.Folder, id.UID, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{Part: id.Part, Peek: true}},
	})
	if err != nil {
		return nil, 0, err
	}
	return decodeTransfer(r, enc), -1, nil
}

// partEncoding returns the Content-Transfer-Encoding of id.Part from
// BODYSTRUCTURE (lower case).
func (s *Session) partEncoding(ctx context.Context, id ids.AttachmentID) (string, error) {
	start := time.Now()
	msgs, err := s.c.Fetch(imap.UIDSetNum(imap.UID(id.UID)), &imap.FetchOptions{
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	logCmd(logging.From(ctx), "UID FETCH", id.Folder, 1, start, err)
	if err != nil {
		return "", fmt.Errorf("imap fetch: %w", err)
	}
	if len(msgs) == 0 || msgs[0].BodyStructure == nil {
		return "", ErrNotFound
	}
	var enc string
	found := false
	msgs[0].BodyStructure.Walk(func(path []int, part imap.BodyStructure) bool {
		if sp, ok := part.(*imap.BodyStructureSinglePart); ok && slices.Equal(path, id.Part) {
			enc, found = strings.ToLower(sp.Encoding), true
		}
		return !found
	})
	if !found {
		return "", ErrNotFound
	}
	return enc, nil
}

// open checks the session and opens id.Folder; ErrStale on a UIDVALIDITY
// mismatch.
func (s *Session) open(ctx context.Context, id ids.MessageID) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	uv, err := s.examine(ctx, id.Folder)
	if err != nil {
		return err
	}
	if uv != id.UIDValidity {
		logWarnStale(ctx, id.Folder)
		return ErrStale
	}
	return nil
}

// fetchLiteral sends a UID FETCH for one UID and returns a reader over the
// first BODY[...] or BINARY[...] literal in the response. The FETCH stays
// open until the reader is closed; s.busy blocks other commands meanwhile.
func (s *Session) fetchLiteral(ctx context.Context, folder string, uid uint32, opts *imap.FetchOptions) (io.ReadCloser, int64, error) {
	start := time.Now()
	cmd := s.c.Fetch(imap.UIDSetNum(imap.UID(uid)), opts)
	fail := func(err error) (io.ReadCloser, int64, error) {
		closeErr := cmd.Close()
		if err == nil {
			err = closeErr
		}
		logCmd(logging.From(ctx), "UID FETCH", folder, 1, start, err)
		if err == nil {
			err = ErrNotFound
		}
		var ie *imap.Error
		if errors.As(err, &ie) {
			return nil, 0, fmt.Errorf("imap fetch: %w", err)
		}
		return nil, 0, err
	}
	msg := cmd.Next()
	if msg == nil {
		return fail(nil)
	}
	// Stop at the literal: calling msg.Next again would discard it.
	var lit imap.LiteralReader
	for lit == nil {
		item := msg.Next()
		if item == nil {
			break
		}
		switch it := item.(type) {
		case imapclient.FetchItemDataBodySection:
			lit = it.Literal
		case imapclient.FetchItemDataBinarySection:
			lit = it.Literal
		}
	}
	if lit == nil {
		return fail(nil)
	}
	s.busy = true
	return &literalReader{r: lit, close: func() error {
		err := cmd.Close() // drains the rest of the literal and the response
		s.busy = false
		logCmd(logging.From(ctx), "UID FETCH", folder, 1, start, err)
		return err
	}}, lit.Size(), nil
}

type literalReader struct {
	r      io.Reader
	close  func() error
	closed bool
}

func (l *literalReader) Read(p []byte) (int, error) {
	if l.closed {
		return 0, errors.New("read on closed imap reader")
	}
	return l.r.Read(p)
}

func (l *literalReader) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	return l.close()
}

// decodeTransfer wraps r with a streaming decoder for enc. Unknown
// encodings (7bit, 8bit, binary, other) pass through unchanged.
func decodeTransfer(r io.ReadCloser, enc string) io.ReadCloser {
	var dec io.Reader
	switch enc {
	case "base64":
		// The decoder skips CR and LF itself.
		dec = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		dec = quotedprintable.NewReader(r)
	default:
		return r
	}
	return struct {
		io.Reader
		io.Closer
	}{dec, r}
}

// Thread returns the UIDs (ascending) of the thread that holds uid in
// folder, from UID THREAD REFERENCES. It returns ErrUnsupported when the
// server lacks THREAD=REFERENCES and ErrNotFound when uid is in no thread.
func (s *Session) Thread(ctx context.Context, folder string, uid uint32) ([]uint32, error) {
	if !slices.Contains(s.c.Caps().ThreadAlgorithms(), imap.ThreadReferences) {
		return nil, ErrUnsupported
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if _, err := s.examine(ctx, folder); err != nil {
		return nil, err
	}
	start := time.Now()
	threads, err := s.c.UIDThread(&imapclient.ThreadOptions{
		Algorithm:      imap.ThreadReferences,
		SearchCriteria: &imap.SearchCriteria{},
	}).Wait()
	logCmd(logging.From(ctx), "UID THREAD", folder, 0, start, err)
	if err != nil {
		return nil, fmt.Errorf("imap thread: %w", err)
	}
	for _, t := range threads {
		uids := flattenThread(t, nil)
		if slices.Contains(uids, uid) {
			slices.Sort(uids)
			return uids, nil
		}
	}
	return nil, ErrNotFound
}

func flattenThread(t imapclient.ThreadData, out []uint32) []uint32 {
	out = append(out, t.Chain...)
	for _, sub := range t.SubThreads {
		out = flattenThread(sub, out)
	}
	return out
}

// ready refuses a new command while the context is done or a reader from
// FetchRaw/FetchPart is still open.
func (s *Session) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.busy {
		return errBusy
	}
	if s.broken {
		return errBroken
	}
	return nil
}
