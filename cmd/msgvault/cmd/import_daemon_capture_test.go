package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

// Losing the daemon's explicit opt-in in config/options/child serialization
// must fail a real native import; inherited input cannot grant that permission.
func TestDaemonImportNotificationsUseEffectiveCaptureSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		notify        bool
	}{
		{"default-off", "", false},
		{"explicit-opt-in", "import_notifications = true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			root := t.TempDir()
			cfg, err := config.LoadConfigFile(config.ConfigFile{Exists: true, LogicalPath: filepath.Join(root, "config.toml"), Content: []byte("[mcp.events]\nenabled = true\nsources = [\"gmail\"]\n" + tc.setting)}, root)
			require.NoError(err)
			cfg.HomeDir = root
			cfg.Data.DataDir = root
			cfg.Server.APIKey = "synthetic-import-owner"
			parent, err := store.Open(cfg.DatabaseDSN())
			require.NoError(err)
			require.NoError(parent.InitSchema())
			require.NoError(runStartupMigrationsForIngest(parent))
			t.Cleanup(func() { Require.NoError(t, parent.Close()) })
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: parent, Logger: testLoggerValue(), OperationGate: api.NewSerialOperationGate()})
			t.Cleanup(func() { Require.NoError(t, server.Shutdown(context.Background())) })
			_, err = newDaemonMCPEventsService(t.Context(), cfg, parent, server)
			require.NoError(err)
			capture, err := daemonMCPEventsStoreConfig(cfg)
			require.NoError(err)
			var beforeEpoch int64
			require.NoError(parent.DB().QueryRow("SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1").Scan(&beforeEpoch))
			childContext := (&storeAPIAdapter{config: cfg, mcpEventsCapture: true, mcpEventsCaptureConfig: &capture}).invocationContext(t.Context())
			childEnv := daemonRuntimeChildEnv(childContext, []string{daemonMCPEventsCaptureConfigEnv + "=forged", daemonMCPEventsCaptureEnv + "=0"})
			var snapshot string
			for _, entry := range childEnv {
				key, value, ok := strings.Cut(entry, "=")
				if ok && key == daemonMCPEventsCaptureConfigEnv {
					snapshot = value
				}
			}
			require.NotEmpty(snapshot)
			require.NotEqual("forged", snapshot)
			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			t.Setenv(daemonMCPEventsCaptureEnv, "1")
			t.Setenv(daemonMCPEventsCaptureConfigEnv, snapshot)
			// A newer disk configuration must not override the running parent's choice.
			reloaded := config.NewDefaultConfig()
			reloaded.HomeDir = root
			reloaded.Data.DataDir = root
			reloaded.Server.APIKey = cfg.Server.APIKey
			child, cleanup, err := openWritableStoreAndInitWithInvocation(&invocation{cfg: reloaded, logger: testDiscardLogger()}, runStartupMigrationsForIngest)
			require.NoError(err)
			t.Cleanup(cleanup)
			mailbox := filepath.Join(t.TempDir(), "Synthetic.mailbox")
			require.NoError(os.MkdirAll(mailbox, 0o700))
			require.NoError(os.WriteFile(filepath.Join(mailbox, "message.eml"), []byte("From: sender@example.net\r\nTo: owner@example.net\r\nMessage-ID: <daemon-import@example.net>\r\nSubject: Synthetic daemon import\r\n\r\nSynthetic body\r\n"), 0o600))
			summary, err := importer.ImportEMLDir(t.Context(), child, mailbox, importer.EMLImportOptions{Identifier: "owner@example.net", NotifyImports: true, NoResume: true})
			if tc.notify {
				require.NoError(err)
				require.NotNil(summary)
				assert.NotEmpty(summary.ImportRunID)
			} else {
				require.ErrorContains(err, "import_notifications_disabled")
			}
			var messages, events, sources int
			require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
			require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM sources").Scan(&sources))
			require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'").Scan(&events))
			want := 0
			if tc.notify {
				want = 1
			}
			assert.Equal(want, messages)
			assert.Equal(want, sources)
			assert.Equal(want, events)
			var afterEpoch int64
			require.NoError(parent.DB().QueryRow("SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1").Scan(&afterEpoch))
			assert.Equal(beforeEpoch, afterEpoch)
		})
	}
}

func TestDaemonImportCaptureRejectsUnknownFields(t *testing.T) {
	require := Require.New(t)

	cfg := store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic-capture"}
	encoded, err := encodeDaemonMCPEventsCaptureConfig(cfg)
	require.NoError(err)
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(err)
	var fields map[string]any
	require.NoError(json.Unmarshal(payload, &fields))
	fields["notify_imports"] = true
	payload, err = json.Marshal(fields)
	require.NoError(err)
	_, err = decodeDaemonMCPEventsCaptureConfig(base64.RawURLEncoding.EncodeToString(payload))
	Assert.ErrorContains(t, err, "invalid daemon MCP Events capture configuration")
}
