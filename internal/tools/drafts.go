package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/auth"
	"github.com/zjor/imap-mcp/internal/drafts"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/mailparse"
)

// noSendNote is in every draft tool description.
const noSendNote = "This server never sends mail: a human reviews and sends the draft from a mail client."

type createDraftIn struct {
	Mode      string   `json:"mode,omitempty" jsonschema:"new (default), reply, reply_all or forward"`
	InReplyTo string   `json:"in_reply_to,omitempty" jsonschema:"message id of the original, required for reply, reply_all and forward"`
	From      string   `json:"from,omitempty" jsonschema:"sender address; default the login name"`
	To        []string `json:"to,omitempty" jsonschema:"recipients like \"Name <a@example.com>\"; for replies default the original sender"`
	Cc        []string `json:"cc,omitempty" jsonschema:"Cc recipients; for reply_all default the other recipients of the original"`
	Bcc       []string `json:"bcc,omitempty" jsonschema:"Bcc recipients"`
	Subject   string   `json:"subject,omitempty" jsonschema:"subject; for replies and forwards default the original subject with Re: or Fwd:"`
	BodyText  string   `json:"body_text,omitempty" jsonschema:"plain text body"`
	BodyHTML  string   `json:"body_html,omitempty" jsonschema:"optional HTML body, sent next to the text body"`
	// ForwardAttachments nil means all attachments of the original.
	ForwardAttachments []string `json:"forward_attachments,omitempty" jsonschema:"forward only: attachment ids of the original to copy; omit for all, [] for none"`
}

type updateDraftIn struct {
	DraftID           string    `json:"draft_id" jsonschema:"draft_id from create_draft or list_drafts"`
	From              *string   `json:"from,omitempty" jsonschema:"new sender address"`
	To                *[]string `json:"to,omitempty" jsonschema:"new recipients"`
	Cc                *[]string `json:"cc,omitempty" jsonschema:"new Cc recipients"`
	Bcc               *[]string `json:"bcc,omitempty" jsonschema:"new Bcc recipients"`
	Subject           *string   `json:"subject,omitempty" jsonschema:"new subject"`
	BodyText          *string   `json:"body_text,omitempty" jsonschema:"new plain text body"`
	BodyHTML          *string   `json:"body_html,omitempty" jsonschema:"new HTML body; empty string removes it"`
	RemoveAttachments []string  `json:"remove_attachments,omitempty" jsonschema:"filenames of attachments to drop"`
}

type deleteDraftIn struct {
	DraftID string `json:"draft_id" jsonschema:"draft_id from create_draft or list_drafts"`
}

type listDraftsIn struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
}

type draftHeadersOut struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Bcc     []string `json:"bcc"`
	Subject string   `json:"subject"`
}

type draftAttachmentOut struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type draftOut struct {
	DraftID     string               `json:"draft_id"`
	Headers     draftHeadersOut      `json:"headers"`
	Attachments []draftAttachmentOut `json:"attachments"`
	UploadURL   string               `json:"upload_url"`
	Warning     string               `json:"warning,omitempty"`
}

type draftItemOut struct {
	DraftID          *string   `json:"draft_id"`
	Editable         bool      `json:"editable"`
	Date             time.Time `json:"date"`
	To               []addrOut `json:"to"`
	UntrustedContent struct {
		Subject string `json:"subject"`
	} `json:"untrusted_content"`
}

type listDraftsOut struct {
	Drafts     []draftItemOut `json:"drafts"`
	NextCursor *string        `json:"next_cursor"`
}

func registerDrafts(srv *mcp.Server, deps Deps) {
	addSessionTool(srv, deps, &mcp.Tool{
		Name: "create_draft",
		Description: "Create a draft in the Drafts folder: a new message, a reply, a reply to all or a forward. " +
			"Replies get In-Reply-To, References and a Re: subject; forwards copy the original's attachments on the server. " +
			"Returns draft_id and upload_url; POST files to upload_url (multipart/form-data, field \"file\") to attach them. " + noSendNote,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
	}, func(in createDraftIn) []any {
		return []any{"mode", in.Mode, "forward_attachments", len(in.ForwardAttachments)}
	}, func(ctx context.Context, s *imapsafe.Session, in createDraftIn) (any, int, error) {
		return createDraft(ctx, s, deps, in)
	})

	addSessionTool(srv, deps, &mcp.Tool{
		Name: "update_draft",
		Description: "Change a draft made by create_draft. Fields left out keep their old values; attachments are kept unless named in remove_attachments. " +
			"The draft_id stays the same. Drafts saved by a mail client cannot be changed. " + noSendNote,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false)},
	}, func(in updateDraftIn) []any {
		return []any{"draft_id", in.DraftID, "remove_attachments", len(in.RemoveAttachments)}
	}, func(ctx context.Context, s *imapsafe.Session, in updateDraftIn) (any, int, error) {
		return updateDraft(ctx, s, deps, in)
	})

	addSessionTool(srv, deps, &mcp.Tool{
		Name:        "delete_draft",
		Description: "Delete a draft made by create_draft. Other messages, and drafts saved by a mail client, are never deleted. " + noSendNote,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(in deleteDraftIn) []any {
		return []any{"draft_id", in.DraftID}
	}, deleteDraft)

	addSessionTool(srv, deps, &mcp.Tool{
		Name: "list_drafts",
		Description: "List the messages in the Drafts folder, newest first. editable is true for drafts made by this server; " +
			"only those have a draft_id and can be changed or deleted. " + noSendNote + " " + injectionNote,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(in listDraftsIn) []any {
		return []any{"limit", clampLimit(in.Limit), "cursor", in.Cursor != ""}
	}, listDrafts)
}

func createDraft(ctx context.Context, s *imapsafe.Session, deps Deps, in createDraftIn) (any, int, error) {
	mode := in.Mode
	if mode == "" {
		mode = "new"
	}
	switch mode {
	case "new":
		if in.InReplyTo != "" {
			return nil, 0, badArg("in_reply_to is only for reply, reply_all and forward")
		}
	case "reply", "reply_all", "forward":
	default:
		return nil, 0, badArg("mode must be new, reply, reply_all or forward")
	}
	if mode != "forward" && in.ForwardAttachments != nil {
		return nil, 0, badArg("forward_attachments is only for mode forward")
	}
	creds, _ := auth.CredentialsFrom(ctx)
	from := in.From
	if from == "" {
		from = defaultFrom(creds.Username)
	}
	input := drafts.Input{
		Mode: mode, From: from, To: in.To, Cc: in.Cc, Bcc: in.Bcc,
		Subject: in.Subject, BodyText: in.BodyText, BodyHTML: in.BodyHTML,
		DraftID: ids.NewDraftID(),
	}

	var sp spool
	defer sp.remove(ctx)
	if mode != "new" {
		mid, err := ids.DecodeMessage(in.InReplyTo)
		if err != nil {
			return nil, 0, badArg("in_reply_to must be a message id from search_emails or read_email")
		}
		_, orig, err := readMessage(ctx, s, deps, mid, "")
		if err != nil {
			return nil, 0, err
		}
		input.Original = &orig
		if mode == "forward" {
			if in.Subject == "" {
				input.Subject = drafts.ForwardSubject(orig.Headers.Subject)
			}
			atts, err := forwardSelection(mid, orig.Attachments, in.ForwardAttachments)
			if err != nil {
				return nil, 0, err
			}
			for _, a := range atts {
				if err := sp.add(ctx, s, ids.AttachmentID{MessageID: mid, Part: a.Part}, a.Filename, a.ContentType); err != nil {
					return nil, 0, err
				}
			}
		} else {
			to, cc, subject, _, _ := drafts.ReplyHeaders(orig.Headers, mode == "reply_all", creds.Username)
			if in.To == nil {
				input.To = to
			}
			if in.Cc == nil {
				input.Cc = cc
			}
			if in.Subject == "" {
				input.Subject = subject
			}
		}
	}

	size, err := storeDraft(ctx, s, &input, &sp)
	if err != nil {
		return nil, 0, err
	}
	audit(ctx, "draft.created", input, &sp, size)
	return toDraftOut(deps, input, &sp), 1, nil
}

func updateDraft(ctx context.Context, s *imapsafe.Session, deps Deps, in updateDraftIn) (any, int, error) {
	// Check first, so nothing is appended when the old copy cannot go.
	if !s.Caps().UIDPlus {
		return nil, 0, imapsafe.ErrUnsupported
	}
	if err := ids.CheckDraftID(in.DraftID); err != nil {
		return nil, 0, err
	}
	uid, uv, err := s.FindDraft(ctx, in.DraftID)
	if err != nil {
		return nil, 0, err
	}
	mid := ids.MessageID{Folder: s.Folders().Drafts, UIDValidity: uv, UID: uid}
	_, old, err := readMessage(ctx, s, deps, mid, "")
	if err != nil {
		return nil, 0, err
	}

	h := old.Headers
	input := drafts.Input{
		Mode: "new", To: addrStrings(h.To), Cc: addrStrings(h.Cc), Bcc: addrStrings(h.Bcc),
		Subject: h.Subject, BodyText: old.Text, BodyHTML: old.HTML, DraftID: in.DraftID,
	}
	if len(h.From) > 0 {
		input.From = addrStrings(h.From[:1])[0]
	}
	if h.InReplyTo != "" {
		// Build sets References to Original.References + Original.MessageID.
		refs := h.References
		if n := len(refs); n > 0 && refs[n-1] == h.InReplyTo {
			refs = refs[:n-1]
		}
		input.Mode = "reply"
		input.Original = &mailparse.Parsed{Headers: mailparse.Headers{MessageID: h.InReplyTo, References: refs}}
	}
	if old.Truncated && (in.BodyText == nil || (old.HTML != "" && in.BodyHTML == nil)) {
		return nil, 0, badArg("the draft body is too large to keep; pass body_text and body_html")
	}
	setIf(&input.From, in.From)
	setIf(&input.To, in.To)
	setIf(&input.Cc, in.Cc)
	setIf(&input.Bcc, in.Bcc)
	setIf(&input.Subject, in.Subject)
	setIf(&input.BodyText, in.BodyText)
	setIf(&input.BodyHTML, in.BodyHTML)

	for _, name := range in.RemoveAttachments {
		if !slices.ContainsFunc(old.Attachments, func(a mailparse.Attachment) bool { return a.Filename == name }) {
			return nil, 0, badArg("the draft has no attachment named " + name)
		}
	}
	var sp spool
	defer sp.remove(ctx)
	for _, a := range old.Attachments {
		if slices.Contains(in.RemoveAttachments, a.Filename) {
			continue
		}
		if err := sp.add(ctx, s, ids.AttachmentID{MessageID: mid, Part: a.Part}, a.Filename, a.ContentType); err != nil {
			return nil, 0, err
		}
	}

	size, err := storeDraft(ctx, s, &input, &sp)
	if err != nil {
		return nil, 0, err
	}
	out := toDraftOut(deps, input, &sp)
	if err := s.RemoveOwnDraft(ctx, in.DraftID, uv, uid); err != nil {
		logging.From(ctx).Error("draft.duplicate_left", "draft_id", in.DraftID, "uid", uid, "error", err.Error())
		out.Warning = "old copy could not be removed"
	}
	audit(ctx, "draft.updated", input, &sp, size)
	return out, 1, nil
}

func deleteDraft(ctx context.Context, s *imapsafe.Session, in deleteDraftIn) (any, int, error) {
	if !s.Caps().UIDPlus {
		return nil, 0, imapsafe.ErrUnsupported
	}
	if err := ids.CheckDraftID(in.DraftID); err != nil {
		return nil, 0, err
	}
	uid, uv, err := s.FindDraft(ctx, in.DraftID)
	if err != nil {
		return nil, 0, err
	}
	if err := s.RemoveOwnDraft(ctx, in.DraftID, uv, uid); err != nil {
		return nil, 0, err
	}
	logging.From(ctx).Info("draft.deleted", "draft_id", in.DraftID, "uid", uid)
	return map[string]bool{"ok": true}, 1, nil
}

func listDrafts(ctx context.Context, s *imapsafe.Session, in listDraftsIn) (any, int, error) {
	folder := s.Folders().Drafts
	if folder == "" {
		return nil, 0, errors.New("the server has no Drafts folder")
	}
	limit := clampLimit(in.Limit)
	var cur *cursorFolder
	if in.Cursor != "" {
		c, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, 0, err
		}
		if len(c.Folders) != 1 || c.Folders[0].Name != folder || c.All {
			return nil, 0, badArg("invalid cursor")
		}
		cur = &c.Folders[0]
	}
	uv, err := s.Examine(ctx, folder)
	if err != nil {
		return nil, 0, err
	}
	var before uint32
	if cur != nil {
		if uv != cur.UIDValidity {
			return nil, 0, imapsafe.ErrStale
		}
		before = cur.Before
	}
	list, err := s.ListDrafts(ctx, before, limit+1)
	if err != nil {
		return nil, 0, err
	}
	out := listDraftsOut{Drafts: make([]draftItemOut, 0, min(len(list), limit))}
	if len(list) > limit {
		list = list[:limit]
		c := cursor{Folders: []cursorFolder{{Name: folder, UIDValidity: uv, Before: list[limit-1].UID}}}.encode()
		out.NextCursor = &c
	}
	for _, d := range list {
		item := draftItemOut{Editable: d.Editable, Date: msgDate(d.Summary), To: toAddrOut(d.To)}
		if d.DraftID != "" {
			id := d.DraftID
			item.DraftID = &id
		}
		item.UntrustedContent.Subject = d.Subject
		out.Drafts = append(out.Drafts, item)
	}
	return out, len(out.Drafts), nil
}

// forwardSelection returns the attachments of the original to forward:
// all when sel is nil, else the ones sel names.
func forwardSelection(mid ids.MessageID, all []mailparse.Attachment, sel []string) ([]mailparse.Attachment, error) {
	if sel == nil {
		return all, nil
	}
	var out []mailparse.Attachment
	for _, s := range sel {
		aid, err := ids.DecodeAttachment(s)
		if err != nil {
			return nil, err
		}
		if aid.MessageID != mid {
			return nil, badArg("forward_attachments must be attachments of the in_reply_to message")
		}
		i := slices.IndexFunc(all, func(a mailparse.Attachment) bool { return slices.Equal(a.Part, aid.Part) })
		if i < 0 {
			return nil, badArg("forward_attachments names a part that is not an attachment of the original")
		}
		out = append(out, all[i])
	}
	return out, nil
}

// storeDraft sets a fresh Message-ID and Date, then builds the draft and
// streams it into APPEND. It returns the message size.
func storeDraft(ctx context.Context, s *imapsafe.Session, in *drafts.Input, sp *spool) (int64, error) {
	in.MessageID = drafts.NewMessageID(in.From)
	in.Date = time.Now()
	parts := sp.parts()
	size, err := drafts.Size(*in, parts)
	if err != nil {
		return 0, badArg("cannot build the draft: " + err.Error())
	}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("draft build panic: %v", r)
			}
			pw.CloseWithError(err)
		}()
		err = drafts.Build(pw, *in, parts)
	}()
	_, err = s.AppendDraft(ctx, size, pr)
	// Unblock Build when APPEND stopped reading, and wait for it so the
	// spool files are closed before they are removed.
	pr.CloseWithError(errors.New("append finished"))
	<-done
	return size, err
}

// spool keeps attachment bytes in temp files while a draft is built. An
// IMAP session cannot FETCH while an APPEND literal streams, and Size and
// Build each read every part.
type spool struct{ files []spoolFile }

type spoolFile struct {
	name, contentType, path string
	size                    int64
}

// add copies one MIME part into a new temp file. The FETCH stream is
// closed before add returns.
func (sp *spool) add(ctx context.Context, s *imapsafe.Session, id ids.AttachmentID, name, contentType string) error {
	f, err := os.CreateTemp("", "imap-mcp-draft-*")
	if err != nil {
		return err
	}
	if name == "" {
		name = "attachment"
	}
	sp.files = append(sp.files, spoolFile{name: name, contentType: contentType, path: f.Name()})
	r, _, err := s.FetchPart(ctx, id)
	if err != nil {
		f.Close()
		return err
	}
	n, cerr := io.Copy(f, r)
	if err := errors.Join(cerr, r.Close(), f.Close()); err != nil {
		return err
	}
	sp.files[len(sp.files)-1].size = n
	return nil
}

func (sp *spool) parts() []drafts.Part {
	out := make([]drafts.Part, 0, len(sp.files))
	for _, f := range sp.files {
		out = append(out, drafts.Part{
			Filename:    f.name,
			ContentType: f.contentType,
			Open:        func() (io.ReadCloser, error) { return os.Open(f.path) },
		})
	}
	return out
}

// remove deletes the temp files. It runs deferred, so also on error and
// panic.
func (sp *spool) remove(ctx context.Context) {
	for _, f := range sp.files {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logging.From(ctx).Error("draft.tempfile_cleanup", "error", err.Error())
		}
	}
}

func toDraftOut(deps Deps, in drafts.Input, sp *spool) draftOut {
	out := draftOut{
		DraftID: in.DraftID,
		Headers: draftHeadersOut{
			From: in.From, To: orEmpty(in.To), Cc: orEmpty(in.Cc), Bcc: orEmpty(in.Bcc), Subject: in.Subject,
		},
		Attachments: make([]draftAttachmentOut, 0, len(sp.files)),
		UploadURL:   deps.PublicURL + "/drafts/" + in.DraftID + "/attachments",
	}
	for _, f := range sp.files {
		out.Attachments = append(out.Attachments, draftAttachmentOut{Filename: f.name, ContentType: f.contentType, Size: f.size})
	}
	return out
}

// audit logs one write: no addresses and no subject (spec 7.8).
func audit(ctx context.Context, event string, in drafts.Input, sp *spool, size int64) {
	logging.From(ctx).Info(event, "draft_id", in.DraftID,
		"recipients", len(in.To)+len(in.Cc)+len(in.Bcc), "attachments", len(sp.files), "size", size)
}

// defaultFrom is the login name when it is an email address, else empty
// (no From header; the mail client fills it in).
func defaultFrom(username string) string {
	if a, err := mail.ParseAddress(username); err == nil && a.Address == username {
		return username
	}
	return ""
}

func addrStrings(in []mailparse.Address) []string {
	var out []string
	for _, a := range in {
		if a.Name == "" {
			out = append(out, a.Email)
			continue
		}
		out = append(out, (&mail.Address{Name: a.Name, Address: a.Email}).String())
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func setIf[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}
