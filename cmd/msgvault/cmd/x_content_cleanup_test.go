package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type xCleanupFixture struct {
	st          *store.Store
	cfg         *config.Config
	source      int64
	message     int64
	backend     vector.Backend
	generations []vector.GenerationID
}

// blockingAnalyticsInitTracker pauses the real initializer after it captures
// its generation but before it can open an engine. Cleanup can then run without
// contending with a cache builder and supersede that captured generation.
type blockingAnalyticsInitTracker struct {
	started chan struct{}
	release chan struct{}
}

func (t *blockingAnalyticsInitTracker) BeginWork() (func(), bool) {
	return t.BeginWorkContext(context.Background())
}

func (t *blockingAnalyticsInitTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	close(t.started)
	select {
	case <-t.release:
		return func() {}, true
	case <-ctx.Done():
		return nil, false
	}
}

func newXCleanupFixture(t *testing.T) *xCleanupFixture {
	t.Helper()
	f := &xCleanupFixture{cfg: xTestConfig(t)}
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		f.st = testutil.NewTestStore(t)
		// Reopening helpers must stay in this fixture's actual clone/schema.
		var database, schema string
		require.NoError(t, f.st.DB().QueryRowContext(t.Context(), `SELECT current_database(),current_schema()`).Scan(&database, &schema))
		dsn, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		dsn.Path = "/" + database
		params := dsn.Query()
		params.Set("search_path", schema)
		dsn.RawQuery = params.Encode()
		f.cfg.Data.DatabaseURL = dsn.String()
	} else {
		f.st = testutil.NewSQLiteTestStoreAt(t, f.cfg.DatabaseDSN())
	}
	source, err := f.st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	f.source = source.ID
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "102", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100", "102"}, Text: "erased constellation secret", CreatedAt: at, FetchedAt: at, Availability: "public", Raw: []byte(`{"post":{"id":"102","text":"erased constellation secret"}}`), Author: &store.XAuthorInput{SourceID: f.source, UserID: "1002", Username: "erased_handle", DisplayName: "Erased Label", FetchedAt: at}})
	require.NoError(t, err)
	f.message = saved.MessageID
	f.cfg.Vector.Embeddings.Dimension = 3
	if f.st.IsPostgreSQL() {
		b, err := pgvector.Open(t.Context(), pgvector.Options{DB: f.st.DB(), Dimension: 3})
		if err != nil && strings.Contains(err.Error(), "pgvector support not compiled") {
			t.Skip("requires pgvector build tag for actual PG vectors")
		}
		require.NoError(t, err)
		f.backend = b
	} else {
		b, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(f.cfg.Data.DataDir, "vectors.db"), MainDB: f.st.DB(), MainPath: f.cfg.DatabaseDSN(), Dimension: 3})
		require.NoError(t, err)
		f.backend = b
	}
	t.Cleanup(func() { assert.NoError(t, f.backend.Close()) })
	for i := range 3 {
		gen, err := f.backend.CreateGeneration(t.Context(), "synthetic-local", 3, f.cfg.Vector.GenerationFingerprint())
		require.NoError(t, err)
		f.generations = append(f.generations, gen)
		require.NoError(t, f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: f.message, Vector: []float32{1, 0, 0}}}))
		if i < 2 {
			require.NoError(t, f.backend.ActivateGeneration(t.Context(), gen, true))
		}
	}
	return f
}

func TestXRemovalLeavesNoReadableCopy(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	var engine = query.NewEngine(f.st.DB(), f.st.IsPostgreSQL())
	var duck *query.DuckDBEngine
	if !f.st.IsPostgreSQL() {
		_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		_testRequire.NoError(err)
		duck, err = query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		_testRequire.NoError(err)
		t.Cleanup(func() { assert.NoError(t, duck.Close()) })
		engine = duck
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	host := httptest.NewServer(server.Router())
	t.Cleanup(host.Close)
	for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", f.message), fmt.Sprintf("/api/v1/cli/message/raw?id=%d", f.message), "/api/v1/search?q=constellation"} {
		response, err := xTestHTTPGet(t, host.Client(), host.URL+path)
		_testRequire.NoError(err)
		data, err := io.ReadAll(response.Body)
		_testRequire.NoError(err)
		_testRequire.NoError(response.Body.Close())
		_testRequire.Equal(http.StatusOK, response.StatusCode, string(data))
		_testAssert.Contains(string(data), "erased constellation secret", path)
	}
	var stale *cacheStaging
	if duck != nil {
		staged, stageErr := newCacheStaging(f.cfg.AnalyticsDir())
		_testRequire.NoError(stageErr)
		stale = staged
		_testRequire.NoError(os.CopyFS(stale.root, os.DirFS(f.cfg.AnalyticsDir())))
	}
	work, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	if stale != nil {
		_testAssert.NoDirExists(stale.root)
	}
	_testAssert.False(work.Pending)
	for _, gen := range f.generations {
		stats, err := f.backend.Stats(t.Context(), gen)
		_testRequire.NoError(err)
		_testAssert.Zero(stats.EmbeddingCount)
	}
	for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", f.message), fmt.Sprintf("/api/v1/cli/message/raw?id=%d", f.message), "/api/v1/search?q=constellation"} {
		response, err := xTestHTTPGet(t, http.DefaultClient, host.URL+path)
		_testRequire.NoError(err)
		body, err := io.ReadAll(response.Body)
		_testRequire.NoError(err)
		_testRequire.NoError(response.Body.Close())
		if strings.Contains(path, "/raw?") {
			_testAssert.Equal(http.StatusNotFound, response.StatusCode, string(body))
		} else {
			_testAssert.Equal(http.StatusOK, response.StatusCode, string(body))
		}
		if strings.Contains(path, "/search?") {
			var result struct {
				Total    int              `json:"total"`
				Messages []jsontext.Value `json:"messages"`
			}
			_testRequire.NoError(json.Unmarshal(body, &result))
			_testAssert.Zero(result.Total)
			_testAssert.Empty(result.Messages)
		}
		_testAssert.NotContains(string(body), "erased constellation secret")
		_testAssert.NotContains(string(body), "erased_handle")
		_testAssert.NotContains(string(body), "Erased Label")
	}
	if duck != nil {
		_, err := duck.QuerySQL(t.Context(), `SELECT 1`)
		_testRequire.Error(err)
		fresh, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		_testRequire.NoError(err)
		defer func() { assert.NoError(t, fresh.Close()) }()
		decoded, err := fresh.QuerySQL(t.Context(), `SELECT * FROM read_parquet('`+filepath.ToSlash(filepath.Join(f.cfg.AnalyticsDir(), "messages", "**", "*.parquet"))+`')`)
		_testRequire.NoError(err)
		bytes, err := json.Marshal(decoded)
		_testRequire.NoError(err)
		_testAssert.NotContains(string(bytes), "constellation")
		_testAssert.NotContains(string(bytes), "Erased Label")
		_testRequire.NoError(filepath.WalkDir(f.cfg.AnalyticsDir(), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".parquet" {
				return nil
			}
			rows, err := fresh.QuerySQL(t.Context(), `SELECT * FROM read_parquet('`+strings.ReplaceAll(filepath.ToSlash(path), "'", "''")+`')`)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			for _, removed := range []string{"constellation secret", "erased_handle", "Erased Label"} {
				assert.NotContains(t, string(encoded), removed, path)
			}
			return nil
		}))
	}
	target, err := f.st.ResolveXPostContext(t.Context(), f.source, "102")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
}

func TestXRemovalPendingFenceOnRealDetailRoute(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}
	host := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead}).Router())
	t.Cleanup(host.Close)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	_testRequire.NoError(err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	response, err := xTestHTTPGet(t, http.DefaultClient, fmt.Sprintf("%s/api/v1/messages/%d", host.URL, f.message))
	_testRequire.NoError(err)
	_testRequire.NoError(response.Body.Close())
	_testAssert.Equal(http.StatusServiceUnavailable, response.StatusCode)
	// A restarted cleaner resumes without tokens, budgets, schedules or API.
	restarted := NewXContentCleanup(f.st, f.cfg, f.backend)
	_testRequire.NoError(restarted.Resume(t.Context()))
	response, err = xTestHTTPGet(t, http.DefaultClient, fmt.Sprintf("%s/api/v1/messages/%d", host.URL, f.message))
	_testRequire.NoError(err)
	bytes, err := io.ReadAll(response.Body)
	_testRequire.NoError(err)
	_testRequire.NoError(response.Body.Close())
	_testAssert.NotEqual(http.StatusServiceUnavailable, response.StatusCode)
	_testAssert.NotContains(string(bytes), "constellation")
}

func TestXRemovalRejectsStaleVectorUpserts(t *testing.T) {
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	_, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	// Models need not run: this is the previously read, now stale worker result.
	for _, gen := range f.generations {
		err := f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: f.message, Vector: []float32{1, 0, 0}}})
		if gen == f.generations[0] {
			_testRequire.ErrorIs(err, vector.ErrGenerationRetired)
		} else {
			_testRequire.NoError(err)
		}
		stats, err := f.backend.Stats(t.Context(), gen)
		_testRequire.NoError(err)
		assert.Zero(t, stats.EmbeddingCount)
	}
}

func TestXRemovalAlreadyOpenParquetReaderRejectsPending(t *testing.T) {
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("Parquet is the SQLite analytics path")
	}
	_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	_testRequire.NoError(err)
	duck, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, duck.Close()) }()
	_, err = duck.QuerySQL(t.Context(), `SELECT count(*) FROM messages`)
	_testRequire.NoError(err)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	_testRequire.NoError(err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	_, err = duck.QuerySQL(t.Context(), `SELECT * FROM messages`)
	_testRequire.Error(err)
}

func TestXRemovalClosesRetiredAnalyticsAndOwnedTempDirectories(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("DuckDB handles apply to the SQLite cache")
	}
	_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	_testRequire.NoError(err)
	var engines []*query.DuckDBEngine
	var paths []string
	for i := range 2 {
		path := filepath.Join(f.cfg.Data.DataDir, fmt.Sprintf("owned-query-%d", i))
		_testRequire.NoError(os.MkdirAll(path, 0700))
		engine, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: path, OwnTempDirectory: true})
		_testRequire.NoError(err)
		engines = append(engines, engine)
		paths = append(paths, path)
		t.Cleanup(func() { assert.NoError(t, engine.Close()) })
		_, err = engine.QuerySQL(t.Context(), `SELECT * FROM messages`)
		_testRequire.NoError(err)
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: engines[0], Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	server.SetAnalyticsEngine(engines[1], api.AnalyticsModeDuckDB)
	staleGeneration := server.ContentAnalyticsGeneration()
	t.Cleanup(func() { assert.NoError(t, server.CloseAnalyticsEngine()) })
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	_testAssert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"cleanup must publish a fresh analytics engine after rebuilding the cache")

	explore := httptest.NewRecorder()
	exploreRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(fmt.Sprintf(
		`{"filters":[{"dimension":"source","values":["%d"]}],"limit":1}`,
		f.source,
	)))
	exploreRequest.Header.Set("Content-Type", "application/json")
	exploreRequest.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	exploreRequest.Header.Set("Authorization", "Bearer "+f.cfg.Server.AuthenticationKey())
	server.Router().ServeHTTP(explore, exploreRequest)
	_testRequire.Equal(http.StatusOK, explore.Code, explore.Body.String(),
		"Explore must remain available without restarting the daemon")

	for i, engine := range engines {
		_, err := engine.QuerySQL(t.Context(), `SELECT 1`)
		_testRequire.Error(err)
		_testAssert.NoDirExists(paths[i])
	}
	// Simulate a background initializer that publishes only after cleanup.
	latePath := filepath.Join(f.cfg.Data.DataDir, "late-owned-query")
	_testRequire.NoError(os.MkdirAll(latePath, 0700))
	late, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: latePath, OwnTempDirectory: true})
	_testRequire.NoError(err)
	server.SetAnalyticsEngine(late, api.AnalyticsModeDuckDB)
	_, err = late.QuerySQL(t.Context(), `SELECT 1`)
	_testRequire.Error(err)
	_testAssert.NoDirExists(latePath)
	_testAssert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode())

	stalePath := filepath.Join(f.cfg.Data.DataDir, "stale-generation-query")
	_testRequire.NoError(os.MkdirAll(stalePath, 0700))
	stale, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: stalePath, OwnTempDirectory: true})
	_testRequire.NoError(err)
	_testAssert.False(server.SetAnalyticsEngineForGeneration(staleGeneration, stale, api.AnalyticsModeDuckDB),
		"a worker opened before cleanup cannot replace the fresh generation")
	_, err = stale.QuerySQL(t.Context(), `SELECT 1`)
	_testRequire.Error(err)
	_testAssert.NoDirExists(stalePath)
}

func TestXRemovalDuringSQLFallbackInitializationReopensFreshAnalytics(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("DuckDB startup initialization applies to the SQLite cache")
	}
	f.cfg.Analytics.Engine = config.AnalyticsEngineAuto
	f.cfg.Analytics.AutoBuildCache = true
	_, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: f.source, OwnerID: "1001", PostID: "103", AuthorID: "1003", ConversationID: "103",
		EditHistory: []string{"103"}, Text: "synthetic cache invalidation", CreatedAt: time.Now().UTC(),
		FetchedAt: time.Now().UTC(), Availability: "public",
	})
	require.NoError(err)
	assert.True(cacheNeedsBuild(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir()).NeedsBuild,
		"the startup initializer must have work to finish after cleanup")
	stubBuildCacheSubprocess(t, func(ctx context.Context, _ bool) error {
		lock, err := acquireCacheBuildLock(ctx, f.cfg.AnalyticsDir())
		if err != nil {
			return err
		}
		defer func() { _ = lock.Unlock() }()
		_, err = buildCacheLocked(ctx, f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true, false)
		return err
	})

	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	initial := query.NewEngine(f.st.DB(), false)
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)},
		Engine: initial, AnalyticsMode: api.AnalyticsModeSQLFallback, Logger: slog.New(slog.DiscardHandler),
		ContentReadGuard: cleaner.AcquireRead,
	})
	cleaner.SetServer(server)
	staleGeneration := server.ContentAnalyticsGeneration()
	tracker := &blockingAnalyticsInitTracker{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(tracker.release) }) }
	initializer := startDaemonAnalyticsInitializer(
		t.Context(), f.cfg, f.st, startupCacheBuildIntentNone, server, nil, tracker,
	)
	t.Cleanup(func() {
		release()
		waitCtx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
		defer cancel()
		require.True(initializer.WaitContext(waitCtx), "analytics initializer must finish before engines close")
		require.NoError(closeDaemonAnalyticsEngines(server, initial, initializer))
	})
	select {
	case <-tracker.started:
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("analytics initializer did not reach its tracked startup boundary")
	}
	assert.Equal(api.AnalyticsModeSQLFallback, server.AnalyticsMode())
	assert.True(server.AnalyticsInitializationActive())
	assert.Equal(staleGeneration, server.ContentAnalyticsGeneration())

	removeCtx, cancelRemove := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelRemove()
	_, err = cleaner.RemovePost(removeCtx, f.source, "100", "removed")
	require.NoError(err)
	assert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"cleanup must reopen analytics when startup was serving SQL fallback")

	explore := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(
		fmt.Sprintf(`{"filters":[{"dimension":"source","values":["%d"]}],"limit":1}`, f.source),
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	server.Router().ServeHTTP(explore, request)
	require.Equal(http.StatusOK, explore.Code, explore.Body.String())

	release()
	waitCtx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer cancel()
	require.True(initializer.WaitContext(waitCtx), "stale initializer must finish after cleanup")
	require.NoError(initializer.Err())
	assert.False(initializer.Swapped(), "the initializer captured before cleanup must be rejected")
	assert.False(server.AnalyticsInitializationActive())
	assert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"rejecting a stale initializer must leave the cleanup-installed engine available")

	afterInit := httptest.NewRecorder()
	afterInitRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(
		fmt.Sprintf(`{"filters":[{"dimension":"source","values":["%d"]}],"limit":1}`, f.source),
	))
	afterInitRequest.Header.Set("Content-Type", "application/json")
	afterInitRequest.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	server.Router().ServeHTTP(afterInit, afterInitRequest)
	require.Equal(http.StatusOK, afterInit.Code, afterInit.Body.String())
}

func TestXRemovalBetweenAnalyticsSnapshotAndRetirementReopensFreshAnalytics(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("DuckDB startup initialization applies to the SQLite cache")
	}
	f.cfg.Analytics.Engine = config.AnalyticsEngineAuto
	f.cfg.Analytics.AutoBuildCache = true
	_, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: f.source, OwnerID: "1001", PostID: "104", AuthorID: "1003", ConversationID: "104",
		EditHistory: []string{"104"}, Text: "synthetic interleaved cache invalidation", CreatedAt: time.Now().UTC(),
		FetchedAt: time.Now().UTC(), Availability: "public",
	})
	require.NoError(err)
	assert.True(cacheNeedsBuild(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir()).NeedsBuild,
		"the startup initializer must have work to finish after cleanup")
	stubBuildCacheSubprocess(t, func(ctx context.Context, _ bool) error {
		lock, err := acquireCacheBuildLock(ctx, f.cfg.AnalyticsDir())
		if err != nil {
			return err
		}
		defer func() { _ = lock.Unlock() }()
		_, err = buildCacheLocked(ctx, f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true, false)
		return err
	})

	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	initial := query.NewEngine(f.st.DB(), false)
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)},
		Engine: initial, AnalyticsMode: api.AnalyticsModeSQLFallback, Logger: slog.New(slog.DiscardHandler),
		ContentReadGuard: cleaner.AcquireRead,
	})
	cleaner.SetServer(server)
	tracker := &blockingAnalyticsInitTracker{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(tracker.release) }) }
	var initializer *daemonAnalyticsInitHandle
	t.Cleanup(func() {
		release()
		if initializer == nil {
			return
		}
		waitCtx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
		defer cancel()
		require.True(initializer.WaitContext(waitCtx), "analytics initializer must finish before engines close")
		require.NoError(closeDaemonAnalyticsEngines(server, initial, initializer))
	})

	cleaner.beforeAnalyticsRetire = func() error {
		assert.Equal(api.AnalyticsModeSQLFallback, server.AnalyticsMode())
		assert.False(server.AnalyticsInitializationActive(),
			"cleanup must first observe the SQL-fallback startup state")
		initializer = startDaemonAnalyticsInitializer(
			t.Context(), f.cfg, f.st, startupCacheBuildIntentNone, server, nil, tracker,
		)
		select {
		case <-tracker.started:
			return nil
		case <-time.After(serveLifecycleTestTimeout):
			return errors.New("analytics initializer did not reach its tracked startup boundary")
		}
	}

	removeCtx, cancelRemove := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelRemove()
	_, err = cleaner.RemovePost(removeCtx, f.source, "100", "removed")
	require.NoError(err)
	require.NotNil(initializer)
	assert.True(server.AnalyticsInitializationActive())

	explore := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(
		fmt.Sprintf(`{"filters":[{"dimension":"source","values":["%d"]}],"limit":1}`, f.source),
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	server.Router().ServeHTTP(explore, request)
	assert.Equal(http.StatusOK, explore.Code, explore.Body.String(),
		"cleanup must reopen Explore if initialization begins after its initial state sample")

	release()
	waitCtx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer cancel()
	require.True(initializer.WaitContext(waitCtx), "stale initializer must finish after cleanup")
	require.NoError(initializer.Err())
	assert.False(initializer.Swapped(), "the interleaved initializer must be rejected after cleanup advances its generation")
	assert.False(server.AnalyticsInitializationActive())
	assert.Equal(api.AnalyticsModeDuckDB, server.AnalyticsMode(),
		"rejection of the stale initializer must preserve the cleanup-installed engine")

	afterInit := httptest.NewRecorder()
	afterInitRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/explore", strings.NewReader(
		fmt.Sprintf(`{"filters":[{"dimension":"source","values":["%d"]}],"limit":1}`, f.source),
	))
	afterInitRequest.Header.Set("Content-Type", "application/json")
	afterInitRequest.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	server.Router().ServeHTTP(afterInit, afterInitRequest)
	require.Equal(http.StatusOK, afterInit.Code, afterInit.Body.String())
}

func TestXRemovalResumesEachDerivedFailureWithoutProvider(t *testing.T) {
	for _, phase := range []string{"store", "vectors", "publication"} {
		t.Run(phase, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXCleanupFixture(t)
			cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
			fail := func() error { return fmt.Errorf("synthetic %s interruption", phase) }
			switch phase {
			case "store":
				cleaner.afterStore = fail
			case "vectors":
				cleaner.afterVectors = fail
			case "publication":
				cleaner.beforePublish = fail
			}
			_, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
			_testRequire.ErrorContains(err, "synthetic")
			pending, err := f.st.XCleanupPendingContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.True(pending)
			restarted := NewXContentCleanup(f.st, f.cfg, f.backend)
			_testRequire.NoError(restarted.Resume(t.Context()))
			pending, err = f.st.XCleanupPendingContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.False(pending)
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				_testRequire.NoError(err)
				_testAssert.Zero(stats.EmbeddingCount)
			}
		})
	}
}

func TestXRemovalResumeBatchesPendingVectorAndCacheCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"103", "104"} {
		_, err := f.st.PersistXPostContext(ctx, store.XPersistPostInput{
			SourceID: f.source, OwnerID: "1001", PostID: id, AuthorID: "1002",
			ConversationID: "100", EditHistory: []string{id},
			Text: "synthetic batch removal " + id, CreatedAt: at, FetchedAt: at,
			Availability: "public", Raw: []byte(`{"post":{"id":"` + id + `"}}`),
		})
		_testRequire.NoError(err)
	}
	execution, err := f.st.AcquireSyncExecutionContext(ctx, f.source)
	_testRequire.NoError(err)
	for _, id := range []string{"102", "103", "104"} {
		_, err := f.st.RemoveXPostContentContext(ctx, f.source, id, "removed")
		_testRequire.NoError(err)
	}
	_testRequire.NoError(execution.Release())
	if !f.st.IsPostgreSQL() {
		_, err = buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		_testRequire.NoError(err)
	}

	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	var storePasses, vectorPasses, publications int
	cleaner.afterStore = func() error { storePasses++; return nil }
	cleaner.afterVectors = func() error { vectorPasses++; return nil }
	cleaner.beforePublish = func() error { publications++; return nil }
	_testRequire.NoError(cleaner.Resume(ctx))
	_testAssert.Equal(1, storePasses, "all pending aliases share one store cleanup pass")
	_testAssert.Equal(1, vectorPasses, "vector deletion runs once for the combined message set")
	_testAssert.Equal(1, publications, "the existing cache is rebuilt once for the pending batch")
	pending, err := f.st.XCleanupPendingContext(ctx)
	_testRequire.NoError(err)
	_testAssert.False(pending)
	if !f.st.IsPostgreSQL() {
		readiness, err := query.InspectCacheReadiness(f.cfg.AnalyticsDir())
		_testRequire.NoError(err)
		_testAssert.Equal(query.CacheReady, readiness)
	}
}

func TestXRemovalStartupFence(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: false}}
	f.cfg.X.Accounts = []config.XAccountConfig{{UserID: "1001", MonthlyBudgetMicros: 0}}
	generation := int64(2)
	credentialError := "invalid_grant"
	_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError})
	_testRequire.NoError(err)
	// #nosec G702 -- Executes the test binary with fixed arguments and no shell.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run", "^TestXContentSpillCrashHelper$")
	child.Env = append(os.Environ(), "MSGVAULT_TEST_X_SPILL_HOME="+f.cfg.HomeDir)
	output, err := child.CombinedOutput()
	_testRequire.NoError(err, string(output))
	pathBytes, err := os.ReadFile(filepath.Join(f.cfg.HomeDir, "crash-spill-path"))
	_testRequire.NoError(err)
	crashSpill := string(pathBytes)
	_testRequire.DirExists(crashSpill)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	cleaner.afterStore = func() error { return errors.New("synthetic process interruption") }
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.ErrorContains(err, "interruption")
	_testRequire.NoError(f.backend.Close())
	_testRequire.NoError(f.st.Close())
	reopened, err := store.Open(f.cfg.DatabaseDSN())
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, reopened.Close()) }()
	resumed := NewXContentCleanup(reopened, f.cfg, nil)
	// This is the exact startup hook called before analytics/workers/readers.
	_, err = resumed.AcquireRead(t.Context())
	_testRequire.Error(err)
	_testRequire.NoError(resumed.ResumeAtStartup(t.Context()))
	_testAssert.NoDirExists(crashSpill, "startup must erase spill files left by a lost process before reporting cleanup complete")
	release, err := resumed.AcquireRead(t.Context())
	_testRequire.NoError(err)
	release()
	backend, closeBackend, err := resumed.cleanupBackend(t.Context())
	_testRequire.NoError(err)
	_testRequire.NotNil(backend)
	defer closeBackend()
	for _, gen := range f.generations {
		stats, err := backend.Stats(t.Context(), gen)
		_testRequire.NoError(err)
		_testAssert.Zero(stats.EmbeddingCount)
	}
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: reopened, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(reopened.DB(), reopened.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: resumed.AcquireRead})
	host := httptest.NewServer(apiServer.Router())
	defer host.Close()
	for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", f.message), fmt.Sprintf("/api/v1/cli/message/raw?id=%d", f.message), "/api/v1/messages?q=constellation"} {
		response, err := xTestHTTPGet(t, host.Client(), host.URL+path)
		_testRequire.NoError(err)
		data, err := io.ReadAll(response.Body)
		_testRequire.NoError(err)
		_testRequire.NoError(response.Body.Close())
		_testAssert.NotEqual(http.StatusServiceUnavailable, response.StatusCode)
		_testAssert.NotContains(string(data), "constellation secret")
	}
	_testAssert.NoDirExists(f.cfg.TokensDir())
}

type xBlockingContentWriter struct {
	*httptest.ResponseRecorder

	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *xBlockingContentWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	n, err := w.ResponseRecorder.Write(data)
	if err != nil {
		return n, fmt.Errorf("write test response: %w", err)
	}
	return n, nil
}
func TestXRemovalDrainsAnAlreadyLoadedHTTPResponse(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	writer := &xBlockingContentWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-writer.release:
		default:
			close(writer.release)
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Router().ServeHTTP(writer, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", f.message), nil))
	}()
	select {
	case <-writer.entered:
	case <-time.After(30 * time.Second):
		_testRequire.FailNow("detail response did not reach the write boundary")
	}
	// Probe the real filesystem fence while bytes already loaded by the actual
	// detail handler are waiting in Write. No scheduling-speed assertion is used.
	probe := flock.New(filepath.Clean(f.cfg.AnalyticsDir()) + ".x-content.lock")
	locked, err := probe.TryLock()
	_testRequire.NoError(err)
	_testAssert.False(locked)
	_testRequire.NoError(probe.Close())
	close(writer.release)
	<-done
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", f.message), nil))
	_testAssert.NotContains(response.Body.String(), "constellation secret")
}

func TestXRemovalSemanticRouteUsesActualDeletedVectors(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	at := time.Now().UTC()
	seed, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "300", AuthorID: "1003", ConversationID: "300", EditHistory: []string{"300"}, Text: "independent seed", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1003", Username: "independent_author", DisplayName: "Independent Author", FetchedAt: at}})
	_testRequire.NoError(err)
	_testRequire.NoError(f.backend.Upsert(t.Context(), f.generations[1], []vector.Chunk{{MessageID: seed.MessageID, Vector: []float32{1, 0, 0}}}))
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Backend: f.backend, VectorCfg: f.cfg.Vector, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	read := func() []int64 {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/search/similar?message_id=%d", seed.MessageID), nil))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result struct {
			Messages []struct {
				ID int64 `json:"id"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		var ids []int64
		for _, message := range result.Messages {
			ids = append(ids, message.ID)
		}
		return ids
	}
	_testAssert.Contains(read(), f.message)
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	_testAssert.NotContains(read(), f.message)
	body, err := f.st.GetMessageBodyText(t.Context(), seed.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal("independent seed", body)
}

func TestXRemovalCachePublicationRace(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("Parquet publication applies to SQLite")
	}
	_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	_testRequire.NoError(err)
	captured := make(chan struct{})
	releaseBuild := make(chan struct{})
	var once sync.Once
	buildCacheAfterSnapshotHook = func() { once.Do(func() { close(captured); <-releaseBuild }) }
	t.Cleanup(func() {
		buildCacheAfterSnapshotHook = nil
		select {
		case <-releaseBuild:
		default:
			close(releaseBuild)
		}
	})
	built := make(chan error, 1)
	go func() {
		_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		built <- err
	}()
	select {
	case <-captured:
	case <-time.After(30 * time.Second):
		_testRequire.FailNow("builder did not capture its real archive snapshot")
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	removed := make(chan error, 1)
	go func() { _, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed"); removed <- err }()
	// Observe actual exclusive cleanup ownership while the old builder remains
	// paused. Timeout bounds this lock transition, not runner throughput.
	probe := flock.New(filepath.Clean(f.cfg.AnalyticsDir()) + ".x-content.lock")
	defer func() { assert.NoError(t, probe.Close()) }()
	_testRequire.Eventually(func() bool {
		ok, err := probe.TryRLock()
		if err != nil {
			return false
		}
		if ok {
			_ = probe.Unlock()
		}
		return !ok
	}, 30*time.Second, 20*time.Millisecond)
	body, err := f.st.GetMessageBodyText(t.Context(), f.message)
	_testRequire.NoError(err)
	_testAssert.Contains(body, "constellation")
	close(releaseBuild)
	_testRequire.NoError(<-built)
	_testRequire.NoError(<-removed)
	fresh, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, fresh.Close()) }()
	decoded, err := fresh.QuerySQL(t.Context(), `SELECT * FROM messages`)
	_testRequire.NoError(err)
	encoded, err := json.Marshal(decoded)
	_testRequire.NoError(err)
	_testAssert.NotContains(string(encoded), "constellation")
}

func TestXRemovalCallerHandlerPendingFence(t *testing.T) {
	_testRequire := require.New(t)

	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	_testRequire.NoError(err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	path := fmt.Sprintf("/api/v1/messages/%d", f.message)
	response := httptest.NewRecorder()
	server.Handler(nil).ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	_testRequire.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	_testRequire.NoError(cleaner.Resume(t.Context()))
	response = httptest.NewRecorder()
	server.Handler(nil).ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	_testRequire.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "constellation secret")
}

func TestXRemovalRetriesFailedOwnedSpillCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission failure requires an unprivileged process")
	}
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("DuckDB spill applies to SQLite")
	}
	_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	_testRequire.NoError(err)
	parent := filepath.Join(f.cfg.Data.DataDir, "restricted-spill-parent")
	path := filepath.Join(parent, "engine-owned")
	_testRequire.NoError(os.MkdirAll(path, 0700))
	engine, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: path, OwnTempDirectory: true})
	_testRequire.NoError(err)
	t.Cleanup(func() { _ = os.Chmod(parent, 0700); _ = os.Chmod(path, 0700); assert.NoError(t, engine.Close()) })
	_testRequire.

		// A pre-existing owned file makes RemoveAll encounter a real permission
		// failure. The production DuckDB engine owns and closes this directory.
		NoError(os.WriteFile(filepath.Join(path, "owned-spill-fixture"), []byte("fixture constellation bytes"), 0600))
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: engine, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	_testRequire.NoError(os.Chmod(path, 0500))
	_testRequire.NoError(os.Chmod(parent, 0500))
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.Error(err)
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.True(pending)
	_testAssert.DirExists(path)
	_testRequire.NoError(os.Chmod(parent, 0700))
	_testRequire.NoError(os.Chmod(path, 0700))
	_testRequire.NoError(cleaner.Resume(t.Context()))
	_testAssert.NoDirExists(path, "a failed Close must remain tracked until a later successful retry")
	pending, err = f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending)
}

func TestXContentSpillCrashHelper(t *testing.T) {
	_testRequire := require.New(t)

	home := os.Getenv("MSGVAULT_TEST_X_SPILL_HOME")
	if home == "" {
		return
	}
	parent, err := query.PrepareDaemonSpillDir(home)
	_testRequire.NoError(err)
	//nolint:usetesting // The child must leave this crash spill for the parent process to recover.
	path, err := os.MkdirTemp(parent, "engine-crash-")
	_testRequire.NoError(err)
	engine, err := query.NewDuckDBEngine(t.Context(), "", "", nil, query.DuckDBOptions{TempDirectory: path, OwnTempDirectory: true, Threads: 2})
	_testRequire.NoError(err)
	_, err = engine.QuerySQL(t.Context(), `SELECT 'fixture constellation bytes'`)
	_testRequire.NoError(err)
	_testRequire.NoError(os.WriteFile(filepath.Join(path, "owned-spill-fixture"), []byte("fixture constellation bytes"), 0600))
	_testRequire.NoError(os.WriteFile(filepath.Join(home, "crash-spill-path"), []byte(path), 0600))
	// Simulate process loss: deliberately bypass the production engine Close.
	os.Exit(0)
}

func TestXRemovalRebuildsCachePublishedWhileWaitingForBuilder(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite Parquet publication boundary")
	}
	cfg := xTestConfig(t)
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	at := time.Now().UTC().Add(-time.Hour)
	_, err = st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100"}, Text: "synthetic cache constellation", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: at}})
	_testRequire.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	builderReady, allowBuilder := make(chan struct{}), make(chan struct{})
	cleanupReady, allowCleanup := make(chan struct{}), make(chan struct{})
	var releaseBuilderOnce, releaseCleanupOnce, publicationOnce sync.Once
	releaseBuilder := func() { releaseBuilderOnce.Do(func() { close(allowBuilder) }) }
	releaseCleanup := func() { releaseCleanupOnce.Do(func() { close(allowCleanup) }) }
	previousHook := buildCacheBeforePublicationMovesHook
	buildCacheBeforePublicationMovesHook = func() error {
		first := false
		publicationOnce.Do(func() { first = true })
		if !first {
			return nil
		}
		close(builderReady)
		select {
		case <-allowBuilder:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { buildCacheBeforePublicationMovesHook = previousHook })
	var wg sync.WaitGroup
	t.Cleanup(func() { releaseBuilder(); releaseCleanup(); cancel(); wg.Wait() })
	built, removed := make(chan error, 1), make(chan error, 1)
	wg.Go(func() {
		_, err := buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
		built <- err
	})
	select {
	case <-builderReady:
	case err := <-built:
		_testRequire.NoError(err)
		_testRequire.FailNow("builder completed before its publication barrier")
	case <-ctx.Done():
		_testRequire.NoError(ctx.Err())
	}
	_testRequire.NoDirExists(cfg.AnalyticsDir(), "the real builder has staged but has not published")
	cleaner := NewXContentCleanup(st, cfg, nil)
	cleaner.beforeCacheLock = func() error {
		close(cleanupReady)
		select {
		case <-allowCleanup:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wg.Go(func() {
		_, err := cleaner.RemovePost(ctx, source.ID, "100", "removed")
		removed <- err
	})
	select {
	case <-cleanupReady:
	case err := <-removed:
		_testRequire.NoError(err)
		_testRequire.FailNow("cleanup completed before its builder-lock barrier")
	case <-ctx.Done():
		_testRequire.NoError(ctx.Err())
	}
	// The in-flight builder publishes while cleanup is about to wait for its
	// lock. The production stat must observe that generation after draining it.
	releaseBuilder()
	select {
	case err := <-built:
		_testRequire.NoError(err)
	case <-ctx.Done():
		_testRequire.NoError(ctx.Err())
	}
	readiness, err := query.InspectCacheReadiness(cfg.AnalyticsDir())
	_testRequire.NoError(err)
	_testRequire.Equal(query.CacheReady, readiness)
	before, err := query.NewDuckDBEngine(t.Context(), cfg.AnalyticsDir(), cfg.DatabaseDSN(), st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	_testRequire.NoError(err)
	rows, err := before.QuerySQL(ctx, `SELECT * FROM messages`)
	_testRequire.NoError(err)
	_testRequire.Len(rows.Rows, 1)
	_testRequire.NoError(before.Close())
	releaseCleanup()
	select {
	case err := <-removed:
		_testRequire.NoError(err)
	case <-ctx.Done():
		_testRequire.NoError(ctx.Err())
	}
	readiness, err = query.InspectCacheReadiness(cfg.AnalyticsDir())
	_testRequire.NoError(err)
	_testRequire.Equal(query.CacheReady, readiness, "a cache published by the drained builder must be rebuilt after removal")
	fresh, err := query.NewDuckDBEngine(t.Context(), cfg.AnalyticsDir(), cfg.DatabaseDSN(), st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, fresh.Close()) }()
	rows, err = fresh.QuerySQL(ctx, `SELECT * FROM messages`)
	_testRequire.NoError(err)
	encoded, err := json.Marshal(rows)
	_testRequire.NoError(err)
	_testAssert.NotContains(string(encoded), "synthetic cache constellation")
	pending, err := st.XCleanupPendingContext(ctx)
	_testRequire.NoError(err)
	_testAssert.False(pending)
}

func TestXRemovalInvalidatesOldMaterializedSearch(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite Parquet reader")
	}
	f := newXCleanupFixture(t)
	_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	_testRequire.NoError(err)
	duck, err := query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, duck.Close()) }()
	page, err := duck.SearchFastWithStats(t.Context(), &search.Query{TextTerms: []string{"constellation"}}, "constellation", query.MessageFilter{}, query.ViewNoStats, 10, 0)
	_testRequire.NoError(err)
	_testRequire.Equal(int64(1), page.TotalCount)
	// The first real search materializes this table in the already-open engine.
	// QuerySQL is owner-authorized read-only SQL, so cached data must be purged
	// even when no subsequent paginated search would otherwise replace it.
	before, err := duck.QuerySQL(t.Context(), `SELECT snippet,from_name FROM _search_matches_1`)
	_testRequire.NoError(err)
	encoded, err := json.Marshal(before)
	_testRequire.NoError(err)
	_testRequire.Contains(string(encoded), "erased constellation secret")
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	work, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	_testRequire.NoError(err)
	_testRequire.False(work.Pending)
	after, err := duck.QuerySQL(t.Context(), `SELECT snippet,from_name FROM _search_matches_1`)
	if err == nil {
		encoded, err = json.Marshal(after)
		_testRequire.NoError(err)
		_testAssert.NotContains(string(encoded), "erased constellation secret")
		_testAssert.NotContains(string(encoded), "Erased Label")
	}
}
