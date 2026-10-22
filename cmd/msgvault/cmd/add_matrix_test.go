package cmd

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
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
	"maunium.net/go/mautrix/id"
)

func TestAddMatrixFailedStagingHonorsSkipWithExistingBackupKey(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	server, cfg, old := setupMatrixRenewal(t)
	key, err := backup.NewMegolmBackupKey()
	req.NoError(err)
	old.BackupKey = base64.RawStdEncoding.EncodeToString(key.Bytes())
	req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), old))
	s := testutil.NewTestStore(t)
	setMatrixTestDatabaseURL(t, s, cfg)
	failMatrixCredentialWrite(t, &savePendingMatrixCredentials, errors.New("synthetic disk full"))
	req.ErrorContains(runAddMatrixForRenewal(t, cfg, server.URL, "password"), "login unchanged")
	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	req.NoError(err)
	check.Equal(old, creds)
	source, err := s.GetSourceByTypeAndIdentifier(sourceTypeMatrix, old.UserID)
	req.NoError(err)
	check.JSONEq(`{"skip_key_backup":true}`, source.SyncConfig.String)
}

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
	for _, mode := range []string{"missing", "invalid", "failed", "skip", "renew without key", "previous login's key"} {
		t.Run(mode, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			server := newRenewalHomeserver(t)
			home := t.TempDir()
			cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
			args := []string{"add-matrix", "--homeserver", server.URL, "--user-id", "@archive:example.org", "--no-default-identity"}
			if mode == "skip" || mode == "renew without key" || mode == "previous login's key" {
				previous := matrixsource.Credentials{Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "OLD", AccessToken: "old"}
				if mode == "skip" || mode == "previous login's key" {
					previous.BackupKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
				}
				if mode == "skip" {
					args = append(args, "--skip-key-backup")
				}
				req.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), previous))
			}

			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			t.Setenv(clirun.EnvMatrixLoginSecret, "password")
			t.Setenv(clirun.EnvMatrixRecoverySecret, "")
			switch mode {
			case "invalid":
				t.Setenv(clirun.EnvMatrixRecoverySecret, "invalid recovery key")
			case "failed":
				key, err := ssss.NewKey("")
				req.NoError(err)
				t.Setenv(clirun.EnvMatrixRecoverySecret, key.RecoveryKey())
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
			case "failed":
				req.Error(err)
				check.Equal(1, logins)
				check.Equal([]string{"Bearer fresh"}, logouts)
			case "skip", "renew without key", "previous login's key":
				req.NoError(err)
				creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), "@archive:example.org")
				req.NoError(err)
				if mode == "previous login's key" {
					check.Equal("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", creds.BackupKey)
					check.Equal(1, logins)
				} else {
					check.Empty(creds.BackupKey)
				}
			}
		})
	}
}

func TestAddMatrixSavesBackupKeyAndPreservesSyncConfig(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	login := newRenewalHomeserver(t)
	key, err := ssss.NewKey("")
	req.NoError(err)
	backupKey, err := backup.NewMegolmBackupKey()
	req.NoError(err)
	mux := http.NewServeMux()
	mux.Handle("/", login.Config.Handler)
	matrixtest.HandleSecretStorage(t, mux, key, backupKey)
	mux.HandleFunc("GET /_matrix/client/v3/room_keys/version", func(w http.ResponseWriter, _ *http.Request) {
		raw, err := json.Marshal(map[string]any{"version": "1", "algorithm": id.KeyBackupAlgorithmMegolmBackupV1, "auth_data": map[string]string{"public_key": base64.RawStdEncoding.EncodeToString(backupKey.PublicKey().Bytes())}})
		if assert.NoError(t, err) {
			_, _ = w.Write(raw)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	s := testutil.NewTestStore(t)
	setMatrixTestDatabaseURL(t, s, cfg)
	source, err := s.GetOrCreateSource(sourceTypeMatrix, "@archive:example.org")
	req.NoError(err)
	req.NoError(s.UpdateSourceSyncConfig(source.ID, `{"skip_key_backup":true,"rooms":["!keep:example.org"],"extra":{"keep":1}}`))
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(clirun.EnvMatrixLoginSecret, "password")
	t.Setenv(clirun.EnvMatrixRecoverySecret, key.RecoveryKey())
	root := newTestRootCmd()
	root.AddCommand(newAddMatrixCmd())
	root.SetArgs([]string{"add-matrix", "--homeserver", server.URL, "--user-id", "@archive:example.org", "--no-default-identity"})
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	req.NoError(root.Execute())
	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), "@archive:example.org")
	req.NoError(err)
	check.Equal(base64.RawStdEncoding.EncodeToString(backupKey.Bytes()), creds.BackupKey)
	source, err = s.GetSourceByID(source.ID)
	req.NoError(err)
	check.JSONEq(`{"skip_key_backup":false,"rooms":["!keep:example.org"],"extra":{"keep":1}}`, source.SyncConfig.String)
}

func TestReadMatrixRecoverySecret(t *testing.T) {
	for _, file := range []bool{false, true} {
		t.Run(strconv.FormatBool(file), func(t *testing.T) {
			req := require.New(t)
			stdin, err := os.Open(os.DevNull)
			req.NoError(err)
			previous := os.Stdin
			os.Stdin = stdin
			t.Cleanup(func() { os.Stdin = previous; _ = stdin.Close() })
			cmd := newAddMatrixCmd()
			t.Cleanup(func() { _ = newAddMatrixCmd() })
			args := []string{"--recovery-passphrase"}
			if file {
				path := filepath.Join(t.TempDir(), "recovery")
				req.NoError(os.WriteFile(path, []byte("  example passphrase  \n"), 0o600))
				args = append(args, "--recovery-file", path)
			}
			req.NoError(cmd.ParseFlags(args))
			secret, err := readMatrixRecoverySecret()
			if file {
				req.NoError(err)
				assert.Equal(t, "  example passphrase  ", secret)
			} else {
				req.EqualError(err, "--recovery-passphrase requires --recovery-file when an interactive prompt is unavailable")
			}
		})
	}
}
