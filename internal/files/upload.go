package files

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjor/imap-mcp/internal/auth"
	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
	"github.com/zjor/imap-mcp/internal/mailparse"
	"github.com/zjor/imap-mcp/internal/tools"
)

// uploadField is the multipart field name of an uploaded file.
const uploadField = "file"

type uploadOut struct {
	DraftID     string                  `json:"draft_id"`
	Attachments []tools.DraftAttachment `json:"attachments"`
	Warning     string                  `json:"warning,omitempty"`
}

// UploadHandler serves POST /drafts/{draft_id}/attachments (spec 7.5
// upload flow). It reads the id from the "draft_id" path value. The whole
// body goes to temp files before the IMAP session opens, so a slow client
// holds no IMAP slot.
func UploadHandler(d *imapsafe.Dialer, maxUpload int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upload(d, maxUpload, w, r)
	})
}

func upload(d *imapsafe.Dialer, maxUpload int64, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(ctx, w, http.StatusMethodNotAllowed, "bad_argument", "method not allowed")
		return
	}
	draftID := r.PathValue("draft_id")
	if err := ids.CheckDraftID(draftID); err != nil {
		fail(ctx, w, "upload.failed", err)
		return
	}
	creds, ok := auth.CredentialsFrom(ctx)
	if !ok {
		writeError(ctx, w, http.StatusUnauthorized, "auth_failed", "missing credentials")
		return
	}

	start := time.Now()
	var files []tools.UploadFile
	defer func() {
		for _, f := range files {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				logging.From(ctx).Error("upload.tempfile_cleanup", "error", err.Error())
			}
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := receive(r, &files); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			writeError(ctx, w, http.StatusRequestEntityTooLarge, "too_large", "upload is larger than the server accepts")
		case ctx.Err() != nil:
			logging.From(ctx).Warn("upload.aborted", "draft_id", draftID, "error", err.Error())
		default:
			fail(ctx, w, "upload.failed", err)
		}
		return
	}

	s, err := d.Open(ctx, creds)
	if err != nil {
		fail(ctx, w, "upload.failed", err)
		return
	}
	defer s.Close()
	res, err := tools.AddAttachments(ctx, s, draftID, files)
	if err != nil {
		fail(ctx, w, "upload.failed", err)
		return
	}

	var total int64
	for _, f := range files {
		total += f.Size
	}
	logging.From(ctx).Info("draft.attachments_added", "draft_id", draftID, "files", len(files),
		"size", total, "draft_size", res.Size, "duration_ms", time.Since(start).Milliseconds())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(uploadOut{DraftID: draftID, Attachments: res.Attachments, Warning: res.Warning})
}

// receive writes every "file" part of the multipart body to a temp file.
// It adds each file to *files as soon as it exists, so the caller can
// remove it on any error or panic.
func receive(r *http.Request, files *[]tools.UploadFile) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return badArg("the body must be multipart/form-data")
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return readErr(err)
		}
		if p.FormName() != uploadField {
			return badArg(`unexpected form field; send files in fields named "file"`)
		}
		tmp, err := os.CreateTemp("", "imap-mcp-upload-*")
		if err != nil {
			return err
		}
		name := mailparse.CleanFilename(p.FileName())
		*files = append(*files, tools.UploadFile{Filename: name, ContentType: partType(p.Header.Get("Content-Type"), name), Path: tmp.Name()})
		n, cerr := io.Copy(tmp, p)
		if err := errors.Join(cerr, tmp.Close()); err != nil {
			return readErr(err)
		}
		(*files)[len(*files)-1].Size = n
	}
	if len(*files) == 0 {
		return badArg(`no files; send them in fields named "file"`)
	}
	return nil
}

// readErr keeps a size error, and turns a broken body into a bad argument.
func readErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return err // disk trouble, not the client
	}
	return badArg("cannot read the multipart body")
}

// partType is the part's own media type when it is valid, else the type
// for the file extension, else application/octet-stream.
func partType(header, name string) string {
	if mt, _, err := mime.ParseMediaType(header); err == nil {
		if i := strings.IndexByte(mt, '/'); i > 0 && i < len(mt)-1 {
			return mt
		}
	}
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// argError is a client mistake; its text goes to the client.
type argError string

func (e argError) Error() string { return string(e) }

func badArg(msg string) error { return argError(msg) }

// classify is tools.Classify plus the errors of this package.
func classify(err error) (code, msg string) {
	var ae argError
	if errors.As(err, &ae) {
		return "bad_argument", string(ae)
	}
	return tools.Classify(err)
}
