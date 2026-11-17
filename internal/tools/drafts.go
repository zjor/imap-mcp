package tools

import (
	"context"
	"errors"
	"net/mail"
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

type draftOut struct {
	DraftID     string            `json:"draft_id"`
	Headers     draftHeadersOut   `json:"headers"`
	Attachments []DraftAttachment `json:"attachments"`
	UploadURL   string            `json:"upload_url"`
	Warning     string            `json:"warning,omitempty"`
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

	uid, size, err := storeDraft(ctx, s, &input, &sp)
	if err != nil {
		return nil, 0, err
	}
	audit(ctx, "draft.created", s.Folders().Drafts, input, sp.files, size, "uid", uid)
	return toDraftOut(deps, input, sp.files), 1, nil
}

func updateDraft(ctx context.Context, s *imapsafe.Session, deps Deps, in updateDraftIn) (any, int, error) {
	rb, err := rebuildDraft(ctx, s, in.DraftID, func(old mailparse.Parsed, input *drafts.Input) (func(mailparse.Attachment) bool, error) {
		if old.Truncated && (in.BodyText == nil || (old.HTML != "" && in.BodyHTML == nil)) {
			return nil, badArg("the draft body is too large to keep; pass body_text and body_html")
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
				return nil, badArg("the draft has no attachment named " + name)
			}
		}
		return keepAllBut(in.RemoveAttachments), nil
	}, nil)
	if err != nil {
		return nil, 0, err
	}
	out := toDraftOut(deps, rb.input, rb.files)
	out.Warning = rb.warning
	audit(ctx, "draft.updated", s.Folders().Drafts, rb.input, rb.files, rb.size, "old_uid", rb.oldUID, "uid", rb.uid)
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
	logging.From(ctx).Info("draft.deleted", "draft_id", in.DraftID, "folder", s.Folders().Drafts, "uid", uid)
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

func toDraftOut(deps Deps, in drafts.Input, files []spoolFile) draftOut {
	return draftOut{
		DraftID: in.DraftID,
		Headers: draftHeadersOut{
			From: in.From, To: orEmpty(in.To), Cc: orEmpty(in.Cc), Bcc: orEmpty(in.Bcc), Subject: in.Subject,
		},
		Attachments: attachmentsOut(files),
		UploadURL:   deps.PublicURL + "/drafts/" + in.DraftID + "/attachments",
	}
}

// audit logs one write: no addresses and no subject (spec 7.8).
// audit logs one draft write with the fields spec §7.8 asks for: draft id,
// UIDs, file names and sizes. extra carries the UID pairs.
func audit(ctx context.Context, event, folder string, in drafts.Input, files []spoolFile, size int64, extra ...any) {
	names := make([]string, 0, len(files))
	sizes := make([]int64, 0, len(files))
	for _, f := range files {
		names = append(names, f.name)
		sizes = append(sizes, f.size)
	}
	args := append([]any{"draft_id", in.DraftID, "folder", folder}, extra...)
	args = append(args, "recipients", len(in.To)+len(in.Cc)+len(in.Bcc), "attachments", len(files),
		"filenames", names, "file_sizes", sizes, "size", size)
	logging.From(ctx).Info(event, args...)
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
