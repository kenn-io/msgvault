package api

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommentModerateAllowedThroughDaemonCLIRunner(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"comment", "moderate", "--delete", "--yes", "42"}))
	assert.True(cliRunCommandAllowed([]string{"comment", "reply", "--body=Thanks", "42"}))
	assert.False(cliRunCommandAllowed([]string{"comment"}))
	assert.False(cliRunCommandAllowed([]string{"comment", "triage", "42"}))
}

func TestRedactedCLIRunArgs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		args, want []string
	}{
		{"separate value", []string{"comment", "reply", "42", "--body", "Thanks"}, []string{"comment", "reply", "42", "--body", "[redacted]"}},
		{"joined value", []string{"comment", "reply", "--body=Thanks", "42"}, []string{"comment", "reply", "--body=[redacted]", "42"}},
		{"missing value", []string{"comment", "reply", "42", "--body"}, []string{"comment", "reply", "42", "--body"}},
		{"other command", []string{"comment", "moderate", "42", "--body", "Thanks"}, []string{"comment", "moderate", "42", "--body", "Thanks"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := slices.Clone(tt.args)
			assert.Equal(t, tt.want, RedactedCLIRunArgs(tt.args))
			assert.Equal(t, original, tt.args, "the caller's args are unchanged")
		})
	}
}
