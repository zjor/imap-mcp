package tools

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
)

// maxThreadRefs caps the Message-IDs searched in Sent (and in the source
// folder without THREAD), so a long References header stays cheap.
const maxThreadRefs = 50

type threadIn struct {
	ID string `json:"id" jsonschema:"id of any message in the thread"`
}

type msgRef struct {
	folder string
	uid    uint32
}

func registerThread(s *mcp.Server, deps Deps) {
	addTool(s, deps, "get_thread",
		"Get all messages of the thread that holds a message, from its folder and from Sent, oldest first. Same shape as read_email, but quoted text is removed from each body.",
		func(in threadIn) []any { return []any{"id", in.ID} },
		func(ctx context.Context, s *imapsafe.Session, in threadIn) (any, int, error) {
			id, err := ids.DecodeMessage(in.ID)
			if err != nil {
				return nil, 0, err
			}
			msgs, err := getThread(ctx, s, deps, id)
			if err != nil {
				return nil, 0, err
			}
			return map[string]any{"messages": msgs}, len(msgs), nil
		})
}

func getThread(ctx context.Context, s *imapsafe.Session, deps Deps, start ids.MessageID) ([]emailOut, error) {
	first, p, err := readMessage(ctx, s, deps, start, "text")
	if err != nil {
		return nil, err
	}
	refs := messageIDs(p.Headers.MessageID, p.Headers.InReplyTo, p.Headers.References)

	// Members in the source folder: THREAD when the server has it,
	// otherwise a header search for the start message's ids.
	uids, err := s.Thread(ctx, start.Folder, start.UID)
	switch {
	case errors.Is(err, imapsafe.ErrUnsupported):
		if uids, err = searchRefs(ctx, s, start.Folder, refs); err != nil {
			return nil, err
		}
	case errors.Is(err, imapsafe.ErrNotFound):
		uids = nil
	case err != nil:
		return nil, err
	}

	seen := map[msgRef]bool{{start.Folder, start.UID}: true}
	msgs := []emailOut{first}
	read := func(folder string, uv uint32, uid uint32) error {
		if seen[msgRef{folder, uid}] {
			return nil
		}
		seen[msgRef{folder, uid}] = true
		e, p, err := readMessage(ctx, s, deps, ids.MessageID{Folder: folder, UIDValidity: uv, UID: uid}, "text")
		if errors.Is(err, imapsafe.ErrNotFound) {
			return nil // removed meanwhile
		}
		if err != nil {
			return err
		}
		refs = append(refs, messageIDs(p.Headers.MessageID, p.Headers.InReplyTo, p.Headers.References)...)
		msgs = append(msgs, e)
		return nil
	}
	for _, u := range uids {
		if err := read(start.Folder, start.UIDValidity, u); err != nil {
			return nil, err
		}
	}

	// Replies the user sent live in Sent.
	if sent := s.Folders().Sent; sent != "" && sent != start.Folder {
		uv, err := s.Examine(ctx, sent)
		if err != nil {
			return nil, err
		}
		found, err := searchRefs(ctx, s, sent, dedupe(refs))
		if err != nil {
			return nil, err
		}
		for _, u := range found {
			if err := read(sent, uv, u); err != nil {
				return nil, err
			}
		}
	}

	for i := range msgs {
		msgs[i].UntrustedContent.Body = stripQuoted(msgs[i].UntrustedContent.Body)
	}
	slices.SortStableFunc(msgs, func(a, b emailOut) int { return a.Headers.Date.Compare(b.Headers.Date) })
	return msgs, nil
}

// searchRefs returns the UIDs (ascending) in folder whose Message-ID,
// In-Reply-To or References header holds one of refs.
func searchRefs(ctx context.Context, s *imapsafe.Session, folder string, refs []string) ([]uint32, error) {
	var all []uint32
	for _, r := range refs {
		for _, h := range []string{"Message-ID", "In-Reply-To", "References"} {
			uids, err := s.SearchHeader(ctx, folder, h, r)
			if err != nil {
				return nil, err
			}
			all = append(all, uids...)
		}
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}

func messageIDs(id, inReplyTo string, references []string) []string {
	var out []string
	for _, v := range append([]string{id, inReplyTo}, references...) {
		if v != "" {
			out = append(out, v)
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		if !seen[v] && len(out) < maxThreadRefs {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

var attributionRe = regexp.MustCompile(`(?i)^(on .+ wrote:|-+ ?original message ?-+)$`)

// stripQuoted removes quoted text: lines starting with ">", and everything
// from an attribution line ("On ... wrote:", "-----Original Message-----")
// that is followed only by quoted or empty lines.
func stripQuoted(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	quoted := func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), ">") }
	cut := len(lines)
	for i, l := range lines {
		if !attributionRe.MatchString(strings.TrimSpace(l)) {
			continue
		}
		rest := lines[i+1:]
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "-") || !slices.ContainsFunc(rest, func(r string) bool {
			return strings.TrimSpace(r) != "" && !quoted(r)
		}) {
			cut = i
			break
		}
	}
	var kept []string
	for _, l := range lines[:cut] {
		if !quoted(l) {
			kept = append(kept, l)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
