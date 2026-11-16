package imapsafe

import (
	"fmt"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// SpecialFolders holds the mailbox names of the special-use folders.
// An empty name means the folder was not found.
type SpecialFolders struct {
	Drafts, Sent, Trash, Junk string
}

// resolveFolders finds special-use folders with LIST. It asks for
// SPECIAL-USE attributes when the server has the extension; otherwise it
// still reads any attributes the plain LIST returns. Overrides win.
func resolveFolders(c *imapclient.Client, caps imap.CapSet, draftsOverride, sentOverride string) (SpecialFolders, error) {
	var opts *imap.ListOptions
	if caps.Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	list, err := c.List("", "*", opts).Collect()
	if err != nil {
		return SpecialFolders{}, fmt.Errorf("imap list: %w", err)
	}

	var f SpecialFolders
	for _, mb := range list {
		for _, a := range mb.Attrs {
			var dst *string
			switch a {
			case imap.MailboxAttrDrafts:
				dst = &f.Drafts
			case imap.MailboxAttrSent:
				dst = &f.Sent
			case imap.MailboxAttrTrash:
				dst = &f.Trash
			case imap.MailboxAttrJunk:
				dst = &f.Junk
			}
			if dst != nil && *dst == "" {
				*dst = mb.Mailbox
			}
		}
	}
	if draftsOverride != "" {
		f.Drafts = draftsOverride
	}
	if sentOverride != "" {
		f.Sent = sentOverride
	}
	return f, nil
}
