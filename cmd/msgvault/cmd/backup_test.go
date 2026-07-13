package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestBackupRestorePackedTargetSelection(t *testing.T) {
	assert := assert.New(t)
	assert.NotNil(backupRestorePackedContentTarget(false), "packed restore is the default")
	assert.Equal(packstore.DefaultLimits(), backupRestorePackedContentTarget(false).Limits())
	assert.Nil(backupRestorePackedContentTarget(true), "explicit loose restore must use Kit's legacy path")
	flag := backupRestoreCmd.Flags().Lookup("loose-attachments")
	require.NotNil(t, flag)
	assert.Equal("false", flag.DefValue)
	integrityFlag := backupRestoreCmd.Flags().Lookup("integrity-check")
	require.NotNil(t, integrityFlag)
	assert.Equal("false", integrityFlag.DefValue)
}

func TestRunBackupRestorePackedDefaultAndExplicitLooseCleanup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "restore-cli@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "restore-cli-thread", "Restore CLI")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "restore-cli-message", MessageType: "email",
	})
	require.NoError(err)
	content := []byte("CLI restore must preserve packed bytes and clear loose-mode authority")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	attachmentsDir := filepath.Join(dataDir, "attachments")
	loosePath := filepath.Join(attachmentsDir, hash[:2], hash)
	require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
	require.NoError(os.WriteFile(loosePath, content, 0o600))
	require.NoError(st.UpsertAttachment(messageID, "restore-cli.bin", "application/octet-stream",
		hash[:2]+"/"+hash, hash, len(content)))
	layout, err := packstore.NewLayout(attachmentsDir, packstore.LayoutOptions{
		Staging: packstore.StagingSameDirectory,
	})
	require.NoError(err)
	maintainer, err := packstore.NewMaintainer(store.NewPackCatalog(st), layout, packstore.MaintainerOptions{})
	require.NoError(err)
	t.Cleanup(func() { _ = maintainer.Close() })
	packed, err := maintainer.Pack(ctx, packstore.PackOptions{})
	require.NoError(err)
	require.Equal(1, packed.BlobsPacked)

	repoPath := filepath.Join(t.TempDir(), "repo")
	repo, err := backup.Init(repoPath)
	require.NoError(err)
	_, err = backup.Create(ctx, repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: dataDir,
		ContentSource: backupapp.NewContentSource(attachmentstore.Wrap(maintainer.Store()), attachmentsDir),
	})
	require.NoError(err)

	savedCfg := cfg
	savedRepo := backupRestoreRepo
	savedTarget := backupRestoreTarget
	savedOverwrite := backupRestoreOverwrite
	savedForceUnlock := backupRestoreForceUnlock
	savedJobs := backupRestoreJobs
	savedLoose := backupRestoreLooseAttachments
	savedIntegrityCheck := backupRestoreIntegrityCheck
	t.Cleanup(func() {
		cfg = savedCfg
		backupRestoreRepo = savedRepo
		backupRestoreTarget = savedTarget
		backupRestoreOverwrite = savedOverwrite
		backupRestoreForceUnlock = savedForceUnlock
		backupRestoreJobs = savedJobs
		backupRestoreLooseAttachments = savedLoose
		backupRestoreIntegrityCheck = savedIntegrityCheck
	})
	cfg = &config.Config{Data: config.DataConfig{DataDir: filepath.Join(t.TempDir(), "live")}}
	backupRestoreRepo = repoPath
	backupRestoreOverwrite = false
	backupRestoreForceUnlock = false
	backupRestoreJobs = 1

	backupRestoreTarget = filepath.Join(t.TempDir(), "packed-target")
	backupRestoreLooseAttachments = false
	backupRestoreIntegrityCheck = false
	var packedOutput bytes.Buffer
	packedCmd := &cobra.Command{Use: "restore"}
	packedCmd.SetContext(ctx)
	packedCmd.SetOut(&packedOutput)
	require.NoError(runBackupRestore(packedCmd, nil))
	assert.Contains(packedOutput.String(), "1 packed in 1 pack(s), 0 loose")
	assert.Contains(packedOutput.String(), "page and blob hashes verified; manifest stats match")
	assert.NotContains(packedOutput.String(), "SQLite integrity_check")
	assertRestoredCLIBlob(t, backupRestoreTarget, hash, content, true)

	backupRestoreTarget = filepath.Join(t.TempDir(), "loose-target")
	backupRestoreLooseAttachments = true
	backupRestoreIntegrityCheck = true
	var looseOutput bytes.Buffer
	looseCmd := &cobra.Command{Use: "restore"}
	looseCmd.SetContext(ctx)
	looseCmd.SetOut(&looseOutput)
	require.NoError(runBackupRestore(looseCmd, nil))
	assert.Contains(looseOutput.String(), "Pack metadata cleared")
	assert.Contains(looseOutput.String(), "SQLite integrity_check ok")
	assertRestoredCLIBlob(t, backupRestoreTarget, hash, content, false)
}

func assertRestoredCLIBlob(t *testing.T, target, hash string, want []byte, packed bool) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	restored, err := store.OpenForTest(filepath.Join(target, "msgvault.db"))
	require.NoError(err)
	defer func() { require.NoError(restored.Close()) }()
	records, err := restored.ListPackRecords()
	require.NoError(err)
	indexed, err := restored.ListIndexedBlobHashes()
	require.NoError(err)
	if packed {
		assert.NotEmpty(records)
		assert.Contains(indexed, hash)
		assert.NoFileExists(filepath.Join(target, "attachments", hash[:2], hash))
	} else {
		assert.Empty(records, "explicit loose restore must clear stale source-vault pack records")
		assert.Empty(indexed, "explicit loose restore must clear stale source-vault mappings")
		assert.FileExists(filepath.Join(target, "attachments", hash[:2], hash))
	}
	blobs, err := attachmentstore.New(store.NewPackCatalog(restored), filepath.Join(target, "attachments"))
	require.NoError(err)
	reader, size, err := blobs.Open(hash)
	require.NoError(err)
	got, err := io.ReadAll(reader)
	require.NoError(err)
	require.NoError(reader.Close())
	require.NoError(blobs.Close())
	assert.Equal(int64(len(want)), size)
	assert.Equal(want, got)
}

func TestPrintBackupRestoreSummaryReportsPackedMixedAndLooseLayouts(t *testing.T) {
	tests := []struct {
		name        string
		looseFlag   bool
		result      backup.RestoreResult
		contains    []string
		notContains []string
	}{
		{
			name: "fully packed",
			result: backup.RestoreResult{SnapshotID: "snap", DBPath: "/target/msgvault.db",
				DBBytes: 10, AttachmentBlobs: 3, AttachmentBytes: 30,
				PackedAttachmentBlobs: 3, AttachmentPacks: 1, Duration: time.Second},
			contains:    []string{"Attachments: 3 (30B); 3 packed in 1 pack(s), 0 loose"},
			notContains: []string{"pack-attachments", "Pack fallbacks:"},
		},
		{
			name: "mixed compatibility fallback",
			result: backup.RestoreResult{SnapshotID: "snap", DBPath: "/target/msgvault.db",
				AttachmentBlobs: 3, AttachmentBytes: 30, PackedAttachmentBlobs: 2,
				LooseAttachmentBlobs: 1, AttachmentPacks: 1,
				PackFallbacks: []packstore.ImportFallback{{PackID: restorePackAForOutput, Hash: packstore.Hash(strings.Repeat("a", 64)), Reason: packstore.FallbackBlobLimit}}},
			contains: []string{"2 packed in 1 pack(s), 1 loose", "Pack fallbacks: blob_limit=1",
				"1 attachment blob(s) remain loose", "msgvault pack-attachments"},
		},
		{
			name:      "explicit loose",
			looseFlag: true,
			result: backup.RestoreResult{SnapshotID: "snap", DBPath: "/target/msgvault.db",
				AttachmentBlobs: 3, AttachmentBytes: 30, LooseAttachmentBlobs: 3},
			contains: []string{"0 packed in 0 pack(s), 3 loose", "restored as loose files by request",
				"msgvault pack-attachments"},
		},
		{
			name: "whole pack fallback",
			result: backup.RestoreResult{SnapshotID: "snap", DBPath: "/target/msgvault.db",
				AttachmentBlobs: 3, AttachmentBytes: 30, LooseAttachmentBlobs: 3,
				PackFallbacks: []packstore.ImportFallback{{PackID: restorePackAForOutput, Reason: packstore.FallbackPackContainerLimit}}},
			contains: []string{"Pack fallbacks: pack_container_limit=1", "3 attachment blob(s) remain loose"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, printBackupRestoreSummary(&out, "/target", &tt.result, tt.looseFlag))
			assert.Contains(t, out.String(), "Verification: page and blob hashes verified")
			assert.NotContains(t, out.String(), "Proof:")
			assert.Equal(t, tt.result.DatabaseIntegrityChecked,
				strings.Contains(out.String(), "SQLite integrity_check ok"))
			for _, want := range tt.contains {
				assert.Contains(t, out.String(), want)
			}
			for _, unwanted := range tt.notContains {
				assert.NotContains(t, out.String(), unwanted)
			}
		})
	}
}

const restorePackAForOutput = "01hzy3v7q8r9s0t1a2v3w4x5y6"

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
