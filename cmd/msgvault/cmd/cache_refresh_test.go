package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
)

func TestDerivedOnlyRefusesStaleSchemaBeforeCreatingStaging(t *testing.T) {
	requirementsForTest := require.New(t)
	parent := t.TempDir()
	dbPath := filepath.Join(parent, "msgvault.db")
	analyticsDir := filepath.Join(parent, "analytics")
	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.InitSchema())
	requirementsForTest.NoError(st.Close())
	requirementsForTest.NoError(os.MkdirAll(analyticsDir, 0o755))
	marker, err := json.Marshal(query.CacheSyncState{
		LastSyncAt:    time.Now().UTC(),
		SchemaVersion: query.CacheSchemaVersion - 1,
	})
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), marker, 0o600))

	_, err = buildCacheDerivedOnly(dbPath, analyticsDir)
	requirementsForTest.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
	staging, globErr := filepath.Glob(filepath.Join(
		parent,
		cacheStagingPrefix(analyticsDir)+"*",
	))
	requirementsForTest.NoError(globErr)
	assert.Empty(t, staging)
}

func TestProductionCacheBuilderOpenSitesUseConfiguredOverrides(t *testing.T) {
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })

	configure := func(dataDir, dbPath string) {
		cfg = config.NewDefaultConfig()
		cfg.Data.DataDir = dataDir
		cfg.Data.DatabaseURL = dbPath
		cfg.Analytics.BuilderMemoryLimit = "1B"
	}

	t.Run("full build", func(t *testing.T) {
		tmp := setupTestSQLite(t)
		configure(tmp, filepath.Join(tmp, "test.db"))

		err := runBuildCacheLocalMode(buildCacheModeFull)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "memory_limit")
	})

	t.Run("derived refresh", func(t *testing.T) {
		tmp := setupTestSQLite(t)
		dbPath := filepath.Join(tmp, "test.db")
		analyticsDir := filepath.Join(tmp, "analytics")
		_, err := buildCache(dbPath, analyticsDir, true)
		require.NoError(t, err)
		configure(tmp, dbPath)

		err = runBuildCacheLocalMode(buildCacheModeDerived)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "memory_limit")
	})
}

func TestDerivedOnlyRefreshCarriesStatsAndRefreshesMembershipRollups(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirementsForTest.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	requirementsForTest.NoError(err)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	_, err = st.DB().Exec(`
		INSERT INTO conversation_participants (conversation_id, participant_id)
		VALUES (102, 3)
	`)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.Close())

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirementsForTest.NoError(err)
	assertionsForTest.True(result.IdentityOnly)

	after, err := query.ReadCacheSyncState(analyticsDir)
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(before.Stats, after.Stats)
	assertionsForTest.NotEqual(
		before.ConversationParticipantsFingerprint,
		after.ConversationParticipantsFingerprint,
	)
	assertionsForTest.False(after.PublishedAt.Before(before.PublishedAt))
	fingerprint, err := query.CacheDatasetFingerprint(analyticsDir)
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(fingerprint, after.DatasetFingerprint)
	assertionsForTest.Equal(messagesBefore,
		snapshotMessagesDatasetBytes(t, analyticsDir))

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "test-duckdb-tmp")),
	)
	requirementsForTest.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var membershipRows int64
	requirementsForTest.NoError(duckDB.QueryRow(`
		SELECT count(DISTINCT message_id)
		FROM read_parquet(?, hive_partitioning = true)
		WHERE conversation_id = 102
		  AND canonical_id = 3
		  AND is_conversation_member
	`, filepath.Join(
		analyticsDir,
		identityindex.DatasetActivity,
		"**",
		"*.parquet",
	)).Scan(&membershipRows))
	assertionsForTest.Positive(membershipRows)
}

func TestDerivedOnlyRefreshWithNoChangesIsANoOp(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	before := snapshotCacheBytes(t, analyticsDir)

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	assertions.True(result.Skipped,
		"unchanged identity state must not republish derived datasets")
	assertions.Equal(before, snapshotCacheBytes(t, analyticsDir),
		"a no-op refresh must leave every cache byte (including the marker) intact")
}

func TestDerivedOnlyFailurePreservesMarkerAndLeavesDetectableDrift(t *testing.T) {
	requirementsForTest := require.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirementsForTest.NoError(err)

	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	_, err = st.LinkParticipants(2, 3)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.Close())

	beforeMarker, err := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirementsForTest.NoError(err)
	sentinel := errors.New("derived publication sentinel")
	derivedPublishBeforeMarkerHook = func() error { return sentinel }
	t.Cleanup(func() { derivedPublishBeforeMarkerHook = nil })

	_, err = buildCacheDerivedOnly(dbPath, analyticsDir)
	requirementsForTest.ErrorIs(err, sentinel)
	afterMarker, readErr := os.ReadFile(query.CacheStatePath(analyticsDir))
	requirementsForTest.NoError(readErr)
	assert.Equal(t, beforeMarker, afterMarker)
	assert.Equal(t, query.CacheDrifted, mustInspectCacheReadiness(t, analyticsDir))
}

func TestStoreAPIIdentityRefreshLaunchesChildWithoutParentCacheLock(t *testing.T) {
	requirementsForTest := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	st, err := store.Open(dbPath)
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(st.InitSchema())
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirementsForTest.NoError(os.MkdirAll(analyticsDir, 0o755))

	const revision = int64(42)
	marker, err := json.Marshal(query.CacheSyncState{IdentityRevision: revision})
	requirementsForTest.NoError(err)
	requirementsForTest.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), marker, 0o600))

	old := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context, string) error {
		lock := flock.New(query.CacheBuildLockPath(analyticsDir))
		locked, lockErr := lock.TryLock()
		require.NoError(t, lockErr)
		require.True(t, locked, "parent daemon must not hold the cache lock")
		require.NoError(t, lock.Unlock())
		return nil
	}
	t.Cleanup(func() { runDerivedCacheSubprocess = old })

	adapter := &storeAPIAdapter{store: st, analyticsDir: analyticsDir}
	got, err := adapter.RefreshIdentityDatasets(context.Background())
	requirementsForTest.NoError(err)
	assert.Equal(t, revision, got)
}

func TestDerivedChildEscalatesTypedPreconditionFailureToFullBuild(t *testing.T) {
	requirements := require.New(t)
	old := executeBuildCacheSubprocessMode
	var modes []buildCacheMode
	executeBuildCacheSubprocessMode = func(
		_ context.Context,
		mode buildCacheMode,
	) error {
		modes = append(modes, mode)
		if mode == buildCacheModeDerived {
			return ErrDerivedRefreshRequiresFullBuild
		}
		return nil
	}
	t.Cleanup(func() { executeBuildCacheSubprocessMode = old })

	// A bare state marker makes readiness CacheInterrupted (an existing
	// cache needing repair), which permits escalation to a full build.
	analyticsDir := t.TempDir()
	marker, err := json.Marshal(query.CacheSyncState{IdentityRevision: 1})
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), marker, 0o600))
	requirements.NoError(runDerivedCacheSubprocess(context.Background(), analyticsDir))
	assert.Equal(t,
		[]buildCacheMode{buildCacheModeDerived, buildCacheModeFull},
		modes,
	)
}

func TestDerivedChildDoesNotEscalateWhenCacheAbsent(t *testing.T) {
	requirements := require.New(t)
	old := executeBuildCacheSubprocessMode
	var modes []buildCacheMode
	executeBuildCacheSubprocessMode = func(
		_ context.Context,
		mode buildCacheMode,
	) error {
		modes = append(modes, mode)
		return ErrDerivedRefreshRequiresFullBuild
	}
	t.Cleanup(func() { executeBuildCacheSubprocessMode = old })

	err := runDerivedCacheSubprocess(context.Background(), t.TempDir())
	requirements.ErrorIs(err, ErrDerivedRefreshRequiresFullBuild)
	assert.Equal(t, []buildCacheMode{buildCacheModeDerived}, modes,
		"a derived refresh must not create a cache that configuration left absent")
}

func TestBuildCacheInternalModesAreMutuallyExclusive(t *testing.T) {
	_, err := requestedBuildCacheMode(true, true, false)
	require.ErrorContains(t, err, "mutually exclusive")
	_, err = requestedBuildCacheMode(false, true, true)
	require.ErrorContains(t, err, "mutually exclusive")
}

func snapshotCacheBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	}))
	return out
}

func snapshotMessagesDatasetBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	return snapshotCacheBytes(t, filepath.Join(root, tableMessages))
}

func TestRebuildCacheAfterWriteReturnsError(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	require.NoError(st.Close())

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}

	sentinel := errors.New("cache export sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return sentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	err = rebuildCacheAfterWrite(dbPath)
	require.ErrorIs(err, sentinel)
	require.ErrorContains(err, "refresh analytics cache")
}

func TestRebuildCacheAfterDerivedRepairRefreshesCurrentCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}
	analyticsDir := cfg.AnalyticsDir()

	st, err := store.Open(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "!cache-repair:example.org", "direct_chat", "Cache repair",
	)
	require.NoError(err)
	sentAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "repair-cache-1",
		MessageType:     "beeper",
		SentAt:          sql.NullTime{Time: sentAt, Valid: true},
		ReceivedAt:      sql.NullTime{Time: sentAt, Valid: true},
		Snippet:         sql.NullString{String: "stale snippet", Valid: true},
		HasAttachments:  true,
		AttachmentCount: 1,
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(
		messageID,
		sql.NullString{String: "stale body", Valid: true},
		sql.NullString{},
	))
	raw, err := json.Marshal(map[string]any{
		"id":         "repair-cache-1",
		"chatID":     "!cache-repair:example.org",
		"accountID":  "signal",
		"senderID":   "@user-a:example.org",
		"senderName": "User A",
		"timestamp":  sentAt,
		"type":       "IMAGE",
		"text":       "https://example.com/post",
		"attachments": []map[string]any{{
			"id": "mxc://example.org/share", "type": "img", "mimeType": "image/jpeg",
		}},
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageRawWithFormat(messageID, raw, "beeper_json"))
	require.NoError(st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{{
		MimeType:           "image/jpeg",
		StoragePath:        "mxc://example.org/share",
		SourceAttachmentID: "beeper:mxc://example.org/share",
		MediaType:          "image",
	}}))
	require.NoError(st.Close())

	_, err = buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "build current-schema cache before repair")
	initialState, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.Equal(query.CacheSchemaVersion, initialState.SchemaVersion)
	assert.Zero(initialState.DerivedDataRevision)

	readCached := func() (string, any) {
		t.Helper()
		engine, openErr := query.NewDuckDBEngine(analyticsDir, "", nil)
		require.NoError(openErr)
		result, queryErr := engine.QuerySQL(context.Background(), `
			SELECT m.snippet, a.attachment_metadata
			FROM messages m
			JOIN attachments a ON a.message_id = m.id
			WHERE m.source_message_id = 'repair-cache-1'`)
		closeErr := engine.Close()
		require.NoError(queryErr)
		require.NoError(closeErr)
		require.Len(result.Rows, 1)
		return fmt.Sprint(result.Rows[0][0]), result.Rows[0][1]
	}

	beforeSnippet, beforeMetadata := readCached()
	assert.Equal("stale snippet", beforeSnippet)
	assert.Nil(beforeMetadata)

	st, err = store.Open(dbPath)
	require.NoError(err)
	sum, err := rederive.Run(
		context.Background(), st, "beeper", source.Identifier, source.ID, nil,
	)
	require.NoError(err)
	require.Zero(sum.Errors)
	require.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(staleness.NeedsBuild)
	assert.True(staleness.HasDerivedDataDrift)
	assert.True(staleness.FullRebuild,
		"an incremental append cannot replace already-cached repaired rows")

	require.NoError(rebuildCacheAfterWrite(dbPath))
	repairedState, err := query.ReadCacheSyncState(analyticsDir)
	require.NoError(err)
	assert.Equal(int64(1), repairedState.DerivedDataRevision)
	afterSnippet, afterMetadata := readCached()
	assert.Equal("https://example.com/post", afterSnippet)
	require.NotNil(afterMetadata)
	assert.JSONEq(`{"shared_url":"https://example.com/post"}`, fmt.Sprint(afterMetadata))
}

func TestScheduledCacheRefreshSkipsWhenAutoBuildCacheDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Analytics: config.AnalyticsConfig{
			AutoBuildCache: false,
		},
	}

	builds := 0
	oldRunBuild := runBuildCacheSubprocess
	runBuildCacheSubprocess = func(context.Context, bool, bool) error {
		builds++
		return errors.New("unexpected scheduled cache build")
	}
	t.Cleanup(func() { runBuildCacheSubprocess = oldRunBuild })

	err := rebuildCacheAfterScheduledSync(context.Background(), "disabled")
	require.NoError(err)
	assert.Zero(builds, "disabled auto_build_cache must not start a cache build")
}

func TestScheduledCacheBuildDelay(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		staleness    cacheStaleness
		interval     time.Duration
		wantDelay    time.Duration
		wantThrottle bool
	}{
		{
			name: "disabled interval",
			staleness: cacheStaleness{
				HasUsablePublication: true,
				PublishedAt:          now.Add(-time.Minute),
			},
		},
		{
			name: "unusable publication",
			staleness: cacheStaleness{
				PublishedAt: now.Add(-time.Minute),
			},
			interval: 6 * time.Hour,
		},
		{
			name: "recent publication",
			staleness: cacheStaleness{
				HasUsablePublication: true,
				PublishedAt:          now.Add(-time.Hour),
			},
			interval:     6 * time.Hour,
			wantDelay:    5 * time.Hour,
			wantThrottle: true,
		},
		{
			name: "elapsed interval",
			staleness: cacheStaleness{
				HasUsablePublication: true,
				PublishedAt:          now.Add(-6 * time.Hour),
			},
			interval: 6 * time.Hour,
		},
		{
			name: "small future skew",
			staleness: cacheStaleness{
				HasUsablePublication: true,
				PublishedAt:          now.Add(time.Hour),
			},
			interval:     6 * time.Hour,
			wantDelay:    7 * time.Hour,
			wantThrottle: true,
		},
		{
			name: "maximum future skew",
			staleness: cacheStaleness{
				HasUsablePublication: true,
				PublishedAt:          now.Add(6 * time.Hour),
			},
			interval:     6 * time.Hour,
			wantDelay:    12 * time.Hour,
			wantThrottle: true,
		},
		{
			name: "large future skew",
			staleness: cacheStaleness{
				HasUsablePublication: true,
				PublishedAt:          now.Add(6*time.Hour + time.Nanosecond),
			},
			interval: 6 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delay, throttle := scheduledCacheBuildDelay(tt.staleness, tt.interval, now)
			assert.Equal(t, tt.wantThrottle, throttle)
			assert.Equal(t, tt.wantDelay, delay)
		})
	}
}

func TestScheduledCacheRefreshMinimumInterval(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	sentinel := errors.New("cache build sentinel")
	tests := []struct {
		name        string
		publishedAt time.Time
		buildErr    error
		wantBuilds  int
	}{
		{
			name:        "recent publication suppresses build",
			publishedAt: now.Add(-time.Hour),
		},
		{
			name:        "elapsed interval permits build",
			publishedAt: now.Add(-7 * time.Hour),
			wantBuilds:  1,
		},
		{
			name:        "large future skew permits repair build",
			publishedAt: now.Add(7 * time.Hour),
			wantBuilds:  1,
		},
		{
			name:        "failed build does not advance publication",
			publishedAt: now.Add(-7 * time.Hour),
			buildErr:    sentinel,
			wantBuilds:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := setupTestSQLiteEmpty(t)
			dbPath := filepath.Join(tmpDir, "test.db")
			analyticsDir := filepath.Join(tmpDir, "analytics")
			writeSyncStateAt(t, analyticsDir, 0, now.Add(-24*time.Hour))
			createFakeParquet(t, analyticsDir)

			state, err := query.ReadCacheSyncState(analyticsDir)
			require.NoError(t, err)
			state.PublishedAt = tt.publishedAt
			stateData, err := json.Marshal(state)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(query.CacheStatePath(analyticsDir), stateData, 0o600))

			db, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = db.Exec(`
				INSERT INTO messages (id, source_id, source_message_id, sent_at)
				VALUES (1, 1, 'new-message', ?)
			`, now)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			savedCfg := cfg
			cfg = &config.Config{
				HomeDir: tmpDir,
				Data: config.DataConfig{
					DataDir:     tmpDir,
					DatabaseURL: dbPath,
				},
				Analytics: config.AnalyticsConfig{
					AutoBuildCache:     true,
					MinRebuildInterval: 6 * time.Hour,
				},
			}
			t.Cleanup(func() { cfg = savedCfg })

			oldNow := scheduledCacheBuildNow
			scheduledCacheBuildNow = func() time.Time { return now }
			t.Cleanup(func() { scheduledCacheBuildNow = oldNow })

			builds := 0
			oldRunBuild := runBuildCacheSubprocess
			runBuildCacheSubprocess = func(context.Context, bool, bool) error {
				builds++
				return tt.buildErr
			}
			t.Cleanup(func() { runBuildCacheSubprocess = oldRunBuild })

			err = rebuildCacheAfterScheduledSync(context.Background(), "test-source")
			if tt.buildErr != nil {
				require.ErrorIs(t, err, tt.buildErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantBuilds, builds)

			after, err := query.ReadCacheSyncState(analyticsDir)
			require.NoError(t, err)
			assert.Equal(t, tt.publishedAt, after.PublishedAt)
		})
	}
}

func TestRepairEncodingReturnsCacheRefreshError(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()
	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{HomeDir: tmpDir, Data: config.DataConfig{DataDir: tmpDir}}
	stateFile := filepath.Join(cfg.AnalyticsDir(), "_last_sync.json")
	require.NoError(os.MkdirAll(cfg.AnalyticsDir(), 0o755))
	require.NoError(os.WriteFile(stateFile, []byte(`{"schema_version":18}`), 0o600))

	sentinel := errors.New("repair cache sentinel")
	buildCacheBeforeMessagesExportHook = func() error { return sentinel }
	t.Cleanup(func() { buildCacheBeforeMessagesExportHook = nil })

	err := runRepairEncodingLocal(&cobra.Command{})
	require.ErrorIs(err, sentinel)
	require.ErrorContains(err, "encoding repair completed")
	require.ErrorContains(err, "analytics cache refresh failed")
	require.NoFileExists(stateFile,
		"a failed post-repair rebuild must leave the old cache marked stale")
}

func TestScheduledCacheRefreshFailurePreservesCompletedSyncRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	const identifier = "imaps://user@example.com@imap.example.com:993"
	source, err := st.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{
		MessagesProcessed: 7,
		MessagesAdded:     5,
		MessagesUpdated:   2,
	}))
	require.NoError(st.CompleteSync(syncID, "cursor-2"))

	savedCfg := cfg
	t.Cleanup(func() { cfg = savedCfg })
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		Analytics: config.AnalyticsConfig{
			AutoBuildCache: true,
		},
	}

	sentinel := errors.New("scheduled cache sentinel")
	oldRunBuild := runBuildCacheSubprocess
	runBuildCacheSubprocess = func(context.Context, bool, bool) error { return sentinel }
	t.Cleanup(func() { runBuildCacheSubprocess = oldRunBuild })

	getOAuthMgr := func(string) (*oauth.Manager, error) {
		return nil, errors.New("unexpected Gmail OAuth path")
	}
	err = runScheduledSync(context.Background(), identifier, st, getOAuthMgr)
	require.ErrorIs(err, sentinel, "cache failure must reach the scheduled job result")
	require.ErrorContains(err, "refresh analytics cache")

	latest, err := st.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(syncID, latest.ID)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(7), latest.MessagesProcessed)
	assert.Equal(int64(5), latest.MessagesAdded)
	assert.Equal(int64(2), latest.MessagesUpdated)
}

func TestConversationTypeDriftDetectedAndRepairedByDerivedRefresh(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	requirements.NotEmpty(before.ConversationTypesFingerprint)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	clean := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.False(clean.NeedsBuild,
		"fresh build must not report staleness (reason: %q)", clean.Reason)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	_, err = st.DB().Exec(
		`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = 102`)
	requirements.NoError(err)
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasConversationTypeDrift)
	assertions.False(staleness.FullRebuild,
		"type drift without new messages must stay repairable by the derived refresh")
	assertions.True(derivedDriftOnly(staleness))
	assertions.Contains(staleness.Reason, "conversation metadata changed")

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	assertions.False(result.Skipped)

	after, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	assertions.NotEqual(before.ConversationTypesFingerprint, after.ConversationTypesFingerprint)
	assertions.Equal(messagesBefore,
		snapshotMessagesDatasetBytes(t, analyticsDir),
		"derived refresh must not rewrite message facts")

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "types-duckdb-tmp")),
	)
	requirements.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var conversationsType string
	requirements.NoError(duckDB.QueryRow(`
		SELECT conversation_type FROM read_parquet(?) WHERE id = 102
	`, filepath.Join(analyticsDir, tableConversations, "*.parquet")).Scan(&conversationsType))
	assertions.Equal("group_chat", conversationsType,
		"republished conversations dataset must carry the new type")
	var staleActivityRows int64
	requirements.NoError(duckDB.QueryRow(`
		SELECT count(*)
		FROM read_parquet(?, hive_partitioning = true)
		WHERE conversation_id = 102 AND conversation_type <> 'group_chat'
	`, filepath.Join(
		analyticsDir, identityindex.DatasetActivity, "**", "*.parquet",
	)).Scan(&staleActivityRows))
	assertions.Zero(staleActivityRows,
		"rebuilt activity rows must embed the new conversation type")

	repaired := cacheNeedsBuild(dbPath, analyticsDir)
	assertions.False(repaired.NeedsBuild,
		"repaired cache must be clean (reason: %q)", repaired.Reason)
}

func TestConversationTitleDriftDetectedAndRepairedByDerivedRefresh(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	_, err = st.DB().Exec(
		`UPDATE conversations SET title = 'Updated cache title' WHERE id = 102`)
	requirements.NoError(err)
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasConversationTypeDrift)
	assertions.False(staleness.FullRebuild,
		"title drift without new messages must stay repairable by the derived refresh")
	assertions.Contains(staleness.Reason, "conversation metadata changed")

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	assertions.Equal(messagesBefore, snapshotMessagesDatasetBytes(t, analyticsDir),
		"derived refresh must not rewrite message facts")

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "title-duckdb-tmp")),
	)
	requirements.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var title string
	requirements.NoError(duckDB.QueryRow(`
		SELECT title FROM read_parquet(?) WHERE id = 102
	`, filepath.Join(analyticsDir, tableConversations, "*.parquet")).Scan(&title))
	assertions.Equal("Updated cache title", title)
}

func TestFullBuildForcedWhenTypeDriftCoincidesWithNewMessages(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	_, err = st.DB().Exec(`
		UPDATE conversations SET conversation_type = 'group_chat' WHERE id = 102;
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, sent_at, subject, snippet)
			VALUES (99, 102, 1, 'msg99', '2026-07-21 10:00:00', 'Late', 'Preview');
	`)
	requirements.NoError(err)
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasNew)
	assertions.True(staleness.HasConversationTypeDrift)
	assertions.True(staleness.FullRebuild,
		"incremental append cannot rewrite committed activity rows under a changed type")
	assertions.False(derivedDriftOnly(staleness))
}

// TestParticipantDisplayNameDriftDetectedAndRepairedByDerivedRefresh pins
// display-name changes to the derived-only cache path: participants.parquet
// and relationship_people must be republished, while message facts remain
// byte-identical and no full rebuild is required.
func TestParticipantDisplayNameDriftDetectedAndRepairedByDerivedRefresh(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	clean := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.False(clean.NeedsBuild,
		"fresh build must not report staleness (reason: %q)", clean.Reason)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	// The identifier backfill API only fills an empty name. Clear the fixture
	// value first so the mutation exercises the production write path that must
	// advance ParticipantDisplayNameRevision without changing the identifier.
	_, err = st.DB().Exec(`UPDATE participants SET display_name = NULL WHERE id = 1`)
	requirements.NoError(err)
	updatedID, err := st.EnsureParticipantByIdentifier(
		"email", "alice@example.com", "Alice Updated",
	)
	requirements.NoError(err)
	requirements.Equal(int64(1), updatedID)
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasParticipantDisplayNameDrift)
	assertions.False(staleness.FullRebuild,
		"display-name drift must stay repairable by the derived refresh")
	assertions.True(derivedDriftOnly(staleness))
	assertions.Contains(staleness.Reason, "participant display names changed")

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	assertions.False(result.Skipped)

	after, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	assertions.NotEqual(
		before.ParticipantDisplayNameRevision,
		after.ParticipantDisplayNameRevision,
	)
	assertions.Equal(messagesBefore, snapshotMessagesDatasetBytes(t, analyticsDir),
		"derived refresh must not rewrite message facts")

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "display-names-duckdb-tmp")),
	)
	requirements.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var participantName string
	requirements.NoError(duckDB.QueryRow(`
		SELECT display_name FROM read_parquet(?) WHERE id = 1
	`, filepath.Join(analyticsDir, tableParticipants, "*.parquet")).Scan(&participantName))
	assertions.Equal("Alice Updated", participantName,
		"republished participants dataset must carry the new display name")

	var displayLabel string
	requirements.NoError(duckDB.QueryRow(`
		SELECT display_label FROM read_parquet(?) WHERE canonical_id = 1
	`, filepath.Join(analyticsDir, identityindex.DatasetPeople, "*.parquet")).Scan(&displayLabel))
	assertions.Equal("Alice Updated", displayLabel,
		"relationship_people label must use the republished participant name")
	var searchable bool
	requirements.NoError(duckDB.QueryRow(`
		SELECT list_contains(search_values, 'alice updated')
		FROM read_parquet(?) WHERE canonical_id = 1
	`, filepath.Join(analyticsDir, identityindex.DatasetPeople, "*.parquet")).Scan(&searchable))
	assertions.True(searchable,
		"relationship_people search values must include the new display name")

	repaired := cacheNeedsBuild(dbPath, analyticsDir)
	assertions.False(repaired.NeedsBuild,
		"repaired cache must be clean (reason: %q)", repaired.Reason)
}

// TestParticipantIdentifierDriftDetectedAndRepairedByDerivedRefresh pins the
// staleness contract for identifier-mapping changes without message
// activity: SetParticipantIdentifier alone must surface as derived-only
// drift (never a full rebuild), and the derived refresh must republish the
// participant_identifiers dataset and rebuild relationship_people search
// values from the new mapping.
func TestParticipantIdentifierDriftDetectedAndRepairedByDerivedRefresh(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	clean := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.False(clean.NeedsBuild,
		"fresh build must not report staleness (reason: %q)", clean.Reason)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	requirements.NoError(st.SetParticipantIdentifier(3, "email", "carol.alias@example.net"))
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasParticipantIdentifierDrift)
	assertions.False(staleness.FullRebuild,
		"identifier drift without new messages must stay repairable by the derived refresh")
	assertions.True(derivedDriftOnly(staleness))
	assertions.Contains(staleness.Reason, "participant identifiers changed")

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	assertions.False(result.Skipped)

	after, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	assertions.NotEqual(before.ParticipantIdentifierRevision, after.ParticipantIdentifierRevision)
	assertions.Equal(messagesBefore,
		snapshotMessagesDatasetBytes(t, analyticsDir),
		"derived refresh must not rewrite message facts")

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "identifiers-duckdb-tmp")),
	)
	requirements.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var mappedParticipant int64
	requirements.NoError(duckDB.QueryRow(`
		SELECT participant_id FROM read_parquet(?)
		WHERE identifier_value = 'carol.alias@example.net'
	`, filepath.Join(analyticsDir, tableParticipantIdentifiers, "*.parquet")).Scan(&mappedParticipant))
	assertions.Equal(int64(3), mappedParticipant,
		"republished participant_identifiers dataset must carry the new mapping")
	var searchable bool
	requirements.NoError(duckDB.QueryRow(`
		SELECT list_contains(search_values, 'carol.alias@example.net')
		FROM read_parquet(?) WHERE canonical_id = 3
	`, filepath.Join(
		analyticsDir, identityindex.DatasetPeople, "*.parquet",
	)).Scan(&searchable))
	assertions.True(searchable,
		"rebuilt relationship_people search values must include the new identifier")

	repaired := cacheNeedsBuild(dbPath, analyticsDir)
	assertions.False(repaired.NeedsBuild,
		"repaired cache must be clean (reason: %q)", repaired.Reason)
}

// TestParticipantIdentifierDriftCreatingParticipantRestagesParticipants pins
// the new-participant variant of identifier drift. Creating an identifier
// without message activity still adds a participants row that the derived
// refresh must publish; the message dataset must remain untouched.
func TestParticipantIdentifierDriftCreatingParticipantRestagesParticipants(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	const newParticipantID int64 = 5
	// This cache fixture intentionally uses the legacy participant schema, so
	// seed the new row directly and let the production identifier write create
	// the drift watermark. The refresh must publish the row even though it has
	// no message activity yet.
	_, err = st.DB().Exec(`
		INSERT INTO participants (id, email_address, domain, display_name)
		VALUES (?, ?, ?, ?)
	`, newParticipantID, "new-cache-participant@example.test", "example.test", "New Cache Participant")
	requirements.NoError(err)
	err = st.SetParticipantIdentifier(
		newParticipantID, "slack", "new-cache-participant",
	)
	requirements.NoError(err)
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasParticipantIdentifierDrift)
	assertions.False(staleness.FullRebuild,
		"identifier drift that creates a participant must stay derived-only")
	assertions.True(derivedDriftOnly(staleness))
	assertions.Contains(staleness.Reason, "participant identifiers changed")

	result, err := buildCacheDerivedOnly(dbPath, analyticsDir)
	requirements.NoError(err)
	assertions.True(result.IdentityOnly)
	assertions.False(result.Skipped)

	after, err := query.ReadCacheSyncState(analyticsDir)
	requirements.NoError(err)
	assertions.NotEqual(before.ParticipantIdentifierRevision, after.ParticipantIdentifierRevision)
	assertions.Equal(messagesBefore, snapshotMessagesDatasetBytes(t, analyticsDir),
		"derived refresh must not rewrite message facts")

	duckDB, err := duckdbutil.Open(
		context.Background(),
		duckdbutil.BuilderPolicy(filepath.Join(tmp, "new-participant-duckdb-tmp")),
	)
	requirements.NoError(err)
	defer func() { require.NoError(t, duckDB.Close()) }()
	var participantName string
	requirements.NoError(duckDB.QueryRow(`
		SELECT display_name FROM read_parquet(?) WHERE id = ?
	`, filepath.Join(analyticsDir, tableParticipants, "*.parquet"), newParticipantID).Scan(&participantName))
	assertions.Equal("New Cache Participant", participantName,
		"identifier drift that creates a participant must republish participants.parquet")

	repaired := cacheNeedsBuild(dbPath, analyticsDir)
	assertions.False(repaired.NeedsBuild,
		"repaired cache must be clean (reason: %q)", repaired.Reason)
}

// TestIncrementalBuildRepairsParticipantIdentifierDriftWithNewMessages pins
// that identifier drift coinciding with new messages does NOT escalate to a
// full rebuild the way link/membership/type drift does: identifiers are not
// baked into per-row activity facts, and incremental builds re-stage the
// participant_identifiers dataset in full anyway.
func TestIncrementalBuildRepairsParticipantIdentifierDriftWithNewMessages(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	tmp := setupTestSQLite(t)
	dbPath := filepath.Join(tmp, "test.db")
	analyticsDir := filepath.Join(tmp, "analytics")
	_, err := buildCache(dbPath, analyticsDir, true)
	requirements.NoError(err)
	messagesBefore := snapshotMessagesDatasetBytes(t, analyticsDir)

	st, err := store.Open(dbPath)
	requirements.NoError(err)
	requirements.NoError(st.SetParticipantIdentifier(3, "email", "carol.alias@example.net"))
	_, err = st.DB().Exec(`
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, sent_at, subject, snippet)
			VALUES (99, 102, 1, 'msg99', '2026-07-21 10:00:00', 'Late', 'Preview');
	`)
	requirements.NoError(err)
	requirements.NoError(st.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	requirements.True(staleness.NeedsBuild)
	assertions.True(staleness.HasNew)
	assertions.True(staleness.HasParticipantIdentifierDrift)
	assertions.False(staleness.FullRebuild,
		"identifier drift with new messages must stay incremental")
	assertions.False(derivedDriftOnly(staleness))

	result, err := buildCache(dbPath, analyticsDir, false)
	requirements.NoError(err)
	assertions.False(result.Skipped)
	messagesAfter := snapshotMessagesDatasetBytes(t, analyticsDir)
	for path, contents := range messagesBefore {
		assertions.Equal(contents, messagesAfter[path],
			"incremental build must leave committed shard %s untouched", path)
	}
	assertions.Greater(len(messagesAfter), len(messagesBefore),
		"incremental build must append a shard for the new message")

	repaired := cacheNeedsBuild(dbPath, analyticsDir)
	assertions.False(repaired.NeedsBuild,
		"incremental build must clear identifier drift (reason: %q)", repaired.Reason)
}
