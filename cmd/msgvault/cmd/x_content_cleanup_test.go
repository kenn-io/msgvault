package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
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

func newXCleanupFixture(t *testing.T) *xCleanupFixture {
	t.Helper()
	f := &xCleanupFixture{cfg: xTestConfig(t)}
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		f.st = testutil.NewTestStore(t)
		// Reopening helpers must stay in this fixture's actual clone/schema.
		var database, schema string
		require.NoError(t, f.st.DB().QueryRow(`SELECT current_database(),current_schema()`).Scan(&database, &schema))
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
	for i := 0; i < 3; i++ {
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
	f := newXCleanupFixture(t)
	var engine query.Engine = query.NewEngine(f.st.DB(), f.st.IsPostgreSQL())
	var duck *query.DuckDBEngine
	if !f.st.IsPostgreSQL() {
		_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		require.NoError(t, err)
		duck, err = query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		require.NoError(t, err)
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
		response, err := host.Client().Get(host.URL + path)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode, string(data))
		assert.Contains(t, string(data), "erased constellation secret", path)
	}
	var stale *cacheStaging
	if duck != nil {
		staged, stageErr := newCacheStaging(f.cfg.AnalyticsDir())
		require.NoError(t, stageErr)
		stale = staged
		require.NoError(t, os.CopyFS(stale.root, os.DirFS(f.cfg.AnalyticsDir())))
	}
	work, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	if stale != nil {
		assert.NoDirExists(t, stale.root)
	}
	assert.False(t, work.Pending)
	for _, gen := range f.generations {
		stats, err := f.backend.Stats(t.Context(), gen)
		require.NoError(t, err)
		assert.Zero(t, stats.EmbeddingCount)
	}
	for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", f.message), fmt.Sprintf("/api/v1/cli/message/raw?id=%d", f.message), "/api/v1/search?q=constellation"} {
		response, err := http.Get(host.URL + path)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		if strings.Contains(path, "/raw?") {
			assert.Equal(t, http.StatusNotFound, response.StatusCode, string(body))
		} else {
			assert.Equal(t, http.StatusOK, response.StatusCode, string(body))
		}
		if strings.Contains(path, "/search?") {
			var result struct {
				Total    int              `json:"total"`
				Messages []jsontext.Value `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(body, &result))
			assert.Zero(t, result.Total)
			assert.Empty(t, result.Messages)
		}
		assert.NotContains(t, string(body), "erased constellation secret")
		assert.NotContains(t, string(body), "erased_handle")
		assert.NotContains(t, string(body), "Erased Label")
	}
	if duck != nil {
		_, err := duck.QuerySQL(t.Context(), `SELECT 1`)
		require.Error(t, err)
		fresh, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		require.NoError(t, err)
		defer func() { assert.NoError(t, fresh.Close()) }()
		decoded, err := fresh.QuerySQL(t.Context(), `SELECT * FROM read_parquet('`+filepath.ToSlash(filepath.Join(f.cfg.AnalyticsDir(), "messages", "**", "*.parquet"))+`')`)
		require.NoError(t, err)
		bytes, err := json.Marshal(decoded)
		require.NoError(t, err)
		assert.NotContains(t, string(bytes), "constellation")
		assert.NotContains(t, string(bytes), "Erased Label")
		require.NoError(t, filepath.WalkDir(f.cfg.AnalyticsDir(), func(path string, entry os.DirEntry, walkErr error) error {
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
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
}

func TestXRemovalPendingFenceOnRealDetailRoute(t *testing.T) {
	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}
	host := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead}).Router())
	t.Cleanup(host.Close)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	require.NoError(t, err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	response, err := http.Get(fmt.Sprintf("%s/api/v1/messages/%d", host.URL, f.message))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	// A restarted cleaner resumes without tokens, budgets, schedules or API.
	restarted := NewXContentCleanup(f.st, f.cfg, f.backend)
	require.NoError(t, restarted.Resume(context.Background()))
	response, err = http.Get(fmt.Sprintf("%s/api/v1/messages/%d", host.URL, f.message))
	require.NoError(t, err)
	bytes, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.NotEqual(t, http.StatusServiceUnavailable, response.StatusCode)
	assert.False(t, strings.Contains(string(bytes), "constellation"))
}

func TestXRemovalRejectsStaleVectorUpserts(t *testing.T) {
	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	_, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	// Models need not run: this is the previously read, now stale worker result.
	for _, gen := range f.generations {
		err := f.backend.Upsert(t.Context(), gen, []vector.Chunk{{MessageID: f.message, Vector: []float32{1, 0, 0}}})
		if gen == f.generations[0] {
			require.ErrorIs(t, err, vector.ErrGenerationRetired)
		} else {
			require.NoError(t, err)
		}
		stats, err := f.backend.Stats(t.Context(), gen)
		require.NoError(t, err)
		assert.Zero(t, stats.EmbeddingCount)
	}
}

func TestXRemovalAlreadyOpenParquetReaderRejectsPending(t *testing.T) {
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("Parquet is the SQLite analytics path")
	}
	_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	require.NoError(t, err)
	duck, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	require.NoError(t, err)
	defer func() { assert.NoError(t, duck.Close()) }()
	_, err = duck.QuerySQL(t.Context(), `SELECT count(*) FROM messages`)
	require.NoError(t, err)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	require.NoError(t, err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	_, err = duck.QuerySQL(t.Context(), `SELECT * FROM messages`)
	require.Error(t, err)
}

func TestXRemovalClosesRetiredAnalyticsAndOwnedTempDirectories(t *testing.T) {
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("DuckDB handles apply to the SQLite cache")
	}
	_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	require.NoError(t, err)
	var engines []*query.DuckDBEngine
	var paths []string
	for i := 0; i < 2; i++ {
		path := filepath.Join(f.cfg.Data.DataDir, fmt.Sprintf("owned-query-%d", i))
		require.NoError(t, os.MkdirAll(path, 0700))
		engine, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: path, OwnTempDirectory: true})
		require.NoError(t, err)
		engines = append(engines, engine)
		paths = append(paths, path)
		t.Cleanup(func() { assert.NoError(t, engine.Close()) })
		_, err = engine.QuerySQL(t.Context(), `SELECT * FROM messages`)
		require.NoError(t, err)
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: engines[0], Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	server.SetAnalyticsEngine(engines[1], api.AnalyticsModeDuckDB)
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	for i, engine := range engines {
		_, err := engine.QuerySQL(t.Context(), `SELECT 1`)
		assert.Error(t, err)
		assert.NoDirExists(t, paths[i])
	}
	// Simulate a background initializer that publishes only after cleanup.
	latePath := filepath.Join(f.cfg.Data.DataDir, "late-owned-query")
	require.NoError(t, os.MkdirAll(latePath, 0700))
	late, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: latePath, OwnTempDirectory: true})
	require.NoError(t, err)
	server.SetAnalyticsEngine(late, api.AnalyticsModeDuckDB)
	_, err = late.QuerySQL(t.Context(), `SELECT 1`)
	assert.Error(t, err)
	assert.NoDirExists(t, latePath)
	assert.Equal(t, api.AnalyticsModeSQLFallback, server.AnalyticsMode())
}

func TestXRemovalResumesEachDerivedFailureWithoutProvider(t *testing.T) {
	for _, phase := range []string{"store", "vectors", "publication"} {
		t.Run(phase, func(t *testing.T) {
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
			require.ErrorContains(t, err, "synthetic")
			pending, err := f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.True(t, pending)
			restarted := NewXContentCleanup(f.st, f.cfg, f.backend)
			require.NoError(t, restarted.Resume(t.Context()))
			pending, err = f.st.XCleanupPendingContext(t.Context())
			require.NoError(t, err)
			assert.False(t, pending)
			for _, gen := range f.generations {
				stats, err := f.backend.Stats(t.Context(), gen)
				require.NoError(t, err)
				assert.Zero(t, stats.EmbeddingCount)
			}
		})
	}
}

func TestXRemovalStartupFence(t *testing.T) {
	f := newXCleanupFixture(t)
	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: false}}
	f.cfg.X.Accounts = []config.XAccountConfig{{UserID: "1001", MonthlyBudgetMicros: 0}}
	generation := int64(2)
	credentialError := "invalid_grant"
	_, err := f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError})
	require.NoError(t, err)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run", "^TestXContentSpillCrashHelper$")
	child.Env = append(os.Environ(), "MSGVAULT_TEST_X_SPILL_HOME="+f.cfg.HomeDir)
	output, err := child.CombinedOutput()
	require.NoError(t, err, string(output))
	pathBytes, err := os.ReadFile(filepath.Join(f.cfg.HomeDir, "crash-spill-path"))
	require.NoError(t, err)
	crashSpill := string(pathBytes)
	require.DirExists(t, crashSpill)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	cleaner.afterStore = func() error { return fmt.Errorf("synthetic process interruption") }
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.ErrorContains(t, err, "interruption")
	require.NoError(t, f.backend.Close())
	require.NoError(t, f.st.Close())
	reopened, err := store.Open(f.cfg.DatabaseDSN())
	require.NoError(t, err)
	defer func() { assert.NoError(t, reopened.Close()) }()
	resumed := NewXContentCleanup(reopened, f.cfg, nil)
	// This is the exact startup hook called before analytics/workers/readers.
	_, err = resumed.AcquireRead(t.Context())
	require.Error(t, err)
	require.NoError(t, resumed.ResumeAtStartup(t.Context()))
	assert.NoDirExists(t, crashSpill, "startup must erase spill files left by a lost process before reporting cleanup complete")
	release, err := resumed.AcquireRead(t.Context())
	require.NoError(t, err)
	release()
	backend, closeBackend, err := resumed.cleanupBackend(t.Context())
	require.NoError(t, err)
	require.NotNil(t, backend)
	defer closeBackend()
	for _, gen := range f.generations {
		stats, err := backend.Stats(t.Context(), gen)
		require.NoError(t, err)
		assert.Zero(t, stats.EmbeddingCount)
	}
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: reopened, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(reopened.DB(), reopened.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: resumed.AcquireRead})
	host := httptest.NewServer(apiServer.Router())
	defer host.Close()
	for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", f.message), fmt.Sprintf("/api/v1/cli/message/raw?id=%d", f.message), "/api/v1/messages?q=constellation"} {
		response, err := host.Client().Get(host.URL + path)
		require.NoError(t, err)
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.NotEqual(t, http.StatusServiceUnavailable, response.StatusCode)
		assert.NotContains(t, string(data), "constellation secret")
	}
	assert.NoDirExists(t, f.cfg.TokensDir())
}

type xBlockingContentWriter struct {
	once sync.Once
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *xBlockingContentWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(data)
}
func TestXRemovalDrainsAnAlreadyLoadedHTTPResponse(t *testing.T) {
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
		server.Router().ServeHTTP(writer, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", f.message), nil))
	}()
	select {
	case <-writer.entered:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "detail response did not reach the write boundary")
	}
	// Probe the real filesystem fence while bytes already loaded by the actual
	// detail handler are waiting in Write. No scheduling-speed assertion is used.
	probe := flock.New(filepath.Clean(f.cfg.AnalyticsDir()) + ".x-content.lock")
	locked, err := probe.TryLock()
	require.NoError(t, err)
	assert.False(t, locked)
	require.NoError(t, probe.Close())
	close(writer.release)
	<-done
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", f.message), nil))
	assert.NotContains(t, response.Body.String(), "constellation secret")
}

func TestXRemovalSemanticRouteUsesActualDeletedVectors(t *testing.T) {
	f := newXCleanupFixture(t)
	at := time.Now().UTC()
	seed, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source, OwnerID: "1001", PostID: "300", AuthorID: "1003", ConversationID: "300", EditHistory: []string{"300"}, Text: "independent seed", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1003", Username: "independent_author", DisplayName: "Independent Author", FetchedAt: at}})
	require.NoError(t, err)
	require.NoError(t, f.backend.Upsert(t.Context(), f.generations[1], []vector.Chunk{{MessageID: seed.MessageID, Vector: []float32{1, 0, 0}}}))
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Backend: f.backend, VectorCfg: f.cfg.Vector, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	read := func() []int64 {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/search/similar?message_id=%d", seed.MessageID), nil))
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
	assert.Contains(t, read(), f.message)
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	assert.NotContains(t, read(), f.message)
	body, err := f.st.GetMessageBodyText(seed.MessageID)
	require.NoError(t, err)
	assert.Equal(t, "independent seed", body)
}

func TestXRemovalCachePublicationRace(t *testing.T) {
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("Parquet publication applies to SQLite")
	}
	_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	require.NoError(t, err)
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
	go func() { _, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true); built <- err }()
	select {
	case <-captured:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "builder did not capture its real archive snapshot")
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	removed := make(chan error, 1)
	go func() { _, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed"); removed <- err }()
	// Observe actual exclusive cleanup ownership while the old builder remains
	// paused. Timeout bounds this lock transition, not runner throughput.
	probe := flock.New(filepath.Clean(f.cfg.AnalyticsDir()) + ".x-content.lock")
	defer func() { assert.NoError(t, probe.Close()) }()
	require.Eventually(t, func() bool {
		ok, err := probe.TryRLock()
		if err != nil {
			return false
		}
		if ok {
			_ = probe.Unlock()
		}
		return !ok
	}, 30*time.Second, 20*time.Millisecond)
	body, err := f.st.GetMessageBodyText(f.message)
	require.NoError(t, err)
	assert.Contains(t, body, "constellation")
	close(releaseBuild)
	require.NoError(t, <-built)
	require.NoError(t, <-removed)
	fresh, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	require.NoError(t, err)
	defer func() { assert.NoError(t, fresh.Close()) }()
	decoded, err := fresh.QuerySQL(t.Context(), `SELECT * FROM messages`)
	require.NoError(t, err)
	encoded, err := json.Marshal(decoded)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "constellation")
}

func TestXRemovalCallerHandlerPendingFence(t *testing.T) {
	f := newXCleanupFixture(t)
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	require.NoError(t, err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	path := fmt.Sprintf("/api/v1/messages/%d", f.message)
	response := httptest.NewRecorder()
	server.Handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.NoError(t, cleaner.Resume(t.Context()))
	response = httptest.NewRecorder()
	server.Handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "constellation secret")
}

func TestXRemovalRetriesFailedOwnedSpillCleanup(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission failure requires an unprivileged process")
	}
	f := newXCleanupFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("DuckDB spill applies to SQLite")
	}
	_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	require.NoError(t, err)
	parent := filepath.Join(f.cfg.Data.DataDir, "restricted-spill-parent")
	path := filepath.Join(parent, "engine-owned")
	require.NoError(t, os.MkdirAll(path, 0700))
	engine, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2, TempDirectory: path, OwnTempDirectory: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chmod(parent, 0700); _ = os.Chmod(path, 0700); assert.NoError(t, engine.Close()) })
	// A pre-existing owned file makes RemoveAll encounter a real permission
	// failure. The production DuckDB engine owns and closes this directory.
	require.NoError(t, os.WriteFile(filepath.Join(path, "owned-spill-fixture"), []byte("fixture constellation bytes"), 0600))
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler)}, Engine: engine, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	require.NoError(t, os.Chmod(path, 0500))
	require.NoError(t, os.Chmod(parent, 0500))
	_, err = cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.Error(t, err)
	pending, err := f.st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.True(t, pending)
	assert.DirExists(t, path)
	require.NoError(t, os.Chmod(parent, 0700))
	require.NoError(t, os.Chmod(path, 0700))
	require.NoError(t, cleaner.Resume(t.Context()))
	assert.NoDirExists(t, path, "a failed Close must remain tracked until a later successful retry")
	pending, err = f.st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestXContentSpillCrashHelper(t *testing.T) {
	home := os.Getenv("MSGVAULT_TEST_X_SPILL_HOME")
	if home == "" {
		return
	}
	parent, err := query.PrepareDaemonSpillDir(home)
	require.NoError(t, err)
	path, err := os.MkdirTemp(parent, "engine-crash-")
	require.NoError(t, err)
	engine, err := query.NewDuckDBEngine("", "", nil, query.DuckDBOptions{TempDirectory: path, OwnTempDirectory: true, Threads: 2})
	require.NoError(t, err)
	_, err = engine.QuerySQL(t.Context(), `SELECT 'fixture constellation bytes'`)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "owned-spill-fixture"), []byte("fixture constellation bytes"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "crash-spill-path"), []byte(path), 0600))
	// Simulate process loss: deliberately bypass the production engine Close.
	os.Exit(0)
}

func TestXRemovalRebuildsCachePublishedWhileWaitingForBuilder(t *testing.T) {
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite Parquet publication boundary")
	}
	cfg := xTestConfig(t)
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	at := time.Now().UTC().Add(-time.Hour)
	_, err = st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100"}, Text: "synthetic cache constellation", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", Username: "example_author", FetchedAt: at}})
	require.NoError(t, err)
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
		built <- err
	}()
	select {
	case <-builderReady:
	case err := <-built:
		require.NoError(t, err)
		require.FailNow(t, "builder completed before its publication barrier")
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	require.NoDirExists(t, cfg.AnalyticsDir(), "the real builder has staged but has not published")
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := cleaner.RemovePost(ctx, source.ID, "100", "removed")
		removed <- err
	}()
	select {
	case <-cleanupReady:
	case err := <-removed:
		require.NoError(t, err)
		require.FailNow(t, "cleanup completed before its builder-lock barrier")
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	// The in-flight builder publishes while cleanup is about to wait for its
	// lock. The production stat must observe that generation after draining it.
	releaseBuilder()
	select {
	case err := <-built:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	readiness, err := query.InspectCacheReadiness(cfg.AnalyticsDir())
	require.NoError(t, err)
	require.Equal(t, query.CacheReady, readiness)
	before, err := query.NewDuckDBEngine(cfg.AnalyticsDir(), cfg.DatabaseDSN(), st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	require.NoError(t, err)
	rows, err := before.QuerySQL(ctx, `SELECT * FROM messages`)
	require.NoError(t, err)
	require.Len(t, rows.Rows, 1)
	require.NoError(t, before.Close())
	releaseCleanup()
	select {
	case err := <-removed:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	readiness, err = query.InspectCacheReadiness(cfg.AnalyticsDir())
	require.NoError(t, err)
	require.Equal(t, query.CacheReady, readiness, "a cache published by the drained builder must be rebuilt after removal")
	fresh, err := query.NewDuckDBEngine(cfg.AnalyticsDir(), cfg.DatabaseDSN(), st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	require.NoError(t, err)
	defer func() { assert.NoError(t, fresh.Close()) }()
	rows, err = fresh.QuerySQL(ctx, `SELECT * FROM messages`)
	require.NoError(t, err)
	encoded, err := json.Marshal(rows)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "synthetic cache constellation")
	pending, err := st.XCleanupPendingContext(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestXRemovalInvalidatesOldMaterializedSearch(t *testing.T) {
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite Parquet reader")
	}
	f := newXCleanupFixture(t)
	_, err := buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
	require.NoError(t, err)
	duck, err := query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
	require.NoError(t, err)
	defer func() { assert.NoError(t, duck.Close()) }()
	page, err := duck.SearchFastWithStats(t.Context(), &search.Query{TextTerms: []string{"constellation"}}, "constellation", query.MessageFilter{}, query.ViewNoStats, 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), page.TotalCount)
	// The first real search materializes this table in the already-open engine.
	// QuerySQL is owner-authorized read-only SQL, so cached data must be purged
	// even when no subsequent paginated search would otherwise replace it.
	before, err := duck.QuerySQL(t.Context(), `SELECT snippet,from_name FROM _search_matches_1`)
	require.NoError(t, err)
	encoded, err := json.Marshal(before)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "erased constellation secret")
	cleaner := NewXContentCleanup(f.st, f.cfg, f.backend)
	work, err := cleaner.RemovePost(t.Context(), f.source, "100", "removed")
	require.NoError(t, err)
	require.False(t, work.Pending)
	after, err := duck.QuerySQL(t.Context(), `SELECT snippet,from_name FROM _search_matches_1`)
	if err == nil {
		encoded, err = json.Marshal(after)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "erased constellation secret")
		assert.NotContains(t, string(encoded), "Erased Label")
	}
}
