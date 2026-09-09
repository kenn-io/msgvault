package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCLIRunDraftAllowlist(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"draft-reply", "42", "--from=alice@example.com", "--body=body"}))
	assert.False(t, cliRunCommandAllowed([]string{"configure-imap-drafts"}))
	assert.False(t, cliRunCommandAllowed([]string{"draft-reply"}))
}
