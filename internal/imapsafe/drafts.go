package imapsafe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/zjor/imap-mcp/internal/drafts"
	"github.com/zjor/imap-mcp/internal/logging"
)

// Draft operations (spec 7.3, 7.6). They hold the only writes in this
// package:
//
//   - APPEND to Folders().Drafts with (\Draft \Seen);
//   - SELECT Drafts, UID STORE <uid> +FLAGS.SILENT (\Deleted) and
//     UID EXPUNGE <uid> for one UID whose X-IMAP-MCP-Draft-Id header equals
//     the draft id, and only with UIDPLUS.
//
// Plain EXPUNGE and CLOSE are never sent: both remove every \Deleted message
// in the folder, including the user's own. After a removal the session
// leaves Drafts selected read-write and forgets the examined folder, so the
// next read sends EXAMINE again. That is safe: SELECT, EXAMINE and LOGOUT
// never expunge (RFC 3501 6.3.1, 6.1.3), and the only flag change this
// package ever sends is the STORE above.

var (
	// ErrNotEditable means no draft with this id exists in Drafts, or the
	// message lacks the marker header, so the server must not change it.
	ErrNotEditable = errors.New("draft not found or not editable")
	// ErrTooLarge means the message exceeds the server's APPEND limit.
	ErrTooLarge = errors.New("draft is larger than the server accepts")

	errNoDrafts = errors.New("the server has no Drafts folder; set IMAP_MCP_DRAFTS_FOLDER")
	errBroken   = errors.New("imap session closed after an aborted APPEND")
	errAborted  = errors.New("append aborted")
)

// headCapture is how many leading bytes of an appended message AppendDraft
// keeps to find the marker header when the server returns no APPENDUID.
const headCapture = 64 << 10

// DraftSummary is the list view of one message in Drafts. DraftID is the
// marker header value ("" when there is none). Editable is true when the
// marker is present and the server has UIDPLUS, so update and delete work.
type DraftSummary struct {
	Summary
	DraftID  string
	Editable bool
}

// wire is the IMAP command surface the draft operations and EXAMINE use.
// clientWire is the real one; tests record calls with a fake. Commands
// other than append and selectFolder act on the selected folder.
type wire interface {
	selectFolder(folder string, readOnly bool) (uidValidity uint32, err error)
	searchHeader(header, value string) ([]uint32, error)
	fetchHeader(uids []uint32, field string) (map[uint32][]byte, error)
	// append sends exactly size bytes from r as the literal. When r gives
	// fewer or more bytes it closes the connection before the command ends,
	// so the server stores nothing, and returns an error wrapping errAborted.
	// uid is 0 when the server sends no APPENDUID.
	append(folder string, flags []imap.Flag, size int64, r io.Reader) (uid, uidValidity uint32, err error)
	storeDeleted(uid uint32) error
	uidExpunge(uid uint32) error
}

type clientWire struct{ c *imapclient.Client }

func (w clientWire) selectFolder(folder string, readOnly bool) (uint32, error) {
	data, err := w.c.Select(folder, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		return 0, err
	}
	return data.UIDValidity, nil
}

func (w clientWire) searchHeader(header, value string) ([]uint32, error) {
	data, err := w.c.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: header, Value: value}},
	}, nil).Wait()
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, u := range data.AllUIDs() {
		out = append(out, uint32(u))
	}
	return out, nil
}

func (w clientWire) fetchHeader(uids []uint32, field string) (map[uint32][]byte, error) {
	var set imap.UIDSet
	for _, u := range uids {
		set.AddNum(imap.UID(u))
	}
	sec := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{field},
		Peek:         true,
	}
	msgs, err := w.c.Fetch(set, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{sec},
	}).Collect()
	if err != nil {
		return nil, err
	}
	out := make(map[uint32][]byte, len(msgs))
	for _, m := range msgs {
		out[uint32(m.UID)] = m.FindBodySection(sec)
	}
	return out, nil
}

func (w clientWire) append(folder string, flags []imap.Flag, size int64, r io.Reader) (uint32, uint32, error) {
	cmd := w.c.Append(folder, size, &imap.AppendOptions{Flags: flags})
	n, err := io.Copy(cmd, io.LimitReader(r, size))
	if err == nil && n < size {
		err = fmt.Errorf("message has %d bytes, expected %d", n, size)
	}
	if err == nil {
		var b [1]byte
		if m, _ := io.ReadFull(r, b[:]); m > 0 {
			err = fmt.Errorf("message has more than %d bytes", size)
		}
	}
	if err != nil {
		// The literal is incomplete or must not be finished. Closing the
		// connection before the command's final CRLF makes the server drop
		// the APPEND. Ending the command instead would desync the stream.
		w.c.Close()
		return 0, 0, fmt.Errorf("%w: %v", errAborted, err)
	}
	if err := cmd.Close(); err != nil {
		w.c.Close()
		return 0, 0, fmt.Errorf("%w: %v", errAborted, err)
	}
	data, err := cmd.Wait()
	if err != nil {
		return 0, 0, err
	}
	return uint32(data.UID), data.UIDValidity, nil
}

func (w clientWire) storeDeleted(uid uint32) error {
	return w.c.Store(imap.UIDSetNum(imap.UID(uid)), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil).Close()
}

func (w clientWire) uidExpunge(uid uint32) error {
	return w.c.UIDExpunge(imap.UIDSetNum(imap.UID(uid))).Close()
}

// AppendDraft stores a message of exactly size bytes from r in the Drafts
// folder with the flags \Draft and \Seen, and returns its UID. It returns
// ErrTooLarge when size exceeds the server's APPENDLIMIT. The UID comes from
// APPENDUID; without it, AppendDraft looks the message up by its marker
// header. When r gives fewer or more than size bytes, the server stores
// nothing, the call fails and the session is closed.
func (s *Session) AppendDraft(ctx context.Context, size int64, r io.Reader) (uint32, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	folder, err := s.draftsFolder()
	if err != nil {
		return 0, err
	}
	if size <= 0 {
		return 0, errors.New("draft is empty")
	}
	if s.caps.AppendLimit > 0 && size > s.caps.AppendLimit {
		return 0, ErrTooLarge
	}
	log := logging.From(ctx)
	head := &limitedBuffer{max: headCapture}
	start := time.Now()
	uid, _, err := s.w.append(folder, []imap.Flag{imap.FlagDraft, imap.FlagSeen}, size, io.TeeReader(r, head))
	logCmd(log, "APPEND", folder, 1, start, err)
	if err != nil {
		if errors.Is(err, errAborted) {
			s.broken = true
		}
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Code == imap.ResponseCodeTooBig {
			return 0, ErrTooLarge
		}
		return 0, fmt.Errorf("imap append: %w", err)
	}
	if uid == 0 {
		id := markerOf(head.Bytes())
		if id == "" {
			return 0, errors.New("appended draft has no marker header")
		}
		if _, err := s.examine(ctx, folder); err != nil {
			return 0, err
		}
		if uid, err = s.findMarked(ctx, folder, id); err != nil {
			return 0, fmt.Errorf("find appended draft: %w", err)
		}
	}
	log.Info("draft.appended", "uid", uid, "size", size)
	return uid, nil
}

// FindDraft opens Drafts with EXAMINE and returns the UID of the newest
// message whose marker header equals draftID exactly, together with the
// folder's UIDVALIDITY. It returns ErrNotEditable when there is none.
func (s *Session) FindDraft(ctx context.Context, draftID string) (uid, uidValidity uint32, err error) {
	if err := s.ready(ctx); err != nil {
		return 0, 0, err
	}
	folder, err := s.draftsFolder()
	if err != nil {
		return 0, 0, err
	}
	if draftID == "" {
		return 0, 0, ErrNotEditable
	}
	uv, err := s.examine(ctx, folder)
	if err != nil {
		return 0, 0, err
	}
	uid, err = s.findMarked(ctx, folder, draftID)
	if err != nil {
		return 0, 0, err
	}
	return uid, uv, nil
}

// findMarked searches the open folder for draftID and checks each hit's
// marker exactly, because SEARCH HEADER matches substrings.
func (s *Session) findMarked(ctx context.Context, folder, draftID string) (uint32, error) {
	log := logging.From(ctx)
	start := time.Now()
	uids, err := s.w.searchHeader(drafts.MarkerHeader, draftID)
	logCmd(log, "UID SEARCH", folder, len(uids), start, err)
	if err != nil {
		return 0, fmt.Errorf("imap search: %w", err)
	}
	if len(uids) == 0 {
		return 0, ErrNotEditable
	}
	markers, err := s.markers(ctx, folder, uids)
	if err != nil {
		return 0, err
	}
	slices.Sort(uids)
	for _, u := range slices.Backward(uids) {
		if markers[u] == draftID {
			return u, nil
		}
	}
	return 0, ErrNotEditable
}

// markers fetches the marker header of uids in the open folder. A UID
// without a single marker maps to "".
func (s *Session) markers(ctx context.Context, folder string, uids []uint32) (map[uint32]string, error) {
	start := time.Now()
	raw, err := s.w.fetchHeader(uids, drafts.MarkerHeader)
	logCmd(logging.From(ctx), "UID FETCH", folder, len(uids), start, err)
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}
	out := make(map[uint32]string, len(raw))
	for u, h := range raw {
		out[u] = markerOf(h)
	}
	return out, nil
}

// RemoveOwnDraft removes the message uid from Drafts, but only when the
// server has UIDPLUS (else ErrUnsupported), Drafts still has uidValidity
// (else ErrStale) and the message's marker header equals draftID (else
// ErrNotEditable). It sends SELECT Drafts, UID STORE uid +FLAGS.SILENT
// (\Deleted) and UID EXPUNGE uid, so other \Deleted messages stay.
func (s *Session) RemoveOwnDraft(ctx context.Context, draftID string, uidValidity, uid uint32) error {
	if !s.caps.UIDPlus {
		return ErrUnsupported
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	folder, err := s.draftsFolder()
	if err != nil {
		return err
	}
	if draftID == "" || uid == 0 {
		return ErrNotEditable
	}
	log := logging.From(ctx)

	// SELECT replaces the examined folder; later reads must EXAMINE again.
	s.examined = ""
	start := time.Now()
	uv, err := s.w.selectFolder(folder, false)
	logCmd(log, "SELECT", folder, 0, start, err)
	if err != nil {
		return fmt.Errorf("imap select %q: %w", folder, err)
	}
	if uv != uidValidity {
		logWarnStale(ctx, folder)
		return ErrStale
	}
	markers, err := s.markers(ctx, folder, []uint32{uid})
	if err != nil {
		return err
	}
	if markers[uid] != draftID {
		log.Warn("draft.not_editable", "uid", uid, "draft_id", draftID)
		return ErrNotEditable
	}

	start = time.Now()
	err = s.w.storeDeleted(uid)
	logCmd(log, "UID STORE", folder, 1, start, err)
	if err != nil {
		return fmt.Errorf("imap store: %w", err)
	}
	start = time.Now()
	err = s.w.uidExpunge(uid)
	logCmd(log, "UID EXPUNGE", folder, 1, start, err)
	if err != nil {
		return fmt.Errorf("imap expunge: %w", err)
	}
	log.Info("draft.removed", "uid", uid, "draft_id", draftID)
	return nil
}

// ListDrafts lists messages in Drafts newest first, like Search with
// beforeUID and limit, with each message's marker.
func (s *Session) ListDrafts(ctx context.Context, beforeUID uint32, limit int) ([]DraftSummary, error) {
	folder, err := s.draftsFolder()
	if err != nil {
		return nil, err
	}
	uids, err := s.Search(ctx, folder, SearchCriteria{}, beforeUID, limit)
	if err != nil {
		return nil, err
	}
	sums, err := s.FetchSummaries(ctx, folder, uids)
	if err != nil || len(sums) == 0 {
		return nil, err
	}
	markers, err := s.markers(ctx, folder, uids)
	if err != nil {
		return nil, err
	}
	out := make([]DraftSummary, len(sums))
	for i, sum := range sums {
		id := markers[sum.UID]
		out[i] = DraftSummary{Summary: sum, DraftID: id, Editable: id != "" && s.caps.UIDPlus}
	}
	return out, nil
}

func (s *Session) draftsFolder() (string, error) {
	if s.folders.Drafts == "" {
		return "", errNoDrafts
	}
	return s.folders.Drafts, nil
}

// markerOf returns the marker value of a header block, or "" when the
// block has no marker or more than one.
func markerOf(header []byte) string {
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(header))).ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	vals := h.Values(drafts.MarkerHeader)
	if len(vals) != 1 {
		return ""
	}
	return vals[0]
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
