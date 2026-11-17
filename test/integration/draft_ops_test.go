//go:build integration

package integration

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"testing/iotest"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/imapsafe"
)

const draftsFolder = "Drafts"

func ownDraft(id string) []byte {
	return []byte("From: a@example.com\r\nSubject: draft\r\nX-IMAP-MCP-Draft-Id: " + id +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n")
}

// appendOwn stores an own draft through imapsafe and returns its UID.
func appendOwn(t *testing.T, s *imapsafe.Session, id string) uint32 {
	t.Helper()
	ctx, _ := testCtx(t)
	raw := ownDraft(id)
	uid, err := s.AppendDraft(ctx, int64(len(raw)), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
	return uid
}

// rawUIDs lists the UIDs in folder with a raw client (EXAMINE).
func rawUIDs(t *testing.T, c imapsafe.Credentials, folder string) []uint32 {
	t.Helper()
	cl := rawClient(t, c)
	defer cl.Close()
	if _, err := cl.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("raw examine: %v", err)
	}
	data, err := cl.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatalf("raw search: %v", err)
	}
	var out []uint32
	for _, u := range data.AllUIDs() {
		out = append(out, uint32(u))
	}
	return out
}

func draftsUV(t *testing.T, s *imapsafe.Session) uint32 {
	t.Helper()
	ctx, _ := testCtx(t)
	uv, err := s.Examine(ctx, draftsFolder)
	if err != nil {
		t.Fatal(err)
	}
	return uv
}

func TestAppendDraft_FlagsAndFolder(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	if s.Folders().Drafts != draftsFolder {
		t.Fatalf("drafts folder %q", s.Folders().Drafts)
	}
	ctx, logs := testCtx(t)
	raw := ownDraft("flags-1")
	uid, err := s.AppendDraft(ctx, int64(len(raw)), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rawUIDs(t, c, draftsFolder), uid) {
		t.Fatalf("uid %d not in Drafts", uid)
	}
	flags := rawFlags(t, c, draftsFolder, uid)
	if !slices.Contains(flags, imap.FlagDraft) || !slices.Contains(flags, imap.FlagSeen) {
		t.Fatalf("flags = %v, want \\Draft and \\Seen", flags)
	}
	if len(rawUIDs(t, c, "INBOX")) != 0 {
		t.Fatal("INBOX not empty")
	}
	if !bytes.Contains(logs.Bytes(), []byte("draft.appended")) {
		t.Fatal("no draft.appended audit line")
	}
}

func TestAppendDraft_ShortReaderLeavesNothing(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	raw := ownDraft("short-1")
	// The reader fails half-way: the literal is never completed.
	r := iotest.TimeoutReader(iotest.HalfReader(bytes.NewReader(raw)))
	if _, err := s.AppendDraft(ctx, int64(len(raw))+100, r); err == nil {
		t.Fatal("append with a short reader succeeded")
	}
	if _, err := s.Examine(ctx, draftsFolder); err == nil {
		t.Fatal("session still usable after an aborted APPEND")
	}
	if n := len(rawUIDs(t, c, draftsFolder)); n != 0 {
		t.Fatalf("Drafts has %d messages, want 0", n)
	}
}

func TestAppendDraft_LongReaderLeavesNothing(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	raw := ownDraft("long-1")
	if _, err := s.AppendDraft(ctx, int64(len(raw))-3, bytes.NewReader(raw)); err == nil {
		t.Fatal("append with a long reader succeeded")
	}
	if n := len(rawUIDs(t, c, draftsFolder)); n != 0 {
		t.Fatalf("Drafts has %d messages, want 0", n)
	}
}

func TestAppendDraft_TooLarge(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	if s.Caps().AppendLimit == 0 {
		t.Skip("server reports no APPENDLIMIT")
	}
	ctx, _ := testCtx(t)
	if _, err := s.AppendDraft(ctx, s.Caps().AppendLimit+1, bytes.NewReader(nil)); !errors.Is(err, imapsafe.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestFindDraft_ExactMatchOnly(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	abc := appendOwn(t, s, "abc")
	appendOwn(t, s, "abcd")
	appendOwn(t, s, "xabc")

	uid, uv, err := s.FindDraft(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if uid != abc || uv != draftsUV(t, s) {
		t.Fatalf("FindDraft(abc) = %d/%d, want %d", uid, uv, abc)
	}
	if _, _, err := s.FindDraft(ctx, "ab"); !errors.Is(err, imapsafe.ErrNotEditable) {
		t.Fatalf("FindDraft(ab) err = %v, want ErrNotEditable", err)
	}
}

func TestFindDraft_NewestCopy(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	appendOwn(t, s, "dup")
	newer := appendOwn(t, s, "dup")
	uid, _, err := s.FindDraft(ctx, "dup")
	if err != nil || uid != newer {
		t.Fatalf("FindDraft = %d, %v; want %d", uid, err, newer)
	}
}

func TestRemoveOwnDraft_RefusesForeignDraft(t *testing.T) {
	c := freshUser(t)
	foreign := seedRaw(t, c, draftsFolder, []byte("From: me@example.com\r\nSubject: mine\r\n\r\nby hand\r\n"))[0]
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	err := s.RemoveOwnDraft(ctx, "any-id", draftsUV(t, s), foreign)
	if !errors.Is(err, imapsafe.ErrNotEditable) {
		t.Fatalf("err = %v, want ErrNotEditable", err)
	}
	if !slices.Contains(rawUIDs(t, c, draftsFolder), foreign) {
		t.Fatal("foreign draft was removed")
	}
	if slices.Contains(rawFlags(t, c, draftsFolder, foreign), imap.FlagDeleted) {
		t.Fatal("foreign draft got \\Deleted")
	}
}

func TestRemoveOwnDraft_KeepsOtherDeleted(t *testing.T) {
	c := freshUser(t)
	foreign := seedRaw(t, c, draftsFolder, []byte("From: me@example.com\r\nSubject: old\r\n\r\nold\r\n"))[0]
	// The user's client flagged it \Deleted but did not expunge.
	cl := rawClient(t, c)
	if _, err := cl.Select(draftsFolder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := cl.Store(imap.UIDSetNum(imap.UID(foreign)), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	cl.Logout().Wait()
	cl.Close()

	s, _ := openSession(t, c)
	ctx, logs := testCtx(t)
	own := appendOwn(t, s, "own-1")
	uid, uv, err := s.FindDraft(ctx, "own-1")
	if err != nil || uid != own {
		t.Fatalf("FindDraft = %d, %v", uid, err)
	}
	if err := s.RemoveOwnDraft(ctx, "own-1", uv, uid); err != nil {
		t.Fatal(err)
	}
	// Reads still work after the removal (the session re-EXAMINEs).
	if _, _, err := s.FindDraft(ctx, "own-1"); !errors.Is(err, imapsafe.ErrNotEditable) {
		t.Fatalf("FindDraft after removal err = %v, want ErrNotEditable", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	uids := rawUIDs(t, c, draftsFolder)
	if !slices.Equal(uids, []uint32{foreign}) {
		t.Fatalf("Drafts UIDs = %v, want [%d]", uids, foreign)
	}
	if !slices.Contains(rawFlags(t, c, draftsFolder, foreign), imap.FlagDeleted) {
		t.Fatal("foreign message lost \\Deleted")
	}
	if !bytes.Contains(logs.Bytes(), []byte("draft.removed")) {
		t.Fatal("no draft.removed audit line")
	}
}

func TestRemoveOwnDraft_UIDMismatch(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	a := appendOwn(t, s, "draft-a")
	b := appendOwn(t, s, "draft-b")
	err := s.RemoveOwnDraft(ctx, "draft-a", draftsUV(t, s), b)
	if !errors.Is(err, imapsafe.ErrNotEditable) {
		t.Fatalf("err = %v, want ErrNotEditable", err)
	}
	if uids := rawUIDs(t, c, draftsFolder); !slices.Equal(uids, []uint32{a, b}) {
		t.Fatalf("Drafts UIDs = %v, want [%d %d]", uids, a, b)
	}
}

func TestRemoveOwnDraft_StaleUIDValidity(t *testing.T) {
	c := freshUser(t)
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	a := appendOwn(t, s, "draft-a")
	err := s.RemoveOwnDraft(ctx, "draft-a", draftsUV(t, s)+1, a)
	if !errors.Is(err, imapsafe.ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if uids := rawUIDs(t, c, draftsFolder); !slices.Equal(uids, []uint32{a}) {
		t.Fatalf("Drafts UIDs = %v, want [%d]", uids, a)
	}
}

func TestListDrafts_MarksOwnDrafts(t *testing.T) {
	c := freshUser(t)
	foreign := seedRaw(t, c, draftsFolder, []byte("From: me@example.com\r\nSubject: mine\r\n\r\nby hand\r\n"))[0]
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	own := appendOwn(t, s, "own-1")

	list, err := s.ListDrafts(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].UID != own || list[1].UID != foreign {
		t.Fatalf("list = %+v", list)
	}
	if list[0].DraftID != "own-1" || !list[0].Editable {
		t.Fatalf("own draft = %+v", list[0])
	}
	if list[1].DraftID != "" || list[1].Editable {
		t.Fatalf("foreign draft = %+v", list[1])
	}
	page, err := s.ListDrafts(ctx, own, 10)
	if err != nil || len(page) != 1 || page[0].UID != foreign {
		t.Fatalf("second page = %+v, %v", page, err)
	}
}
