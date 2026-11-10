package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestWhatsAppTextRejectsRemoteDaemon(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")
	command := &cobra.Command{Use: "test"}
	command.SetContext(ctx)
	command.AddCommand(newImportWhatsAppCmd())
	command.SetArgs([]string{"import-whatsapp", filepath.Join(t.TempDir(), "chat.txt"), "--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "group", "--timezone", "UTC", "--date-order", "dmy"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestWhatsAppTextCommand(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	home := t.TempDir()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	path := filepath.Join(t.TempDir(), "chat.txt")
	require.NoError(t, os.WriteFile(path, []byte("13/6/26, 10:00 - Mira: asters\n"), 0600))
	command := &cobra.Command{Use: "test"}
	command.SetContext(ctx)
	command.AddCommand(newImportWhatsAppCmd())
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"import-whatsapp", path, "--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "group", "--timezone", "UTC", "--date-order", "dmy", "--no-default-identity"})
	require.NoError(t, command.Execute())
	assert.Contains(t, out.String(), "1 records")
	assert.Contains(t, out.String(), "1 added")
}

func TestWhatsAppTextFlagValidation(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	for _, flags := range [][]string{
		{"--format", "unknown"},
		{"--format", "text", "--limit", "1"},
		{"--format", "text", "--contacts", "contacts.vcf"},
		{"--format", "text", "--after", "2026-01-01"},
		{"--format", "text", "--full"},
		{"--format", "text"},
		{"--format", "database", "--chat-id", "garden"},
	} {
		t.Run(flags[len(flags)-1], func(t *testing.T) {
			command := &cobra.Command{Use: "test"}
			command.AddCommand(newImportWhatsAppCmd())
			command.SetArgs(append([]string{"import-whatsapp", "fictional.txt", "--phone", "+12025550123"}, flags...))
			require.Error(t, command.Execute())
		})
	}
}
