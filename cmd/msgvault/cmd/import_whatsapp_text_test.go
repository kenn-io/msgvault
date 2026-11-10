package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whatsapp"
)

func TestWhatsAppTextRejectsRemoteDaemon(t *testing.T) {
	oldPhone, oldMediaDir, oldContacts := importPhone, importMediaDir, importContacts
	oldLimit, oldDisplayName, oldNoDefaultIdentity := importLimit, importDisplayName, noDefaultIdentityImportWhatsApp
	t.Cleanup(func() {
		importPhone, importMediaDir, importContacts = oldPhone, oldMediaDir, oldContacts
		importLimit, importDisplayName, noDefaultIdentityImportWhatsApp = oldLimit, oldDisplayName, oldNoDefaultIdentity
	})
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
	oldPhone, oldMediaDir, oldContacts := importPhone, importMediaDir, importContacts
	oldLimit, oldDisplayName, oldNoDefaultIdentity := importLimit, importDisplayName, noDefaultIdentityImportWhatsApp
	t.Cleanup(func() {
		importPhone, importMediaDir, importContacts = oldPhone, oldMediaDir, oldContacts
		importLimit, importDisplayName, noDefaultIdentityImportWhatsApp = oldLimit, oldDisplayName, oldNoDefaultIdentity
	})
	for _, name := range []string{"complete", "incomplete", "multiline"} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			markDaemonCLISubprocessForTest(t)
			cfg := config.NewDefaultConfig()
			home := t.TempDir()
			cfg.HomeDir = home
			cfg.Data.DataDir = home
			payload := "13/6/26, 10:00 - Mira: asters\n13/6/26, 10:00 - Mira: asters\n"
			args := []string{"--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "group", "--timezone", "UTC", "--date-order", "dmy", "--no-default-identity"}
			if name == "multiline" {
				payload = "13/6/26, 10:00 - Mira: asters\nMeet at 10:01\nThe price is £3.50\n"
			}
			if name == "incomplete" {
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
			if name == "incomplete" {
				require.ErrorContains(err, "fictional attachment failure")
				assert.Contains(out.String(), "Import incomplete: 1 records")
				assert.Contains(out.String(), "1 added, 0 unchanged")
				assert.Contains(out.String(), "Media: 0 stored")
			} else if name == "multiline" {
				require.NoError(err)
				st, err := store.Open(cfg.DatabaseDSN())
				require.NoError(err)
				defer func() { assert.NoError(st.Close()) }()
				var body string
				require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT body_text FROM message_bodies").Scan(&body))
				assert.Equal("asters\nMeet at 10:01\nThe price is £3.50", body)
			} else {
				require.NoError(err)
				assert.Contains(out.String(), "2 records")
				assert.Contains(out.String(), "2 added")
				assert.Contains(out.String(), "Indistinguishable duplicates: 1\n")
			}
		})
	}
}

func TestWhatsAppTextCommandPreflightArchive(t *testing.T) {
	for _, boundary := range []string{"13 Jun 2026, noon - Ivo: two", "13/6/26 10:01 - Messages are encrypted"} {
		for _, subprocess := range []bool{false, true} {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("boundary=%s/subprocess=%t/existing=%t", boundary, subprocess, existing), func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					if subprocess {
						markDaemonCLISubprocessForTest(t)
					} else {
						t.Setenv(daemonCLISubprocessEnv, "")
					}
					cfg := config.NewDefaultConfig()
					cfg.HomeDir = t.TempDir()
					cfg.Data.DataDir = cfg.HomeDir
					if existing {
						st, err := store.Open(cfg.DatabaseDSN())
						require.NoError(err)
						require.NoError(st.InitSchemaContext(t.Context()))
						opts := whatsapp.TextOptions{Phone: "+12025550123", ChatID: "garden", ChatType: "group", DateOrder: "dmy", Timezone: "UTC"}
						seed := filepath.Join(t.TempDir(), "seed.txt")
						for _, content := range []string{"13/6/26, 10:00 - Mira: one\n", "13/6/26, 10:00 - Mira: one\n13/6/26, 10:01 - Ivo: two\n"} {
							require.NoError(os.WriteFile(seed, []byte(content), 0600))
							input, err := whatsapp.LoadText(t.Context(), seed, opts)
							require.NoError(err)
							_, err = whatsapp.NewImporter(st, nil).ImportText(t.Context(), input)
							require.NoError(err)
						}
						require.NoError(st.Close())
					}
					path := filepath.Join(t.TempDir(), "chat.txt")
					require.NoError(os.WriteFile(path, []byte("13/6/26, 10:00 - Mira: one\n"+boundary+"\n"), 0600))
					command := &cobra.Command{Use: "test"}
					command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
					command.AddCommand(newImportWhatsAppCmd())
					var out bytes.Buffer
					command.SetOut(&out)
					command.SetErr(&out)
					command.SetArgs([]string{"import-whatsapp", path, "--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "group", "--timezone", "UTC", "--date-order", "dmy"})
					require.ErrorContains(command.Execute(), "line 2: unsupported or malformed TXT header")
					assert.NotContains(out.String(), "Import complete:")
					assert.NotContains(out.String(), "Import incomplete:")
					if !existing {
						_, err := os.Stat(cfg.DatabaseDSN())
						assert.True(os.IsNotExist(err))
						return
					}
					st, err := store.Open(cfg.DatabaseDSN())
					require.NoError(err)
					defer func() { assert.NoError(st.Close()) }()
					var sources, conversations, messages, bodies, syncs int
					require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT (SELECT COUNT(*) FROM sources), (SELECT COUNT(*) FROM conversations), (SELECT COUNT(*) FROM messages), (SELECT COUNT(*) FROM message_bodies), (SELECT COUNT(*) FROM sync_runs)").Scan(&sources, &conversations, &messages, &bodies, &syncs))
					assert.Equal([]int{1, 1, 2, 2, 2}, []int{sources, conversations, messages, bodies, syncs})
					rows, err := st.DB().QueryContext(t.Context(), "SELECT body_text FROM message_bodies ORDER BY message_id")
					require.NoError(err)
					defer func() { assert.NoError(rows.Close()) }()
					var texts []string
					for rows.Next() {
						var body string
						require.NoError(rows.Scan(&body))
						texts = append(texts, body)
					}
					require.NoError(rows.Err())
					assert.Equal([]string{"one", "two"}, texts)
				})
			}
		}
	}
}

func TestWhatsAppTextFlagValidation(t *testing.T) {
	oldPhone, oldMediaDir, oldContacts := importPhone, importMediaDir, importContacts
	oldLimit, oldDisplayName, oldNoDefaultIdentity := importLimit, importDisplayName, noDefaultIdentityImportWhatsApp
	t.Cleanup(func() {
		importPhone, importMediaDir, importContacts = oldPhone, oldMediaDir, oldContacts
		importLimit, importDisplayName, noDefaultIdentityImportWhatsApp = oldLimit, oldDisplayName, oldNoDefaultIdentity
	})
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
		{"database-chat", []string{"--format", "database", "--chat-id", "garden"}, "--chat-id requires --format text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			command := &cobra.Command{Use: "test"}
			command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			command.AddCommand(newImportWhatsAppCmd())
			command.SetArgs(append([]string{"import-whatsapp", "fictional.txt", "--phone", "+12025550123"}, tc.flags...))
			require.ErrorContains(t, command.Execute(), tc.want)
		})
	}
}
