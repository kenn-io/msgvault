package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFeedCommandsAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"source", "feeds", "add", "https://example.com/feed.xml"},
		{"source", "feeds", "add", "--opml", "/subscriptions.opml"},
		{"source", "feeds", "list", "--all", "--json"},
		{"source", "feeds", "remove", "https://example.com/feed.xml"},
		{"source", "feeds", "sync", "--full"},
	} {
		assert.True(t, cliRunCommandAllowed(args), "%v", args)
	}
}
