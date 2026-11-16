package imapsafe

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/zjor/imap-mcp/internal/logging"
)

// logCmd writes the debug line for one IMAP command (spec 7.8). It never
// logs arguments other than the folder name and a UID count.
func logCmd(log *slog.Logger, cmd, folder string, uids int, start time.Time, err error) {
	log.Debug("imap.cmd",
		"cmd", cmd,
		"folder", folder,
		"uids", uids,
		"duration_ms", time.Since(start).Milliseconds(),
		"status", cmdStatus(err))
}

// cmdStatus is "OK", the server's NO/BAD, or "error" for client-side
// failures (closed connection, timeout).
func cmdStatus(err error) string {
	if err == nil {
		return "OK"
	}
	var ie *imap.Error
	if errors.As(err, &ie) {
		return string(ie.Type)
	}
	return "error"
}

func logWarnStale(ctx context.Context, folder string) {
	logging.From(ctx).Warn("imap.stale_id", "folder", folder)
}
