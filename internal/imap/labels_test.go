package imap

import (
	"strings"
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

func TestSystemRoleForMailboxTrustsSpecialUseNotName(t *testing.T) {
	assert.Equal(t, "sent", systemRoleForMailbox([]imap.MailboxAttr{imap.MailboxAttrSent}))
	assert.Empty(t, systemRoleForMailbox(nil), "a folder name is not part of role classification")
}

func TestSystemRoleForMailbox(t *testing.T) {
	tests := []struct {
		name  string
		attrs []imap.MailboxAttr
		want  string
	}{
		{name: "empty", attrs: []imap.MailboxAttr{}},
		{name: "drafts", attrs: []imap.MailboxAttr{imap.MailboxAttrDrafts}, want: "drafts"},
		{name: "sent then drafts", attrs: []imap.MailboxAttr{imap.MailboxAttrSent, imap.MailboxAttrDrafts}, want: "drafts"},
		{name: "drafts then sent", attrs: []imap.MailboxAttr{imap.MailboxAttrDrafts, imap.MailboxAttrSent}, want: "drafts"},
		{name: "repeated conflict", attrs: []imap.MailboxAttr{imap.MailboxAttrSent, imap.MailboxAttrJunk, imap.MailboxAttrDrafts, imap.MailboxAttrSent, imap.MailboxAttrDrafts}, want: "drafts"},
		{name: "unrelated", attrs: []imap.MailboxAttr{imap.MailboxAttrArchive, imap.MailboxAttrTrash, imap.MailboxAttrJunk}},
		{name: "near matches", attrs: []imap.MailboxAttr{`\drafts`, `\DRAFTS`, `\Draft`, ` \Drafts`, `\Drafts `, `x\Drafts`, `\Draftsx`, `\sent`, `\SENT`, `prefix\Sent suffix`, ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, systemRoleForMailbox(tt.attrs))
		})
	}
}

func FuzzSystemRoleForMailbox(f *testing.F) {
	for _, seed := range []string{
		"", `\Sent`, `\Drafts`, "\\Sent\x00\\Drafts", "\\Drafts\x00\\Sent",
		"\\Sent\x00\\Junk\x00\\Drafts\x00\\Sent\x00\\Drafts",
		"\\Archive\x00\\Trash\x00\\Flagged", "\\drafts\x00\\Draft\x00 \\Drafts\x00\\Sent ",
	} {
		f.Add(seed, "")
	}
	f.Fuzz(func(t *testing.T, raw, extra string) {
		parts := append(strings.Split(raw, "\x00"), extra)
		attrs := make([]imap.MailboxAttr, len(parts))
		present := make(map[string]bool, len(parts))
		for i, part := range parts {
			attrs[i] = imap.MailboxAttr(part)
			present[part] = true
		}
		// Pin all four evidence combinations independently of check order.
		roles := map[[2]bool]string{
			{false, false}: "", {true, false}: "sent",
			{false, true}: "drafts", {true, true}: "drafts",
		}
		want := roles[[2]bool{present[`\Sent`], present[`\Drafts`]}]
		assert.Equal(t, want, systemRoleForMailbox(attrs))
	})
}
