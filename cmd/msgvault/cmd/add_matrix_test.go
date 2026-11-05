package cmd

import (
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/matrix/matrixtest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/crypto/utils"
	"maunium.net/go/mautrix/id"
)

func setMatrixTestDatabaseURL(t *testing.T, s *store.Store, cfg *config.Config) {
	t.Helper()
	req := require.New(t)
	if s.IsPostgreSQL() {
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		req.NoError(err)
		var database, schema string
		req.NoError(s.DB().QueryRow(`SELECT current_database(), current_schema()`).Scan(&database, &schema))
		u.Path = "/" + database
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		cfg.Data.DatabaseURL = u.String()
	} else {
		req.NoError(s.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&cfg.Data.DatabaseURL))
	}
}

func TestAddMatrixBackupSetup(t *testing.T) {
	for _, mode := range []string{"missing", "invalid", "loader failure", "skip", "secret storage"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			server := newRenewalHomeserver(t)
			homeserver := server.URL
			var backupKey *backup.MegolmBackupKey
			var recoveryKey string
			if mode == "secret storage" {
				key, err := ssss.NewKey("")
				req.NoError(err)
				backupKey, err = backup.NewMegolmBackupKey()
				req.NoError(err)
				mux := http.NewServeMux()
				mux.Handle("/", server.Config.Handler)
				matrixtest.HandleSecretStorage(t, mux, key, backupKey)
				mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
					raw, err := json.Marshal(map[string]any{"version": "1", "algorithm": id.KeyBackupAlgorithmMegolmBackupV1, "auth_data": map[string]string{"public_key": base64.RawStdEncoding.EncodeToString(backupKey.PublicKey().Bytes())}})
					if assert.NoError(t, err) {
						_, _ = w.Write(raw)
					}
				})
				secrets := httptest.NewServer(mux)
				t.Cleanup(secrets.Close)
				homeserver, recoveryKey = secrets.URL, key.RecoveryKey()
			}
			home := t.TempDir()
			cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
			args := []string{"add-matrix", "--homeserver", homeserver, "--user-id", "@archive:example.org", "--no-default-identity"}
			if mode == "skip" {
				previous := matrixsource.Credentials{Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "OLD", AccessToken: "old", BackupKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
				args = append(args, "--skip-key-backup")
				req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), previous))
			}

			var s *store.Store
			var source *store.Source
			if mode == "skip" || mode == "secret storage" {
				s = testutil.NewTestStore(t)
				setMatrixTestDatabaseURL(t, s, cfg)
				var err error
				source, err = s.GetOrCreateSource(sourceTypeMatrix, "@archive:example.org")
				req.NoError(err)
				if mode == "secret storage" {
					req.NoError(s.UpdateSourceSyncConfig(source.ID, `{"skip_key_backup":true,"rooms":["!keep:example.org"],"extra":{"keep":1}}`))
				}
			}
			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			t.Setenv(clirun.EnvMatrixLoginSecret, "password")
			t.Setenv(clirun.EnvMatrixRecoverySecret, recoveryKey)
			switch mode {
			case "invalid":
				t.Setenv(clirun.EnvMatrixRecoverySecret, "invalid recovery key")
			case "loader failure":
				t.Setenv(clirun.EnvMatrixRecoverySecret, utils.EncodeBase58RecoveryKey(make([]byte, 32)))
			}
			root := newTestRootCmd()
			root.AddCommand(newAddMatrixCmd())
			root.SetArgs(args)
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			logins, logouts := server.snapshot()
			switch mode {
			case "missing":
				req.ErrorContains(err, "recovery secret required")
				check.Zero(logins)
				check.Empty(logouts)
			case "invalid":
				req.ErrorContains(err, "invalid recovery key")
				check.Zero(logins)
				check.Empty(logouts)
			case "loader failure":
				req.ErrorContains(err, "load Matrix secret storage")
				check.Equal(1, logins)
				check.Equal([]string{"Bearer fresh"}, logouts)
			case "skip", "secret storage":
				req.NoError(err)
				creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), "@archive:example.org")
				req.NoError(err)
				if mode == "secret storage" {
					check.Equal(base64.RawStdEncoding.EncodeToString(backupKey.Bytes()), creds.BackupKey)
					source, err = s.GetSourceByID(source.ID)
					req.NoError(err)
					check.JSONEq(`{"skip_key_backup":false,"rooms":["!keep:example.org"],"extra":{"keep":1}}`, source.SyncConfig.String)
				} else {
					check.Empty(creds.BackupKey)
					source, err = s.GetSourceByID(source.ID)
					req.NoError(err)
					check.JSONEq(`{"skip_key_backup":true}`, source.SyncConfig.String)
				}
			}
		})
	}
}

func TestReadMatrixRecoverySecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		file bool
	}{
		{"passphrase requires file without prompt", false},
		{"passphrase disables normalization", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := require.New(t)
			stdin, err := os.Open(os.DevNull)
			req.NoError(err)
			previous := os.Stdin
			os.Stdin = stdin
			t.Cleanup(func() { os.Stdin = previous; _ = stdin.Close() })
			cmd := newAddMatrixCmd()
			t.Cleanup(func() {
				addMatrixRecoveryFile = ""
				addMatrixRecoveryIsPassphrase = false
			})
			args := []string{"--recovery-passphrase"}
			if tc.file {
				path := filepath.Join(t.TempDir(), "recovery")
				req.NoError(os.WriteFile(path, []byte("  example passphrase  \n"), 0o600))
				args = append(args, "--recovery-file", path)
			}
			req.NoError(cmd.ParseFlags(args))
			secret, err := readMatrixRecoverySecret()
			if tc.file {
				req.NoError(err)
				assert.Equal(t, "  example passphrase  ", secret)
			} else {
				req.EqualError(err, "--recovery-passphrase requires --recovery-file when an interactive prompt is unavailable")
			}
		})
	}
}
