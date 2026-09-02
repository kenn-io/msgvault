package cmd

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
)

func TestImportSlackdumpCommandRequiresIdentity(t *testing.T) {
	command := newImportSlackdumpCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"export.zip"})

	err := command.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, `required flag(s) "me" not set`)
}

func TestImportSlackdumpCommandRejectsNegativeLimitBeforeRouting(t *testing.T) {
	command := newImportSlackdumpCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--me", "UALICE", "--limit", "-1", "export.zip"})

	err := command.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "--limit must be non-negative")
}

func TestImportSlackdumpCommandForwardsFlagsAndPath(t *testing.T) {
	command := newImportSlackdumpCmd()
	require.NoError(t, command.ParseFlags([]string{
		"--me", "alice@example.com",
		"--limit", "25",
		"--max-media-mb", "7",
		"--no-default-identity",
		"relative/export.zip",
	}))

	args, err := daemonCLIArgsFromCobra(command, command.Flags().Args())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"import-slackdump",
		"--limit=25",
		"--max-media-mb=7",
		"--me=alice@example.com",
		"--no-default-identity",
		"relative/export.zip",
	}, args)
}

func TestRunImportSlackdumpRejectsMissingSourcePath(t *testing.T) {
	command := &cobra.Command{Use: "import-slackdump"}
	command.SetContext(context.Background())
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)

	err := runImportSlackdump(command, filepath.Join(t.TempDir(), "missing.zip"), slackdumpCLIOptions{Me: "UALICE"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "source path not found")
}

func TestResolveSlackdumpMediaPolicyPreservesWorkspaceRulesAndOverridesOnlySize(t *testing.T) {
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = config.NewDefaultConfig()
	disabled := false
	cfg.Slack = config.SlackConfig{
		Media:                &disabled,
		MediaScope:           "direct",
		MediaMaxParticipants: 4,
		MaxMediaMB:           11,
		AccountsConfig: map[string]config.MediaAccountConfig{
			"T001": {MaxMediaMB: 7},
		},
	}

	want := attachmentpolicy.Policy{
		Scope:           attachmentpolicy.ScopeDirect,
		MaxParticipants: 4,
		MaxBytes:        7 << 20,
		DisabledReason:  attachmentpolicy.SkipPolicyScope,
	}
	assert.Equal(t, want, resolveSlackdumpMediaPolicy("T001", 0))
	want.MaxBytes = 3 << 20
	assert.Equal(t, want, resolveSlackdumpMediaPolicy("T001", 3))
}
