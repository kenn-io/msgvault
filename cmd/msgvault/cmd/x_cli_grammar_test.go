package cmd

import (
	"bytes"
	"os"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestXCLICommandGroupsUseCanonicalPathsAndStableWireNames(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	root := newGroupedXCommandRoot(t)

	add := findDirectCLICommand(findDirectCLICommand(findDirectCLICommand(root, "source"), "x"), "add")
	sync := findDirectCLICommand(findDirectCLICommand(findDirectCLICommand(root, "source"), "x"), "sync")
	draftReply := findDirectCLICommand(findDirectCLICommand(root, "draft"), "reply")
	messageReply := findDirectCLICommand(findDirectCLICommand(root, "message"), "reply")
	_testRequire.NotNil(add)
	_testRequire.NotNil(sync)
	_testRequire.NotNil(draftReply)
	_testRequire.NotNil(messageReply)
	legacyAdd := findDirectCLICommand(root, "add-x")
	legacySync := findDirectCLICommand(root, "sync-x")
	legacyDraftReply := findDirectCLICommand(root, "draft-reply")
	legacySendReply := findDirectCLICommand(root, "send-reply")
	_testRequire.NotNil(legacyAdd)
	_testRequire.NotNil(legacySync)
	_testRequire.NotNil(legacyDraftReply)
	_testRequire.NotNil(legacySendReply)
	_testAssert.True(legacyAdd.Hidden)
	_testAssert.True(legacySync.Hidden)
	_testAssert.True(legacyDraftReply.Hidden)
	_testAssert.True(legacySendReply.Hidden)
	_testAssert.NotSame(legacyDraftReply.Flags().Lookup("body"), draftReply.Flags().Lookup("body"), "canonical flags copy public metadata")
	_testAssert.Equal("msgvault source x add", add.CommandPath())
	_testAssert.Equal("msgvault source x sync", sync.CommandPath())
	_testAssert.Equal("msgvault draft reply", draftReply.CommandPath())
	_testAssert.Equal("msgvault message reply", messageReply.CommandPath())
	_testAssert.True(agentDelegatedCapable(draftReply), "draft reply keeps its existing delegated draft capability")
	_testAssert.False(agentDelegatedCapable(messageReply), "sending remains owner-only")
	_testAssert.Equal([]string{"add-x"}, daemonCLICommandPath(add))
	_testAssert.Equal([]string{"sync-x"}, daemonCLICommandPath(sync))
	_testAssert.Equal([]string{"draft-reply"}, daemonCLICommandPath(draftReply))
	_testAssert.Equal([]string{"send-reply"}, daemonCLICommandPath(messageReply))
	_testRequire.NoError(draftReply.Flags().Set("body", "Reviewed reply"))
	_testAssert.Equal("Reviewed reply", legacyDraftReply.Flags().Lookup("body").Value.String(), "both definitions retain the same flag value")
	args, err := daemonCLIArgsFromCobra(draftReply, []string{"42"})
	_testRequire.NoError(err)
	_testAssert.Equal([]string{"draft-reply", "--body=Reviewed reply", "42"}, args)
}

func TestXCLICommandHelpAndUsageHintsUseCanonicalPaths(t *testing.T) {
	t.Run("X help groups maintenance and names generic backup restore", func(t *testing.T) {
		root := newGroupedXCommandRoot(t)
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs([]string{"source", "x", "--help"})
		require.NoError(t, root.Execute())
		assert.Contains(t, output.String(), "removal and removal-record export are options on source x sync")
		assert.Contains(t, output.String(), "generic msgvault backup restore command")
	})

	t.Run("root help hides legacy paths and lists canonical groups", func(t *testing.T) {
		assert := assert.New(t)
		root := newGroupedXCommandRoot(t)
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetArgs([]string{"--help"})
		require.NoError(t, root.Execute())
		assert.Contains(output.String(), "source")
		assert.Contains(output.String(), "draft")
		assert.Contains(output.String(), "message")
		assert.NotContains(output.String(), "add-x")
		assert.NotContains(output.String(), "sync-x")
		assert.NotContains(output.String(), "draft-reply")
		assert.NotContains(output.String(), "send-reply")
	})

	t.Run("legacy root paths execute with stderr migration notices", func(t *testing.T) {
		cases := []struct {
			name, legacy, canonical string
			args                    []string
			wantOutput, wantError   string
		}{
			{
				name: "add-x forwards to setup handler", legacy: "add-x", canonical: "source x add",
				args:       []string{"add-x", "--headless", "--client-id", "example-client"},
				wantOutput: "msgvault source x add --client-id example-client",
			},
			{
				name: "legacy help prints migration notice", legacy: "add-x", canonical: "source x add",
				args: []string{"add-x", "--help"}, wantOutput: "Authorize an X account with a local browser and stage its protected credentials.",
			},
			{
				name: "legacy flag errors print migration notice", legacy: "add-x", canonical: "source x add",
				args: []string{"add-x", "--unknown"}, wantError: "unknown flag: --unknown",
			},
			{
				name: "sync-x validates with canonical hint", legacy: "sync-x", canonical: "source x sync",
				args:      []string{"sync-x", "--archive"},
				wantError: "hint: run 'msgvault source x sync --help'",
			},
			{
				name: "draft-reply creates a managed draft", legacy: "draft-reply", canonical: "draft reply",
				args:      []string{"draft-reply", "42"},
				wantError: "hint: run 'msgvault draft reply <message-id> --body <text>'",
			},
			{
				name: "send-reply uses the generic reply command", legacy: "send-reply", canonical: "message reply",
				args:      []string{"send-reply"},
				wantError: "hint: run 'msgvault message reply --help'",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := newGroupedXCommandRoot(t)
				var stdout, stderr bytes.Buffer
				root.SetOut(&stdout)
				root.SetErr(&stderr)
				root.SetArgs(tc.args)
				err := root.Execute()
				if tc.wantError == "" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantError)
				}
				assert.Contains(t, stderr.String(), "Command \""+tc.legacy+"\" is deprecated, use 'msgvault "+tc.canonical+"' instead")
				assert.NotContains(t, stdout.String(), "is deprecated")
				if tc.wantOutput != "" {
					assert.Contains(t, stdout.String(), tc.wantOutput)
				}
			})
		}
	})

	t.Run("legacy notices stay out of daemon subprocess output", func(t *testing.T) {
		t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
		root := newGroupedXCommandRoot(t)
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs([]string{"add-x", "--help"})
		require.NoError(t, root.Execute())
		assert.Empty(t, stderr.String())
		assert.Contains(t, stdout.String(), "Authorize an X account with a local browser and stage its protected credentials.")
	})

	t.Run("headless setup prints canonical commands", func(t *testing.T) {
		assert := assert.New(t)
		root := newGroupedXCommandRoot(t)
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs([]string{"source", "x", "add", "--headless", "--client-id", "example-client"})
		require.NoError(t, root.Execute())
		assert.Contains(output.String(), "msgvault source x add --client-id example-client")
		assert.NotContains(output.String(), "add-x")
		assert.NotContains(output.String(), "is deprecated")
	})

	t.Run("sync help has examples", func(t *testing.T) {
		_testAssert := assert.New(t)

		root := newGroupedXCommandRoot(t)
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs([]string{"source", "x", "sync", "--help"})
		require.NoError(t, root.Execute())
		_testAssert.Contains(output.String(), "Examples:")
		_testAssert.Contains(output.String(), "msgvault source x sync --source-id 42")
		_testAssert.NotContains(output.String(), "sync-x")
	})

	t.Run("sync validation points to canonical help", func(t *testing.T) {
		root := newGroupedXCommandRoot(t)
		root.SetArgs([]string{"source", "x", "sync", "--archive"})
		err := root.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hint: run 'msgvault source x sync --help'")
	})

	t.Run("sync clone retains mutually exclusive job flags", func(t *testing.T) {
		root := newGroupedXCommandRoot(t)
		root.SetArgs([]string{"source", "x", "sync", "--job-id", "job-example", "--new-job"})
		err := root.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "none of the others can be")
	})

	t.Run("draft creation explains its required body", func(t *testing.T) {
		root := newGroupedXCommandRoot(t)
		root.SetArgs([]string{"draft", "reply", "42"})
		err := root.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hint: run 'msgvault draft reply <message-id> --body <text>'")
	})

	t.Run("reply validation points to canonical help", func(t *testing.T) {
		root := newGroupedXCommandRoot(t)
		root.SetArgs([]string{"message", "reply"})
		err := root.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "hint: run 'msgvault message reply --help'")
	})
}

func TestExecuteRootContextRegistersCanonicalXCLIPaths(t *testing.T) {
	root := newRootCommand()
	root.AddCommand(
		newAddXCmd(defaultXCommandDeps()),
		newSyncXCmd(),
		newDraftReplyCommand(),
		newSendReplyCommand(),
	)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"source", "x", "add", "--help"})

	require.NoError(t, executeRootContext(t.Context(), root))
	assert.Contains(t, output.String(), "msgvault source x add")
	assert.Contains(t, output.String(), "Examples:")
}

func newGroupedXCommandRoot(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(
		newAddXCmd(defaultXCommandDeps()),
		newSyncXCmd(),
		newDraftReplyCommand(),
		newSendReplyCommand(),
	)
	ensureXCLICommandGroups(root)
	root.SetContext(withInvocation(t.Context(), &invocation{cfg: &config.Config{}}))
	return root
}
