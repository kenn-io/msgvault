package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
)

func TestXScheduledDiscoveryUsesExactNumericOwner(t *testing.T) {
	f := newXCollectionFixture(t)
	other, err := f.st.GetOrCreateSource("x", "1002")
	require.NoError(t, err)
	// Imported/legacy rows can predate today's source-label conflict validator.
	_, err = f.st.DB().Exec(f.st.Rebind("UPDATE sources SET display_name=? WHERE id=?"), "1001", other.ID)
	require.NoError(t, err)
	_, err = f.st.GetOrCreateSource("discord", "1001")
	require.NoError(t, err)
	sources, _, err := findScheduledSyncSources(f.st, "1001")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, f.source.ID, sources[0].ID)
	assert.Equal(t, "x", sources[0].SourceType)
	sources, _, err = findScheduledSyncSources(f.st, "@example_owner")
	require.NoError(t, err)
	assert.Empty(t, sources)
	assert.Zero(t, f.calls.Load())
}

func TestXPausedSourceRetentionStatus(t *testing.T) {
	f := newXCollectionFixture(t)
	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: false}}
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	old := time.Now().UTC().Add(-48 * time.Hour)
	post, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", AuthorID: "1002", PostID: "301", ConversationID: "301", EditHistory: []string{"301"}, Text: "synthetic retained secret", CreatedAt: old, FetchedAt: old, Availability: "public"})
	require.NoError(t, err)
	require.NoError(t, f.st.RecordXCredentialErrorContext(t.Context(), f.source.ID, "1001", 1, "invalid_grant"))
	require.NoError(t, f.st.UpdateSourceDisplayNameContext(t.Context(), f.source.ID, "synthetic retained secret"))
	run, err := f.st.StartSync(f.source.ID, "incremental")
	require.NoError(t, err)
	require.NoError(t, f.st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run, SourceMessageID: "301", Phase: "fetch", Status: "error", ErrorKind: "provider", ErrorMessage: "synthetic retained secret"}))
	require.NoError(t, f.st.FailSync(run, "synthetic retained secret"))
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	f.adapter.xCleanup = cleaner
	var clock atomic.Int64
	clock.Store(old.Add(time.Hour).UnixNano())
	server := api.NewServerWithOptions(api.ServerOptions{Clock: func() time.Time { return time.Unix(0, clock.Load()) }, Config: f.cfg, Store: f.adapter, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	host := httptest.NewServer(server.Router())
	t.Cleanup(host.Close)
	get := func(path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", f.cfg.Server.APIKey)
		resp, err := host.Client().Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode, data
	}
	status, data := get("/api/v1/sources/status")
	require.Equal(t, http.StatusOK, status, string(data))
	var response struct {
		Sources []struct {
			X map[string]any `json:"x"`
		} `json:"sources"`
	}
	require.NoError(t, json.Unmarshal(data, &response))
	require.Len(t, response.Sources, 1)
	assert.NotNil(t, response.Sources[0].X, "paused/revoked zero-budget sources still expose local X status")
	assert.NotContains(t, string(data), "synthetic retained secret")
	var typed api.SourceStatusResponse
	require.NoError(t, json.Unmarshal(data, &typed))
	require.NotNil(t, typed.Sources[0].X)
	assert.False(t, typed.Sources[0].X.Retention.Overdue)
	clock.Store(old.Add(48 * time.Hour).UnixNano())
	status, data = get("/api/v1/sources/status")
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, json.Unmarshal(data, &typed))
	assert.True(t, typed.Sources[0].X.Retention.Overdue)
	assert.Equal(t, int64(1), typed.Sources[0].X.Retention.Outstanding)
	assert.Equal(t, "invalid_grant", typed.Sources[0].X.CredentialError)
	assert.Equal(t, int64(1), typed.Sources[0].X.Generation)
	assert.Zero(t, typed.Sources[0].X.MonthlyBudgetMicros)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "301", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	status, data = get("/api/v1/sources/status")
	assert.Equal(t, http.StatusOK, status, string(data))
	require.NoError(t, json.Unmarshal(data, &typed))
	assert.True(t, typed.Sources[0].X.Retention.PendingRemoval)
	assert.NotContains(t, string(data), "synthetic retained secret")
	status, _ = get(fmt.Sprintf("/api/v1/messages/%d", post.MessageID))
	assert.Equal(t, http.StatusServiceUnavailable, status)
	status, _ = get("/api/v1/search?q=constellation")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Zero(t, f.calls.Load(), "status/removal must not refresh credentials or call X")
}

func TestXIngestionThroughDaemonAndScheduler(t *testing.T) {
	f := newXCollectionFixture(t)
	// Register through the real verified-Me path before collecting. A raw
	// source/token fixture does not yet carry the account-identity evidence.
	registration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/2/users/me", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"data":{"id":"1001","username":"example_owner","name":"Example Owner","protected":false}}`)
	}))
	t.Cleanup(registration.Close)
	registrationDeps := f.deps
	registrationDeps.apiBaseURL = registration.URL + "/2"
	registrationDeps.httpClient = registration.Client()
	registered, err := registerXAccountWithDeps(t.Context(), f.st, f.cfg, "1001", registrationDeps)
	require.NoError(t, err)
	require.Equal(t, f.source.ID, registered.ID)
	identities, err := f.st.ListAccountIdentities(f.source.ID)
	require.NoError(t, err)
	require.Len(t, identities, 1)

	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: true, Schedule: "0 0 1 1 *"}}
	f.cfg.Analytics.AutoBuildCache = true
	// Exercise the real cache builder at the existing worker boundary, without
	// spawning the test executable as a production build-cache subprocess.
	if !f.st.IsPostgreSQL() {
		var seq int
		var name, path string
		require.NoError(t, f.st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
		f.cfg.Data.DatabaseURL = path
	}
	built := make(chan error, 4)
	jobsCtx, cancelJobs := context.WithCancel(t.Context())
	jobs := newCacheBuildJobs(jobsCtx, nil, func(ctx context.Context, _ buildCacheMode) error {
		var err error
		if !f.st.IsPostgreSQL() {
			_, err = buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		}
		built <- err
		return err
	})
	t.Cleanup(func() {
		cancelJobs()
		ctx, cancel := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
		defer cancel()
		require.True(t, jobs.waitContext(ctx))
	})
	f.adapter.cacheJobs = jobs
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	f.adapter.xCleanup = cleaner
	d := f.deps
	d.cleanup = cleaner
	ctx := testInvocationContext(t.Context(), f.cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	completed := make(chan error, 4)
	sched, media := newServeSchedulers(func(ctx context.Context, id string) error {
		err := runScheduledSyncWithXDeps(ctx, id, f.st, nil, state, d)
		completed <- err
		return err
	}, slog.New(slog.DiscardHandler), nil, nil, nil)
	t.Cleanup(func() { <-sched.Stop().Done(); <-media.Stop().Done() })
	require.NoError(t, sched.AddAccount("1001", "0 0 1 1 *"))
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Engine: query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()), Scheduler: &schedulerAdapter{scheduler: sched, media: media}, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(apiServer)
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", apiServer.Router())
	host := httptest.NewServer(mux)
	t.Cleanup(host.Close)
	f.server = host

	oldRebuild := rebuildCacheAfterScheduledSourceRun
	rebuildCacheAfterScheduledSourceRun = func(ctx context.Context, _ string) error {
		var err error
		if !f.st.IsPostgreSQL() {
			_, err = buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		}
		built <- err
		return err
	}
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = oldRebuild })
	require.NoError(t, sched.TriggerSync("1001", false))
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow(t, "scheduled X sync did not complete")
	}
	require.NoError(t, <-built)
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--json", "--build-cache")
	require.NoError(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	assert.Equal(t, "complete", summary.Status)
	assert.Zero(t, summary.MessagesAdded)
	if !f.st.IsPostgreSQL() {
		require.NoError(t, <-built)
	}
	var graphBefore, ownersBefore int
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&graphBefore))
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM account_identities").Scan(&ownersBefore))
	require.NoError(t, sched.TriggerSync("1001", false))
	select {
	case err = <-completed:
		require.NoError(t, err)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow(t, "scheduled X sync did not complete")
	}
	require.NoError(t, <-built)
	var graphAfter, ownersAfter int
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&graphAfter))
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM account_identities").Scan(&ownersAfter))
	assert.Equal(t, graphBefore, graphAfter)
	assert.Equal(t, ownersBefore, ownersAfter)
	assert.Positive(t, ownersAfter)
	control, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	assert.Equal(t, "101", control.Own.Watermark)
	assert.Equal(t, "201", control.Mentions.Watermark)

	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "201")
	require.NoError(t, err)
	var participant int64
	require.NoError(t, f.st.DB().QueryRow("SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1002'").Scan(&participant))
	_, _, err = f.st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), participant, new("Curated Reply"))
	require.NoError(t, err)
	rootPost, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "101")
	require.NoError(t, err)
	rootDetail := fmt.Sprintf("/api/v1/messages/%d", rootPost.MessageID)
	replyDetail := fmt.Sprintf("/api/v1/messages/%d", target.MessageID)
	for _, path := range []string{fmt.Sprintf("/api/v1/cli/message/thread?id=%d", target.MessageID), rootDetail, replyDetail, "/api/v1/people/contact-candidates?query=Curated%20Reply", "/api/v1/conversations/search?q=example_author"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", f.cfg.Server.APIKey)
		resp, err := host.Client().Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, string(data))
		if strings.Contains(path, "thread") {
			var thread query.ThreadPage
			require.NoError(t, json.Unmarshal(data, &thread))
			var nativeIDs []string
			for _, message := range thread.Messages {
				nativeIDs = append(nativeIDs, message.SourceMessageID)
			}
			assert.ElementsMatch(t, []string{"101", "201"}, nativeIDs)
			assert.Equal(t, f.source.ID, thread.SourceID)
		} else if path == rootDetail {
			assert.Contains(t, string(data), "owner constellation")
		} else if path == replyDetail {
			assert.Contains(t, string(data), "reply constellation")
		} else if strings.Contains(path, "contact-candidates") {
			assert.Contains(t, string(data), "Curated Reply")
		}
	}
	// A stale scheduler callback cannot collect a paused account.
	f.cfg.Accounts[0].Enabled = false
	before := f.calls.Load()
	err = runScheduledSyncWithXDeps(ctx, "1001", f.st, nil, invocationFromContext(ctx), d)
	require.Error(t, err)
	assert.Equal(t, before, f.calls.Load())
}

func TestXScheduledSyncRetiresDaemonReaders(t *testing.T) {
	f := newXCollectionFixture(t)
	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: true, Schedule: "0 0 1 1 *"}}
	f.cfg.Analytics.AutoBuildCache = false
	at := time.Now().UTC().Add(-48 * time.Hour)
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", AuthorID: "1002", PostID: "301", ConversationID: "301", EditHistory: []string{"301"}, Text: "retired scheduled constellation", CreatedAt: at, FetchedAt: at, Availability: "public"})
	require.NoError(t, err)
	var engine query.Engine = query.NewEngine(f.st.DB(), f.st.IsPostgreSQL())
	var duck *query.DuckDBEngine
	if !f.st.IsPostgreSQL() {
		var seq int
		var name, path string
		require.NoError(t, f.st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
		f.cfg.Data.DatabaseURL = path
		_, err = buildCache(f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		require.NoError(t, err)
		duck, err = query.NewDuckDBEngine(f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, duck.Close()) })
		engine = duck
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	f.adapter.xCleanup = cleaner
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	read := func() string {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", saved.MessageID), nil)
		req.Header.Set("X-API-Key", f.cfg.Server.APIKey)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		return response.Body.String()
	}
	assert.Contains(t, read(), "retired scheduled constellation")
	oldContentQuery := fmt.Sprintf("SELECT snippet FROM messages WHERE id=%d", saved.MessageID)
	if duck != nil {
		result, err := duck.QuerySQL(t.Context(), oldContentQuery)
		require.NoError(t, err)
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		require.Contains(t, string(encoded), "retired scheduled constellation")
	}

	var reads atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path == "/2/tweets" {
			assert.Equal(t, "301", r.URL.Query().Get("ids"))
			fmt.Fprint(w, `{"errors":[{"resource_id":"301","type":"https://api.x.com/2/problems/resource-not-found"}],"meta":{"result_count":0}}`)
		} else {
			assert.Contains(t, []string{"/2/users/1001/tweets", "/2/users/1001/mentions"}, r.URL.Path)
			fmt.Fprint(w, `{"meta":{"result_count":0}}`)
		}
	}))
	t.Cleanup(provider.Close)
	d := f.deps
	d.apiBaseURL = provider.URL + "/2"
	d.httpClient = provider.Client()
	d.cleanup = cleaner
	ctx := testInvocationContext(t.Context(), f.cfg, invocationOptions{})
	require.NoError(t, runScheduledSyncWithXDeps(ctx, "1001", f.st, nil, invocationFromContext(ctx), d))
	assert.Equal(t, int32(5), reads.Load(), "one refresh plus ordinary timeline/mentions and overlap, no archive search")
	assert.NotContains(t, read(), "retired scheduled constellation")
	if duck != nil {
		result, queryErr := duck.QuerySQL(t.Context(), oldContentQuery)
		if queryErr == nil {
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "retired scheduled constellation", "the old native handle must not return removed content")
		}
	}
	pending, err := f.st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.False(t, pending)
}
