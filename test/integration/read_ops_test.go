//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
)

// rawFlags reads the flags of uid in folder with a raw client (EXAMINE, so
// the check itself changes nothing).
func rawFlags(t *testing.T, c imapsafe.Credentials, folder string, uid uint32) []imap.Flag {
	t.Helper()
	cl := rawClient(t, c)
	defer cl.Close()
	if _, err := cl.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("raw examine: %v", err)
	}
	msgs, err := cl.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("raw fetch flags: %v (%d msgs)", err, len(msgs))
	}
	return msgs[0].Flags
}

func openSession(t *testing.T, c imapsafe.Credentials) (*imapsafe.Session, *bytes.Buffer) {
	t.Helper()
	ctx, logs := testCtx(t)
	s, err := testDialer(t).Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, logs
}

func TestFetch_DoesNotSetSeen(t *testing.T) {
	c := freshUser(t)
	uid := seed(t, c, "INBOX", "with-attachment.eml")[0]
	ctx, logs := testCtx(t)
	s, err := testDialer(t).Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	uv, err := s.Examine(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	sums, err := s.FetchSummaries(ctx, "INBOX", []uint32{uid})
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Fatalf("summaries = %d, want 1", len(sums))
	}
	sum := sums[0]
	if sum.UID != uid || !sum.Unread || !sum.HasAttachments || sum.Subject != "Report attached" ||
		sum.MessageID != "att-1@example.com" || len(sum.From) != 1 || sum.From[0].Addr != "alice@example.com" ||
		sum.InternalDate.IsZero() || sum.Date.IsZero() || !bytes.Contains(sum.Snippet, []byte("Please see the report.")) {
		t.Errorf("summary = %+v", sum)
	}

	id := ids.MessageID{Folder: "INBOX", UIDValidity: uv, UID: uid}
	r, err := s.FetchRaw(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Contains(raw, []byte("Subject: Report attached")) {
		t.Fatalf("FetchRaw = %q, %v", raw, err)
	}

	pr, size, err := s.FetchPart(ctx, ids.AttachmentID{MessageID: id, Part: []int{2}})
	if err != nil {
		t.Fatal(err)
	}
	part, err := io.ReadAll(pr)
	pr.Close()
	if want := "%PDF-1.4\n% fake pdf\n"; err != nil || string(part) != want {
		t.Fatalf("FetchPart = %q, %v; want %q", part, err, want)
	}
	if size != -1 && size != int64(len(part)) {
		t.Errorf("size = %d, want %d or -1", size, len(part))
	}
	s.Close()

	if flags := rawFlags(t, c, "INBOX", uid); slices.Contains(flags, imap.FlagSeen) {
		t.Fatalf("flags = %v, read set \\Seen", flags)
	}

	out := logs.String()
	for _, want := range []string{"msg=imap.cmd", "cmd=EXAMINE", `cmd="UID FETCH"`, "duration_ms=", "status=OK", "uids=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %q", want)
		}
	}
	for _, secret := range []string{"Report attached", "Please see the report", c.Password} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

func TestListFolders_UTF7FolderRoundTrip(t *testing.T) {
	const name = "Odeslaná pošta"
	c := freshUser(t)
	uid := seed(t, c, name, "plain.eml")[0]
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)

	folders, err := s.ListFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]imapsafe.Folder{}
	for _, f := range folders {
		byName[f.Name] = f
	}
	f, ok := byName[name]
	if !ok {
		t.Fatalf("%q not listed: %+v", name, folders)
	}
	if f.Delimiter != "/" || f.Total != 1 || f.Unread != 1 {
		t.Errorf("folder = %+v", f)
	}
	if byName["INBOX"].Role != "inbox" || byName["Drafts"].Role != "drafts" || byName["Sent"].Role != "sent" {
		t.Errorf("roles: %+v", folders)
	}

	uv, err := s.Examine(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ids.DecodeMessage(ids.EncodeMessage(ids.MessageID{Folder: name, UIDValidity: uv, UID: uid}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.FetchRaw(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if raw, _ := io.ReadAll(r); !bytes.Contains(raw, []byte("Hello Bob.")) {
		t.Errorf("raw = %q", raw)
	}
}

func TestSearch_NewestFirstAndBeforeUID(t *testing.T) {
	c := freshUser(t)
	u := seed(t, c, "INBOX", "search-1.eml", "search-2.eml", "search-3.eml", "plain.eml")
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)

	got, err := s.Search(ctx, "INBOX", imapsafe.SearchCriteria{Subject: "Weekly"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{u[2], u[1], u[0]}; !slices.Equal(got, want) {
		t.Errorf("newest first = %v, want %v", got, want)
	}

	got, err = s.Search(ctx, "INBOX", imapsafe.SearchCriteria{From: "carol@example.com"}, u[2], 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{u[1]}; !slices.Equal(got, want) {
		t.Errorf("before %d limit 1 = %v, want %v", u[2], got, want)
	}

	got, err = s.Search(ctx, "INBOX", imapsafe.SearchCriteria{}, u[0], 10)
	if err != nil || len(got) != 0 {
		t.Errorf("before first uid = %v, %v; want empty", got, err)
	}

	read := false
	got, err = s.Search(ctx, "INBOX", imapsafe.SearchCriteria{Unread: &read}, 0, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("read mail = %v, %v; want empty", got, err)
	}
}

func TestFetchRaw_StaleUIDValidity(t *testing.T) {
	c := freshUser(t)
	uid := seed(t, c, "INBOX", "with-attachment.eml")[0]
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)

	uv, err := s.Examine(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	stale := ids.MessageID{Folder: "INBOX", UIDValidity: uv + 1, UID: uid}
	if _, err := s.FetchRaw(ctx, stale); !errors.Is(err, imapsafe.ErrStale) {
		t.Errorf("FetchRaw err = %v, want ErrStale", err)
	}
	if _, _, err := s.FetchPart(ctx, ids.AttachmentID{MessageID: stale, Part: []int{2}}); !errors.Is(err, imapsafe.ErrStale) {
		t.Errorf("FetchPart err = %v, want ErrStale", err)
	}

	// The session still works after the refusal.
	missing := ids.MessageID{Folder: "INBOX", UIDValidity: uv, UID: uid + 100}
	if _, err := s.FetchRaw(ctx, missing); !errors.Is(err, imapsafe.ErrNotFound) {
		t.Errorf("missing uid err = %v, want ErrNotFound", err)
	}
}

func TestSearchHeaderAndThread(t *testing.T) {
	c := freshUser(t)
	u := seed(t, c, "INBOX", "search-1.eml", "search-2.eml", "reply-1.eml")
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)

	got, err := s.SearchHeader(ctx, "INBOX", "Message-ID", "<reply-1@example.com>")
	if err != nil || !slices.Equal(got, []uint32{u[2]}) {
		t.Errorf("SearchHeader = %v, %v; want [%d]", got, err, u[2])
	}

	if !s.Caps().Thread {
		t.Skip("server has no THREAD")
	}
	got, err = s.Thread(ctx, "INBOX", u[2])
	if err != nil || !slices.Equal(got, []uint32{u[0], u[2]}) {
		t.Errorf("Thread = %v, %v; want [%d %d]", got, err, u[0], u[2])
	}
}

// bigAttachmentMail returns a mail with a size-byte attachment (part 2) in
// base64 and the SHA-256 of the decoded attachment.
func bigAttachmentMail(size int) ([]byte, [32]byte) {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	sum := sha256.Sum256(data)
	b64 := base64.StdEncoding.EncodeToString(data)
	var m bytes.Buffer
	m.Grow(len(b64)*78/76 + 1024)
	fmt.Fprint(&m, "From: Alice <alice@example.com>\r\nTo: Bob <bob@example.com>\r\n"+
		"Subject: Big file\r\nDate: Mon, 16 Nov 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n"+
		"--b1\r\nContent-Type: text/plain\r\n\r\nBig file.\r\n"+
		"--b1\r\nContent-Type: application/octet-stream\r\n"+
		"Content-Disposition: attachment; filename=\"big.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	for i := 0; i < len(b64); i += 76 {
		m.WriteString(b64[i:min(i+76, len(b64))])
		m.WriteString("\r\n")
	}
	m.WriteString("--b1--\r\n")
	return m.Bytes(), sum
}

func TestFetchPart_Streams20MB(t *testing.T) {
	const size = 20 << 20
	c := freshUser(t)
	raw, want := bigAttachmentMail(size)
	uid := seedRaw(t, c, "INBOX", raw)[0]
	raw = nil
	s, _ := openSession(t, c)
	ctx, _ := testCtx(t)
	uv, err := s.Examine(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc

	r, gotSize, err := s.FetchPart(ctx, ids.AttachmentID{
		MessageID: ids.MessageID{Folder: "INBOX", UIDValidity: uv, UID: uid},
		Part:      []int{2},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var n, nextSample int64
	for {
		k, err := r.Read(buf)
		h.Write(buf[:k])
		n += int64(k)
		if n >= nextSample {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
			nextSample = n + 1<<20
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read after %d bytes: %v", n, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	t.Logf("caps.Binary=%v size=%d read=%d heap growth=%d KB", s.Caps().Binary, gotSize, n, (peak-base)>>10)
	if n != size {
		t.Errorf("read %d bytes, want %d", n, size)
	}
	if gotSize != -1 && gotSize != size {
		t.Errorf("reported size = %d, want %d or -1", gotSize, size)
	}
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Error("decoded content differs")
	}
	if growth := int64(peak) - int64(base); growth >= 8<<20 {
		t.Errorf("heap grew by %d bytes while streaming, want < 8 MB", growth)
	}
}
