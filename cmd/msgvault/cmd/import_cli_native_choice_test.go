package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// Dropping the public flag or forwarding a default instead of its parsed value
// must change actual native parser writes, not just the command's flag table.
func TestImportCommandNotificationChoiceReachesNativeParser(t *testing.T) {
	for _, format := range []string{"eml", "maildir", "synctech_sms"} {
		for _, choice := range []struct {
			name            string
			global, request bool
		}{
			{"default-silent", false, false}, {"global-only-silent", true, false},
			{"run-only-rejected", false, true}, {"both-opt-ins", true, true},
		} {
			t.Run(format+"/"+choice.name, func(t *testing.T) {
				assert := Assert.New(t)

				require := Require.New(t)

				cfg, parent := importCommandCaptureFixture(t, choice.global)
				input := t.TempDir()
				var command *cobra.Command
				var args []string
				switch format {
				case "eml", "maildir":
					directory := filepath.Join(input, "Synthetic.mailbox")
					name := "message.eml"
					if format == "maildir" {
						directory = filepath.Join(input, "cur")
						name = "synthetic:2,S"
						for _, part := range []string{"cur", "new", "tmp"} {
							require.NoError(os.MkdirAll(filepath.Join(input, part), 0700))
						}
					}
					require.NoError(os.MkdirAll(directory, 0700))
					require.NoError(os.WriteFile(filepath.Join(directory, name), []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <cli-import@example.net>\r\nSubject: Synthetic import\r\n\r\nSynthetic body\r\n"), 0600))
					if format == "eml" {
						command = newImportEMLCommand()
					} else {
						command = newImportMaildirCommand()
					}
					args = []string{input, "--identifier", "owner@example.net", "--source-type", "gmail", "--no-resume", "--no-default-identity"}
				case "synctech_sms":
					file := filepath.Join(input, "synthetic.xml")
					require.NoError(os.WriteFile(file, []byte(`<smses count="1"><sms address="+15550001001" date="978350400000" type="1" body="Synthetic body" read="1" status="-1"/></smses>`), 0600))
					command = newImportSynctechSMSCmd()
					args = []string{file, "--owner-phone", "+15550001000"}
				}
				if choice.request {
					args = append(args, "--notify-imports")
				}
				command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
				command.SetOut(io.Discard)
				command.SetErr(io.Discard)
				command.SetArgs(args)
				err := command.Execute()
				rejected := choice.request && !choice.global
				if rejected {
					require.ErrorContains(err, "import_notifications_disabled")
				} else {
					require.NoError(err)
				}
				var sources, messages, runs, events int
				require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM sources").Scan(&sources))
				require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
				require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM import_runs").Scan(&runs))
				require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'").Scan(&events))
				if rejected {
					assert.Zero(sources)
					assert.Zero(messages)
					assert.Zero(runs)
					assert.Zero(events)
					return
				}
				assert.Equal(1, sources)
				assert.Equal(1, messages)
				assert.Equal(1, runs)
				wantEvents := 0
				if choice.request {
					wantEvents = 1
				}
				assert.Equal(wantEvents, events)
				var nativeFormat string
				var frozen bool
				require.NoError(parent.DB().QueryRow("SELECT import_format, notify_imports FROM import_runs").Scan(&nativeFormat, &frozen))
				assert.Equal(format, nativeFormat)
				assert.Equal(choice.request, frozen)
			})
		}
	}
}

func importCommandCaptureFixture(t *testing.T, notify bool) (*config.Config, *store.Store) {
	t.Helper()
	root := t.TempDir()
	cfg, err := config.LoadConfigFile(config.ConfigFile{Exists: true, LogicalPath: filepath.Join(root, "config.toml"), Content: []byte(fmt.Sprintf("[mcp.events]\nenabled = true\nsources = [\"gmail\"]\nimport_notifications = %t\n", notify))}, root)
	Require.NoError(t, err)
	cfg.HomeDir = root
	cfg.Data.DataDir = root
	cfg.Server.APIKey = "synthetic-cli-owner"
	parent, err := store.Open(cfg.DatabaseDSN())
	Require.NoError(t, err)
	Require.NoError(t, parent.InitSchema())
	Require.NoError(t, runStartupMigrationsForIngest(parent))
	t.Cleanup(func() { Require.NoError(t, parent.Close()) })
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: parent, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
	t.Cleanup(func() { Require.NoError(t, server.Shutdown(context.Background())) })
	_, err = newDaemonMCPEventsService(t.Context(), cfg, parent, server)
	Require.NoError(t, err)
	capture, err := daemonMCPEventsStoreConfig(cfg)
	Require.NoError(t, err)
	childContext := (&storeAPIAdapter{config: cfg, mcpEventsCapture: true, mcpEventsCaptureConfig: &capture}).invocationContext(t.Context())
	childEnv := daemonRuntimeChildEnv(childContext, []string{daemonMCPEventsCaptureConfigEnv + "=forged"})
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(daemonMCPEventsCaptureEnv, "1")
	for _, entry := range childEnv {
		if key, value, ok := strings.Cut(entry, "="); ok && key == daemonMCPEventsCaptureConfigEnv {
			t.Setenv(key, value)
		}
	}
	return cfg, parent
}
