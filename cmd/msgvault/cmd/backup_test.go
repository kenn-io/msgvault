package cmd

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestResolveBackupRepoPrecedence(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	tests := []struct {
		name        string
		flagValue   string
		configRepo  string
		wantRepo    string
		wantErr     bool
		wantErrText string
	}{
		{
			name:       "flag wins over config",
			flagValue:  "/flag/repo",
			configRepo: "/config/repo",
			wantRepo:   "/flag/repo",
		},
		{
			name:       "config used when flag empty",
			flagValue:  "",
			configRepo: "/config/repo",
			wantRepo:   "/config/repo",
		},
		{
			name:        "error when neither is set",
			flagValue:   "",
			configRepo:  "",
			wantErr:     true,
			wantErrText: "backup: no repository configured; pass --repo or set [backup] repo in config.toml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cfg = &config.Config{Backup: config.BackupConfig{Repo: tt.configRepo}}

			repo, err := resolveBackupRepo(tt.flagValue)

			if tt.wantErr {
				require.Error(err)
				assert.EqualError(err, tt.wantErrText)
				return
			}
			require.NoError(err)
			assert.Equal(tt.wantRepo, repo)
		})
	}
}

// TestRefuseRestoreIntoLiveDaemonHomeBlocksIncompatibleDaemon pins the guard
// against a daemon whose API version does not match this client's: such a
// daemon (left running across a CLI upgrade or downgrade) is invisible to
// the compatible-runtime lookup, yet it still owns the archive's SQLite
// database, so restoring into its home must be refused all the same.
func TestRefuseRestoreIntoLiveDaemonHomeBlocksIncompatibleDaemon(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "v-test",
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(err, "split listener address")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: "v-test",
		Metadata: map[string]string{
			runtimeHost:       host,
			runtimePort:       portText,
			runtimeAPIVersion: strconv.Itoa(daemonAPIVersion + 1),
		},
	})
	require.NoError(err, "write runtime record")

	require.Nil(findDaemonRuntime(dataDir),
		"precondition: the daemon must read as incompatible to this client")
	require.NotNil(findAnyDaemonRuntime(dataDir),
		"the incompatible daemon still responds and must be discoverable")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{Data: config.DataConfig{DataDir: dataDir}}

	err = refuseRestoreIntoLiveDaemonHome(dataDir)
	require.ErrorContains(err, "running daemon",
		"restore into the live archive home must be refused even when the daemon is incompatible")
	require.NoError(refuseRestoreIntoLiveDaemonHome(t.TempDir()),
		"a target outside the archive home stays allowed")

	// The guard compares filesystem identity, not path strings, so an
	// aliased spelling of the same home (a symlink here; a case-variant
	// path on case-insensitive filesystems) is refused too.
	alias := filepath.Join(t.TempDir(), "home-alias")
	if err := os.Symlink(dataDir, alias); err != nil {
		t.Skip("symlinks not supported on this platform")
	}
	require.ErrorContains(refuseRestoreIntoLiveDaemonHome(alias), "running daemon",
		"an aliased path to the archive home must be refused")
}

func TestResolveBackupRepoNilConfig(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = nil

	repo, err := resolveBackupRepo("/flag/repo")

	require.NoError(t, err)
	assert.Equal(t, "/flag/repo", repo)
}

func TestClearRestoredPackMetadataDoesNotMigrateLegacyDatabase(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := store.Open(dbPath)
	require.NoError(err)
	_, err = legacy.DB().Exec(`CREATE TABLE legacy_marker (id INTEGER PRIMARY KEY)`)
	require.NoError(err)
	require.NoError(legacy.Close())

	require.NoError(clearRestoredPackMetadata(dbPath))

	got, err := store.Open(dbPath)
	require.NoError(err)
	defer func() { require.NoError(got.Close()) }()
	var packTables int
	err = got.DB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table'
		  AND name IN ('attachment_pack_index', 'attachment_packs')`).Scan(&packTables)
	require.NoError(err)
	assert.Zero(packTables, "restore cleanup must not initialize unrelated current schema")
	var markerTables int
	err = got.DB().QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'legacy_marker'`).Scan(&markerTables)
	require.NoError(err)
	assert.Equal(1, markerTables, "legacy database remains otherwise intact")
}
