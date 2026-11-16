package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/emersion/go-imap/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/logging"
)

// Tool error codes (the "code" field of the error shape).
const (
	codeAuthFailed  = "auth_failed"
	codeStaleID     = "stale_id"
	codeBadID       = "bad_id"
	codeNotFound    = "not_found"
	codeUnsupported = "unsupported"
	codeIMAPError   = "imap_error"
	codeBadArgument = "bad_argument"
	codeNotEditable = "not_editable"
	codeTooLarge    = "too_large"
)

// argError is a caller mistake in the arguments. Its text goes to the
// client, so it must never hold IMAP server text.
type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

func badArg(msg string) error { return &argError{msg} }

// classify maps err to a code and a fixed message. The message never holds
// IMAP server text.
func classify(err error) (code, message string) {
	var ae *argError
	var ie *imap.Error
	switch {
	case errors.As(err, &ae):
		return codeBadArgument, ae.msg
	case errors.Is(err, ids.ErrBadID):
		return codeBadID, "invalid id"
	case errors.Is(err, imapsafe.ErrAuth):
		return codeAuthFailed, "IMAP login failed: wrong username or password"
	case errors.Is(err, imapsafe.ErrNotEditable):
		return codeNotEditable, "draft not found or not editable"
	case errors.Is(err, imapsafe.ErrTooLarge):
		return codeTooLarge, "draft is larger than the server accepts"
	case errors.Is(err, imapsafe.ErrStale):
		return codeStaleID, "message id is stale, search again"
	case errors.Is(err, imapsafe.ErrNotFound):
		return codeNotFound, "message or part not found"
	case errors.As(err, &ie) && ie.Code == imap.ResponseCodeNonExistent:
		return codeNotFound, "folder not found"
	case errors.Is(err, imapsafe.ErrUnsupported):
		return codeUnsupported, "not supported on this server"
	}
	return codeIMAPError, "IMAP server error"
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// errorResult is the tool result for err: IsError set and the JSON error
// shape as text.
func errorResult(ctx context.Context, code, message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(ErrorJSON(ctx, code, message))}}}
}

// Classify maps err to the tool error code and its fixed message. The HTTP
// download endpoint uses it to share the code mapping.
func Classify(err error) (code, message string) { return classify(err) }

// ErrorJSON returns the JSON error shape ({"error":{code,message,request_id}})
// for ctx's request.
func ErrorJSON(ctx context.Context, code, message string) []byte {
	b, _ := json.Marshal(map[string]errorBody{"error": {
		Code: code, Message: message, RequestID: logging.RequestID(ctx),
	}})
	return b
}
