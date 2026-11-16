package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/zjor/imap-mcp/internal/drafts"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/mailparse"
)

// DraftAttachment describes one attachment of a stored draft.
type DraftAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// UploadFile is an uploaded file the caller already wrote to disk. The
// caller owns Path and removes it.
type UploadFile struct {
	Filename, ContentType, Path string
	Size                        int64
}

// AddedAttachments is the result of AddAttachments.
type AddedAttachments struct {
	// Attachments lists every attachment of the new draft: old and new.
	Attachments []DraftAttachment
	// Size is the size of the new message.
	Size int64
	// Warning is set when the old copy could not be removed.
	Warning string
}

// AddAttachments rewrites the own draft draftID with its old attachments
// plus files (spec 7.5 upload flow, 7.6). It works like update_draft with
// extra files.
func AddAttachments(ctx context.Context, s *imapsafe.Session, draftID string, files []UploadFile) (AddedAttachments, error) {
	extra := make([]spoolFile, 0, len(files))
	for _, f := range files {
		extra = append(extra, spoolFile{name: f.Filename, contentType: f.ContentType, path: f.Path, size: f.Size, external: true})
	}
	rb, err := rebuildDraft(ctx, s, draftID, func(old mailparse.Parsed, _ *drafts.Input) (func(mailparse.Attachment) bool, error) {
		if old.Truncated {
			return nil, badArg("the draft body is too large to keep; set body_text and body_html with update_draft")
		}
		return func(mailparse.Attachment) bool { return true }, nil
	}, extra)
	if err != nil {
		return AddedAttachments{}, err
	}
	return AddedAttachments{Attachments: attachmentsOut(rb.files), Size: rb.size, Warning: rb.warning}, nil
}

// editFunc changes the new draft input, which starts as a copy of the old
// draft. It returns which old attachments to keep.
type editFunc func(old mailparse.Parsed, in *drafts.Input) (keep func(mailparse.Attachment) bool, err error)

type rebuilt struct {
	input   drafts.Input
	files   []spoolFile
	size    int64
	warning string
}

// rebuildDraft replaces the own draft draftID: it reads the old copy, lets
// edit change it, appends the new copy with the kept old attachments plus
// extra, then removes the old copy. The draft id stays the same.
func rebuildDraft(ctx context.Context, s *imapsafe.Session, draftID string, edit editFunc, extra []spoolFile) (rebuilt, error) {
	// Check first, so nothing is appended when the old copy cannot go.
	if !s.Caps().UIDPlus {
		return rebuilt{}, imapsafe.ErrUnsupported
	}
	if err := ids.CheckDraftID(draftID); err != nil {
		return rebuilt{}, err
	}
	uid, uv, err := s.FindDraft(ctx, draftID)
	if err != nil {
		return rebuilt{}, err
	}
	mid := ids.MessageID{Folder: s.Folders().Drafts, UIDValidity: uv, UID: uid}
	_, old, err := readMessage(ctx, s, Deps{}, mid, "")
	if err != nil {
		return rebuilt{}, err
	}
	input := draftInput(old, draftID)
	keep, err := edit(old, &input)
	if err != nil {
		return rebuilt{}, err
	}

	var sp spool
	defer sp.remove(ctx)
	for _, a := range old.Attachments {
		if !keep(a) {
			continue
		}
		if err := sp.add(ctx, s, ids.AttachmentID{MessageID: mid, Part: a.Part}, a.Filename, a.ContentType); err != nil {
			return rebuilt{}, err
		}
	}
	sp.files = append(sp.files, extra...)

	size, err := storeDraft(ctx, s, &input, &sp)
	if err != nil {
		return rebuilt{}, err
	}
	out := rebuilt{input: input, files: sp.files, size: size}
	if err := s.RemoveOwnDraft(ctx, draftID, uv, uid); err != nil {
		logging.From(ctx).Error("draft.duplicate_left", "draft_id", draftID, "uid", uid, "error", err.Error())
		out.warning = "old copy could not be removed"
	}
	return out, nil
}

// draftInput is the input that rebuilds old as it is.
func draftInput(old mailparse.Parsed, draftID string) drafts.Input {
	h := old.Headers
	input := drafts.Input{
		Mode: "new", To: addrStrings(h.To), Cc: addrStrings(h.Cc), Bcc: addrStrings(h.Bcc),
		Subject: h.Subject, BodyText: old.Text, BodyHTML: old.HTML, DraftID: draftID,
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
	return input
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
	// external files belong to the caller; remove skips them.
	external bool
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

// remove deletes the temp files it owns. It runs deferred, so also on
// error and panic.
func (sp *spool) remove(ctx context.Context) {
	for _, f := range sp.files {
		if f.external {
			continue
		}
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logging.From(ctx).Error("draft.tempfile_cleanup", "error", err.Error())
		}
	}
}

func attachmentsOut(files []spoolFile) []DraftAttachment {
	out := make([]DraftAttachment, 0, len(files))
	for _, f := range files {
		out = append(out, DraftAttachment{Filename: f.name, ContentType: f.contentType, Size: f.size})
	}
	return out
}

// keepAllBut keeps the attachments whose name is not in names.
func keepAllBut(names []string) func(mailparse.Attachment) bool {
	return func(a mailparse.Attachment) bool { return !slices.Contains(names, a.Filename) }
}
