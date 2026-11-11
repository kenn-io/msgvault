package oauth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHeadlessAccountArgsCanonicalPaths(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"add-account", []string{"msgvault", "source", "gmail", "add", "reader@example.com", "--oauth-app", "work team", "--readonly"}},
		{"add-calendar", []string{"msgvault", "source", "calendar", "add", "reader@example.com", "--oauth-app", "work team", "--write"}},
		{"sync-calendar", []string{"msgvault", "source", "calendar", "sync", "reader@example.com", "--oauth-app", "work team"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			assert.Equal(t, tc.want, HeadlessAccountArgs(tc.command, "reader@example.com", "work team", true, true))
		})
	}
}
