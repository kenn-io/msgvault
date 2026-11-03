package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meta"
)

func renderedMetaHelp(t *testing.T, command *cobra.Command) string {
	t.Helper()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--help"})
	require.NoError(t, command.Execute())
	return output.String()
}

func TestBroadcastHelpUsesConciseConsentDescription(t *testing.T) {
	got := renderedMetaHelp(t, newBroadcastCommand())
	assert.Contains(t, got, "Refresh subscription consent")
	assert.NotContains(t, got, "complete token-list walk")
}

func TestMetaMessagesHelpDescribesProfileRequirements(t *testing.T) {
	for _, tc := range []struct {
		name, want, omit string
		command          func() *cobra.Command
	}{
		{
			name:    "add",
			want:    "With no identifier, register all configured inbox profiles.",
			omit:    "Verify Page identity and the linked Instagram account",
			command: func() *cobra.Command { return newAddMetaMessagesCmd(meta.PlatformMessenger) },
		},
		{
			name:    "sync",
			want:    "Requires registered inbox profiles.",
			omit:    "Backfill registered inbox profiles",
			command: func() *cobra.Command { return newSyncMetaMessagesCmd(meta.PlatformMessenger) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderedMetaHelp(t, tc.command())
			assert.Contains(t, got, tc.want)
			assert.NotContains(t, got, tc.omit)
		})
	}
}
