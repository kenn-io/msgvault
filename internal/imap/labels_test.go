package imap

import (
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
)

func TestClassifyLabelType(t *testing.T) {
	tests := []struct {
		name    string
		mailbox string
		attrs   []imap.MailboxAttr
		want    string
	}{
		// Name-based detection
		{name: "INBOX", mailbox: "INBOX", want: "system"},
		{name: "inbox lowercase", mailbox: "inbox", want: "system"},
		{name: "Sent", mailbox: "Sent", want: "system"},
		{name: "Sent Items", mailbox: "Sent Items", want: "system"},
		{name: "Sent Messages", mailbox: "Sent Messages", want: "system"},
		{name: "Drafts", mailbox: "Drafts", want: "system"},
		{name: "Draft", mailbox: "Draft", want: "system"},
		{name: "Trash", mailbox: "Trash", want: "system"},
		{name: "Deleted Items", mailbox: "Deleted Items", want: "system"},
		{name: "Deleted Messages", mailbox: "Deleted Messages", want: "system"},
		{name: "Junk", mailbox: "Junk", want: "system"},
		{name: "Bulk Mail", mailbox: "Bulk Mail", want: "system"},
		{name: "Spam", mailbox: "Spam", want: "system"},
		{name: "Archive", mailbox: "Archive", want: "system"},
		{name: "All Mail", mailbox: "All Mail", want: "system"},
		{name: "Gmail All Mail", mailbox: "[Gmail]/All Mail", want: "system"},

		// Attribute-based detection
		{
			name:    "attr Sent",
			mailbox: "custom-sent",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrSent},
			want:    "system",
		},
		{
			name:    "attr Drafts",
			mailbox: "custom-drafts",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrDrafts},
			want:    "system",
		},
		{
			name:    "attr Trash",
			mailbox: "custom-trash",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrTrash},
			want:    "system",
		},
		{
			name:    "attr Junk",
			mailbox: "custom-junk",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrJunk},
			want:    "system",
		},
		{
			name:    "attr All",
			mailbox: "custom-all",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrAll},
			want:    "system",
		},
		{
			name:    "attr Archive",
			mailbox: "custom-archive",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrArchive},
			want:    "system",
		},
		{
			name:    "attr Flagged",
			mailbox: "custom-flagged",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrFlagged},
			want:    "system",
		},

		// Custom folder → user
		{name: "custom folder", mailbox: "Projects/Work", want: "user"},
		{name: "custom with attrs", mailbox: "MyFolder",
			attrs: []imap.MailboxAttr{imap.MailboxAttrHasChildren},
			want:  "user",
		},

		// Attribute takes priority over name
		{
			name:    "attr overrides unknown name",
			mailbox: "Papierkorb",
			attrs:   []imap.MailboxAttr{imap.MailboxAttrTrash},
			want:    "system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLabelType(tt.mailbox, tt.attrs)
			assert.Equal(t, tt.want, got, "classifyLabelType(%q, %v)", tt.mailbox, tt.attrs)
		})
	}
}
