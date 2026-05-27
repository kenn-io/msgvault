package imap

import (
	"strings"

	imap "github.com/emersion/go-imap/v2"
)

// systemAttrs maps RFC 6154 special-use attributes to system labels.
var systemAttrs = map[imap.MailboxAttr]bool{
	imap.MailboxAttrSent:    true,
	imap.MailboxAttrDrafts:  true,
	imap.MailboxAttrTrash:   true,
	imap.MailboxAttrJunk:    true,
	imap.MailboxAttrAll:     true,
	imap.MailboxAttrArchive: true,
	imap.MailboxAttrFlagged: true,
}

// systemNames lists folder names (lowercase) that are system labels
// across common IMAP providers.
var systemNames = map[string]bool{
	"inbox":            true,
	"sent":             true,
	"sent items":       true,
	"sent messages":    true,
	"drafts":           true,
	"draft":            true,
	"trash":            true,
	"deleted items":    true,
	"deleted messages": true,
	"junk":             true,
	"bulk mail":        true,
	"spam":             true,
	"archive":          true,
	"all mail":         true,
	"[gmail]/all mail": true,
}

// labelTypeSystem is the label_type value for standard IMAP folders.
const labelTypeSystem = "system"

// classifyLabelType returns "system" for standard IMAP folders
// (detected via RFC 6154 attributes or well-known folder names)
// and "user" for everything else.
func classifyLabelType(
	mailbox string,
	attrs []imap.MailboxAttr,
) string {
	for _, a := range attrs {
		if systemAttrs[a] {
			return labelTypeSystem
		}
	}
	if systemNames[strings.ToLower(mailbox)] {
		return labelTypeSystem
	}
	return "user"
}
