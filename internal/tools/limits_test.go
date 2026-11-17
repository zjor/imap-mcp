package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/ids"
)

var day0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// folderOf builds n messages with UIDs 1..n, one day apart; att reports
// which UIDs have attachments.
func folderOf(n int, att func(uid uint32) bool) map[uint32]fakeMsg {
	f := map[uint32]fakeMsg{}
	for u := uint32(1); u <= uint32(n); u++ {
		f[u] = fakeMsg{date: day0.AddDate(0, 0, int(u)), hasAtt: att != nil && att(u)}
	}
	return f
}

func runSearch(t *testing.T, mb *fakeMailbox, in searchIn) searchOut {
	t.Helper()
	out, _, err := searchEmails(context.Background(), mb, in)
	if err != nil {
		t.Fatalf("search %+v: %v", in, err)
	}
	return out.(searchOut)
}

func uidsOf(t *testing.T, out searchOut) []uint32 {
	t.Helper()
	var u []uint32
	for _, m := range out.Messages {
		id, err := ids.DecodeMessage(m.ID)
		if err != nil {
			t.Fatal(err)
		}
		u = append(u, id.UID)
	}
	return u
}

func TestSearchAll_SkipsRefusedFolder(t *testing.T) {
	mb := &fakeMailbox{
		folders: map[string]map[uint32]fakeMsg{"INBOX": folderOf(3, nil), "Sent": folderOf(2, nil)},
		refuse:  map[string]bool{"Private": true},
	}
	out := runSearch(t, mb, searchIn{Folder: "all"})
	if len(out.Messages) != 5 || out.NextCursor != nil {
		t.Fatalf("got %d messages, cursor %v; want 5, none", len(out.Messages), out.NextCursor)
	}

	// Paging in "all" mode keeps skipping it.
	out = runSearch(t, mb, searchIn{Folder: "all", Limit: 2})
	c := cursor{All: true, Folders: []cursorFolder{{Name: "Private", UIDValidity: 7, Before: 9}}}
	c.Folders = append(c.Folders, mustDecode(t, *out.NextCursor).Folders...)
	out = runSearch(t, mb, searchIn{Cursor: c.encode(), Limit: 10})
	if len(out.Messages) != 3 {
		t.Errorf("page 2 = %d messages, want 3", len(out.Messages))
	}

	// A single refused folder is still an error.
	_, _, err := searchEmails(context.Background(), mb, searchIn{Folder: "Private"})
	var ie *imap.Error
	if !errors.As(err, &ie) {
		t.Errorf("single folder error = %v, want the tagged NO", err)
	}
}

func mustDecode(t *testing.T, s string) cursor {
	t.Helper()
	c, err := decodeCursor(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSearch_HasAttachmentsBudget(t *testing.T) {
	old := scanBudget
	scanBudget = 30
	defer func() { scanBudget = old }()

	// 100 messages; only UIDs 5 and 60 have attachments.
	mb := &fakeMailbox{folders: map[string]map[uint32]fakeMsg{
		"INBOX": folderOf(100, func(u uint32) bool { return u == 5 || u == 60 }),
	}}
	yes := true
	in := searchIn{HasAttachments: &yes, Limit: 5}

	var got []uint32
	pages := 0
	for {
		pages++
		mb.summaryUIDs = nil
		out := runSearch(t, mb, in)
		got = append(got, uidsOf(t, out)...)
		// Summaries are fetched only for messages that pass the filter.
		for _, u := range mb.summaryUIDs {
			if u != 5 && u != 60 {
				t.Errorf("page %d: summary fetched for non-matching uid %d", pages, u)
			}
		}
		if out.NextCursor == nil {
			break
		}
		in.Cursor = *out.NextCursor
		if pages > 10 {
			t.Fatal("too many pages")
		}
	}
	// 100 candidates / 30 per call: UID 60 on page 2, UID 5 on page 4.
	if !slices.Equal(got, []uint32{60, 5}) || pages != 4 {
		t.Errorf("uids = %v over %d pages, want [60 5] over 4", got, pages)
	}

	// First page: short (empty) with a cursor at the lowest scanned UID.
	out := runSearch(t, mb, searchIn{HasAttachments: &yes, Limit: 5})
	if len(out.Messages) != 0 || out.NextCursor == nil {
		t.Fatalf("page 1 = %d messages, cursor %v", len(out.Messages), out.NextCursor)
	}
	if c := mustDecode(t, *out.NextCursor); c.Folders[0].Before != 71 {
		t.Errorf("cursor bound = %d, want 71 (lowest scanned)", c.Folders[0].Before)
	}
}

func TestSearch_HasAttachmentsNoGapsNoDuplicates(t *testing.T) {
	old := scanBudget
	scanBudget = 7
	defer func() { scanBudget = old }()

	att := func(u uint32) bool { return u%3 == 0 }
	mb := &fakeMailbox{folders: map[string]map[uint32]fakeMsg{"INBOX": folderOf(50, att)}}
	yes := true
	in := searchIn{HasAttachments: &yes, Limit: 2}
	var got []uint32
	for range 100 {
		out := runSearch(t, mb, in)
		got = append(got, uidsOf(t, out)...)
		if out.NextCursor == nil {
			break
		}
		in.Cursor = *out.NextCursor
	}
	var want []uint32
	for u := uint32(50); u >= 1; u-- {
		if att(u) {
			want = append(want, u)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("uids = %v, want %v", got, want)
	}
}

func TestGetThread_CapsMessagesAndBodies(t *testing.T) {
	old := maxThreadMessages
	maxThreadMessages = 5
	defer func() { maxThreadMessages = old }()

	inbox := folderOf(8, nil)
	big := strings.Repeat("ž", maxThreadBody) // 2 bytes per rune
	inbox[8] = fakeMsg{date: day0.AddDate(0, 0, 8),
		raw: fmt.Sprintf("Message-ID: <big@x>\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", day0.AddDate(0, 0, 8).Format(time.RFC1123Z), big)}
	mb := &fakeMailbox{
		folders: map[string]map[uint32]fakeMsg{"INBOX": inbox, "Sent": folderOf(3, nil)},
		sent:    "Sent",
	}
	start := ids.MessageID{Folder: "INBOX", UIDValidity: 7, UID: 1}
	out, err := getThread(context.Background(), mb, Deps{}, start)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(out.Messages) != 5 {
		t.Fatalf("truncated = %v, %d messages; want true, 5", out.Truncated, len(out.Messages))
	}
	// The newest five (by date): INBOX 4..8 (Sent 1..3 are older), oldest first.
	var got []uint32
	for _, m := range out.Messages {
		id, _ := ids.DecodeMessage(m.ID)
		if id.Folder != "INBOX" {
			t.Errorf("unexpected %+v", id)
		}
		got = append(got, id.UID)
	}
	if !slices.Equal(got, []uint32{4, 5, 6, 7, 8}) {
		t.Errorf("uids = %v, want [4 5 6 7 8]", got)
	}
	// The start message (read first, then dropped by the cap) plus the five
	// kept; the others are never read in full.
	if mb.rawReads != 6 {
		t.Errorf("full reads = %d, want 6", mb.rawReads)
	}
	last := out.Messages[4]
	if !last.BodyTruncated || len(last.UntrustedContent.Body) > maxThreadBody || !strings.HasPrefix(big, last.UntrustedContent.Body) {
		t.Errorf("big body: truncated=%v len=%d", last.BodyTruncated, len(last.UntrustedContent.Body))
	}
	if out.Messages[0].BodyTruncated {
		t.Error("small body marked truncated")
	}

	// Under the cap: no truncated flag.
	maxThreadMessages = 50
	out, err = getThread(context.Background(), mb, Deps{}, start)
	if err != nil || out.Truncated || len(out.Messages) != 11 {
		t.Errorf("uncapped: %v, truncated=%v, %d messages", err, out.Truncated, len(out.Messages))
	}
}

func TestGetThread_SkipsUnparseableMember(t *testing.T) {
	inbox := folderOf(3, nil)
	inbox[2] = fakeMsg{date: day0.AddDate(0, 0, 2), raw: "Subject: Secret quarterly\r\nnumbers inside\r\n\r\nbody\r\n"}
	mb := &fakeMailbox{folders: map[string]map[uint32]fakeMsg{"INBOX": inbox}}
	out, err := getThread(context.Background(), mb, Deps{}, ids.MessageID{Folder: "INBOX", UIDValidity: 7, UID: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []uint32
	for _, m := range out.Messages {
		id, _ := ids.DecodeMessage(m.ID)
		got = append(got, id.UID)
	}
	if !slices.Equal(got, []uint32{1, 3}) {
		t.Errorf("uids = %v, want [1 3]", got)
	}
}

func TestReadEmail_UnparseableError(t *testing.T) {
	inbox := map[uint32]fakeMsg{1: {date: day0, raw: "Subject: Secret quarterly\r\nnumbers inside\r\n\r\nbody\r\n"}}
	mb := &fakeMailbox{folders: map[string]map[uint32]fakeMsg{"INBOX": inbox}}
	_, _, err := readMessage(context.Background(), mb, Deps{}, ids.MessageID{Folder: "INBOX", UIDValidity: 7, UID: 1}, "")
	code, msg := classify(err)
	if code != codeUnparseable || strings.Contains(msg, "numbers inside") {
		t.Errorf("classify(%v) = %q, %q", err, code, msg)
	}
}
