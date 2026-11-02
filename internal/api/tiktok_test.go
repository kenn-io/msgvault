package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTikTokDaemonCLIAllowlist(t *testing.T) {
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
		assert.Equal(t, tc.allowed, cliRunCommandAllowed(tc.args), tc.args)
	}
	job, ok := SchedulerJobNameForSource("tiktok_comments", "example")
	assert.True(t, ok)
	assert.Equal(t, "tiktok-comments:example", job)
}
