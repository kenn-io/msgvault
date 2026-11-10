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
	"go.kenn.io/msgvault/internal/store"
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
	for _, incomplete := range []bool{false, true} {
		name := "complete"
		if incomplete {
			name = "incomplete"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			markDaemonCLISubprocessForTest(t)
			cfg := config.NewDefaultConfig()
			home := t.TempDir()
			cfg.HomeDir = home
			cfg.Data.DataDir = home
			payload := "13/6/26, 10:00 - Mira: asters\n"
			args := []string{"--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "group", "--timezone", "UTC", "--date-order", "dmy", "--no-default-identity"}
			if incomplete {
				st, err := store.Open(cfg.DatabaseDSN())
				require.NoError(err)
				t.Cleanup(func() { assert.NoError(st.Close()) })
				require.NoError(st.InitSchemaContext(t.Context()))
				_, err = st.DB().ExecContext(t.Context(), "CREATE TRIGGER reject_text_media BEFORE INSERT ON attachments BEGIN SELECT RAISE(FAIL, 'fictional attachment failure'); END")
				require.NoError(err)
				payload = "13/6/26, 10:00 - Mira: <attached: note.txt>\n"
				media := t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(media, "note.txt"), []byte("fictional seed list"), 0600))
				args = append(args, "--media-dir", media)
			}
			path := filepath.Join(t.TempDir(), "chat.txt")
			require.NoError(os.WriteFile(path, []byte(payload), 0600))
			command := &cobra.Command{Use: "test"}
			command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			command.AddCommand(newImportWhatsAppCmd())
			var out bytes.Buffer
			command.SetOut(&out)
			command.SetErr(&out)
			command.SetArgs(append([]string{"import-whatsapp", path}, args...))
			err := command.Execute()
			if incomplete {
				require.ErrorContains(err, "fictional attachment failure")
				assert.Contains(out.String(), "Import incomplete: 1 records")
				assert.Contains(out.String(), "1 added, 0 unchanged")
				assert.Contains(out.String(), "Media: 0 stored")
			} else {
				require.NoError(err)
				assert.Contains(out.String(), "1 records")
				assert.Contains(out.String(), "1 added")
			}
		})
	}
}

func TestWhatsAppTextFlagValidation(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	validText := []string{"--format", "text", "--chat-id", "garden", "--chat-type", "group", "--date-order", "dmy", "--timezone", "UTC"}
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"unknown-format", []string{"--format", "unknown"}, "--format must be database or text"},
		{"limit", append(append([]string{}, validText...), "--limit", "1"), "--limit is not supported"},
		{"contacts", append(append([]string{}, validText...), "--contacts", "contacts.vcf"), "--contacts is not supported"},
		{"after", append(append([]string{}, validText...), "--after", "2026-01-01"), "--after is not supported"},
		{"full", append(append([]string{}, validText...), "--full"), "--full is not supported"},
		{"missing-chat", []string{"--format", "text"}, "--chat-id is required"},
		{"database-chat", []string{"--format", "database", "--chat-id", "garden"}, "--chat-id requires --format text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := &cobra.Command{Use: "test"}
			command.AddCommand(newImportWhatsAppCmd())
			command.SetArgs(append([]string{"import-whatsapp", "fictional.txt", "--phone", "+12025550123"}, tc.flags...))
			require.ErrorContains(t, command.Execute(), tc.want)
		})
	}
}
