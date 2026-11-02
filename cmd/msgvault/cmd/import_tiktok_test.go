package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/tiktokexport"
)

func TestImportTikTokCommand(t *testing.T) {
	root := &cobra.Command{Use: "msgvault"}
	parent := &cobra.Command{Use: "import"}
	parent.AddCommand(newImportTikTokCommand())
	root.AddCommand(parent)
	cmd, remaining, err := root.Find([]string{"import", "tiktok", "example-account", "export.zip"})
	require.NoError(t, err)
	assert.Equal(t, "msgvault import tiktok", cmd.CommandPath())
	assert.Equal(t, []string{"example-account", "export.zip"}, remaining)
	runArgs, err := daemonCLIArgsFromCobra(cmd, remaining)
	require.NoError(t, err)
	assert.Equal(t, []string{"import", "tiktok", "example-account", "export.zip"}, runArgs)
	assert.True(t, manualSyncCLICommand(runArgs))
	assert.False(t, manualSyncCLICommand([]string{"import", "--type", "whatsapp"}))
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"import", "tiktok", "--json"})
	require.Error(t, root.Execute())
	var result map[string]any
	require.NoError(t, json.Unmarshal(stderr.Bytes(), &result))
	assert.Contains(t, result, "error")
	assert.Contains(t, result, "hint")
}

func TestImportTikTokJSONFlagError(t *testing.T) {
	cmd := newImportTikTokCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", "--unknown"})
	require.Error(t, cmd.Execute())
	assert.Empty(t, stdout.String())
	var result map[string]any
	require.NoError(t, json.Unmarshal(stderr.Bytes(), &result))
	assert.Contains(t, result["error"], "unknown flag")
}

func TestImportTikTokJSONConfigurationError(t *testing.T) {
	root := &cobra.Command{Use: "msgvault", PersistentPreRunE: func(*cobra.Command, []string) error { return errors.New("synthetic configuration failure") }}
	parent := &cobra.Command{Use: "import"}
	parent.AddCommand(newImportTikTokCommand())
	root.AddCommand(parent)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"import", "tiktok", "example-account", "export.zip", "--json"})
	require.Error(t, root.Execute())
	assert.Empty(t, stdout.String())
	var result map[string]any
	require.NoError(t, json.Unmarshal(stderr.Bytes(), &result))
	assert.Equal(t, "synthetic configuration failure", result["error"])
}

func TestImportTikTokEndToEnd(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/tiktokexport/testdata/json")
	require.NoError(t, err)
	cmd := newImportTikTokCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	state := newInvocation()
	state.cfg = config.NewDefaultConfig()
	state.logger = slog.New(slog.DiscardHandler)
	// Always isolate DataDir, which config.NewDefaultConfig derives eagerly.
	state.cfg.HomeDir = home
	state.cfg.Data.DataDir = home
	cmd.SetContext(withInvocation(context.Background(), state))
	require.NoError(t, cmd.Flags().Set("json", "true"))
	captured := captureStdout(t)
	err = cmd.RunE(cmd, []string{"example-account", fixture})
	processOutput := captured()
	require.NoError(t, err)
	assert.Empty(t, processOutput, "JSON imports must not print cache progress on process stdout")
	var summary tiktokexport.Summary
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &summary))
	assert.Equal(t, int64(4), summary.MessagesAdded)
	assert.Contains(t, stderr.String(), "1 unknown")
}
