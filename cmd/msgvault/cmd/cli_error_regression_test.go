package cmd

import (
	"bytes"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/logging"
	"io"
	"log/slog"
	"testing"
)

func TestProxyProgressDoesNotSuppressGuidance(t *testing.T) {
	for _, progress := range []string{`info: {"error":{"message":"example"}}` + "\n", "Error: a quoted example\nprogress continues\n"} {
		err := proxiedChildError{stderr: &childStderr{tail: []byte(progress)}}
		assert.False(t, err.alreadyReported(), progress)
	}
}

func TestImportGroupDoesNotEmitLegacyNotice(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"import", "--type", "whatsapp"})
	require.Error(t, executeRootContext(t.Context(), rootCmd))
	assert.NotContains(t, out.String(), "deprecated")
}

func TestProxyTerminalDiagnosticRemainsLastWithDebugLogging(t *testing.T) {
	var diagnostic bytes.Buffer
	root := newRootCommand()
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		inv := prepareInvocation(cmd)
		inv.logger = slog.New(slog.NewTextHandler(&diagnostic, &slog.HandlerOptions{Level: slog.LevelDebug}))
		inv.logResult = &logging.Result{}
		return nil
	}
	terminal := "{\"error\":{\"code\":\"unavailable\",\"message\":\"fixture failure\",\"hint\":\"retry\",\"exit_code\":7}}\n"
	root.AddCommand(&cobra.Command{Use: "fixture", RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := io.WriteString(cmd.ErrOrStderr(), terminal)
		require.NoError(t, err)
		return proxiedChildError{stderr: &childStderr{tail: []byte(terminal)}, code: 7, errorReported: true}
	}})
	root.SetErr(&diagnostic)
	root.SetArgs([]string{"fixture"})
	require.Error(t, executeRootContext(t.Context(), root))
	assert.Equal(t, terminal, diagnostic.String())
}
