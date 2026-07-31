package imap

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterMailboxes(t *testing.T) {
	cases := []struct {
		name     string
		all      []string
		include  []string
		exclude  []string
		expected []string
	}{
		{
			name:     "full list when no filters",
			all:      []string{"Inbox", "Sent", "Trash"},
			expected: []string{"Inbox", "Sent", "Trash"},
		},
		{
			name:     "include only",
			all:      []string{"Inbox", "Sent Items", "Trash", "Archive"},
			include:  []string{"Inbox", "Sent Items"},
			expected: []string{"Inbox", "Sent Items"},
		},
		{
			name:     "include only case insensitive",
			all:      []string{"Inbox", "Sent Items", "trash", "Archive"},
			include:  []string{"inbox", "SENT ITEMS"},
			expected: []string{"Inbox", "Sent Items"},
		},
		{
			name:     "exclude only",
			all:      []string{"Inbox", "Sent Items", "Trash", "Archive"},
			exclude:  []string{"Trash"},
			expected: []string{"Inbox", "Sent Items", "Archive"},
		},
		{
			name:     "exclude only case insensitive",
			all:      []string{"Inbox", "Sent Items", "trash", "Archive"},
			exclude:  []string{"TRASH"},
			expected: []string{"Inbox", "Sent Items", "Archive"},
		},
		{
			name:     "include and exclude",
			all:      []string{"Inbox", "Sent Items", "Trash", "Archive", "Projects"},
			include:  []string{"Inbox", "Projects", "Trash"},
			exclude:  []string{"Trash"},
			expected: []string{"Inbox", "Projects"},
		},
		{
			name:     "include no match returns empty slice",
			all:      []string{"Inbox", "Sent"},
			include:  []string{"Deleted Items"},
			expected: []string{},
		},
		{
			name:     "exclude all returns empty slice",
			all:      []string{"Inbox", "Sent"},
			exclude:  []string{"Inbox", "Sent"},
			expected: []string{},
		},
		{
			name:     "empty input",
			all:      nil,
			include:  []string{"Inbox"},
			expected: []string{},
		},
		{
			name:     "include with duplicates",
			all:      []string{"Inbox", "Sent", "Inbox", "Trash"},
			include:  []string{"Inbox", "Sent"},
			expected: []string{"Inbox", "Sent", "Inbox"},
		},
		{
			name:     "exclude only removes specified",
			all:      []string{"Inbox", "Deleted Items", "Search Results", "Sent Items"},
			exclude:  []string{"Deleted Items", "Search Results"},
			expected: []string{"Inbox", "Sent Items"},
		},
		{
			name:     "mixed case mailboxes",
			all:      []string{"Inbox", "Sent Items", "trash"},
			include:  []string{"inbox", "sent items"},
			expected: []string{"Inbox", "Sent Items"},
		},
		{
			name:     "nested folders",
			all:      []string{"Projects/Alpha", "Projects/Beta", "Inbox"},
			include:  []string{"Projects/Alpha", "inbox"},
			expected: []string{"Projects/Alpha", "Inbox"},
		},
		{
			name:     "non-matching filter returns empty slice",
			all:      []string{"Inbox", "Sent", "Trash"},
			include:  []string{"Nonexistent"},
			expected: []string{},
		},
		{
			name:     "empty include single exclude",
			all:      []string{"Inbox", "Sent", "Trash"},
			exclude:  []string{"Trash"},
			expected: []string{"Inbox", "Sent"},
		},
		{
			name:     "empty exclude single include",
			all:      []string{"Inbox", "Sent", "Trash"},
			include:  []string{"Inbox"},
			expected: []string{"Inbox"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, filterMailboxes(tc.all, tc.include, tc.exclude))
		})
	}
}
