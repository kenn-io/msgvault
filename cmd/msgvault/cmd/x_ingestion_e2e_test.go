package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	other, err := f.st.GetOrCreateSource("x", "1002")
	_testRequire.NoError(err)
	// Imported/legacy rows can predate today's source-label conflict validator.
	_, err = f.st.DB().Exec(f.st.Rebind("UPDATE sources SET display_name=? WHERE id=?"), "1001", other.ID)
	_testRequire.NoError(err)
	_, err = f.st.GetOrCreateSource("discord", "1001")
	_testRequire.NoError(err)
	sources, _, err := findScheduledSyncSources(t.Context(), f.st, "1001")
	_testRequire.NoError(err)
	_testRequire.Len(sources, 1)
	_testAssert.Equal(f.source.ID, sources[0].ID)
	_testAssert.Equal("x", sources[0].SourceType)
	sources, _, err = findScheduledSyncSources(t.Context(), f.st, "@example_owner")
	_testRequire.NoError(err)
	_testAssert.Empty(sources)
	_testAssert.Zero(f.calls.Load())
}

func TestXPausedSourceRetentionStatus(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: false}}
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	old := time.Now().UTC().Add(-48 * time.Hour)
	post, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", AuthorID: "1002", PostID: "301", ConversationID: "301", EditHistory: []string{"301"}, Text: "synthetic retained secret", CreatedAt: old, FetchedAt: old, Availability: "public"})
	_testRequire.NoError(err)
	_testRequire.NoError(f.st.RecordXCredentialErrorContext(t.Context(), f.source.ID, "1001", 1, "invalid_grant"))
	_testRequire.NoError(f.st.UpdateSourceDisplayNameContext(t.Context(), f.source.ID, "synthetic retained secret"))
	run, err := f.st.StartSync(f.source.ID, "incremental")
	_testRequire.NoError(err)
	_testRequire.NoError(f.st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run, SourceMessageID: "301", Phase: "fetch", Status: "error", ErrorKind: "provider", ErrorMessage: "synthetic retained secret"}))
	_testRequire.NoError(f.st.FailSync(run, "synthetic retained secret"))
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
		req.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
		resp, err := host.Client().Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode, data
	}
	status, data := get("/api/v1/sources/status")
	_testRequire.Equal(http.StatusOK, status, string(data))
	var response struct {
		Sources []struct {
			X map[string]any `json:"x"`
		} `json:"sources"`
	}
	_testRequire.NoError(json.Unmarshal(data, &response))
	_testRequire.Len(response.Sources, 1)
	_testAssert.NotNil(response.Sources[0].X, "paused/revoked zero-budget sources still expose local X status")
	_testAssert.NotContains(string(data), "synthetic retained secret")
	var typed api.SourceStatusResponse
	_testRequire.NoError(json.Unmarshal(data, &typed))
	_testRequire.NotNil(typed.Sources[0].X)
	_testAssert.False(typed.Sources[0].X.Retention.Overdue)
	clock.Store(old.Add(48 * time.Hour).UnixNano())
	status, data = get("/api/v1/sources/status")
	_testRequire.Equal(http.StatusOK, status)
	_testRequire.NoError(json.Unmarshal(data, &typed))
	_testAssert.True(typed.Sources[0].X.Retention.Overdue)
	_testAssert.Equal(int64(1), typed.Sources[0].X.Retention.Outstanding)
	_testAssert.Equal("invalid_grant", typed.Sources[0].X.CredentialError)
	_testAssert.Equal(int64(1), typed.Sources[0].X.Generation)
	_testAssert.Zero(typed.Sources[0].X.MonthlyBudgetMicros)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "301", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	status, data = get("/api/v1/sources/status")
	_testAssert.Equal(http.StatusOK, status, string(data))
	_testRequire.NoError(json.Unmarshal(data, &typed))
	_testAssert.True(typed.Sources[0].X.Retention.PendingRemoval)
	_testAssert.NotContains(string(data), "synthetic retained secret")
	status, _ = get(fmt.Sprintf("/api/v1/messages/%d", post.MessageID))
	_testAssert.Equal(http.StatusServiceUnavailable, status)
	status, _ = get("/api/v1/search?q=constellation")
	_testAssert.Equal(http.StatusServiceUnavailable, status)
	_testAssert.Zero(f.calls.Load(), "status/removal must not refresh credentials or call X")
}

func TestXIngestionThroughDaemonAndScheduler(t *testing.T) {
	// Register through the real verified-Me path before collecting. A raw
	// source/token fixture does not yet carry the account-identity evidence.
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)

	registration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/2/users/me", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"data":{"id":"1001","username":"example_owner","name":"Example Owner","protected":false}}`)
	}))
	t.Cleanup(registration.Close)
	registrationDeps := f.deps
	registrationDeps.apiBaseURL = registration.URL + "/2"
	registrationDeps.httpClient = registration.Client()
	registered, err := registerXAccountWithDeps(t.Context(), f.st, f.cfg, "1001", registrationDeps)
	_testRequire.NoError(err)
	_testRequire.Equal(f.source.ID, registered.ID)
	identities, err := f.st.ListAccountIdentities(f.source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(identities, 1)

	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: true, Schedule: "0 0 1 1 *"}}
	f.cfg.Analytics.AutoBuildCache = true
	// Exercise the real cache builder at the existing worker boundary, without
	// spawning the test executable as a production build-cache subprocess.
	if !f.st.IsPostgreSQL() {
		var seq int
		var name, path string
		_testRequire.NoError(f.st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
		f.cfg.Data.DatabaseURL = path
	}
	built := make(chan error, 4)
	jobsCtx, cancelJobs := context.WithCancel(t.Context())
	jobs := newCacheBuildJobs(jobsCtx, nil, func(ctx context.Context, _ buildCacheMode) error {
		var err error
		if !f.st.IsPostgreSQL() {
			_, err = buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
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
	_testRequire.NoError(sched.AddAccount("1001", "0 0 1 1 *"))
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
			_, err = buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		}
		built <- err
		return err
	}
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = oldRebuild })
	_testRequire.NoError(sched.TriggerSync("1001", false))
	select {
	case err := <-completed:
		_testRequire.NoError(err)
	case <-time.After(serveLifecycleTestTimeout):
		_testRequire.FailNow("scheduled X sync did not complete")
	}
	_testRequire.NoError(<-built)
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--json", "--build-cache")
	_testRequire.NoError(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testAssert.Equal("complete", summary.Status)
	_testAssert.Zero(summary.MessagesAdded)
	if !f.st.IsPostgreSQL() {
		_testRequire.NoError(<-built)
	}
	var graphBefore, ownersBefore int
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&graphBefore))
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM account_identities").Scan(&ownersBefore))
	_testRequire.NoError(sched.TriggerSync("1001", false))
	select {
	case err = <-completed:
		_testRequire.NoError(err)
	case <-time.After(serveLifecycleTestTimeout):
		_testRequire.FailNow("scheduled X sync did not complete")
	}
	_testRequire.NoError(<-built)
	var graphAfter, ownersAfter int
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&graphAfter))
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM account_identities").Scan(&ownersAfter))
	_testAssert.Equal(graphBefore, graphAfter)
	_testAssert.Equal(ownersBefore, ownersAfter)
	_testAssert.Positive(ownersAfter)
	control, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal("101", control.Own.Watermark)
	_testAssert.Equal("201", control.Mentions.Watermark)

	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "201")
	_testRequire.NoError(err)
	var participant int64
	_testRequire.NoError(f.st.DB().QueryRow("SELECT participant_id FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1002'").Scan(&participant))
	_, _, err = f.st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), participant, new("Curated Reply"))
	_testRequire.NoError(err)
	rootPost, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "101")
	_testRequire.NoError(err)
	rootDetail := fmt.Sprintf("/api/v1/messages/%d", rootPost.MessageID)
	replyDetail := fmt.Sprintf("/api/v1/messages/%d", target.MessageID)
	for _, path := range []string{fmt.Sprintf("/api/v1/cli/message/thread?id=%d", target.MessageID), rootDetail, replyDetail, "/api/v1/people/contact-candidates?query=Curated%20Reply", "/api/v1/conversations/search?q=example_author"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.URL+path, nil)
		_testRequire.NoError(err)
		req.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
		resp, err := host.Client().Do(req)
		_testRequire.NoError(err)
		data, err := io.ReadAll(resp.Body)
		_testRequire.NoError(err)
		_testRequire.NoError(resp.Body.Close())
		_testRequire.Equal(http.StatusOK, resp.StatusCode, string(data))
		if strings.Contains(path, "thread") {
			var thread query.ThreadPage
			_testRequire.NoError(json.Unmarshal(data, &thread))
			var nativeIDs []string
			for _, message := range thread.Messages {
				nativeIDs = append(nativeIDs, message.SourceMessageID)
			}
			_testAssert.ElementsMatch([]string{"101", "201"}, nativeIDs)
			_testAssert.Equal(f.source.ID, thread.SourceID)
		} else if path == rootDetail {
			_testAssert.Contains(string(data), "owner constellation")
		} else if path == replyDetail {
			_testAssert.Contains(string(data), "reply constellation")
		} else if strings.Contains(path, "contact-candidates") {
			_testAssert.Contains(string(data), "Curated Reply")
		}
	}
	// A stale scheduler callback cannot collect a paused account.
	f.cfg.Accounts[0].Enabled = false
	before := f.calls.Load()
	err = runScheduledSyncWithXDeps(ctx, "1001", f.st, nil, invocationFromContext(ctx), d)
	_testRequire.Error(err)
	_testAssert.Equal(before, f.calls.Load())
}

func TestXFirstSeenWithheldPostPurgesConnectedCopiesBeforeRetry(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	foreign, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	retained, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: foreign.ID, OwnerID: "2001", PostID: "700", AuthorID: "1001", ConversationID: "700",
		EditHistory: []string{"700"}, Text: "synthetic retained copy", CreatedAt: f.at, FetchedAt: f.at,
		Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: f.at},
	})
	_testRequire.NoError(err)

	var ownPages, lookups atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/2/tweets":
			lookups.Add(1)
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"700","author_id":"1001","conversation_id":"700","created_at":%q,"text":"synthetic withheld post","edit_history_tweet_ids":["700"],"withheld":{"copyright":{"scope":"test"}}}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]},"meta":{"result_count":1}}`, f.at.Format(time.RFC3339Nano))
		case "/2/users/1001/tweets":
			if ownPages.Add(1) == 1 {
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"700","author_id":"1001","conversation_id":"700","created_at":%q,"text":"synthetic withheld post","edit_history_tweet_ids":["700"],"withheld":{"copyright":{"scope":"test"}}}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":false}]},"meta":{"result_count":1}}`, f.at.Format(time.RFC3339Nano))
				return
			}
			_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
		default:
			_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
		}
	}))
	t.Cleanup(provider.Close)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	deps := f.deps
	deps.apiBaseURL = provider.URL + "/2"
	deps.httpClient = provider.Client()
	deps.cleanup = cleaner

	_, importErr := importXSourceWithDeps(t.Context(), f.st, f.cfg, f.source, xapi.ImportOptions{}, deps)
	_testRequire.ErrorIs(importErr, store.ErrXRemovalNeedsOwner, "cross-source cleanup must resume after the sync releases source ownership")
	_testAssert.Zero(lookups.Load(), "a first-seen withheld post must become terminal before another paid lookup")
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending, "the owner cleanup coordinator must finish the connected chain after sync releases it")
	body, err := f.st.GetMessageBodyText(t.Context(), retained.MessageID)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
	target, err := f.st.ResolveXPostContext(t.Context(), foreign.ID, "700")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)

	third, err := f.st.GetOrCreateSource("x", "3001")
	_testRequire.NoError(err)
	blocked, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: third.ID, OwnerID: "3001", PostID: "702", AuthorID: "1001", ConversationID: "702",
		EditHistory: []string{"700", "702"}, Text: "must remain suppressed", CreatedAt: f.at, FetchedAt: f.at,
		Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: f.at},
	})
	_testRequire.NoError(err)
	_testAssert.True(blocked.Skipped, "suppression must cover newly discovered copies across accounts")

	summary, err := importXSourceWithDeps(t.Context(), f.st, f.cfg, f.source, xapi.ImportOptions{}, deps)
	_testRequire.NoError(err)
	_testRequire.NotNil(summary)
	_testAssert.True(summary.OwnComplete)
	_testAssert.Zero(lookups.Load(), "a completed cleanup resolves the saved walk without another lookup")
}

func TestXFirstSeenProtectedPostCommitsCleanupWithAlias(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	foreign, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	retained, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: foreign.ID, OwnerID: "2001", PostID: "699", AuthorID: "1002", ConversationID: "699",
		EditHistory: []string{"699"}, Text: "synthetic protected-author copy", CreatedAt: f.at, FetchedAt: f.at,
		Availability: "public", Raw: []byte(`{"post":{"id":"699"}}`),
		Author: &store.XAuthorInput{UserID: "1002", FetchedAt: f.at},
	})
	_testRequire.NoError(err)
	installXAliasPersistenceProbe(t, f.st)

	var mentionPages, lookups atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/2/users/1001/tweets":
			_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
		case "/2/users/1001/mentions":
			if mentionPages.Add(1) > 1 {
				_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"700","author_id":"1002","conversation_id":"100","created_at":%q,"text":"synthetic protected reply","edit_history_tweet_ids":["700"]}],"includes":{"users":[{"id":"1002","username":"protected_author","protected":true}]},"meta":{"result_count":1}}`, f.at.Format(time.RFC3339Nano))
		case "/2/tweets":
			lookups.Add(1)
			_, _ = fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"700","type":"https://api.x.com/2/problems/resource-not-found"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	f.deps.apiBaseURL = provider.URL + "/2"
	f.deps.httpClient = provider.Client()
	f.deps.cleanup = cleaner
	f.adapter.xDeps = &f.deps

	firstSummary, importErr := importXSourceWithDeps(t.Context(), f.st, f.cfg, f.source, xapi.ImportOptions{}, f.deps)
	_testRequire.ErrorIs(importErr, store.ErrXRemovalNeedsOwner,
		"foreign protected-author copies drain only after the importer releases source ownership")
	_testRequire.NotNil(firstSummary)
	_testAssert.Zero(lookups.Load(), "protected evidence must not need another provider lookup")
	var suppressed bool
	var cleanupPending int
	var reason string
	_testRequire.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT
		suppressed, cleanup_pending, suppression_reason
		FROM x_test_alias_persistence WHERE source_id=? AND post_id='700'`),
		f.source.ID).Scan(&suppressed, &cleanupPending, &reason))
	_testAssert.True(suppressed, "the first alias INSERT must already suppress protected content")
	_testAssert.Equal(1, cleanupPending, "the first alias INSERT must durably queue cleanup")
	_testAssert.Equal("protected", reason, "the first alias INSERT must retain the provider reason")
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending, "the owner coordinator drains protected-author copies after sync releases")
	localTarget, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "700")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", localTarget.Availability,
		"suppressed targets stay unavailable to send resolution")
	assertXStoredSuppressionReason(t, f.st, f.source.ID, "700", "protected")
	body, err := f.st.GetMessageBodyText(t.Context(), retained.MessageID)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
	var rawRows int
	_testRequire.NoError(f.st.DB().QueryRowContext(t.Context(),
		f.st.Rebind(`SELECT COUNT(*) FROM message_raw WHERE message_id=?`), retained.MessageID).Scan(&rawRows))
	_testAssert.Zero(rawRows)
	foreignTarget, err := f.st.ResolveXPostContext(t.Context(), foreign.ID, "699")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", foreignTarget.Availability,
		"protected-author cleanup suppresses every known copy")
	assertXStoredSuppressionReason(t, f.st, foreign.ID, "699", "protected")

	summary, err := importXSourceWithDeps(t.Context(), f.st, f.cfg, f.source, xapi.ImportOptions{}, f.deps)
	_testRequire.NoError(err)
	_testRequire.NotNil(summary)
	_testAssert.True(summary.MentionsComplete, "the saved walk resolves locally once cleanup clears")
	_testAssert.Zero(lookups.Load(), "completed local cleanup resolves the saved walk without another provider lookup")
}

func TestXImporterBatchesSamePageUnavailableCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	posts := []struct {
		id   string
		text string
	}{
		{id: "801", text: "synthetic retained batch post one"},
		{id: "802", text: "synthetic retained batch post two"},
	}
	messageIDs := make(map[string]int64, len(posts))
	for _, post := range posts {
		saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{
			SourceID: f.source.ID, OwnerID: "1001", PostID: post.id, AuthorID: "1002",
			ConversationID: post.id, EditHistory: []string{post.id}, Text: post.text,
			CreatedAt: f.at, FetchedAt: at, Availability: "public",
			Raw:    []byte(`{"post":{"id":"` + post.id + `"}}`),
			Author: &store.XAuthorInput{UserID: "1002", FetchedAt: at},
		})
		_testRequire.NoError(err)
		messageIDs[post.id] = saved.MessageID
	}

	var ownPages atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/2/users/1001/tweets" && ownPages.Add(1) == 1 {
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"801","author_id":"1002","conversation_id":"801","created_at":%q,"text":"synthetic withheld one","edit_history_tweet_ids":["801"],"withheld":{"copyright":{"scope":"test"}}},{"id":"802","author_id":"1002","conversation_id":"802","created_at":%q,"text":"synthetic withheld two","edit_history_tweet_ids":["802"],"withheld":{"copyright":{"scope":"test"}}}],"includes":{"users":[{"id":"1002","username":"example_author","protected":false}]},"meta":{"result_count":2}}`, f.at.Format(time.RFC3339Nano), f.at.Format(time.RFC3339Nano))
			return
		}
		_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
	}))
	t.Cleanup(provider.Close)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	var storePasses, vectorPasses, publications int
	cleaner.afterStore = func() error { storePasses++; return nil }
	cleaner.afterVectors = func() error { vectorPasses++; return nil }
	cleaner.beforePublish = func() error { publications++; return nil }
	if !f.st.IsPostgreSQL() {
		var sequence int
		var name, path string
		_testRequire.NoError(f.st.DB().QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&sequence, &name, &path))
		_testRequire.NotEmpty(path, "cache builder must use the actual SQLite test archive")
		f.cfg.Data.DatabaseURL = path
		_, err := buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		_testRequire.NoError(err)
	}
	deps := f.deps
	deps.apiBaseURL = provider.URL + "/2"
	deps.httpClient = provider.Client()
	deps.cleanup = cleaner

	summary, err := importXSourceWithDeps(t.Context(), f.st, f.cfg, f.source, xapi.ImportOptions{}, deps)
	_testRequire.NoError(err)
	_testRequire.NotNil(summary)
	_testAssert.True(summary.OwnComplete)
	_testAssert.Equal(1, storePasses, "same-page suppressions share one Store cleanup pass")
	_testAssert.Equal(1, vectorPasses, "same-page suppressions share one vector cleanup pass")
	_testAssert.Equal(1, publications, "same-page suppressions publish analytics once")
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending)
	for _, post := range posts {
		target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, post.id)
		_testRequire.NoError(err)
		// Resolution normalizes suppressed targets to removed; Store retains the provider reason.
		_testAssert.Equal("removed", target.Availability,
			"suppressed targets resolve as unavailable to send resolution")
		assertXStoredSuppressionReason(t, f.st, f.source.ID, post.id, "withheld")
		body, err := f.st.GetMessageBodyText(t.Context(), messageIDs[post.id])
		_testRequire.NoError(err)
		_testAssert.Empty(body)
	}
	if !f.st.IsPostgreSQL() {
		readiness, err := query.InspectCacheReadiness(f.cfg.AnalyticsDir())
		_testRequire.NoError(err)
		_testAssert.Equal(query.CacheReady, readiness)
	}
}

func installXAliasPersistenceProbe(t *testing.T, st *store.Store) {
	t.Helper()
	require := require.New(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE TABLE x_test_alias_persistence (
			source_id BIGINT NOT NULL,
			post_id TEXT NOT NULL,
			suppressed BOOLEAN NOT NULL,
			cleanup_pending INTEGER NOT NULL,
			suppression_reason TEXT,
			PRIMARY KEY (source_id, post_id)
		)`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE FUNCTION x_test_record_alias_persistence() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				INSERT INTO x_test_alias_persistence(source_id,post_id,suppressed,cleanup_pending,suppression_reason)
				VALUES (NEW.source_id, NEW.post_id, NEW.suppressed_at IS NOT NULL, NEW.cleanup_pending, NEW.suppression_reason);
				RETURN NEW;
			END;
			$$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER x_test_alias_insert AFTER INSERT ON x_post_versions
			FOR EACH ROW EXECUTE FUNCTION x_test_record_alias_persistence()`)
		require.NoError(err)
		return
	}
	_, err := st.DB().Exec(`CREATE TABLE x_test_alias_persistence (
		source_id INTEGER NOT NULL,
		post_id TEXT NOT NULL,
		suppressed BOOLEAN NOT NULL,
		cleanup_pending INTEGER NOT NULL,
		suppression_reason TEXT,
		PRIMARY KEY (source_id, post_id)
	)`)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER x_test_alias_insert AFTER INSERT ON x_post_versions
		BEGIN
			INSERT INTO x_test_alias_persistence(source_id,post_id,suppressed,cleanup_pending,suppression_reason)
			VALUES(NEW.source_id,NEW.post_id,NEW.suppressed_at IS NOT NULL,NEW.cleanup_pending,NEW.suppression_reason);
		END`)
	require.NoError(err)
}

func TestXScheduledSyncRetiresDaemonReaders(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	f.cfg.Accounts = []config.AccountSchedule{{Email: "1001", Enabled: true, Schedule: "0 0 1 1 *"}}
	f.cfg.Analytics.AutoBuildCache = false
	at := time.Now().UTC().Add(-48 * time.Hour)
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", AuthorID: "1002", PostID: "301", ConversationID: "301", EditHistory: []string{"301"}, Text: "retired scheduled constellation", CreatedAt: at, FetchedAt: at, Availability: "public"})
	_testRequire.NoError(err)
	var engine = query.NewEngine(f.st.DB(), f.st.IsPostgreSQL())
	var duck *query.DuckDBEngine
	if !f.st.IsPostgreSQL() {
		var seq int
		var name, path string
		_testRequire.NoError(f.st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
		f.cfg.Data.DatabaseURL = path
		_, err = buildCache(t.Context(), f.cfg.DatabaseDSN(), f.cfg.AnalyticsDir(), true)
		_testRequire.NoError(err)
		duck, err = query.NewDuckDBEngine(t.Context(), f.cfg.AnalyticsDir(), f.cfg.DatabaseDSN(), f.st.DB(), query.DuckDBOptions{DisableSQLiteScanner: true, Threads: 2})
		_testRequire.NoError(err)
		t.Cleanup(func() { assert.NoError(t, duck.Close()) })
		engine = duck
	}
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	f.adapter.xCleanup = cleaner
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler), ContentReadGuard: cleaner.AcquireRead})
	cleaner.SetServer(server)
	read := func() string {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", saved.MessageID), nil)
		req.Header.Set("X-Api-Key", f.cfg.Server.APIKey)
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		return response.Body.String()
	}
	_testAssert.Contains(read(), "retired scheduled constellation")
	oldContentQuery := fmt.Sprintf("SELECT snippet FROM messages WHERE id=%d", saved.MessageID)
	if duck != nil {
		result, err := duck.QuerySQL(t.Context(), oldContentQuery)
		_testRequire.NoError(err)
		encoded, err := json.Marshal(result)
		_testRequire.NoError(err)
		_testRequire.Contains(string(encoded), "retired scheduled constellation")
	}

	var reads atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path == "/2/tweets" {
			assert.Equal(t, "301", r.URL.Query().Get("ids"))
			_, writeErr := fmt.Fprint(w, `{"errors":[{"resource_type":"tweet","resource_id":"301","type":"https://api.x.com/2/problems/resource-not-found"}],"meta":{"result_count":0}}`)
			assert.NoError(t, writeErr)
		} else {
			assert.Contains(t, []string{"/2/users/1001/tweets", "/2/users/1001/mentions"}, r.URL.Path)
			_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
		}
	}))
	t.Cleanup(provider.Close)
	d := f.deps
	d.apiBaseURL = provider.URL + "/2"
	d.httpClient = provider.Client()
	d.cleanup = cleaner
	ctx := testInvocationContext(t.Context(), f.cfg, invocationOptions{})
	_testRequire.NoError(runScheduledSyncWithXDeps(ctx, "1001", f.st, nil, invocationFromContext(ctx), d))
	_testAssert.Equal(int32(5), reads.Load(), "one refresh plus ordinary timeline/mentions and overlap, no archive search")
	_testAssert.NotContains(read(), "retired scheduled constellation")
	if duck != nil {
		result, queryErr := duck.QuerySQL(t.Context(), oldContentQuery)
		if queryErr == nil {
			encoded, err := json.Marshal(result)
			_testRequire.NoError(err)
			_testAssert.NotContains(string(encoded), "retired scheduled constellation", "the old native handle must not return removed content")
		}
	}
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending)
}
