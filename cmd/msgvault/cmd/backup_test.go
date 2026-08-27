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
	looseTarget := backupRestorePackedContentTarget(true)
	require.NotNil(t, looseTarget, "explicit loose restore uses the catalog-aware fallback path")
	looseLimits := looseTarget.Limits()
	assert.Equal(int64(1), looseLimits.PackBytes, "explicit loose restore rejects every pack container")
	flag := backupRestoreCmd.Flags().Lookup("loose-attachments")
	require.NotNil(t, flag)
	assert.Equal("false", flag.DefValue)
	integrityFlag := backupRestoreCmd.Flags().Lookup("integrity-check")
	require.NotNil(t, integrityFlag)
	assert.Equal("false", integrityFlag.DefValue)
}

func TestBackupRestoreTargetCoordinatorMatchesConfiguredDatabasePath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	lockDir := t.TempDir()
	target := t.TempDir()

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{
		DataDir:     lockDir,
		DatabaseURL: "file:" + filepath.Join(target, "msgvault.db"),
	}}
	coordinator, coordinated, err := backupRestoreTargetCoordinator(target, true)
	require.NoError(err, "select restore coordination by configured database path")
	require.True(coordinated, "restoring the configured database requires ownership coordination")
	require.NotNil(coordinator, "configured database target receives a coordinator")

	root, err := os.OpenRoot(target)
	require.NoError(err, "pin restore target")
	t.Cleanup(func() { require.NoError(root.Close(), "close restore target root") })
	lease, err := coordinator.AcquireRestoreTarget(context.Background(), root)
	require.NoError(err, "acquire configured database restore coordination")
	t.Cleanup(func() { require.NoError(lease.Release(), "release restore coordination") })

	writer, writeErr := tryAcquireWriteOwnerLock(lockDir)
	if writer != nil {
		require.NoError(writer.Close(), "release unexpected configured archive writer")
	}
	assert.Nil(writer, "configured archive writer must remain excluded")
	var heldErr writeOwnerLockHeldError
	require.ErrorAs(writeErr, &heldErr, "configured data-directory write lock is held")
	assert.NoFileExists(daemonOwnerLockPath(target),
		"separate database target must not receive the data-directory lock artifacts")
	assert.NoFileExists(writeOwnerLockPath(target),
		"separate database target must not receive the write lock artifact")
}

func TestBackupRestoreTargetCoordinatorRejectsPrimaryDatabaseAsVectorBackend(t *testing.T) {
	require := require.New(t)
	target := t.TempDir()

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: target}}
	cfg.Vector.DBPath = filepath.Join(target, "msgvault.db")

	coordinator, coordinated, err := backupRestoreTargetCoordinator(target, true)
	require.ErrorContains(err, "vector database path resolves to the restored archive database")
	require.Nil(coordinator)
	require.False(coordinated)
}

func TestBackupRestoreTargetCoordinatorDefersMissingCaseVariantMatch(t *testing.T) {
	require := require.New(t)
	parent := t.TempDir()
	probe := filepath.Join(parent, "CaseProbe")
	require.NoError(os.Mkdir(probe, 0o700), "create filesystem case-sensitivity probe")
	probeInfo, err := os.Stat(probe)
	require.NoError(err, "stat filesystem case-sensitivity probe")
	foldedProbeInfo, err := os.Stat(filepath.Join(parent, "caseprobe"))
	if err != nil || !os.SameFile(probeInfo, foldedProbeInfo) {
		t.Skip("filesystem is case-sensitive")
	}

	configuredDataDir := filepath.Join(parent, "FreshArchive")
	target := filepath.Join(parent, "fresharchive")
	require.NoDirExists(configuredDataDir, "configured archive starts absent")
	require.NoDirExists(target, "restore target starts absent")
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: configuredDataDir}}

	coordinator, coordinated, err := backupRestoreTargetCoordinator(target, false)
	require.NoError(err, "select conditional restore coordination")
	require.True(coordinated,
		"missing case variants require comparison after Kit pins the target")
	require.NotNil(coordinator, "case-variant target receives a conditional coordinator")
	require.NoDirExists(target, "coordinator selection remains non-mutating")

	require.NoError(os.Mkdir(target, 0o700), "create restore target through folded spelling")
	root, err := os.OpenRoot(target)
	require.NoError(err, "pin folded restore target")
	t.Cleanup(func() { require.NoError(root.Close(), "close folded restore target root") })
	lease, err := coordinator.AcquireRestoreTarget(context.Background(), root)
	require.NoError(err, "acquire folded restore target coordination")
	t.Cleanup(func() { require.NoError(lease.Release(), "release folded restore coordination") })

	writer, writeErr := tryAcquireWriteOwnerLock(configuredDataDir)
	if writer != nil {
		require.NoError(writer.Close(), "release unexpected case-variant writer")
	}
	assert.Nil(t, writer, "case-variant writer must remain excluded")
	var heldErr writeOwnerLockHeldError
	require.ErrorAs(writeErr, &heldErr, "case-variant data-directory write lock is held")
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
	profileFingerprint := strings.Repeat("d", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + profileFingerprint, Fingerprint: profileFingerprint,
		Provider: "synthetic", Endpoint: "https://documents.example.test/v1",
		Region: localValue, Model: "extract-test", RetentionPosture: "standard",
		TrainingPosture: "opted-out", AllowedMediaTypes: []string{"application/pdf"},
		PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err = st.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	var vectorGenerationID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		INSERT INTO document_vector_generations
			(fingerprint, target_extraction_profile_id, embedding_profile, model, dimension, state)
		VALUES (?, ?, 'vector.embeddings', 'embed-test', 3, 'active') RETURNING id`),
		strings.Repeat("e", 64), profile.ID).Scan(&vectorGenerationID))
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO document_vector_publications
			(generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
			 extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence,
			 token, state, created_at, updated_at)
		VALUES (?, 'extraction-restore', ?, ?, 'input-restore', 1, 'chunk-restore', ?, 1,
		        'token-restore', 'ready', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		vectorGenerationID, profile.ID, strings.Repeat("f", 64), strings.Repeat("a", 64))
	require.NoError(err)
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
	assertRestoredDocumentVectorsInvalidated(t, backupRestoreTarget)

	backupRestoreTarget = filepath.Join(t.TempDir(), "loose-target")
	require.NoError(os.MkdirAll(backupRestoreTarget, 0o700))
	staleVectorPath := filepath.Join(backupRestoreTarget, "vectors.db")
	require.NoError(os.WriteFile(staleVectorPath, []byte("stale derived vectors"), 0o600))
	backupRestoreOverwrite = true
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
	assertRestoredDocumentVectorsInvalidated(t, backupRestoreTarget)
	assert.NoFileExists(staleVectorPath, "overwrite restore removes the excluded derived vector backend")
}

func TestRunBackupRestoreIntoNonexistentConfiguredDataDir(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourceDir := t.TempDir()
	dbPath := filepath.Join(sourceDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "open source store")
	require.NoError(st.InitSchema(), "initialize source schema")
	require.NoError(st.Close(), "close source store")
	attachmentsDir := filepath.Join(sourceDir, "attachments")
	require.NoError(os.MkdirAll(attachmentsDir, 0o700), "create source attachments")

	repoPath := filepath.Join(t.TempDir(), "repo")
	repo, err := backup.Init(repoPath)
	require.NoError(err, "initialize backup repository")
	_, err = backup.Create(ctx, repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: sourceDir,
	})
	require.NoError(err, "create source snapshot")

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
	target := filepath.Join(t.TempDir(), "fresh-archive")
	cfg = &config.Config{Data: config.DataConfig{DataDir: target}}
	backupRestoreRepo = repoPath
	backupRestoreTarget = target
	backupRestoreOverwrite = false
	backupRestoreForceUnlock = false
	backupRestoreJobs = 1
	backupRestoreLooseAttachments = false
	backupRestoreIntegrityCheck = false
	assert.NoDirExists(target, "restore target starts absent")

	cmd := &cobra.Command{Use: "restore"}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	require.NoError(runBackupRestore(cmd, nil), "restore into fresh configured archive home")
	assert.FileExists(filepath.Join(target, "msgvault.db"), "restored database")
	assert.FileExists(daemonOwnerLockPath(target), "restore exclusion lock artifact is preserved")
	held, probeErr := daemonOwnerLockHeld(target)
	require.NoError(probeErr, "probe released restore exclusion")
	assert.False(held, "restore releases daemon exclusion after all cleanup")
}

func TestRunBackupRestoreRejectsDaemonClaimAfterPreflight(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourceDir := t.TempDir()
	dbPath := filepath.Join(sourceDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "open source store")
	require.NoError(st.InitSchema(), "initialize source schema")
	require.NoError(st.Close(), "close source store")
	attachmentsDir := filepath.Join(sourceDir, "attachments")
	require.NoError(os.MkdirAll(attachmentsDir, 0o700), "create source attachments")

	repoPath := filepath.Join(t.TempDir(), "repo")
	repo, err := backup.Init(repoPath)
	require.NoError(err, "initialize backup repository")
	_, err = backup.Create(ctx, repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: sourceDir,
	})
	require.NoError(err, "create source snapshot")

	savedCfg := cfg
	savedRepo := backupRestoreRepo
	savedTarget := backupRestoreTarget
	savedOverwrite := backupRestoreOverwrite
	savedForceUnlock := backupRestoreForceUnlock
	savedJobs := backupRestoreJobs
	savedLoose := backupRestoreLooseAttachments
	savedIntegrityCheck := backupRestoreIntegrityCheck
	savedAfterPreflight := backupRestoreAfterDaemonPreflight
	t.Cleanup(func() {
		cfg = savedCfg
		backupRestoreRepo = savedRepo
		backupRestoreTarget = savedTarget
		backupRestoreOverwrite = savedOverwrite
		backupRestoreForceUnlock = savedForceUnlock
		backupRestoreJobs = savedJobs
		backupRestoreLooseAttachments = savedLoose
		backupRestoreIntegrityCheck = savedIntegrityCheck
		backupRestoreAfterDaemonPreflight = savedAfterPreflight
	})
	target := t.TempDir()
	liveDatabase := []byte("live database must remain untouched")
	require.NoError(os.WriteFile(filepath.Join(target, "msgvault.db"), liveDatabase, 0o600),
		"seed live target database")
	cfg = &config.Config{Data: config.DataConfig{DataDir: target}}
	backupRestoreRepo = repoPath
	backupRestoreTarget = target
	backupRestoreOverwrite = true
	backupRestoreForceUnlock = false
	backupRestoreJobs = 1
	backupRestoreLooseAttachments = false
	backupRestoreIntegrityCheck = false
	var owner *daemonOwnerLock
	backupRestoreAfterDaemonPreflight = func() {
		owner, err = tryAcquireDaemonOwnerLock(target)
		require.NoError(err, "claim daemon ownership after restore preflight")
	}
	t.Cleanup(func() {
		if owner != nil {
			require.NoError(owner.Close(), "release raced daemon ownership")
		}
	})

	cmd := &cobra.Command{Use: "restore"}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	err = runBackupRestore(cmd, nil)
	require.ErrorContains(err, "daemon is already running",
		"restore must reject ownership acquired after its early probe")
	got, readErr := os.ReadFile(filepath.Join(target, "msgvault.db"))
	require.NoError(readErr, "read target database after rejected restore")
	assert.Equal(liveDatabase, got, "rejected restore preserves the live target database")
}

func TestDaemonRestoreTargetCoordinatorHoldsDaemonAndWriteLeasesThroughoutRestore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	sourceDir := t.TempDir()
	dbPath := filepath.Join(sourceDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "open source store")
	require.NoError(st.InitSchema(), "initialize source schema")
	require.NoError(st.Close(), "close source store")
	attachmentsDir := filepath.Join(sourceDir, "attachments")
	require.NoError(os.MkdirAll(attachmentsDir, 0o700), "create source attachments")

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err, "initialize backup repository")
	_, err = backup.Create(ctx, repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: sourceDir,
	})
	require.NoError(err, "create source snapshot")

	target := t.TempDir()
	customVectorPath := filepath.Join(t.TempDir(), "custom-vectors.db")
	for _, path := range []string{
		customVectorPath,
		customVectorPath + "-wal",
		customVectorPath + "-shm",
		customVectorPath + "-journal",
	} {
		require.NoError(os.WriteFile(path, []byte("stale vector backend"), 0o600),
			"seed excluded custom vector backend")
	}
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: target}}
	cfg.Vector.DBPath = customVectorPath
	coordinator, coordinated, err := backupRestoreTargetCoordinator(target, true)
	require.NoError(err, "select configured restore coordination")
	require.True(coordinated, "configured archive restore requires ownership coordination")
	require.NotNil(coordinator, "configured archive restore receives a coordinator")
	checkedDuringRestore := false
	_, err = backup.Restore(ctx, repo, backupapp.New("test"), backup.RestoreOptions{
		TargetDir:         target,
		Overwrite:         true,
		TargetCoordinator: coordinator,
		Progress: func(event backup.ProgressEvent) {
			if checkedDuringRestore || event.Stage != backup.ProgressStageAttachments {
				return
			}
			checkedDuringRestore = true
			contender, acquireErr := tryAcquireDaemonOwnerLock(target)
			if contender != nil {
				require.NoError(contender.Close(), "release unexpected contender ownership")
			}
			var heldErr daemonOwnerLockHeldError
			require.ErrorAs(acquireErr, &heldErr,
				"daemon ownership must remain excluded during restore")
			writer, writeErr := tryAcquireWriteOwnerLock(target)
			if writer != nil {
				require.NoError(writer.Close(), "release unexpected direct-writer ownership")
			}
			var writeHeldErr writeOwnerLockHeldError
			require.ErrorAs(writeErr, &writeHeldErr,
				"direct SQLite writers must remain excluded during restore")
		},
	})
	require.NoError(err, "restore with daemon target coordination")
	require.True(checkedDuringRestore, "ownership was checked during restore")
	assert.NoFileExists(customVectorPath,
		"restore lease removes the configured vector backend before releasing ownership")
	assert.NoFileExists(customVectorPath+"-wal", "restore lease removes the vector WAL")
	assert.NoFileExists(customVectorPath+"-shm", "restore lease removes the vector shared-memory file")
	assert.NoFileExists(customVectorPath+"-journal", "restore lease removes the vector journal")
	owner, err := tryAcquireDaemonOwnerLock(target)
	require.NoError(err, "target coordination releases after restore")
	require.NoError(owner.Close(), "release post-restore ownership")
	writer, err := tryAcquireWriteOwnerLock(target)
	require.NoError(err, "target coordination releases direct-writer exclusion after restore")
	require.NoError(writer.Close(), "release post-restore direct-writer ownership")
}

func TestDaemonRestoreTargetCoordinatorPreservesVectorsWithoutPublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	target := t.TempDir()
	databasePath := filepath.Join(target, "msgvault.db")
	require.NoError(os.WriteFile(databasePath, []byte("current database"), 0o600),
		"seed current database")
	customVectorPath := filepath.Join(t.TempDir(), "custom-vectors.db")
	require.NoError(os.WriteFile(customVectorPath, []byte("current vectors"), 0o600),
		"seed current vector backend")

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: target}}
	cfg.Vector.DBPath = customVectorPath
	coordinator, coordinated, err := backupRestoreTargetCoordinator(target, true)
	require.NoError(err, "select configured restore coordination")
	require.True(coordinated, "configured archive restore requires ownership coordination")

	root, err := os.OpenRoot(target)
	require.NoError(err, "pin restore target")
	t.Cleanup(func() { require.NoError(root.Close(), "close restore target root") })
	lease, err := coordinator.AcquireRestoreTarget(t.Context(), root)
	require.NoError(err, "acquire restore coordination")
	require.NoError(lease.Release(), "release without publishing a restored database")

	assert.FileExists(customVectorPath,
		"a failed restore must preserve the vector backend for the unchanged database")
}

func TestDaemonRestoreTargetLeaseRefusesToRemovePublishedDatabase(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	target := t.TempDir()
	databasePath := filepath.Join(target, "msgvault.db")
	require.NoError(os.WriteFile(databasePath, []byte("current database"), 0o600),
		"seed current database")

	root, err := os.OpenRoot(target)
	require.NoError(err, "pin restore target")
	t.Cleanup(func() { require.NoError(root.Close(), "close restore target root") })
	coordinator := &daemonRestoreTargetCoordinator{
		overwrite:            true,
		configuredVectorPath: databasePath,
	}
	lease, err := coordinator.newRestoreTargetLease(root, pinnedRestoreTargetDataDir)
	require.NoError(err, "capture pre-restore database identity")
	replacementPath := filepath.Join(target, "replacement.db")
	require.NoError(os.WriteFile(replacementPath, []byte("restored database"), 0o600),
		"write replacement database")
	require.NoError(os.Rename(replacementPath, databasePath), "publish replacement database")

	err = lease.Release()
	require.ErrorContains(err, "vector database path resolves to the restored archive database")
	assert.FileExists(databasePath, "vector cleanup must not remove the published archive database")
}

func TestDaemonRestoreTargetCoordinatorCanonicalizesMissingSymlinkedParent(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	sourceDir := t.TempDir()
	dbPath := filepath.Join(sourceDir, "msgvault.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "open source store")
	require.NoError(st.InitSchema(), "initialize source schema")
	require.NoError(st.Close(), "close source store")
	attachmentsDir := filepath.Join(sourceDir, "attachments")
	require.NoError(os.MkdirAll(attachmentsDir, 0o700), "create source attachments")

	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err, "initialize backup repository")
	_, err = backup.Create(ctx, repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: sourceDir,
	})
	require.NoError(err, "create source snapshot")

	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	linkedParent := filepath.Join(parent, "linked")
	require.NoError(os.Mkdir(realParent, 0o700), "create real archive parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	configuredDataDir := filepath.Join(linkedParent, "fresh", "archive")
	target := filepath.Join(realParent, "fresh", "archive")
	require.NoDirExists(configuredDataDir, "configured archive starts absent")
	require.NoDirExists(target, "restore target starts absent")

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: configuredDataDir}}
	coordinator, coordinated, err := backupRestoreTargetCoordinator(target, false)
	require.NoError(err, "select restore target coordination")
	require.True(coordinated,
		"resolved target must match an absent configured archive beneath a symlinked parent")
	require.NotNil(coordinator, "matching configured archive receives daemon exclusion")
	require.NoDirExists(target, "coordinator selection must not create the archive")

	checkedDuringRestore := false
	_, err = backup.Restore(ctx, repo, backupapp.New("test"), backup.RestoreOptions{
		TargetDir:         target,
		Overwrite:         true,
		TargetCoordinator: coordinator,
		Progress: func(event backup.ProgressEvent) {
			if checkedDuringRestore || event.Stage != backup.ProgressStageAttachments {
				return
			}
			checkedDuringRestore = true
			contender, acquireErr := tryAcquireDaemonOwnerLock(configuredDataDir)
			if contender != nil {
				require.NoError(contender.Close(), "release unexpected contender ownership")
			}
			var heldErr daemonOwnerLockHeldError
			require.ErrorAs(acquireErr, &heldErr,
				"daemon ownership through the symlinked spelling must remain excluded")
		},
	})
	require.NoError(err, "restore through canonicalized daemon target coordination")
	require.True(checkedDuringRestore, "daemon ownership was checked during restore")
	owner, err := tryAcquireDaemonOwnerLock(configuredDataDir)
	require.NoError(err, "restore releases canonicalized daemon exclusion")
	require.NoError(owner.Close(), "release post-restore daemon ownership")
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

func assertRestoredDocumentVectorsInvalidated(t *testing.T, target string) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	restored, err := store.OpenForTest(filepath.Join(target, "msgvault.db"))
	require.NoError(err)
	defer func() { require.NoError(restored.Close()) }()
	active, err := restored.GetActiveDocumentVectorGeneration(t.Context())
	require.NoError(err)
	assert.Nil(active)
	var publications int64
	require.NoError(restored.DB().QueryRow(`SELECT COUNT(*) FROM document_vector_publications`).Scan(&publications))
	assert.Zero(publications)
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
			contains:    []string{"0 packed in 0 pack(s), 3 loose", "restored as loose files"},
			notContains: []string{"msgvault pack-attachments"},
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
			require := require.New(t)
			assert := assert.New(t)
			var out bytes.Buffer
			require.NoError(printBackupRestoreSummary(&out, "/target", &tt.result, tt.looseFlag))
			assert.Contains(out.String(), "Verification: page and blob hashes verified")
			assert.NotContains(out.String(), "Proof:")
			assert.Equal(tt.result.DatabaseIntegrityChecked,
				strings.Contains(out.String(), "SQLite integrity_check ok"))
			for _, want := range tt.contains {
				assert.Contains(out.String(), want)
			}
			for _, unwanted := range tt.notContains {
				assert.NotContains(out.String(), unwanted)
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
			runtimeCreateTime: matchingProcessCreateTime(t),
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

func TestRefuseRestoreIntoLiveDaemonHomeBlocksUnverifiableDaemonOwner(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	owner, err := tryAcquireDaemonOwnerLock(dataDir)
	require.NoError(err, "acquire daemon ownership")
	t.Cleanup(func() { require.NoError(owner.Close(), "release daemon ownership") })
	stubProcessCreateTimeMillis(t, func(int) (int64, bool) { return 1_000, true })

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1",
		Service: daemonService,
		Metadata: map[string]string{
			runtimeCreateTime: "10000",
		},
	})
	require.NoError(err, "write mismatched runtime record")

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: dataDir}}

	err = refuseRestoreIntoLiveDaemonHome(dataDir)
	require.ErrorContains(err, "running daemon",
		"held daemon ownership must block restore even when endpoint identity is unverifiable")
}

func TestRefuseRestoreIntoLiveDaemonHomeFailsClosedWhenLockCannotBeProbed(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	require.NoError(os.Mkdir(daemonOwnerLockPath(dataDir), 0o700),
		"make daemon lock path unopenable as a file")

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{Data: config.DataConfig{DataDir: dataDir}}

	err := refuseRestoreIntoLiveDaemonHome(dataDir)
	require.ErrorContains(err, "inspect daemon ownership",
		"restore must fail closed when daemon ownership cannot be determined")
}

func TestResolveBackupRepoNilConfig(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = nil

	repo, err := resolveBackupRepo("/flag/repo")

	require.NoError(t, err)
	assert.Equal(t, "/flag/repo", repo)
}
