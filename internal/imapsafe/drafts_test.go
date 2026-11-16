package imapsafe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/logging"
)

// wireCall is one recorded command of fakeWire. Folder is the target folder
// for APPEND and SELECT/EXAMINE, and the selected folder for every other
// command.
type wireCall struct {
	Cmd      string
	Folder   string
	ReadOnly bool // selected read-only (EXAMINE) at the time of the call
	UIDs     []uint32
	Flags    []imap.Flag
}

type fakeMsg struct {
	uid    uint32
	header string // raw header block returned for the marker fetch
}

// fakeWire is a recording fake of the IMAP command surface. Each folder has
// a UIDVALIDITY and a list of messages.
type fakeWire struct {
	uv       map[string]uint32
	msgs     map[string][]fakeMsg
	selected string
	readOnly bool
	nextUID  uint32
	noUID    bool // APPEND returns no APPENDUID
	calls    []wireCall
}

func (f *fakeWire) rec(c wireCall) { f.calls = append(f.calls, c) }

func (f *fakeWire) selectFolder(folder string, readOnly bool) (uint32, error) {
	cmd := "SELECT"
	if readOnly {
		cmd = "EXAMINE"
	}
	f.rec(wireCall{Cmd: cmd, Folder: folder, ReadOnly: readOnly})
	f.selected, f.readOnly = folder, readOnly
	return f.uv[folder], nil
}

func (f *fakeWire) searchHeader(header, value string) ([]uint32, error) {
	f.rec(wireCall{Cmd: "UID SEARCH", Folder: f.selected, ReadOnly: f.readOnly})
	var out []uint32
	for _, m := range f.msgs[f.selected] {
		if strings.Contains(m.header, value) {
			out = append(out, m.uid)
		}
	}
	return out, nil
}

func (f *fakeWire) fetchHeader(uids []uint32, field string) (map[uint32][]byte, error) {
	f.rec(wireCall{Cmd: "UID FETCH", Folder: f.selected, ReadOnly: f.readOnly, UIDs: uids})
	out := map[uint32][]byte{}
	for _, m := range f.msgs[f.selected] {
		if slices.Contains(uids, m.uid) {
			out[m.uid] = []byte(m.header)
		}
	}
	return out, nil
}

func (f *fakeWire) append(folder string, flags []imap.Flag, size int64, r io.Reader) (uint32, uint32, error) {
	f.rec(wireCall{Cmd: "APPEND", Folder: folder, Flags: flags})
	raw, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return 0, 0, err
	}
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	var marker string
	for _, l := range strings.Split(head, "\r\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(k, "X-IMAP-MCP-Draft-Id") {
			marker = "X-IMAP-MCP-Draft-Id:" + v + "\r\n\r\n"
		}
	}
	f.nextUID++
	f.msgs[folder] = append(f.msgs[folder], fakeMsg{uid: f.nextUID, header: marker})
	if f.noUID {
		return 0, 0, nil
	}
	return f.nextUID, f.uv[folder], nil
}

func (f *fakeWire) storeDeleted(uid uint32) error {
	f.rec(wireCall{Cmd: "UID STORE", Folder: f.selected, ReadOnly: f.readOnly, UIDs: []uint32{uid}})
	return nil
}

func (f *fakeWire) uidExpunge(uid uint32) error {
	f.rec(wireCall{Cmd: "UID EXPUNGE", Folder: f.selected, ReadOnly: f.readOnly, UIDs: []uint32{uid}})
	msgs := f.msgs[f.selected]
	f.msgs[f.selected] = slices.DeleteFunc(msgs, func(m fakeMsg) bool { return m.uid == uid })
	return nil
}

func newFakeSession(uidPlus bool) (*Session, *fakeWire) {
	w := &fakeWire{
		uv: map[string]uint32{"Drafts": 7, "INBOX": 9},
		msgs: map[string][]fakeMsg{
			"Drafts": {
				{uid: 1, header: "X-IMAP-MCP-Draft-Id: own-1\r\n\r\n"},
				{uid: 2, header: "\r\n"}, // a draft the user wrote
			},
			"INBOX": {{uid: 1, header: "X-IMAP-MCP-Draft-Id: own-1\r\n\r\n"}},
		},
		nextUID: 10,
	}
	s := &Session{
		w:       w,
		caps:    Caps{UIDPlus: uidPlus},
		folders: SpecialFolders{Drafts: "Drafts", Sent: "Sent"},
		log:     discardLogger(),
	}
	return s, w
}

func draftMsg(id string) []byte {
	return []byte("X-IMAP-MCP-Draft-Id: " + id + "\r\nSubject: x\r\n\r\nbody\r\n")
}

// TestNoWriteOutsideDrafts checks the write surface of Session: every write
// command targets the Drafts folder, removal needs the marker, and Session
// has no exported method beyond the allowlist.
func TestNoWriteOutsideDrafts(t *testing.T) {
	ctx := logging.With(context.Background(), discardLogger())

	t.Run("method allowlist", func(t *testing.T) {
		// Adding an exported method to Session must break this test on
		// purpose: review it against spec 7.3, then extend the list.
		allow := []string{
			"AppendDraft", "Caps", "Close", "Examine", "FetchEnvelopes",
			"FetchPart", "FetchRaw", "FetchSummaries", "FindDraft",
			"Folders", "HasAttachments", "ListDrafts", "ListFolders",
			"RemoveOwnDraft", "Search", "SearchHeader", "Thread",
		}
		typ := reflect.TypeOf(&Session{})
		var got []string
		for i := range typ.NumMethod() {
			got = append(got, typ.Method(i).Name)
		}
		if !slices.Equal(got, allow) {
			t.Fatalf("exported *Session methods:\n got  %v\n want %v", got, allow)
		}
	})

	t.Run("scenarios", func(t *testing.T) {
		scenarios := []struct {
			name string
			run  func(s *Session, w *fakeWire) error
			want error
		}{
			{"append", func(s *Session, _ *fakeWire) error {
				raw := draftMsg("new-1")
				_, err := s.AppendDraft(ctx, int64(len(raw)), bytes.NewReader(raw))
				return err
			}, nil},
			{"append without APPENDUID", func(s *Session, w *fakeWire) error {
				w.noUID = true
				raw := draftMsg("new-2")
				uid, err := s.AppendDraft(ctx, int64(len(raw)), bytes.NewReader(raw))
				if err == nil && uid != 11 {
					return fmt.Errorf("uid %d, want 11", uid)
				}
				return err
			}, nil},
			{"remove own", func(s *Session, _ *fakeWire) error {
				return s.RemoveOwnDraft(ctx, "own-1", 7, 1)
			}, nil},
			{"remove foreign", func(s *Session, _ *fakeWire) error {
				return s.RemoveOwnDraft(ctx, "own-1", 7, 2)
			}, ErrNotEditable},
			{"remove with empty id", func(s *Session, _ *fakeWire) error {
				return s.RemoveOwnDraft(ctx, "", 7, 2)
			}, ErrNotEditable},
			{"remove after uidvalidity change", func(s *Session, _ *fakeWire) error {
				return s.RemoveOwnDraft(ctx, "own-1", 6, 1)
			}, ErrStale},
			{"find then remove", func(s *Session, _ *fakeWire) error {
				uid, uv, err := s.FindDraft(ctx, "own-1")
				if err != nil {
					return err
				}
				return s.RemoveOwnDraft(ctx, "own-1", uv, uid)
			}, nil},
		}
		for _, sc := range scenarios {
			t.Run(sc.name, func(t *testing.T) {
				s, w := newFakeSession(true)
				if err := sc.run(s, w); !errors.Is(err, sc.want) {
					t.Fatalf("err = %v, want %v", err, sc.want)
				}
				checkCalls(t, w.calls)
				if sc.want != nil {
					for _, c := range w.calls {
						if c.Cmd == "UID STORE" || c.Cmd == "UID EXPUNGE" {
							t.Fatalf("refused call still sent %s", c.Cmd)
						}
					}
				}
				if len(w.msgs["INBOX"]) != 1 {
					t.Fatal("INBOX changed")
				}
			})
		}
	})

	t.Run("remove without UIDPLUS sends nothing", func(t *testing.T) {
		s, w := newFakeSession(false)
		if err := s.RemoveOwnDraft(ctx, "own-1", 7, 1); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported", err)
		}
		if len(w.calls) != 0 {
			t.Fatalf("calls = %v, want none", w.calls)
		}
	})

	t.Run("append over APPENDLIMIT sends nothing", func(t *testing.T) {
		s, w := newFakeSession(true)
		s.caps.AppendLimit = 10
		raw := draftMsg("big")
		if _, err := s.AppendDraft(ctx, int64(len(raw)), bytes.NewReader(raw)); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
		if len(w.calls) != 0 {
			t.Fatalf("calls = %v, want none", w.calls)
		}
	})

	t.Run("remove marks examined folder as gone", func(t *testing.T) {
		s, w := newFakeSession(true)
		if _, err := s.Examine(ctx, "Drafts"); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveOwnDraft(ctx, "own-1", 7, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Examine(ctx, "Drafts"); err != nil {
			t.Fatal(err)
		}
		last := w.calls[len(w.calls)-1]
		if last.Cmd != "EXAMINE" || last.Folder != "Drafts" {
			t.Fatalf("read after removal sent %+v, want EXAMINE Drafts", last)
		}
	})
}

// checkCalls enforces the write rules of spec 7.3 on a recorded call list.
func checkCalls(t *testing.T, calls []wireCall) {
	t.Helper()
	var selectedRW string // folder SELECTed read-write, "" otherwise
	for _, c := range calls {
		switch c.Cmd {
		case "APPEND":
			if c.Folder != "Drafts" || !slices.Contains(c.Flags, imap.FlagDraft) {
				t.Fatalf("APPEND to %q with %v", c.Folder, c.Flags)
			}
		case "SELECT":
			if c.Folder != "Drafts" {
				t.Fatalf("SELECT %q", c.Folder)
			}
			selectedRW = c.Folder
		case "EXAMINE":
			selectedRW = ""
		case "UID STORE", "UID EXPUNGE":
			if selectedRW != "Drafts" || c.ReadOnly || len(c.UIDs) != 1 {
				t.Fatalf("%s on %q (read-only %v) uids %v", c.Cmd, c.Folder, c.ReadOnly, c.UIDs)
			}
		case "UID SEARCH", "UID FETCH":
		default:
			t.Fatalf("unexpected command %q", c.Cmd)
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
