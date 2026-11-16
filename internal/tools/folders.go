package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/imapsafe"
)

type listFoldersIn struct{}

type folderOut struct {
	Name      string `json:"name"`
	Delimiter string `json:"delimiter"`
	Role      string `json:"role"`
	// Total and Unread are null when the server refused STATUS.
	Total  *uint32 `json:"total"`
	Unread *uint32 `json:"unread"`
}

func registerFolders(s *mcp.Server, deps Deps) {
	addTool(s, deps, "list_folders",
		"List the mailbox folders with their special-use role (inbox, sent, drafts, trash, junk, archive) and total and unread message counts. Counts are null when the server does not report them.",
		func(listFoldersIn) []any { return nil },
		func(ctx context.Context, s mailbox, _ listFoldersIn) (any, int, error) {
			folders, err := s.ListFolders(ctx)
			if err != nil {
				return nil, 0, err
			}
			out := make([]folderOut, 0, len(folders))
			for _, f := range folders {
				out = append(out, toFolderOut(f))
			}
			return map[string]any{"folders": out}, len(out), nil
		})
}

func toFolderOut(f imapsafe.Folder) folderOut {
	o := folderOut{Name: f.Name, Delimiter: f.Delimiter, Role: f.Role}
	if f.CountsKnown {
		total, unread := f.Total, f.Unread
		o.Total, o.Unread = &total, &unread
	}
	return o
}
