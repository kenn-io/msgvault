package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTikTokDaemonCLIAllowlist(t *testing.T) {
	assertions := assert.New(t)
	for _, tc := range []struct {
		args    []string
		allowed bool
	}{
		{[]string{"source", "tiktok-comments", "add", "example"}, true},
		{[]string{"source", "tiktok-comments", "sync", "example", "--json"}, true},
		{[]string{"source", "tiktok-comments", "delete", "example"}, false},
		{[]string{"source", "other", "sync"}, false},
		{[]string{"source", "tiktok-comments"}, false},
		{[]string{"source", "tiktok-messages", "add", "example"}, true},
		{[]string{"source", "tiktok-messages", "sync", "example"}, true},
		{[]string{"source", "tiktok-messages", "status"}, true},
		{[]string{"source", "tiktok-messages", "replay"}, true},
		{[]string{"source", "tiktok-messages", "send"}, false},
	} {
		assertions.Equal(tc.allowed, cliRunCommandAllowed(tc.args), tc.args)
	}
	job, ok := SchedulerJobNameForSource("tiktok_comments", "example")
	assertions.True(ok)
	assertions.Equal("tiktok-comments:example", job)
	job, ok = SchedulerJobNameForSource("tiktok_messages", "example")
	assertions.True(ok)
	assertions.Equal("tiktok-messages:example", job)
	assertions.Equal("source tiktok-messages status", cliRunCommandWords([]string{"source", "tiktok-messages", "status"}))
	assertions.True(cliRunReadOnlyCommands["source tiktok-messages status"])
}
