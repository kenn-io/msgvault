package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

type failingCardDAVService struct {
	scheduledCardDAVFixture
}

func (f *failingCardDAVService) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	return carddav.SyncResult{}, errors.New("later book failed")
}

func TestCardDAVSchedulerRequestsCacheRebuildAfterFailedSync(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	requests := make(chan string, 2)
	refresher := newBackgroundCacheRefresher(t.Context(), func(_ context.Context, id string) error {
		requests <- id
		return nil
	}, nil)
	old := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = old
		req.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &failingCardDAVService{}
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, slog.New(slog.DiscardHandler), config.DefaultCardDAVConnection, testInvocationWithConfig(cfg)))
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	select {
	case id := <-requests:
		req.Equal("carddav", id)
	case <-time.After(10 * time.Second):
		req.FailNow("cache refresh was not requested")
	}
}

func TestFreshCardDAVIdentityCacheSkipsRebuild(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msgvault.db")
	analyticsDir := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	_, err := buildCache(t.Context(), dbPath, analyticsDir, true)
	req.NoError(err)
	archiveRevision, err := st.IdentityRevisionContext(t.Context())
	req.NoError(err)

	old := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context, string) error {
		return errors.New("fresh identity cache triggered a rebuild")
	}
	t.Cleanup(func() { runDerivedCacheSubprocess = old })

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	cfg.Data.DatabaseURL = dbPath
	adapter := &storeAPIAdapter{store: st, config: cfg, analyticsDir: analyticsDir}
	got, err := adapter.RefreshIdentityDatasets(t.Context())
	req.NoError(err)
	req.Equal(archiveRevision, got)
}

func TestStaleCardDAVParticipantIdentifierCacheStillRefreshes(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msgvault.db")
	analyticsDir := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	req.NoError(os.MkdirAll(analyticsDir, 0o700))
	participantID, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
	req.NoError(err)
	identityRevision, err := st.IdentityRevisionContext(t.Context())
	req.NoError(err)
	identifierRevision, err := st.ParticipantIdentifierRevisionContext(t.Context())
	req.NoError(err)
	now := time.Now().UTC()
	marker, err := json.Marshal(query.CacheSyncState{
		IdentityRevision:              identityRevision,
		ParticipantIdentifierRevision: identifierRevision,
		SchemaVersion:                 query.CacheSchemaVersion,
		LastSyncAt:                    now,
		PublishedAt:                   now,
		DatasetFingerprint:            "committed",
	})
	req.NoError(err)
	req.NoError(os.WriteFile(query.CacheStatePath(analyticsDir), marker, 0o600))
	req.NoError(st.SetParticipantIdentifier(participantID, "apple_id", "carddav-cache-stale"))

	refreshErr := errors.New("stale participant identifier cache triggered refresh")
	old := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context, string) error { return refreshErr }
	t.Cleanup(func() { runDerivedCacheSubprocess = old })

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	cfg.Data.DatabaseURL = dbPath
	adapter := &storeAPIAdapter{store: st, config: cfg, analyticsDir: analyticsDir}
	_, err = adapter.RefreshIdentityDatasets(t.Context())
	req.ErrorIs(err, refreshErr)
}

func TestFailedCardDAVIdentityRefreshQueuesRealCacheRepair(t *testing.T) {
	req := require.New(t)
	checks := assert.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msgvault.db")
	analyticsDir := filepath.Join(dir, "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	first, err := st.EnsureParticipant("first@example.test", "First", "example.test")
	req.NoError(err)
	second, err := st.EnsureParticipant("second@example.test", "Second", "example.test")
	req.NoError(err)
	_, err = buildCache(t.Context(), dbPath, analyticsDir, true)
	req.NoError(err)
	before, err := query.ReadCacheSyncState(analyticsDir)
	req.NoError(err)
	_, err = st.LinkParticipants(first, second)
	req.NoError(err)
	revision, err := st.IdentityRevision()
	req.NoError(err)
	checks.Greater(revision, before.IdentityRevision)
	req.True(cacheNeedsBuildForScheduledRefresh(t.Context(), dbPath, analyticsDir).NeedsBuild)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	cfg.Data.DatabaseURL = dbPath
	cfg.Analytics.AutoBuildCache = true
	refreshErr := errors.New("identity cache refresh interrupted")
	oldDerived, oldScheduled := runDerivedCacheSubprocess, runScheduledBuildCacheSubprocess
	runDerivedCacheSubprocess = func(context.Context, string) error { return refreshErr }
	repaired := make(chan struct{}, 1)
	runScheduledBuildCacheSubprocess = func(ctx context.Context) error {
		_, err := buildCacheDerivedOnly(ctx, dbPath, analyticsDir)
		if err == nil {
			repaired <- struct{}{}
		}
		return err
	}
	t.Cleanup(func() {
		runDerivedCacheSubprocess = oldDerived
		runScheduledBuildCacheSubprocess = oldScheduled
	})

	adapter := &storeAPIAdapter{store: st, config: cfg, analyticsDir: analyticsDir}
	refresher := newBackgroundCacheRefresher(adapter.invocationContext(t.Context()), nil, nil)
	oldRefresher := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = oldRefresher
		req.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	_, err = adapter.RefreshIdentityDatasets(t.Context())
	req.ErrorIs(err, refreshErr)
	select {
	case <-repaired:
	case <-time.After(5 * time.Second):
		req.FailNow("background worker did not publish the repaired identity cache")
	}
	after, err := query.ReadCacheSyncState(analyticsDir)
	req.NoError(err)
	checks.Equal(revision, after.IdentityRevision)
	checks.False(cacheNeedsBuildForScheduledRefresh(t.Context(), dbPath, analyticsDir).NeedsBuild)
}
