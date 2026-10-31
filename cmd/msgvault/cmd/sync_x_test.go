package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	xapi "go.kenn.io/msgvault/internal/x"
)

type xCollectionFixture struct {
	st               *store.Store
	cfg              *config.Config
	source           *store.Source
	deps             xCommandDeps
	adapter          *storeAPIAdapter
	server, provider *httptest.Server
	calls            atomic.Int32
	cacheCalls       atomic.Int32
	mu               sync.Mutex
	failMentions     bool
	entered          chan struct{}
	cancelRun        context.CancelFunc
	at               time.Time
}

func newXCollectionFixture(t *testing.T) *xCollectionFixture {
	t.Helper()
	f := &xCollectionFixture{st: testutil.NewTestStore(t), cfg: xTestConfig(t), at: time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)}
	f.cfg.Server.APIKey = "example-owner-key"
	if f.st.IsPostgreSQL() {
		f.cfg.Data.DatabaseURL = os.Getenv("MSGVAULT_TEST_DB")
	}
	var err error
	f.source, err = f.st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	policy, err := f.cfg.XAccount("1001")
	require.NoError(t, err)
	policy.MonthlyBudgetMicros = 100_000_000
	f.cfg.X.Accounts = []config.XAccountConfig{policy}
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	require.NoError(t, manager.Save(t.Context(), xTestRecord("collection-access")))
	record, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, 0, store.XControlPatch{OwnerID: "1001", Generation: &record.Generation})
	require.NoError(t, err)
	f.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		assert.Equal(t, "Bearer collection-access", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		mentions := r.URL.Path == "/2/users/1001/mentions"
		require.True(t, mentions || r.URL.Path == "/2/users/1001/tweets", r.URL.Path)
		f.mu.Lock()
		fail, entered := f.failMentions, f.entered
		f.mu.Unlock()
		if mentions && entered != nil {
			close(entered)
			<-r.Context().Done()
			return
		}
		if mentions && fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"title":"Synthetic unavailable"}`)
			return
		}
		if start := r.URL.Query().Get("start_time"); start != "" {
			lower, err := time.Parse(time.RFC3339, start)
			require.NoError(t, err)
			if !f.at.After(lower) {
				fmt.Fprint(w, `{"meta":{"result_count":0}}`)
				return
			}
		}
		id, author, text, refs := "101", "1001", "owner constellation", ""
		if mentions {
			id, author, text, refs = "201", "1002", "reply constellation", `,"referenced_tweets":[{"type":"replied_to","id":"101"}]`
		}
		fmt.Fprintf(w, `{"data":[{"id":%q,"author_id":%q,"conversation_id":"101","created_at":%q,"text":%q,"note_tweet":{"text":%q}%s}],"includes":{"users":[{"id":%q,"username":"example_author","name":"Example Author","protected":false}]},"meta":{"result_count":1}}`, id, author, f.at.Format(time.RFC3339), text, text, refs, author)
	}))
	t.Cleanup(f.provider.Close)
	f.deps = xCommandDeps{apiBaseURL: f.provider.URL + "/2", httpClient: f.provider.Client()}
	jobsCtx, cancelJobs := context.WithCancel(t.Context())
	jobs := newCacheBuildJobs(jobsCtx, nil, func(context.Context, buildCacheMode) error { f.cacheCalls.Add(1); return nil })
	t.Cleanup(func() {
		cancelJobs()
		ctx, cancel := context.WithTimeout(context.Background(), serveLifecycleTestTimeout)
		defer cancel()
		require.True(t, jobs.waitContext(ctx))
	})
	f.adapter = &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xDeps: &f.deps, cacheJobs: jobs}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: f.adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	runCtx, cancelRun := context.WithCancel(t.Context())
	f.cancelRun = cancelRun
	t.Cleanup(cancelRun)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(runCtx, cancel)
		defer stop()
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(f.server.Close)
	return f
}
func (f *xCollectionFixture) clientConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := xTestConfig(t)
	cfg.Remote.URL = f.server.URL
	cfg.Remote.AllowInsecure = true
	cfg.Remote.APIKey = f.cfg.Server.AuthenticationKey()
	return cfg
}
func (f *xCollectionFixture) run(t *testing.T, cfg *config.Config, args ...string) (string, string, error) {
	t.Helper()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	root := &cobra.Command{Use: "msgvault"}
	root.SetContext(ctx)
	root.AddCommand(newSyncXCmd())
	silenceUsageInRunE(root)
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"sync-x"}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), stderr.String(), err
}
func TestSyncXSelectedDaemonExactSummary(t *testing.T) {
	f := newXCollectionFixture(t)
	cfg := f.clientConfig(t)
	out, _, err := f.run(t, cfg, "--source-id", fmt.Sprint(f.source.ID), "--after", f.at.Add(-time.Hour).Format(time.RFC3339), "--json")
	require.NoError(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	assert.Equal(t, f.source.ID, summary.SourceID)
	assert.Equal(t, "complete", summary.Status)
	assert.Equal(t, int64(2), summary.MessagesAdded)
	assert.Zero(t, summary.MessagesUpdated)
	assert.Equal(t, int64(2), summary.MessagesProcessed)
	assert.True(t, summary.OwnComplete)
	assert.True(t, summary.MentionsComplete)
	assert.Equal(t, "101", summary.OwnWatermark)
	assert.Equal(t, "201", summary.MentionsWatermark)
	assert.Equal(t, int64(30_000), summary.RunEstimatedMicros)
	assert.Equal(t, int64(3_000_000), summary.RunReservedMicros)
	assert.Equal(t, int64(30_000), summary.EstimatedMicros)
	assert.Equal(t, int64(3_000_000), summary.ReservedMicros)
	var messages int
	require.NoError(t, f.st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	assert.Equal(t, 2, messages)
	_, err = os.Stat(cfg.DatabaseDSN())
	assert.ErrorIs(t, err, os.ErrNotExist, "client must not create a second archive")
	assert.Equal(t, int32(4), f.calls.Load())
}
func TestSyncXPartialErrorStillPrintsJSONAndQueuesCache(t *testing.T) {
	f := newXCollectionFixture(t)
	f.failMentions = true
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--json", "--build-cache")
	require.Error(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	assert.Equal(t, "partial", summary.Status)
	assert.NotEmpty(t, summary.Code)
	assert.True(t, summary.OwnComplete)
	assert.False(t, summary.MentionsComplete)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Positive(t, summary.ReservedMicros)
	ctx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer cancel()
	require.True(t, f.adapter.cacheJobs.waitContext(ctx))
	if f.st.IsPostgreSQL() {
		assert.Zero(t, f.cacheCalls.Load())
	} else {
		assert.Equal(t, int32(1), f.cacheCalls.Load())
	}
}
func TestSyncXExclusiveAfterAndInvalidSelection(t *testing.T) {
	f := newXCollectionFixture(t)
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--after", f.at.Format(time.RFC3339), "--json")
	require.NoError(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	assert.Zero(t, summary.MessagesAdded)
	before := f.calls.Load()
	wrong, err := f.st.GetOrCreateSource("gmail", "example@example.test")
	require.NoError(t, err)
	badOwner, err := f.st.GetOrCreateSource("x", "example_handle")
	require.NoError(t, err)
	retired, err := f.st.GetOrCreateSource("x", "1003")
	require.NoError(t, err)
	_, err = f.st.DB().Exec(f.st.Rebind("INSERT INTO source_settings(merged_into_source_id,source_id,history_only,retired_at) VALUES(?,?,TRUE,CURRENT_TIMESTAMP) ON CONFLICT(source_id) DO UPDATE SET merged_into_source_id=excluded.merged_into_source_id,history_only=TRUE,retired_at=excluded.retired_at"), f.source.ID, retired.ID)
	require.NoError(t, err)
	for _, id := range []int64{wrong.ID, badOwner.ID, retired.ID, 999999} {
		_, _, err = f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(id), "--json")
		require.Error(t, err)
	}
	assert.Equal(t, before, f.calls.Load())
}
func TestSyncXReadModelAcknowledgementIsLocalAndPreservesState(t *testing.T) {
	f := newXCollectionFixture(t)
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	model, credential := store.XModelErrorResourceShape, "revoked"
	spend := []store.XSpendBucket{{Month: "2026-10", SpentMicros: 123, ReservedMicros: 500}}
	reservations := []store.XReservation{{ID: "held", Kind: "lookup", Month: "2026-10", AmountMicros: 500, State: "unknown"}}
	walk := store.XWalk{Endpoint: "timeline", Fingerprint: "preserved", Watermark: "100"}
	jobs := []store.XArchiveJob{{ID: "closed-fixture", Fingerprint: "preserved-job", Username: "example_owner", CapMicros: 1000, SpentMicros: 50, Complete: true}}
	refresh := store.XRefreshState{Cursor: "700", Outstanding: 2, PendingRemoval: true}
	mentions := store.XWalk{Endpoint: "mentions", Fingerprint: "saved-mentions", Watermark: "90"}
	state, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &model, CredentialError: &credential, Own: &walk, Mentions: &mentions, Jobs: &jobs, Refresh: &refresh, SpendBuckets: &spend, Reservations: &reservations})
	require.NoError(t, err)
	// Static schema fixtures prove acknowledgement cannot erase suppression or
	// durable receipts; no future draft/send API is invoked.
	_, err = f.st.DB().Exec(f.st.Rebind("INSERT INTO x_post_versions(source_id,post_id,canonical_id,version_position,availability,suppressed_at) VALUES(?,'500','500',0,'removed',?)"), f.source.ID, f.at)
	require.NoError(t, err)
	_, err = f.st.DB().Exec(f.st.Rebind("INSERT INTO x_reply_attempts(draft_id,attempt_id,source_id,owner_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,reserved_micros) VALUES('fixture-draft','fixture-attempt',?,'1001',7,'500','500','synthetic-digest','remote_unknown','held',500)"), f.source.ID)
	require.NoError(t, err)
	beforeTarget, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "500")
	require.NoError(t, err)
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
	lock, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	_, _, err = f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--acknowledge-read-model", "--json")
	require.Error(t, err)
	require.NoError(t, lock.Release())
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--acknowledge-read-model", "--json")
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	assert.Equal(t, "read_model_acknowledged", result["status"])
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	assert.Empty(t, after.ModelError)
	assert.Equal(t, state.CredentialError, after.CredentialError)
	assert.Equal(t, state.Own, after.Own)
	assert.Equal(t, state.SpendBuckets, after.SpendBuckets)
	assert.Equal(t, state.Reservations, after.Reservations)
	assert.Equal(t, state.Generation, after.Generation)
	expected := state
	expected.ModelError = ""
	expected.Revision++
	assert.Equal(t, expected, after)
	afterTarget, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "500")
	require.NoError(t, err)
	assert.Equal(t, beforeTarget, afterTarget)
	var receiptState string
	var reserved int64
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind("SELECT state,reserved_micros FROM x_reply_attempts WHERE source_id=? AND draft_id='fixture-draft' AND attempt_id='fixture-attempt'"), f.source.ID).Scan(&receiptState, &reserved))
	assert.Equal(t, "remote_unknown", receiptState)
	assert.Equal(t, int64(500), reserved)
	var runs int
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind("SELECT count(*) FROM sync_runs WHERE source_id=?"), f.source.ID).Scan(&runs))
	assert.Zero(t, runs)

	assert.Zero(t, f.calls.Load())
	assert.Zero(t, f.cacheCalls.Load())
	_, err = os.Stat(f.cfg.TokensDir())
	assert.ErrorIs(t, err, os.ErrNotExist)
}
func TestSyncXHTTPRejectsDelegatedAndUnsafeForms(t *testing.T) {
	f := newXCollectionFixture(t)
	valid := []string{"sync-x", "--source-id", fmt.Sprint(f.source.ID)}
	for _, req := range []api.CLIRunRequest{{Args: valid, Cwd: "/tmp"}, {Args: valid, Env: map[string]string{"MSGVAULT_DISCORD_TOKEN": "synthetic-secret"}}, {Args: append(append([]string{}, valid...), "--source-id=7")}, {Args: append(append([]string{}, valid...), "--token-file=/tmp/example")}} {
		data, err := json.Marshal(req)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(data))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
		resp, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	grantRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/agent-tokens", strings.NewReader(fmt.Sprintf(`{"label":"example-agent","permissions":["draft.create"],"source_ids":[%d]}`, f.source.ID)))
	require.NoError(t, err)
	grantRequest.Header.Set("Content-Type", "application/json")
	grantRequest.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	resp, err := http.DefaultClient.Do(grantRequest)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(data))
	var grant struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(data, &grant))
	body, err := json.Marshal(api.CLIRunRequest{Args: valid})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	resp, err = http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	archiveBody, err := json.Marshal(api.CLIRunRequest{Args: append(append([]string{}, valid...), "--archive", "--after=2020-01-01")})
	require.NoError(t, err)
	archiveRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(archiveBody))
	require.NoError(t, err)
	archiveRequest.Header.Set("Content-Type", "application/json")
	archiveRequest.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	archiveResponse, err := http.DefaultClient.Do(archiveRequest)
	require.NoError(t, err)
	require.NoError(t, archiveResponse.Body.Close())
	assert.Equal(t, http.StatusBadRequest, archiveResponse.StatusCode)
	// Runtime grant-decision proofs are reserved for add-account, never sync-x.
	proofReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	require.NoError(t, err)
	proofReq.Header.Set("Content-Type", "application/json")
	proofReq.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	proofReq.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, "synthetic-invalid-proof")
	resp, err = http.DefaultClient.Do(proofReq)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Zero(t, f.calls.Load())
}
func TestSyncXNeverFallsBackToSubprocess(t *testing.T) {
	f := newXCollectionFixture(t)
	for _, req := range []api.CLIRunRequest{{Args: []string{"sync-x", "--unknown"}}, {Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source.ID), "--archive"}}, {Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source.ID)}, GrantDecided: true}, {Args: []string{"sync-x", "--source-id", fmt.Sprint(f.source.ID), "--acknowledge-read-model"}}} {
		called := false
		err := f.adapter.runCLICommandWithRunner(t.Context(), req, nil, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			called = true
			return nil
		})
		assert.False(t, called)
		if !strings.Contains(strings.Join(req.Args, " "), "acknowledge") {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
		}
	}
}
func TestSyncXCancellationKeepsPartialJSON(t *testing.T) {
	f := newXCollectionFixture(t)
	f.entered = make(chan struct{})
	barrierCtx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	defer f.cancelRun()
	reached := make(chan bool, 1)
	go func() {
		select {
		case <-f.entered:
			f.cancelRun()
			reached <- true
		case <-barrierCtx.Done():
			f.cancelRun()
			reached <- false
		}
	}()
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--json")
	stop() // Release the observer even when a RED scaffold never reaches HTTP.
	select {
	case hit := <-reached:
		require.True(t, hit, "collection did not reach the observable mentions request")
	case <-t.Context().Done():
		require.FailNow(t, "test cancelled while waiting for request observer")
	}
	require.Error(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	assert.Equal(t, "interrupted", summary.Code)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.True(t, summary.OwnComplete)
	assert.False(t, summary.MentionsComplete)
}

func TestSyncXHumanPartialCoverageAndNoCache(t *testing.T) {
	f := newXCollectionFixture(t)
	state, seedErr := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, seedErr)
	month := time.Now().UTC().Format("2006-01")
	priorSpend := []store.XSpendBucket{{Month: month, SpentMicros: 1_234_567, ReservedMicros: 12_000_000}}
	priorReservations := []store.XReservation{{ID: "preexisting-unknown", Kind: "lookup", Month: month, AmountMicros: 12_000_000, State: "unknown"}}
	_, seedErr = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", SpendBuckets: &priorSpend, Reservations: &priorReservations})
	require.NoError(t, seedErr)
	f.failMentions = true
	f.cfg.Analytics.AutoBuildCache = true
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--no-build-cache")
	require.Error(t, err)
	for _, text := range []string{"X collection partial:", "messages_processed: 1", "messages_added: 1", "messages_updated: 0", "own_complete: true", "mentions_complete: false", "Run estimated: USD 0.015000", "Run unsettled reserved: USD 4.500000", "Month-to-date estimated: USD 1.249567", "Month-to-date unsettled reserved: USD 16.500000", "code: provider_error"} {
		assert.Contains(t, out, text)
	}
	assert.Zero(t, f.cacheCalls.Load())
}

type xArchiveCommandRequest struct{ Query, After, Before, Order, MaxResults, JobID string }
type xArchiveCommandFixture struct {
	*xCollectionFixture
	username       string
	rejectMentions bool
	foreignOwn     bool
	searches       []xArchiveCommandRequest
}

func newXArchiveCommandFixture(t *testing.T) *xArchiveCommandFixture {
	t.Helper()
	f := &xArchiveCommandFixture{xCollectionFixture: newXCollectionFixture(t), username: "example_owner"}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		assert.Equal(t, "Bearer collection-access", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		name := f.username
		foreign := f.foreignOwn
		f.mu.Unlock()
		if r.URL.Path == "/2/users/me" {
			fmt.Fprintf(w, `{"data":{"id":"1001","username":%q,"name":"Example Owner","protected":false}}`, name)
			return
		}
		if !assert.Equal(t, "/2/tweets/search/all", r.URL.Path) {
			w.WriteHeader(400)
			return
		}
		q := r.URL.Query()
		assert.Empty(t, q.Get("since_id"))
		assert.Empty(t, q.Get("until_id"))
		assert.Empty(t, q.Get("pagination_token"))
		control, err := f.st.LoadXControlContext(r.Context(), f.source.ID)
		if !assert.NoError(t, err) {
			w.WriteHeader(500)
			return
		}
		jobID := ""
		for _, reservation := range control.Reservations {
			if reservation.State == "pending" {
				jobID = reservation.JobID
			}
		}
		assert.NotEmpty(t, jobID, "job-scoped reservation must be durable before HTTP")
		f.mu.Lock()
		f.searches = append(f.searches, xArchiveCommandRequest{Query: q.Get("query"), After: q.Get("start_time"), Before: q.Get("end_time"), Order: q.Get("sort_order"), MaxResults: q.Get("max_results"), JobID: jobID})
		reject := f.rejectMentions
		f.mu.Unlock()
		if reject && strings.HasPrefix(q.Get("query"), "to:") {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"title":"Synthetic archive denial"}`)
			return
		}
		id, author, text, refs := "301", "1001", "archive owner constellation", ""
		if foreign {
			author = "1003"
		}
		if strings.HasPrefix(q.Get("query"), "to:") {
			id, author, text, refs = "302", "1002", "archive public reply", `,"referenced_tweets":[{"type":"replied_to","id":"301"}]`
			name = "example_reply"
		}
		fmt.Fprintf(w, `{"data":[{"id":%q,"author_id":%q,"conversation_id":"301","created_at":%q,"text":%q,"note_tweet":{"text":%q}%s}],"includes":{"users":[{"id":%q,"username":%q,"name":"Example Author","protected":false}]},"meta":{"result_count":1}}`, id, author, f.at.Format(time.RFC3339), text, text, refs, author, name)
	}))
	t.Cleanup(provider.Close)
	f.deps = xCommandDeps{apiBaseURL: provider.URL + "/2", httpClient: provider.Client()}
	_, err := registerXAccountWithDeps(t.Context(), f.st, f.cfg, "1001", f.deps)
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.calls.Load(), "fixture setup uses actual verified Me registration")
	f.calls.Store(0)
	return f
}
func (f *xArchiveCommandFixture) runArchive(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := []string{"--source-id", fmt.Sprint(f.source.ID), "--archive", "--after=2020-01-01"}
	out, _, err := f.run(t, f.clientConfig(t), append(args, extra...)...)
	return out, err
}
func TestSyncXArchivePreviewUsesVerifiedCacheWithoutNetwork(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	require.NoError(t, f.st.UpdateSourceDisplayName(f.source.ID, "Synthetic editable source label"))
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	model, revoked := store.XModelErrorResourceShape, "revoked"
	state, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &model, CredentialError: &revoked})
	require.NoError(t, err)
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	f.cfg.X.Accounts[0].OwnedReads = true
	require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
	f.cfg.Analytics.AutoBuildCache = true
	out, err := f.runArchive(t, "--json", "--build-cache")
	require.NoError(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	require.NotNil(t, summary.Archive)
	a := summary.Archive
	assert.True(t, a.Preview)
	assert.Empty(t, a.JobID)
	assert.Nil(t, a.CapMicros)
	assert.Nil(t, a.RemainingMicros)
	assert.Equal(t, "example_owner", a.Username)
	assert.Equal(t, "from:example_owner -is:retweet", a.OwnQuery)
	assert.Equal(t, "to:example_owner -is:retweet", a.MentionsQuery)
	assert.Equal(t, "2020-01-01", a.After.Format("2006-01-02"))
	assert.False(t, a.Before.IsZero())
	assert.Equal(t, int64(1_500_000), a.PageReservationMicros)
	assert.True(t, a.TotalUnknown)
	assert.Zero(t, summary.SyncRunID)
	human, err := f.runArchive(t)
	require.NoError(t, err)
	for _, part := range []string{"from:example_owner -is:retweet", "to:example_owner -is:retweet", "After (exclusive): 2020-01-01", "Before (frozen):", "Conservative page reservation (100 results): USD 1.500000", "Total estimate: unknown", "historical handles are unverified", "old bare mentions, quotes and nested replies may be absent", "Unavailable content cannot be retrieved"} {
		assert.Contains(t, human, part)
	}
	assert.NotContains(t, human, "Synthetic editable source label")
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	assert.Equal(t, state, after)
	assert.Zero(t, f.calls.Load())
	assert.Zero(t, f.cacheCalls.Load())
	_, err = os.Stat(f.cfg.TokensDir())
	assert.ErrorIs(t, err, os.ErrNotExist)
	empty := ""
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, after.Revision, store.XControlPatch{OwnerID: "1001", Username: &empty})
	require.NoError(t, err)
	_, err = f.runArchive(t)
	require.ErrorContains(t, err, "reconnect")
	assert.Zero(t, f.calls.Load())
}
func TestSyncXArchiveLifetimeCapResumeRenameAndFreshJob(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	liveOwn := store.XWalk{Endpoint: "timeline", Fingerprint: "saved-live-own", Watermark: "99", Complete: true}
	liveMentions := store.XWalk{Endpoint: "mentions", Fingerprint: "saved-live-mentions", Watermark: "88", Complete: true}
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Own: &liveOwn, Mentions: &liveMentions})
	require.NoError(t, err)
	raw, err := f.runArchive(t, "--max-spend=0.15", "--json", "--build-cache")
	require.Error(t, err)
	var first xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &first))
	require.NotNil(t, first.Archive)
	a := first.Archive
	require.NotEmpty(t, a.JobID)
	assert.False(t, a.Preview)
	assert.Equal(t, "budget_exhausted", first.Code)
	assert.Equal(t, int64(1), first.MessagesAdded)
	assert.Equal(t, int64(150_000), *a.CapMicros)
	assert.Equal(t, int64(15_000), a.SpentMicros)
	assert.Zero(t, a.ReservedMicros)
	assert.Equal(t, int64(135_000), *a.RemainingMicros)
	assert.True(t, a.OwnComplete)
	assert.False(t, a.MentionsComplete)
	assert.True(t, a.TotalUnknown)
	ctx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	require.True(t, f.adapter.cacheJobs.waitContext(ctx))
	if f.st.IsPostgreSQL() {
		assert.Zero(t, f.cacheCalls.Load())
	} else {
		assert.Equal(t, int32(1), f.cacheCalls.Load())
	}
	f.mu.Lock()
	requests := append([]xArchiveCommandRequest{}, f.searches...)
	f.mu.Unlock()
	require.Len(t, requests, 1)
	assert.Equal(t, a.JobID, requests[0].JobID)
	assert.Equal(t, a.OwnQuery, requests[0].Query)
	assert.Equal(t, "10", requests[0].MaxResults)
	assert.Equal(t, "recency", requests[0].Order)
	assert.Equal(t, a.After.Format(time.RFC3339), requests[0].After)
	assert.Equal(t, a.Before.Format(time.RFC3339), requests[0].Before)
	beforeRepeat := f.calls.Load()
	raw, err = f.runArchive(t, "--max-spend=0.15", "--json")
	require.Error(t, err)
	var repeat xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &repeat))
	assert.Equal(t, a.JobID, repeat.Archive.JobID)
	assert.Equal(t, a.Before, repeat.Archive.Before)
	assert.Equal(t, a.SpentMicros, repeat.Archive.SpentMicros)
	assert.Equal(t, a.RemainingMicros, repeat.Archive.RemainingMicros)
	assert.Equal(t, beforeRepeat, f.calls.Load())
	f.mu.Lock()
	f.username = "renamed_owner"
	f.mu.Unlock()
	_, err = registerXAccountWithDeps(t.Context(), f.st, f.cfg, "1001", f.deps)
	require.NoError(t, err)
	raw, err = f.runArchive(t, "--max-spend=0.30", "--json")
	require.NoError(t, err)
	var resumed xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &resumed))
	require.NotNil(t, resumed.Archive)
	r := resumed.Archive
	assert.Equal(t, a.JobID, r.JobID)
	assert.Equal(t, a.Before, r.Before)
	assert.Equal(t, "example_owner", r.Username)
	assert.Equal(t, int64(300_000), *r.CapMicros)
	assert.Equal(t, int64(30_000), r.SpentMicros)
	assert.Equal(t, int64(270_000), *r.RemainingMicros)
	assert.True(t, r.OwnComplete)
	assert.True(t, r.MentionsComplete)
	assert.Equal(t, int64(1), resumed.MessagesAdded)
	f.mu.Lock()
	resumedRequests := append([]xArchiveCommandRequest{}, f.searches...)
	f.mu.Unlock()
	require.Len(t, resumedRequests, 2)
	assert.Equal(t, r.JobID, resumedRequests[1].JobID)
	assert.Equal(t, r.MentionsQuery, resumedRequests[1].Query)
	assert.Equal(t, a.Before.Format(time.RFC3339), resumedRequests[1].Before)

	beforeRepeat = f.calls.Load()
	human, err := f.runArchive(t, "--job-id="+r.JobID, "--max-spend=0.30")
	require.NoError(t, err)
	assert.Equal(t, beforeRepeat, f.calls.Load())
	for _, part := range []string{"Job ID: " + r.JobID, "Lifetime cap: USD 0.300000", "Lifetime estimated spent: USD 0.030000", "Lifetime unsettled reserved: USD 0.000000", "Remaining allowance: USD 0.270000", "Archive own walk complete: true", "Archive mentions walk complete: true"} {
		assert.Contains(t, human, part)
	}
	raw, err = f.runArchive(t, "--new-job", "--max-spend=0.30", "--json")
	require.NoError(t, err)
	var fresh xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &fresh))
	assert.NotEqual(t, r.JobID, fresh.Archive.JobID)
	assert.Equal(t, "renamed_owner", fresh.Archive.Username)
	assert.Equal(t, "from:renamed_owner -is:retweet", fresh.Archive.OwnQuery)
	assert.False(t, fresh.Archive.Before.Before(r.Before))
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	require.Len(t, after.Jobs, 2)
	assert.Equal(t, &liveOwn, after.Own)
	assert.Equal(t, &liveMentions, after.Mentions)
	// Selecting an existing job without a cap is still a local preview under holds.
	model, revoked := store.XModelErrorResourceShape, "revoked"
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, after.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &model, CredentialError: &revoked})
	require.NoError(t, err)
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	require.NoError(t, os.RemoveAll(f.cfg.TokensDir()))
	beforeRepeat = f.calls.Load()
	raw, err = f.runArchive(t, "--job-id="+r.JobID, "--json")
	require.NoError(t, err)
	var preview xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &preview))
	assert.True(t, preview.Archive.Preview)
	assert.Equal(t, r.JobID, preview.Archive.JobID)
	assert.Equal(t, r.Before, preview.Archive.Before)
	assert.Equal(t, r.Username, preview.Archive.Username)
	assert.Equal(t, r.CapMicros, preview.Archive.CapMicros)
	assert.Equal(t, r.SpentMicros, preview.Archive.SpentMicros)
	assert.Equal(t, beforeRepeat, f.calls.Load())
}
func TestSyncXArchiveZeroCapAndBelowMinimumMakeNoRequest(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	raw, err := f.runArchive(t, "--max-spend=0", "--json")
	require.Error(t, err)
	var zero xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &zero))
	require.NotNil(t, zero.Archive)
	assert.False(t, zero.Archive.Preview)
	require.NotEmpty(t, zero.Archive.JobID)
	require.NotNil(t, zero.Archive.CapMicros)
	assert.Zero(t, *zero.Archive.CapMicros)
	assert.Zero(t, f.calls.Load())
	raw, err = f.runArchive(t, "--max-spend=0.149999", "--json")
	require.Error(t, err)
	var low xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &low))
	assert.Equal(t, zero.Archive.JobID, low.Archive.JobID)
	assert.Equal(t, int64(149_999), *low.Archive.CapMicros)
	assert.Zero(t, low.Archive.SpentMicros)
	assert.Zero(t, low.Archive.ReservedMicros)
	assert.Zero(t, f.calls.Load())
}

func TestSyncXArchiveProviderErrorKeepsLifetimeReservationAndCache(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	f.mu.Lock()
	f.rejectMentions = true
	f.mu.Unlock()
	out, err := f.runArchive(t, "--max-spend=5", "--build-cache")
	require.Error(t, err)
	for _, text := range []string{"X archive partial:", "messages_added: 1", "Lifetime cap: USD 5.000000", "Lifetime estimated spent: USD 0.015000", "Lifetime unsettled reserved: USD 1.500000", "Remaining allowance: USD 3.485000", "Archive own walk complete: true", "Archive mentions walk complete: false", "Total estimate: unknown", "Resolve the reported provider, credential or coverage error before retrying", "unsettled requests remain reserved", "repeating a cap never renews it"} {
		assert.Contains(t, out, text)
	}
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	require.Len(t, state.Jobs, 1)
	assert.Equal(t, int64(15_000), state.Jobs[0].SpentMicros)
	assert.Equal(t, int64(1_500_000), state.Jobs[0].ReservedMicros)
	assert.Equal(t, int32(2), f.calls.Load())
	ctx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	require.True(t, f.adapter.cacheJobs.waitContext(ctx))
	if f.st.IsPostgreSQL() {
		assert.Zero(t, f.cacheCalls.Load())
	} else {
		assert.Equal(t, int32(1), f.cacheCalls.Load())
	}
}

func TestSyncXArchiveOwnAuthorBoundaryStillChargesOriginalResources(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	f.mu.Lock()
	f.foreignOwn = true
	f.mu.Unlock()
	raw, err := f.runArchive(t, "--max-spend=5", "--json")
	require.NoError(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(raw), &summary))
	require.NotNil(t, summary.Archive)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, int64(30_000), summary.Archive.SpentMicros)
	var ownPosts, replies int
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind("SELECT count(*) FROM messages WHERE source_id=? AND source_message_id='301'"), f.source.ID).Scan(&ownPosts))
	assert.Zero(t, ownPosts)
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind("SELECT count(*) FROM messages WHERE source_id=? AND source_message_id='302'"), f.source.ID).Scan(&replies))
	assert.Equal(t, 1, replies, "mixed-author to: results stay eligible")
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	require.Len(t, state.Jobs, 1)
	assert.Empty(t, state.Jobs[0].Own.Watermark, "a foreign author cannot publish owner progress")
	assert.Equal(t, "302", state.Jobs[0].Mentions.Watermark)
}

func TestSyncXArchiveScopeFailureKeepsArchiveModeAndAdvice(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	out, err := f.runArchive(t, "--job-id=synthetic-missing-job", "--max-spend=1.00")
	require.Error(t, err)
	assert.Contains(t, out, "X archive partial:")
	assert.Contains(t, out, "archive_scope")
	assert.NotContains(t, out, "X collection")
	assert.Contains(t, out, "existing job")
	out, err = f.runArchive(t, "--job-id=synthetic-missing-job", "--max-spend=1.00", "--json")
	require.Error(t, err)
	var summary xapi.ImportSummary
	require.NoError(t, json.Unmarshal([]byte(out), &summary))
	assert.Equal(t, "archive_scope", summary.Code)
	assert.Zero(t, summary.SyncRunID)
	assert.Zero(t, f.calls.Load())
}

func TestSyncXOrdinaryCollectionInstallsActualCleanup(t *testing.T) {
	f := newXCollectionFixture(t)
	now := time.Now().UTC()
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "201", AuthorID: "1002", ConversationID: "101", EditHistory: []string{"201"}, Text: "synthetic protected content", CreatedAt: f.at, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/users/1001/mentions" || r.URL.Query().Get("start_time") != "" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":"201","author_id":"1002","conversation_id":"101","created_at":%q,"text":"synthetic protected content"}],"includes":{"users":[{"id":"1002","username":"example_author","protected":true}]}}`, f.at.Format(time.RFC3339Nano))
	}))
	defer provider.Close()
	f.deps.apiBaseURL = provider.URL + "/2"
	f.deps.httpClient = provider.Client()
	_, _, err = f.run(t, f.clientConfig(t), "--source-id", fmt.Sprint(f.source.ID), "--json", "--no-build-cache")
	require.NoError(t, err)
	body, err := f.st.GetMessageBodyText(saved.MessageID)
	require.NoError(t, err)
	assert.Empty(t, body)
	pending, err := f.st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestSyncXArchivePreviewDoesNotResumePendingCleanup(t *testing.T) {
	f := newXArchiveCommandFixture(t)
	_, err := f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "999", "removed")
	require.NoError(t, err)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	var callbacks atomic.Int32
	cleaner.afterStore = func() error { callbacks.Add(1); return fmt.Errorf("synthetic pending cleanup") }
	f.adapter.xCleanup = cleaner
	before, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	_, err = f.runArchive(t, "--json")
	require.NoError(t, err)
	assert.Zero(t, callbacks.Load())
	pending, err := f.st.XCleanupPendingContext(t.Context())
	require.NoError(t, err)
	assert.True(t, pending)
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, f.calls.Load())
}
