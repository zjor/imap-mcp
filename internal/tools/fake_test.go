package tools

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
)

// fakeMsg is one message in fakeMailbox.
type fakeMsg struct {
	date   time.Time
	hasAtt bool
	raw    string // full message for FetchRaw; generated when empty
}

// fakeMailbox is an in-memory mailbox. Search ignores criteria (every
// message matches); THREAD returns every UID of the folder.
type fakeMailbox struct {
	folders map[string]map[uint32]fakeMsg
	refuse  map[string]bool // EXAMINE answers tagged NO
	sent    string

	summaryUIDs []uint32 // every UID passed to FetchSummaries
	rawReads    int
}

func (f *fakeMailbox) ListFolders(context.Context) ([]imapsafe.Folder, error) {
	var out []imapsafe.Folder
	for name := range f.folders {
		out = append(out, imapsafe.Folder{Name: name, Delimiter: "/"})
	}
	for name := range f.refuse {
		out = append(out, imapsafe.Folder{Name: name, Delimiter: "/"})
	}
	slices.SortFunc(out, func(a, b imapsafe.Folder) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (f *fakeMailbox) Folders() imapsafe.SpecialFolders { return imapsafe.SpecialFolders{Sent: f.sent} }

func (f *fakeMailbox) Examine(_ context.Context, folder string) (uint32, error) {
	if f.refuse[folder] {
		return 0, fmt.Errorf("imap examine %q: %w", folder, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "no rights"})
	}
	if _, ok := f.folders[folder]; !ok {
		return 0, fmt.Errorf("imap examine: %w", &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent})
	}
	return 7, nil
}

func (f *fakeMailbox) uids(folder string) []uint32 {
	var out []uint32
	for u := range f.folders[folder] {
		out = append(out, u)
	}
	slices.Sort(out)
	return out
}

func (f *fakeMailbox) Search(_ context.Context, folder string, _ imapsafe.SearchCriteria, before uint32, _ int) ([]uint32, error) {
	var out []uint32
	for _, u := range f.uids(folder) {
		if before == 0 || u < before {
			out = append(out, u)
		}
	}
	slices.Reverse(out)
	return out, nil
}

func (f *fakeMailbox) SearchHeader(_ context.Context, folder, _, _ string) ([]uint32, error) {
	return f.uids(folder), nil
}

func (f *fakeMailbox) Thread(_ context.Context, folder string, _ uint32) ([]uint32, error) {
	return f.uids(folder), nil
}

func (f *fakeMailbox) HasAttachments(_ context.Context, folder string, uids []uint32) (map[uint32]bool, error) {
	out := map[uint32]bool{}
	for _, u := range uids {
		if m, ok := f.folders[folder][u]; ok {
			out[u] = m.hasAtt
		}
	}
	return out, nil
}

func (f *fakeMailbox) FetchSummaries(_ context.Context, folder string, uids []uint32) ([]imapsafe.Summary, error) {
	f.summaryUIDs = append(f.summaryUIDs, uids...)
	return f.FetchEnvelopes(context.Background(), folder, uids)
}

func (f *fakeMailbox) FetchEnvelopes(_ context.Context, folder string, uids []uint32) ([]imapsafe.Summary, error) {
	var out []imapsafe.Summary
	for _, u := range uids {
		if m, ok := f.folders[folder][u]; ok {
			out = append(out, imapsafe.Summary{UID: u, Date: m.date, HasAttachments: m.hasAtt, MessageID: fmt.Sprintf("%s-%d@x", folder, u)})
		}
	}
	return out, nil
}

func (f *fakeMailbox) FetchRaw(_ context.Context, id ids.MessageID) (io.ReadCloser, error) {
	m, ok := f.folders[id.Folder][id.UID]
	if !ok {
		return nil, imapsafe.ErrNotFound
	}
	f.rawReads++
	raw := m.raw
	if raw == "" {
		raw = fmt.Sprintf("Message-ID: <%s-%d@x>\r\nDate: %s\r\nSubject: s\r\n\r\nbody %d\r\n", id.Folder, id.UID, m.date.Format(time.RFC1123Z), id.UID)
	}
	return io.NopCloser(strings.NewReader(raw)), nil
}
