package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
)

func TestCLIRunCommandAllowedInlineCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"add-inline", "--chat-id", "123"},
		{"add-inline", "--transport", "cli", "--chat-id", "123"},
		{"sync-inline"},
		{"sync-inline", "api.inline.chat:user:42", "--full"},
		{"backfill-inline-media"},
	} {
		assert.True(t, cliRunCommandAllowed(args), "%v must reach the daemon", args)
	}
	assert.False(t, cliRunCommandAllowed([]string{"inline"}))
}

func TestInlineSourceSchedulerClassification(t *testing.T) {
	assertions := assert.New(t)

	t.Parallel()
	name, ok := SchedulerJobNameForSource("inline", "api.inline.chat:user:42")
	require.True(t, ok)
	assertions.Equal(InlineJobName, name)
	classification := classifySourceScheduling("inline", "api.inline.chat:user:42")
	assertions.Equal(sourceScheduleGeneric, classification.kind)
	assertions.Equal(name, classification.jobName)
}

func TestInlineOAuthHandoffIsOnlyAcceptedForAccountSetup(t *testing.T) {
	assertions := assert.New(t)

	t.Parallel()
	srv := &Server{}
	assertions.True(srv.cliRunEnvAllowedForCommand([]string{"add-inline"}, clirun.EnvInlineOAuth))
	assertions.False(srv.cliRunEnvAllowedForCommand([]string{"sync-inline"}, clirun.EnvInlineOAuth))
	assertions.False(srv.cliRunEnvAllowedForCommand([]string{"export-messages"}, clirun.EnvInlineOAuth))
	assertions.False(srv.cliRunEnvAllowedForCommand(nil, clirun.EnvInlineOAuth))
}
