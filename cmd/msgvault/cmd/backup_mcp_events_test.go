package cmd

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPEventsBackupRestoreTokens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include bool
		missing string
	}{
		{name: "default"}, {name: "include tokens", include: true},
		{name: "missing store", include: true, missing: providercredentials.EventsFilename},
		{name: "missing key", include: true, missing: providercredentials.ServerKeyFilename},
	} {
		t.Run(tc.name, func(t *testing.T) {
			include := tc.include
			require := require.New(t)
			assert := assert.New(t)
			data := t.TempDir()
			cfg := &config.Config{HomeDir: t.TempDir()}
			cfg.Data.DataDir = data
			dbPath := filepath.Join(data, "msgvault.db")
			attachments := filepath.Join(data, "attachments")
			require.NoError(os.MkdirAll(attachments, 0700))
			st, err := store.OpenForTest(t.Context(), dbPath)
			require.NoError(err)
			require.NoError(st.InitSchema())
			t.Cleanup(func() { _ = st.Close() })
			source, err := st.GetOrCreateSource("gmail", "fixture@example.test")
			require.NoError(err)
			req := mcpevents.MCPEventsSubscribeRequest{Name: mcpevents.Name, Arguments: mcpevents.MCPEventsArguments{SourceID: source.ID}, Delivery: mcpevents.MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/private-path?token=fixture-token", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
			raw, err := json.Marshal(req)
			require.NoError(err)
			req, err = mcpevents.DecodeSubscribe(raw)
			require.NoError(err)
			service := mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true}
			_, err = service.Subscribe(t.Context(), req, func(context.Context, string, string, []byte) error { return nil })
			require.NoError(err)
			key, err := providercredentials.EnsureServerKey(cfg.TokensDir())
			require.NoError(err)
			require.NoError(os.WriteFile(filepath.Join(cfg.TokensDir(), "account.token"), []byte("account fixture"), 0600))
			if tc.missing != "" {
				require.NoError(os.Remove(filepath.Join(cfg.TokensDir(), tc.missing)))
			}
			oldTokens, oldPlain := backupCreateIncludeTokens, backupCreateAllowPlaintextSecrets
			backupCreateIncludeTokens, backupCreateAllowPlaintextSecrets = include, true
			t.Cleanup(func() { backupCreateIncludeTokens, backupCreateAllowPlaintextSecrets = oldTokens, oldPlain })
			extras, err := backupExtrasSpec(cfg)
			require.NoError(err)
			repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
			require.NoError(err)
			app := backupapp.New("test")
			_, err = backup.Create(t.Context(), repo, app, backup.CreateOptions{DBPath: dbPath, ContentDir: attachments, DataDir: data, Extras: extras, IncludeTokens: include, AllowPlaintextSecrets: true})
			require.NoError(err)
			dest := filepath.Join(t.TempDir(), "restored")
			oldRepo, oldTarget, oldOverwrite := backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite
			oldJobs, oldIntegrity := backupRestoreJobs, backupRestoreIntegrityCheck
			t.Cleanup(func() {
				backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite = oldRepo, oldTarget, oldOverwrite
				backupRestoreJobs, backupRestoreIntegrityCheck = oldJobs, oldIntegrity
			})
			backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite = repo.Root(), dest, false
			backupRestoreJobs, backupRestoreIntegrityCheck = 1, false
			command := &cobra.Command{Use: "restore"}
			command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			require.NoError(runBackupRestore(command, nil))
			if include && tc.missing != providercredentials.ServerKeyFilename {
				value, err := providercredentials.ReadSecretFile(filepath.Join(dest, "tokens", providercredentials.ServerKeyFilename))
				require.NoError(err)
				assert.Equal(key, value)
			}
			if include {
				token, err := os.ReadFile(filepath.Join(dest, "tokens", "account.token"))
				require.NoError(err)
				assert.Equal("account fixture", string(token))
			}
			db, err := store.OpenForTest(t.Context(), filepath.Join(dest, "msgvault.db"))
			require.NoError(err)
			t.Cleanup(func() { _ = db.Close() })
			recovered := mcpevents.Service{Store: db, TokensDir: filepath.Join(dest, "tokens"), Enabled: true}
			targets, err := recovered.Native(t.Context())
			require.NoError(err)
			assert.Equal(include && tc.missing != providercredentials.EventsFilename, len(targets) == 1)
			if len(targets) == 1 {
				assert.Equal(req.Delivery.URL, targets[0].URL)
			}
		})
	}
}
