package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	require.NoError(t, err)
	assert.False(t, ok, "want false on closed stdin")
	assert.Contains(t, out.String(), `Cancelled. Drop --permanent to use trash deletion without elevated permissions.`,
		"output missing verbatim cancellation message on EOF")
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
	assert.ErrorContains(t, err, "no confirmation input (stdin closed); --all-hidden cannot be skipped with --yes")
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
	require.NoError(t, err, "want nil (cancel-on-EOF) on closed stdin")
	assert.False(t, ok, "want false on closed stdin")
}

func TestConfirmEmbedUsesCommandIO(t *testing.T) {
	var errOut bytes.Buffer
	cmd := &cobra.Command{Use: "test"}
	cmd.SetIn(strings.NewReader("yes\n"))
	cmd.SetErr(&errOut)

	assert.True(t, confirmEmbed(cmd, "Proceed? "))
	assert.Equal(t, "Proceed? [y/N]: ", errOut.String())
}
