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
	} {
		assertions.Equal(tc.allowed, cliRunCommandAllowed(tc.args), tc.args)
	}
	job, ok := SchedulerJobNameForSource("tiktok_comments", "example")
	assertions.True(ok)
	assertions.Equal("tiktok-comments:example", job)
}
