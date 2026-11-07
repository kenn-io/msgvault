package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFeedCommandsAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"add-feed", "https://example.com/feed.xml"},
		{"add-feed", "--opml", "/subscriptions.opml"},
		{"list-feeds", "--all", "--json"},
		{"remove-feed", "https://example.com/feed.xml"},
		{"sync-feeds", "--full"},
	} {
		assert.True(t, cliRunCommandAllowed(args), "%v", args)
	}
}
