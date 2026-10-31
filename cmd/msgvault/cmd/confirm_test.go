package cmd

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpsPromptEOFIsAnError(t *testing.T) {
	t.Run("deduplicate", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		ok, err := readYesNoConfirmation(bufio.NewReader(strings.NewReader("")))
		require.Error(err)
		assert.False(ok)
		assert.Equal(2, ExitCode(err))
		require.ErrorIs(err, io.EOF)
		require.ErrorContains(err, "--yes")
	})
	for _, mode := range []string{deleteStagedConfirmModePermanent, deleteStagedConfirmModeTrash} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			assert := assert.New(t)
			ok, err := confirmDeleteStaged(strings.NewReader(""), &out, mode)
			require.Error(t, err)
			assert.False(ok)
			assert.Equal(2, ExitCode(err))
			assert.ErrorIs(err, io.EOF)
		})
	}
}

func TestDestructiveConfirmationRejectsOversizedInput(t *testing.T) {
	for _, mode := range []ConfirmMode{ConfirmModePermanent, ConfirmModeAllHidden, ConfirmModeYesNo} {
		var out bytes.Buffer
		confirmed, err := confirmDestructive(strings.NewReader(strings.Repeat("x", bufio.MaxScanTokenSize)+"\n"), &out, mode)
		require.Error(t, err)
		require.ErrorContains(t, err, "read confirmation:")
		assert.False(t, confirmed)
	}
}

func TestGCClosedInputDoesNotReportSuccess(t *testing.T) {
	command := newGCCmd()
	command.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	command.SetOut(&out)
	err := command.RunE(command, nil)
	require.Error(t, err)
	assert := assert.New(t)
	assert.Equal(2, ExitCode(err))
	require.ErrorContains(t, err, "--yes")
	assert.NotContains(out.String(), "Aborted.")
}

func TestDraftGetRejectsBothSelectors(t *testing.T) {
	command := newDraftGetCommand()
	require.NoError(t, command.Flags().Set("conversation", "42"))
	err := command.Args(command, []string{"draft-example"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "not both")
}

func TestConfirmDestructive_Permanent_LiteralDeleteAccepted(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("delete\n"), &out, ConfirmModePermanent)
	require.NoError(t, err)
	assert.True(t, ok, "want true after typing 'delete'")
}

func TestConfirmDestructive_Permanent_NonDeleteRejected(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("y\n"), &out, ConfirmModePermanent)
	require.NoError(t, err)
	assert.False(t, ok, "want false when input is 'y' under Permanent mode")
	assert.Contains(t, out.String(), `Cancelled. Drop --permanent to use trash deletion without elevated permissions.`,
		"output missing verbatim cancellation message")
}

func TestConfirmDestructive_Permanent_StdinClosed(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader(""), &out, ConfirmModePermanent)
	require.Error(t, err)
	assert.False(t, ok, "want false on closed stdin")
	assert.ErrorContains(t, err, `type "delete"`)
}

func TestConfirmDestructive_AllHidden_YesAccepted(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("y\n"), &out, ConfirmModeAllHidden)
	require.NoError(t, err)
	assert.True(t, ok, "want true on 'y' under AllHidden mode")
}

func TestConfirmDestructive_AllHidden_NoRejected(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("n\n"), &out, ConfirmModeAllHidden)
	require.NoError(t, err)
	assert.False(t, ok, "want false on 'n'")
}

func TestConfirmDestructive_AllHidden_StdinClosed(t *testing.T) {
	var out bytes.Buffer
	_, err := confirmDestructive(strings.NewReader(""), &out, ConfirmModeAllHidden)
	require.Error(t, err, "want named error on closed stdin")
	assert.ErrorContains(t, err, "--all-hidden requires interactive confirmation")
}

func TestConfirmDestructive_YesNo_YesAccepted(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("y\n"), &out, ConfirmModeYesNo)
	require.NoError(t, err)
	assert.True(t, ok, "want true on 'y' under YesNo mode")
}

func TestConfirmDestructive_YesNo_NoRejected(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("n\n"), &out, ConfirmModeYesNo)
	require.NoError(t, err)
	assert.False(t, ok, "want false on 'n'")
}

func TestConfirmDestructive_YesNo_StdinClosed(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader(""), &out, ConfirmModeYesNo)
	require.Error(t, err, "closed stdin must not report a successful cancellation")
	require.ErrorContains(t, err, "--yes")
	assert.False(t, ok, "want false on closed stdin")
}

func TestConfirmEmbedUsesCommandIO(t *testing.T) {
	var errOut bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetErr(&errOut)

	confirmed, err := confirmEmbed(cmd, "Proceed? ")
	require.NoError(t, err)
	assert.True(t, confirmed)
	assert.Equal(t, "Proceed? [y/N]: \n", errOut.String())
}
