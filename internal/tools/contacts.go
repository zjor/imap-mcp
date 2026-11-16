package tools

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zjor/imap-mcp/internal/imapsafe"
)

const (
	contactMonths = 12
	// maxContactScan caps the newest matching messages read per folder.
	maxContactScan = 1000
)

type contactsIn struct {
	Query string `json:"query" jsonschema:"text to find in a name or address, for example a name or a domain"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum contacts, default 20, max 100"`
}

type contactOut struct {
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	LastContact  time.Time `json:"last_contact"`
	MessageCount int       `json:"message_count"`
}

func registerContacts(s *mcp.Server, deps Deps) {
	addTool(s, deps, "search_contacts",
		"Find people by name or address in the From, To and Cc of messages in INBOX and Sent from the last 12 months. Returns name, email, last contact date and message count, most recent contact first.",
		func(in contactsIn) []any { return []any{"limit", clampLimit(in.Limit), "filters", []string{"query"}} },
		func(ctx context.Context, s *imapsafe.Session, in contactsIn) (any, int, error) {
			debugFilters(ctx, "search_contacts", "query", in.Query)
			if strings.TrimSpace(in.Query) == "" {
				return nil, 0, badArg("query must not be empty")
			}
			contacts, err := searchContacts(ctx, s, in.Query, clampLimit(in.Limit))
			if err != nil {
				return nil, 0, err
			}
			return map[string]any{"contacts": contacts}, len(contacts), nil
		})
}

func searchContacts(ctx context.Context, s *imapsafe.Session, query string, limit int) ([]contactOut, error) {
	since := time.Now().AddDate(0, -contactMonths, 0)
	q := strings.ToLower(query)
	byEmail := map[string]*contactOut{}

	folders := []string{"INBOX"}
	if sent := s.Folders().Sent; sent != "" && sent != "INBOX" {
		folders = append(folders, sent)
	}
	for _, folder := range folders {
		// IMAP SEARCH has no OR in SearchCriteria, so run one search per
		// header and merge.
		var uids []uint32
		for _, c := range []imapsafe.SearchCriteria{{From: query}, {To: query}, {Cc: query}} {
			c.Since = since
			found, err := s.Search(ctx, folder, c, 0, 0)
			if err != nil {
				return nil, err
			}
			uids = append(uids, found...)
		}
		slices.Sort(uids)
		uids = slices.Compact(uids)
		slices.Reverse(uids)
		if len(uids) > maxContactScan {
			uids = uids[:maxContactScan]
		}
		sums, err := s.FetchEnvelopes(ctx, folder, uids)
		if err != nil {
			return nil, err
		}
		for _, m := range sums {
			date := msgDate(m)
			counted := map[string]bool{}
			for _, a := range slices.Concat(m.From, m.To, m.Cc) {
				email := strings.ToLower(a.Addr)
				if email == "" || counted[email] || !(strings.Contains(email, q) || strings.Contains(strings.ToLower(a.Name), q)) {
					continue
				}
				counted[email] = true
				c := byEmail[email]
				if c == nil {
					c = &contactOut{Email: email}
					byEmail[email] = c
				}
				c.MessageCount++
				if !date.Before(c.LastContact) {
					c.LastContact = date
					if a.Name != "" {
						c.Name = a.Name
					}
				}
				if c.Name == "" {
					c.Name = a.Name
				}
			}
		}
	}

	out := make([]contactOut, 0, len(byEmail))
	for _, c := range byEmail {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b contactOut) int {
		if c := b.LastContact.Compare(a.LastContact); c != 0 {
			return c
		}
		return strings.Compare(a.Email, b.Email)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
