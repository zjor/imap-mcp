// Package apierr builds the JSON error shape shared by tool results and the
// HTTP endpoints: {"error":{"code","message","request_id"}}.
package apierr

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/zjor/imap-mcp/internal/logging"
)

type body struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// JSON returns the error shape for ctx's request.
func JSON(ctx context.Context, code, message string) []byte {
	b, _ := json.Marshal(map[string]body{"error": {
		Code: code, Message: message, RequestID: logging.RequestID(ctx),
	}})
	return b
}

// Write sends the error shape with status.
func Write(ctx context.Context, w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(JSON(ctx, code, message))
}
