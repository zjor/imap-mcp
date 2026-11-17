package tools

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/ids"
	"github.com/zjor/imap-mcp/internal/imapsafe"
	"github.com/zjor/imap-mcp/internal/mailparse"
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
		"Get the messages of the thread that holds a message, from its folder and from Sent, oldest first. Same shape as read_email, but quoted text is removed from each body and each body is cut at 64 KiB (body_truncated). At most the newest 50 messages are returned (truncated: true when there were more).",
		func(in threadIn) []any { return []any{"id", in.ID} },
		func(ctx context.Context, s mailbox, in threadIn) (any, int, error) {
			id, err := ids.DecodeMessage(in.ID)
			if err != nil {
				return nil, 0, err
			}
			out, err := getThread(ctx, s, deps, id)
			if err != nil {
				return nil, 0, err
			}
			return out, len(out.Messages), nil
		})
}

// maxThreadMessages caps the messages get_thread reads; the newest are
// kept. A variable so tests can lower it.
var maxThreadMessages = 50

// maxThreadBody caps each body get_thread returns, in bytes of text.
const maxThreadBody = 64 << 10

type threadOut struct {
	Messages []emailOut `json:"messages"`
	// Truncated is true when the thread had more than maxThreadMessages
	// messages and only the newest were returned.
	Truncated bool `json:"truncated,omitempty"`
}

// threadCand is a thread member found by THREAD or a header search, before
// its full message is read.
type threadCand struct {
	id   ids.MessageID
	date time.Time
}

func getThread(ctx context.Context, s mailbox, deps Deps, start ids.MessageID) (threadOut, error) {
	first, p, err := readMessage(ctx, s, deps, start, "text")
	if err != nil {
		return threadOut{}, err
	}
	refs := messageIDs(p.Headers.MessageID, p.Headers.InReplyTo, p.Headers.References)

	// Members in the source folder: THREAD when the server has it,
	// otherwise a header search for the start message's ids.
	uids, err := s.Thread(ctx, start.Folder, start.UID)
	switch {
	case errors.Is(err, imapsafe.ErrUnsupported):
		if uids, err = searchRefs(ctx, s, start.Folder, refs); err != nil {
			return threadOut{}, err
		}
	case errors.Is(err, imapsafe.ErrNotFound):
		uids = nil
	case err != nil:
		return threadOut{}, err
	}

	// Collect the members with ENVELOPE only; full messages are read
	// after the cap.
	cands := []threadCand{{id: start, date: first.Headers.Date}}
	seen := map[msgRef]bool{{start.Folder, start.UID}: true}
	add := func(folder string, uv uint32, uids []uint32) error {
		var todo []uint32
		for _, u := range uids {
			if !seen[msgRef{folder, u}] {
				seen[msgRef{folder, u}] = true
				todo = append(todo, u)
			}
		}
		sums, err := s.FetchEnvelopes(ctx, folder, todo)
		if err != nil {
			return err
		}
		for _, m := range sums {
			cands = append(cands, threadCand{id: ids.MessageID{Folder: folder, UIDValidity: uv, UID: m.UID}, date: msgDate(m)})
			refs = append(refs, messageIDs(m.MessageID, "", m.InReplyTo)...)
		}
		return nil
	}
	if err := add(start.Folder, start.UIDValidity, uids); err != nil {
		return threadOut{}, err
	}

	// Replies the user sent live in Sent.
	if sent := s.Folders().Sent; sent != "" && sent != start.Folder {
		uv, err := s.Examine(ctx, sent)
		if err != nil {
			return threadOut{}, err
		}
		found, err := searchRefs(ctx, s, sent, dedupe(refs))
		if err != nil {
			return threadOut{}, err
		}
		if err := add(sent, uv, found); err != nil {
			return threadOut{}, err
		}
	}

	var out threadOut
	slices.SortStableFunc(cands, func(a, b threadCand) int { return a.date.Compare(b.date) })
	if len(cands) > maxThreadMessages {
		cands = cands[len(cands)-maxThreadMessages:]
		out.Truncated = true
	}
	for _, c := range cands {
		e := first
		if c.id != start {
			e, _, err = readMessage(ctx, s, deps, c.id, "text")
			if errors.Is(err, imapsafe.ErrNotFound) || errors.Is(err, mailparse.ErrUnparseable) {
				continue // removed meanwhile, or broken: keep the others
			}
			if err != nil {
				return threadOut{}, err
			}
		}
		body, cut := capText(stripQuoted(e.UntrustedContent.Body), maxThreadBody)
		e.UntrustedContent.Body = body
		e.BodyTruncated = e.BodyTruncated || cut
		out.Messages = append(out.Messages, e)
	}
	slices.SortStableFunc(out.Messages, func(a, b emailOut) int { return a.Headers.Date.Compare(b.Headers.Date) })
	return out, nil
}

// capText cuts s to at most n bytes at a rune boundary.
func capText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// searchRefs returns the UIDs (ascending) in folder whose Message-ID,
// In-Reply-To or References header holds one of refs.
func searchRefs(ctx context.Context, s mailbox, folder string, refs []string) ([]uint32, error) {
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
