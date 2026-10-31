package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Missing guidance on a registered leaf prevents discovery from the binary alone.
func TestOpsLeavesExplainTheirPurposeAndUsage(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	groups := strings.Fields("activity agent-token attachment backup cache calendar credentials daemon database deduplicate deletion docbank documents draft embeddings eval fts gc kata logs mcp message multimodal openapi policy repair serve setup skills update version")
	var leaves int
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		if command.Hidden {
			return
		}
		if command.RunE != nil || command.Run != nil {
			leaves++
			assert.NotEmpty(t, strings.TrimSpace(command.Long), command.CommandPath()+" needs concept guidance")
			assert.Contains(t, command.Example, "msgvault ", command.CommandPath()+" needs a practical invocation")
		}
		for _, child := range command.Commands() {
			visit(child)
		}
	}
	for _, group := range groups {
		command, rest, err := rootCmd.Find([]string{group})
		require.NoError(t, err)
		require.Empty(t, rest)
		visit(command)
	}
	assert.Greater(t, leaves, 100, "the complete ops command area must stay covered")
}

func TestCalendarInvalidTimeNamesValueAndFormat(t *testing.T) {
	_, err := parseCalendarBound("bad-time", "", false)
	require.Error(t, err)
	assert.ErrorContains(t, err, "bad-time")
	assert.ErrorContains(t, err, "2026-10-10T09:00:00-07:00")
}

func TestCalendarMissingAccountNamesTheFlag(t *testing.T) {
	command := newCalendarControlCmd(nil)
	command.SetArgs([]string{"freebusy", "primary"})
	err := command.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "--account")
}
