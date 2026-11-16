//go:build integration

package integration

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/drafts"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/mailparse"
)

type draftOut struct {
	DraftID string `json:"draft_id"`
	Headers struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Cc      []string `json:"cc"`
		Bcc     []string `json:"bcc"`
		Subject string   `json:"subject"`
	} `json:"headers"`
	Attachments []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	} `json:"attachments"`
	UploadURL string `json:"upload_url"`
	Warning   string `json:"warning"`
}

type listDraftsOut struct {
	Drafts []struct {
		DraftID          *string   `json:"draft_id"`
		Editable         bool      `json:"editable"`
		To               []addrOut `json:"to"`
		UntrustedContent struct {
			Subject string `json:"subject"`
		} `json:"untrusted_content"`
	} `json:"drafts"`
	NextCursor *string `json:"next_cursor"`
}

// reportPDF is the decoded attachment of with-attachment.eml.
const reportPDF = "%PDF-1.4\n% fake pdf\n"

// ownDraftRaw fetches the single message in Drafts whose marker is
// draftID, with a raw client, and returns its UID and bytes.
func ownDraftRaw(t *testing.T, c imapsafe.Credentials, draftID string) (uint32, []byte) {
	t.Helper()
	cl := rawClient(t, c)
	defer cl.Close()
	if _, err := cl.Select(draftsFolder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("raw examine: %v", err)
	}
	data, err := cl.UIDSearch(&imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: drafts.MarkerHeader, Value: draftID}},
	}, nil).Wait()
	if err != nil {
		t.Fatalf("raw search: %v", err)
	}
	uids := data.AllUIDs()
	if len(uids) != 1 {
		t.Fatalf("drafts with marker %s: %v, want one", draftID, uids)
	}
	return uint32(uids[0]), rawMessage(t, cl, uint32(uids[0]))
}

// rawMessage fetches BODY.PEEK[] of uid in the selected folder.
func rawMessage(t *testing.T, cl *imapclient.Client, uid uint32) []byte {
	t.Helper()
	sec := &imap.FetchItemBodySection{Peek: true}
	msgs, err := cl.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{sec},
	}).Collect()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("raw fetch %d: %v (%d msgs)", uid, err, len(msgs))
	}
	return msgs[0].FindBodySection(sec)
}

// draftAttachments returns filename → decoded bytes of the attachments in
// raw.
func draftAttachments(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	out := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		if h, ok := p.Header.(*gomail.AttachmentHeader); ok {
			name, _ := h.Filename()
			b, _ := io.ReadAll(p.Body)
			out[name] = b
		}
	}
	return out
}

func parseDraft(t *testing.T, raw []byte) mailparse.Parsed {
	t.Helper()
	p, err := mailparse.Parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse draft: %v", err)
	}
	return p
}

func headerOf(t *testing.T, raw []byte, key string) string {
	t.Helper()
	e, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read draft header: %v", err)
	}
	return e.Header.Get(key)
}

// firstID returns the id of the only message search_emails finds in INBOX.
func firstID(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	var s searchOut
	mustCall(t, cs, "search_emails", nil, &s)
	if len(s.Messages) != 1 {
		t.Fatalf("search = %+v", s)
	}
	return s.Messages[0].ID
}

func tempSpools(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(os.TempDir(), "imap-mcp-draft-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreateDraft_New(t *testing.T) {
	c := freshUser(t)
	srv := startServer(t)
	cs := srv.connect(t, c)

	subject := "Nabídka – Žluťoučký kůň"
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{
		"mode": "new", "from": "Me <me@example.com>", "to": []string{"Bob <bob@example.com>"},
		"bcc": []string{"audit@example.com"}, "subject": subject, "body_text": "Dobrý den",
	}, &d)
	if ids.CheckDraftID(d.DraftID) != nil {
		t.Fatalf("draft_id = %q", d.DraftID)
	}
	if d.UploadURL != testPublicURL+"/drafts/"+d.DraftID+"/attachments" {
		t.Errorf("upload_url = %q", d.UploadURL)
	}
	if d.Headers.From != "Me <me@example.com>" || !slices.Equal(d.Headers.To, []string{"Bob <bob@example.com>"}) ||
		!slices.Equal(d.Headers.Bcc, []string{"audit@example.com"}) || d.Headers.Subject != subject || len(d.Attachments) != 0 {
		t.Errorf("result = %+v", d)
	}

	uid, raw := ownDraftRaw(t, c, d.DraftID)
	p := parseDraft(t, raw)
	if p.Headers.Subject != subject || strings.TrimSpace(p.Text) != "Dobrý den" ||
		len(p.Headers.To) != 1 || p.Headers.To[0].Email != "bob@example.com" || p.Headers.From[0].Email != "me@example.com" {
		t.Errorf("draft = %+v", p)
	}
	if !strings.HasSuffix(p.Headers.MessageID, "@example.com") || p.Headers.Date.IsZero() {
		t.Errorf("message id %q, date %v", p.Headers.MessageID, p.Headers.Date)
	}
	if flags := rawFlags(t, c, draftsFolder, uid); !slices.Contains(flags, imap.FlagDraft) {
		t.Errorf("flags = %v", flags)
	}

	logs := srv.logs.String()
	if !strings.Contains(logs, "draft.created") {
		t.Error("no draft.created audit line")
	}
	for _, secret := range []string{subject, "bob@example.com", "audit@example.com", "Dobrý den"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}

	// From defaults to the login name; mode is validated.
	if te := call(t, cs, "create_draft", map[string]any{"mode": "draft"}, nil); te == nil || te.Error.Code != "bad_argument" {
		t.Errorf("bad mode: %+v", te)
	}
	if te := call(t, cs, "create_draft", map[string]any{"mode": "new", "to": []string{"not an address"}}, nil); te == nil || te.Error.Code != "bad_argument" {
		t.Errorf("bad address: %+v", te)
	}
}

func TestCreateDraft_ReplyThreading(t *testing.T) {
	c := freshUser(t)
	seed(t, c, "INBOX", "reply-1.eml")
	cs := startServer(t).connect(t, c)
	id := firstID(t, cs)

	for _, args := range []map[string]any{{"mode": "reply"}, {"mode": "reply", "in_reply_to": "bogus"}} {
		if te := call(t, cs, "create_draft", args, nil); te == nil || te.Error.Code != "bad_argument" {
			t.Errorf("%v: %+v", args, te)
		}
	}

	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "reply", "in_reply_to": id, "body_text": "Sure."}, &d)
	if d.Headers.Subject != "Re: Weekly note 1" || !slices.Equal(d.Headers.To, []string{`"Bob" <bob@example.com>`}) || len(d.Headers.Cc) != 0 {
		t.Errorf("result headers = %+v", d.Headers)
	}
	_, raw := ownDraftRaw(t, c, d.DraftID)
	p := parseDraft(t, raw)
	if p.Headers.InReplyTo != "reply-1@example.com" ||
		!slices.Equal(p.Headers.References, []string{"search-1@example.com", "reply-1@example.com"}) ||
		p.Headers.Subject != "Re: Weekly note 1" || len(p.Headers.To) != 1 || p.Headers.To[0].Email != "bob@example.com" {
		t.Errorf("draft headers = %+v", p.Headers)
	}
}

func TestCreateDraft_ReplyAll(t *testing.T) {
	c := freshUser(t)
	seedRaw(t, c, "INBOX", []byte("From: Alice <alice@example.com>\nTo: Bob <bob@example.com>, Carol <carol@example.com>\n"+
		"Cc: Dave <dave@example.com>\nSubject: Plan\nDate: Mon, 16 Nov 2026 10:00:00 +0000\nMessage-ID: <plan-1@example.com>\n\nHi all\n"))
	cs := startServer(t).connect(t, c)
	id := firstID(t, cs)

	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "reply_all", "in_reply_to": id, "body_text": "OK"}, &d)
	_, raw := ownDraftRaw(t, c, d.DraftID)
	p := parseDraft(t, raw)
	emails := func(as []mailparse.Address) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.Email)
		}
		return out
	}
	if got := emails(p.Headers.To); !slices.Equal(got, []string{"alice@example.com"}) {
		t.Errorf("to = %v", got)
	}
	if got := emails(p.Headers.Cc); !slices.Equal(got, []string{"bob@example.com", "carol@example.com", "dave@example.com"}) {
		t.Errorf("cc = %v", got)
	}
	if p.Headers.Subject != "Re: Plan" || p.Headers.InReplyTo != "plan-1@example.com" {
		t.Errorf("headers = %+v", p.Headers)
	}
}

func TestCreateDraft_ForwardCopiesAttachments(t *testing.T) {
	c := freshUser(t)
	seed(t, c, "INBOX", "with-attachment.eml")
	cs := startServer(t).connect(t, c)
	id := firstID(t, cs)
	spoolsBefore := tempSpools(t)

	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "forward", "in_reply_to": id,
		"to": []string{"carol@example.com"}, "body_text": "FYI"}, &d)
	if d.Headers.Subject != "Fwd: Report attached" || len(d.Attachments) != 1 ||
		d.Attachments[0].Filename != "report.pdf" || d.Attachments[0].ContentType != "application/pdf" ||
		d.Attachments[0].Size != int64(len(reportPDF)) {
		t.Errorf("result = %+v", d)
	}
	_, raw := ownDraftRaw(t, c, d.DraftID)
	if got := draftAttachments(t, raw); len(got) != 1 || string(got["report.pdf"]) != reportPDF {
		t.Errorf("attachments = %q", got)
	}
	if p := parseDraft(t, raw); p.Headers.Subject != "Fwd: Report attached" || p.Headers.InReplyTo != "" {
		t.Errorf("headers = %+v", p.Headers)
	}
	if after := tempSpools(t); len(after) != len(spoolsBefore) {
		t.Errorf("temp files left: %v", after)
	}

	// An explicit empty list forwards no attachments.
	var none draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "forward", "in_reply_to": id, "forward_attachments": []string{}}, &none)
	if len(none.Attachments) != 0 {
		t.Errorf("attachments = %+v", none.Attachments)
	}
}

// forwardDraft creates a forward of with-attachment.eml and returns it.
func forwardDraft(t *testing.T, c imapsafe.Credentials, cs *mcp.ClientSession) draftOut {
	t.Helper()
	seed(t, c, "INBOX", "with-attachment.eml")
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "forward", "in_reply_to": firstID(t, cs),
		"to": []string{"carol@example.com"}, "body_text": "FYI"}, &d)
	return d
}

func TestUpdateDraft_KeepsIDAndAttachments(t *testing.T) {
	c := freshUser(t)
	srv := startServer(t)
	cs := srv.connect(t, c)
	d := forwardDraft(t, c, cs)
	oldUID, _ := ownDraftRaw(t, c, d.DraftID)

	var u draftOut
	mustCall(t, cs, "update_draft", map[string]any{"draft_id": d.DraftID, "subject": "Fwd: the report"}, &u)
	if u.DraftID != d.DraftID || u.Warning != "" || u.UploadURL != d.UploadURL || u.Headers.Subject != "Fwd: the report" ||
		!slices.Equal(u.Headers.To, []string{"carol@example.com"}) || len(u.Attachments) != 1 {
		t.Errorf("result = %+v", u)
	}
	if uids := rawUIDs(t, c, draftsFolder); len(uids) != 1 || uids[0] == oldUID {
		t.Fatalf("Drafts UIDs = %v (old %d), want one new copy", uids, oldUID)
	}
	_, raw := ownDraftRaw(t, c, d.DraftID)
	p := parseDraft(t, raw)
	if p.Headers.Subject != "Fwd: the report" || strings.TrimSpace(p.Text) != "FYI" ||
		len(p.Headers.To) != 1 || p.Headers.To[0].Email != "carol@example.com" {
		t.Errorf("draft = %+v", p)
	}
	if got := draftAttachments(t, raw); string(got["report.pdf"]) != reportPDF {
		t.Errorf("attachments = %q", got)
	}
	if !strings.Contains(srv.logs.String(), "draft.updated") {
		t.Error("no draft.updated audit line")
	}

	if te := call(t, cs, "update_draft", map[string]any{"draft_id": "nope"}, nil); te == nil || te.Error.Code != "bad_id" {
		t.Errorf("bad draft id: %+v", te)
	}
}

func TestUpdateDraft_RemoveAttachment(t *testing.T) {
	c := freshUser(t)
	cs := startServer(t).connect(t, c)
	d := forwardDraft(t, c, cs)

	if te := call(t, cs, "update_draft", map[string]any{"draft_id": d.DraftID, "remove_attachments": []string{"other.pdf"}}, nil); te == nil || te.Error.Code != "bad_argument" {
		t.Errorf("unknown name: %+v", te)
	}
	var u draftOut
	mustCall(t, cs, "update_draft", map[string]any{"draft_id": d.DraftID, "remove_attachments": []string{"report.pdf"}}, &u)
	if len(u.Attachments) != 0 {
		t.Errorf("attachments = %+v", u.Attachments)
	}
	_, raw := ownDraftRaw(t, c, d.DraftID)
	if got := draftAttachments(t, raw); len(got) != 0 {
		t.Errorf("draft still has %q", got)
	}
	if p := parseDraft(t, raw); p.Headers.Subject != "Fwd: Report attached" {
		t.Errorf("subject = %q", p.Headers.Subject)
	}
}

func TestUpdateDraft_AfterClientResave(t *testing.T) {
	c := freshUser(t)
	cs := startServer(t).connect(t, c)
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "to": []string{"bob@example.com"}, "body_text": "v1"}, &d)
	oldUID, raw := ownDraftRaw(t, c, d.DraftID)

	// A mail client re-saves the draft: a copy without the marker, then the
	// original is expunged.
	var kept [][]byte
	for _, line := range bytes.SplitAfter(raw, []byte("\r\n")) {
		if !bytes.HasPrefix(bytes.ToLower(line), []byte(strings.ToLower(drafts.MarkerHeader)+":")) {
			kept = append(kept, line)
		}
	}
	clientCopy := bytes.Join(kept, nil)
	if len(clientCopy) == len(raw) {
		t.Fatal("marker header not found in the draft")
	}
	copyUID := seedMsgs(t, c, draftsFolder, seedMsg{raw: clientCopy, flags: []imap.Flag{imap.FlagDraft}})[0]
	cl := rawClient(t, c)
	if _, err := cl.Select(draftsFolder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := cl.Store(imap.UIDSetNum(imap.UID(oldUID)), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := cl.UIDExpunge(imap.UIDSetNum(imap.UID(oldUID))).Close(); err != nil {
		t.Fatal(err)
	}
	cl.Close()

	te := call(t, cs, "update_draft", map[string]any{"draft_id": d.DraftID, "body_text": "v2"}, nil)
	if te == nil || te.Error.Code != "not_editable" {
		t.Fatalf("update = %+v, want not_editable", te)
	}
	if uids := rawUIDs(t, c, draftsFolder); !slices.Equal(uids, []uint32{copyUID}) {
		t.Fatalf("Drafts UIDs = %v, want only the client copy %d", uids, copyUID)
	}
	cl = rawClient(t, c)
	defer cl.Close()
	if _, err := cl.Select(draftsFolder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	if got := rawMessage(t, cl, copyUID); !bytes.Equal(got, clientCopy) {
		t.Errorf("client copy changed:\n%q\nwant\n%q", got, clientCopy)
	}
}

func TestDeleteDraft_ForeignDraftRefused(t *testing.T) {
	c := freshUser(t)
	srv := startServer(t)
	cs := srv.connect(t, c)
	foreign := seedRaw(t, c, draftsFolder, []byte("From: me@example.com\nSubject: mine\n\nhand-written\n"))[0]
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "body_text": "x"}, &d)

	if te := call(t, cs, "delete_draft", map[string]any{"draft_id": ids.NewDraftID()}, nil); te == nil || te.Error.Code != "not_editable" {
		t.Errorf("unknown draft id: %+v", te)
	}
	if te := call(t, cs, "delete_draft", map[string]any{"draft_id": "x"}, nil); te == nil || te.Error.Code != "bad_id" {
		t.Errorf("bad draft id: %+v", te)
	}
	if n := len(rawUIDs(t, c, draftsFolder)); n != 2 {
		t.Fatalf("Drafts has %d messages, want 2", n)
	}

	var ok struct {
		OK bool `json:"ok"`
	}
	mustCall(t, cs, "delete_draft", map[string]any{"draft_id": d.DraftID}, &ok)
	if !ok.OK {
		t.Error("ok = false")
	}
	if uids := rawUIDs(t, c, draftsFolder); !slices.Equal(uids, []uint32{foreign}) {
		t.Errorf("Drafts UIDs = %v, want only the foreign draft %d", uids, foreign)
	}
	if !strings.Contains(srv.logs.String(), "draft.deleted") {
		t.Error("no draft.deleted audit line")
	}
}

func TestListDrafts_EditableFlag(t *testing.T) {
	c := freshUser(t)
	cs := startServer(t).connect(t, c)
	seedRaw(t, c, draftsFolder, []byte("From: me@example.com\nTo: Ann <ann@example.com>\nSubject: hand-written\n\nhi\n"))
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "to": []string{"Bob <bob@example.com>"}, "subject": "by agent"}, &d)

	var page listDraftsOut
	mustCall(t, cs, "list_drafts", map[string]any{"limit": 1}, &page)
	if len(page.Drafts) != 1 || page.NextCursor == nil {
		t.Fatalf("page 1 = %+v", page)
	}
	own := page.Drafts[0]
	if own.DraftID == nil || *own.DraftID != d.DraftID || !own.Editable || own.UntrustedContent.Subject != "by agent" ||
		len(own.To) != 1 || own.To[0].Email != "bob@example.com" {
		t.Errorf("own draft = %+v", own)
	}

	mustCall(t, cs, "list_drafts", map[string]any{"limit": 1, "cursor": *page.NextCursor}, &page)
	if len(page.Drafts) != 1 || page.NextCursor != nil {
		t.Fatalf("page 2 = %+v", page)
	}
	foreign := page.Drafts[0]
	if foreign.DraftID != nil || foreign.Editable || foreign.UntrustedContent.Subject != "hand-written" {
		t.Errorf("foreign draft = %+v", foreign)
	}

	if te := call(t, cs, "list_drafts", map[string]any{"cursor": "junk"}, nil); te == nil || te.Error.Code != "bad_argument" {
		t.Errorf("bad cursor: %+v", te)
	}
}

func TestDraftToolDescriptions_SayNoSending(t *testing.T) {
	cs := startServer(t).connect(t, freshUser(t))
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, tool := range res.Tools {
		switch tool.Name {
		case "create_draft", "update_draft", "delete_draft", "list_drafts":
			found++
			if !strings.Contains(tool.Description, "never sends mail") {
				t.Errorf("%s description: %q", tool.Name, tool.Description)
			}
		}
	}
	if found != 4 {
		t.Errorf("found %d of 4 draft tools", found)
	}
}

// update_draft and an upload rewrite the own draft only. A foreign message
// flagged \Deleted (not expunged) and a foreign draft without the marker
// stay as they were, and the \Deleted flag stays set (spec 10).
func TestDraftWrites_LeaveForeignDraftsAlone(t *testing.T) {
	c := freshUser(t)
	foreignRaw := [][]byte{
		[]byte("From: me@example.com\r\nSubject: deleted by my client\r\n\r\nold\r\n"),
		[]byte("From: me@example.com\r\nSubject: my own draft\r\n\r\nwork in progress\r\n"),
	}
	foreign := seedMsgs(t, c, draftsFolder,
		seedMsg{raw: foreignRaw[0], flags: []imap.Flag{imap.FlagDraft}},
		seedMsg{raw: foreignRaw[1], flags: []imap.Flag{imap.FlagDraft}})
	cl := rawClient(t, c)
	if _, err := cl.Select(draftsFolder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := cl.Store(imap.UIDSetNum(imap.UID(foreign[0])), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	cl.Logout().Wait()
	cl.Close()

	srv := startServer(t)
	cs := srv.connect(t, c)
	var d draftOut
	mustCall(t, cs, "create_draft", map[string]any{"mode": "new", "to": []string{"bob@example.com"}, "subject": "Offer", "body_text": "Hi"}, &d)
	mustCall(t, cs, "update_draft", map[string]any{"draft_id": d.DraftID, "subject": "Offer v2"}, nil)
	decodeUpload(t, srv.upload(t, c, d.DraftID, filePart("a.txt", "text/plain", "hello")))

	own, _ := ownDraftRaw(t, c, d.DraftID)
	if uids := rawUIDs(t, c, draftsFolder); !slices.Equal(uids, []uint32{foreign[0], foreign[1], own}) {
		t.Fatalf("Drafts UIDs = %v, want %v and the own draft %d", uids, foreign, own)
	}
	cl = rawClient(t, c)
	defer cl.Close()
	if _, err := cl.Select(draftsFolder, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	for i, uid := range foreign {
		if got := rawMessage(t, cl, uid); !bytes.Equal(got, foreignRaw[i]) {
			t.Errorf("foreign message %d changed: %q", uid, got)
		}
	}
	if !slices.Contains(rawFlags(t, c, draftsFolder, foreign[0]), imap.FlagDeleted) {
		t.Error("foreign message lost \\Deleted")
	}
	if slices.Contains(rawFlags(t, c, draftsFolder, foreign[1]), imap.FlagDeleted) {
		t.Error("foreign draft got \\Deleted")
	}
}
