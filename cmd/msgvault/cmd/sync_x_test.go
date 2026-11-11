package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
		if !assert.True(t, mentions || r.URL.Path == "/2/users/1001/tweets", r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
			_, _ = fmt.Fprint(w, `{"title":"Synthetic unavailable"}`)
			return
		}
		if start := r.URL.Query().Get("start_time"); start != "" {
			lower, err := time.Parse(time.RFC3339, start)
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !f.at.After(lower) {
				_, _ = fmt.Fprint(w, `{"meta":{"result_count":0}}`)
				return
			}
		}
		id, author, text, refs := "101", "1001", "owner constellation", ""
		if mentions {
			id, author, text, refs = "201", "1002", "reply constellation", `,"referenced_tweets":[{"type":"replied_to","id":"101"}]`
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"author_id":%q,"conversation_id":"101","created_at":%q,"text":%q,"note_tweet":{"text":%q}%s}],"includes":{"users":[{"id":%q,"username":"example_author","name":"Example Author","protected":false}]},"meta":{"result_count":1}}`, id, author, f.at.Format(time.RFC3339), text, text, refs, author)
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	cfg := f.clientConfig(t)
	out, _, err := f.run(t, cfg, "--source-id", strconv.FormatInt(f.source.ID, 10), "--after", f.at.Add(-time.Hour).Format(time.RFC3339), "--json")
	_testRequire.NoError(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testAssert.Equal(f.source.ID, summary.SourceID)
	_testAssert.Equal("complete", summary.Status)
	_testAssert.Equal(int64(2), summary.MessagesAdded)
	_testAssert.Zero(summary.MessagesUpdated)
	_testAssert.Equal(int64(2), summary.MessagesProcessed)
	_testAssert.True(summary.OwnComplete)
	_testAssert.True(summary.MentionsComplete)
	_testAssert.Equal("101", summary.OwnWatermark)
	_testAssert.Equal("201", summary.MentionsWatermark)
	_testAssert.Equal(int64(30_000), summary.RunEstimatedMicros)
	_testAssert.Equal(int64(3_000_000), summary.RunReservedMicros)
	_testAssert.Equal(int64(30_000), summary.EstimatedMicros)
	_testAssert.Equal(int64(3_000_000), summary.ReservedMicros)
	var messages int
	_testRequire.NoError(f.st.DB().QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	_testAssert.Equal(2, messages)
	_, err = os.Stat(cfg.DatabaseDSN())
	_testRequire.ErrorIs(err, os.ErrNotExist, "client must not create a second archive")
	_testAssert.Equal(int32(4), f.calls.Load())
}
func TestSyncXPartialErrorStillPrintsJSONAndQueuesCache(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	f.failMentions = true
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--json", "--build-cache")
	_testRequire.Error(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testAssert.Equal("partial", summary.Status)
	_testAssert.NotEmpty(summary.Code)
	_testAssert.True(summary.OwnComplete)
	_testAssert.False(summary.MentionsComplete)
	_testAssert.Equal(int64(1), summary.MessagesAdded)
	_testAssert.Positive(summary.ReservedMicros)
	ctx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer cancel()
	_testRequire.True(f.adapter.cacheJobs.waitContext(ctx))
	if f.st.IsPostgreSQL() {
		_testAssert.Zero(f.cacheCalls.Load())
	} else {
		_testAssert.Equal(int32(1), f.cacheCalls.Load())
	}
}
func TestSyncXExclusiveAfterAndInvalidSelection(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--after", f.at.Format(time.RFC3339), "--json")
	_testRequire.NoError(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testAssert.Zero(summary.MessagesAdded)
	before := f.calls.Load()
	wrong, err := f.st.GetOrCreateSource("gmail", "example@example.test")
	_testRequire.NoError(err)
	badOwner, err := f.st.GetOrCreateSource("x", "example_handle")
	_testRequire.NoError(err)
	retired, err := f.st.GetOrCreateSource("x", "1003")
	_testRequire.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind("INSERT INTO source_settings(merged_into_source_id,source_id,history_only,retired_at) VALUES(?,?,TRUE,CURRENT_TIMESTAMP) ON CONFLICT(source_id) DO UPDATE SET merged_into_source_id=excluded.merged_into_source_id,history_only=TRUE,retired_at=excluded.retired_at"), f.source.ID, retired.ID)
	_testRequire.NoError(err)
	for _, id := range []int64{wrong.ID, badOwner.ID, retired.ID, 999999} {
		_, _, err = f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(id, 10), "--json")
		_testRequire.Error(err)
	}
	_testAssert.Equal(before, f.calls.Load())
}
func TestSyncXReadModelAcknowledgementIsLocalAndPreservesState(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	model, credential := store.XModelErrorResourceShape, "revoked"
	spend := []store.XSpendBucket{{Month: "2026-10", SpentMicros: 123, ReservedMicros: 500}}
	reservations := []store.XReservation{{ID: "held", Kind: "lookup", Month: "2026-10", AmountMicros: 500, State: "unknown"}}
	walk := store.XWalk{Endpoint: "timeline", Fingerprint: "preserved", Watermark: "100"}
	jobs := []store.XArchiveJob{{ID: "closed-fixture", Fingerprint: "preserved-job", Username: "example_owner", CapMicros: 1000, SpentMicros: 50, Complete: true}}
	refresh := store.XRefreshState{Cursor: "700", Outstanding: 2, PendingRemoval: true}
	mentions := store.XWalk{Endpoint: "mentions", Fingerprint: "saved-mentions", Watermark: "90"}
	state, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &model, CredentialError: &credential, Own: &walk, Mentions: &mentions, Jobs: &jobs, Refresh: &refresh, SpendBuckets: &spend, Reservations: &reservations})
	_testRequire.NoError(err)
	// Static schema fixtures prove acknowledgement cannot erase suppression or
	// durable receipts; no future draft/send API is invoked.
	_, err = f.st.DB().Exec(f.st.Rebind("INSERT INTO x_post_versions(source_id,post_id,canonical_id,version_position,availability,suppressed_at) VALUES(?,'500','500',0,'removed',?)"), f.source.ID, f.at)
	_testRequire.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind("INSERT INTO x_reply_attempts(draft_id,attempt_id,source_id,owner_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,reserved_micros) VALUES('fixture-draft','fixture-attempt',?,'1001',7,'500','500','synthetic-digest','remote_unknown','held',500)"), f.source.ID)
	_testRequire.NoError(err)
	beforeTarget, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "500")
	_testRequire.NoError(err)
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
	lock, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_, _, err = f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--acknowledge-read-model", "--json")
	_testRequire.Error(err)
	_testRequire.NoError(lock.Release())
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--acknowledge-read-model", "--json")
	_testRequire.NoError(err)
	var result map[string]any
	_testRequire.NoError(json.Unmarshal([]byte(out), &result))
	_testAssert.Equal("read_model_acknowledged", result["status"])
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(after.ModelError)
	_testAssert.Equal(state.CredentialError, after.CredentialError)
	_testAssert.Equal(state.Own, after.Own)
	_testAssert.Equal(state.SpendBuckets, after.SpendBuckets)
	_testAssert.Equal(state.Reservations, after.Reservations)
	_testAssert.Equal(state.Generation, after.Generation)
	expected := state
	expected.ModelError = ""
	expected.Revision++
	_testAssert.Equal(expected, after)
	afterTarget, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "500")
	_testRequire.NoError(err)
	_testAssert.Equal(beforeTarget, afterTarget)
	var receiptState string
	var reserved int64
	_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind("SELECT state,reserved_micros FROM x_reply_attempts WHERE source_id=? AND draft_id='fixture-draft' AND attempt_id='fixture-attempt'"), f.source.ID).Scan(&receiptState, &reserved))
	_testAssert.Equal("remote_unknown", receiptState)
	_testAssert.Equal(int64(500), reserved)
	var runs int
	_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind("SELECT count(*) FROM sync_runs WHERE source_id=?"), f.source.ID).Scan(&runs))
	_testAssert.Zero(runs)
	_testAssert.Zero(f.calls.Load())
	_testAssert.Zero(f.cacheCalls.Load())
	_, err = os.Stat(f.cfg.TokensDir())
	_testRequire.ErrorIs(err, os.ErrNotExist)
}
func TestSyncXHTTPRejectsDelegatedAndUnsafeForms(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	valid := []string{"sync-x", "--source-id", strconv.FormatInt(f.source.ID, 10)}
	for _, req := range []api.CLIRunRequest{{Args: valid, Cwd: "/tmp"}, {Args: valid, Env: map[string]string{"MSGVAULT_DISCORD_TOKEN": "synthetic-secret"}}, {Args: append(append([]string{}, valid...), "--source-id=7")}, {Args: append(append([]string{}, valid...), "--token-file=/tmp/example")}} {
		data, err := json.Marshal(req)
		_testRequire.NoError(err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(data))
		_testRequire.NoError(err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
		resp, err := http.DefaultClient.Do(request)
		_testRequire.NoError(err)
		_testRequire.NoError(resp.Body.Close())
		_testAssert.Equal(http.StatusBadRequest, resp.StatusCode)
	}
	grantRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/agent-tokens", strings.NewReader(fmt.Sprintf(`{"label":"example-agent","permissions":["draft.create"],"source_ids":[%d]}`, f.source.ID)))
	_testRequire.NoError(err)
	grantRequest.Header.Set("Content-Type", "application/json")
	grantRequest.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	resp, err := http.DefaultClient.Do(grantRequest)
	_testRequire.NoError(err)
	data, err := io.ReadAll(resp.Body)
	_testRequire.NoError(err)
	_testRequire.NoError(resp.Body.Close())
	_testRequire.Equal(http.StatusCreated, resp.StatusCode, string(data))
	var grant struct {
		Secret string `json:"secret"`
	}
	_testRequire.NoError(json.Unmarshal(data, &grant))
	body, err := json.Marshal(api.CLIRunRequest{Args: valid})
	_testRequire.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	_testRequire.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	resp, err = http.DefaultClient.Do(request)
	_testRequire.NoError(err)
	_testRequire.NoError(resp.Body.Close())
	_testAssert.Equal(http.StatusBadRequest, resp.StatusCode)

	archiveBody, err := json.Marshal(api.CLIRunRequest{Args: append(append([]string{}, valid...), "--archive", "--after=2020-01-01")})
	_testRequire.NoError(err)
	archiveRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(archiveBody))
	_testRequire.NoError(err)
	archiveRequest.Header.Set("Content-Type", "application/json")
	archiveRequest.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	archiveResponse, err := http.DefaultClient.Do(archiveRequest)
	_testRequire.NoError(err)
	_testRequire.NoError(archiveResponse.Body.Close())
	_testAssert.Equal(http.StatusBadRequest, archiveResponse.StatusCode)
	// Runtime grant-decision proofs are reserved for add-account, never sync-x.
	proofReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	_testRequire.NoError(err)
	proofReq.Header.Set("Content-Type", "application/json")
	proofReq.Header.Set("X-Api-Key", f.cfg.Server.AuthenticationKey())
	proofReq.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, "synthetic-invalid-proof")
	resp, err = http.DefaultClient.Do(proofReq)
	_testRequire.NoError(err)
	_testRequire.NoError(resp.Body.Close())
	_testAssert.Equal(http.StatusBadRequest, resp.StatusCode)
	_testAssert.Zero(f.calls.Load())
}
func TestSyncXNeverFallsBackToSubprocess(t *testing.T) {
	f := newXCollectionFixture(t)
	for _, req := range []api.CLIRunRequest{{Args: []string{"sync-x", "--unknown"}}, {Args: []string{"sync-x", "--source-id", strconv.FormatInt(f.source.ID, 10), "--archive"}}, {Args: []string{"sync-x", "--source-id", strconv.FormatInt(f.source.ID, 10)}, GrantDecided: true}, {Args: []string{"sync-x", "--source-id", strconv.FormatInt(f.source.ID, 10), "--acknowledge-read-model"}}} {
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

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
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--json")
	stop() // Release the observer even when a RED scaffold never reaches HTTP.
	select {
	case hit := <-reached:
		_testRequire.True(hit, "collection did not reach the observable mentions request")
	case <-t.Context().Done():
		_testRequire.FailNow("test cancelled while waiting for request observer")
	}
	_testRequire.Error(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testAssert.Equal("interrupted", summary.Code)
	_testAssert.Equal(int64(1), summary.MessagesAdded)
	_testAssert.True(summary.OwnComplete)
	_testAssert.False(summary.MentionsComplete)
}

func TestSyncXHumanPartialCoverageAndNoCache(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	state, seedErr := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(seedErr)
	month := time.Now().UTC().Format("2006-01")
	priorSpend := []store.XSpendBucket{{Month: month, SpentMicros: 1_234_567, ReservedMicros: 12_000_000}}
	priorReservations := []store.XReservation{{ID: "preexisting-unknown", Kind: "lookup", Month: month, AmountMicros: 12_000_000, State: "unknown"}}
	_, seedErr = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", SpendBuckets: &priorSpend, Reservations: &priorReservations})
	_testRequire.NoError(seedErr)
	f.failMentions = true
	f.cfg.Analytics.AutoBuildCache = true
	out, _, err := f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--no-build-cache")
	_testRequire.Error(err)
	for _, text := range []string{"X collection partial:", "messages_processed: 1", "messages_added: 1", "messages_updated: 0", "own_complete: true", "mentions_complete: false", "Run estimated: USD 0.015000", "Run unsettled reserved: USD 4.500000", "Month-to-date estimated: USD 1.249567", "Month-to-date unsettled reserved: USD 16.500000", "code: provider_error"} {
		_testAssert.Contains(out, text)
	}
	_testAssert.Zero(f.cacheCalls.Load())
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
			_, _ = fmt.Fprintf(w, `{"data":{"id":"1001","username":%q,"name":"Example Owner","protected":false}}`, name)
			return
		}
		if !assert.Equal(t, "/2/tweets/search/all", r.URL.Path) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		assert.Empty(t, q.Get("since_id"))
		assert.Empty(t, q.Get("until_id"))
		assert.Empty(t, q.Get("pagination_token"))
		control, err := f.st.LoadXControlContext(r.Context(), f.source.ID)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
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
			_, writeErr := fmt.Fprint(w, `{"title":"Synthetic archive denial"}`)
			assert.NoError(t, writeErr)
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
		_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"author_id":%q,"conversation_id":"301","created_at":%q,"text":%q,"note_tweet":{"text":%q}%s}],"includes":{"users":[{"id":%q,"username":%q,"name":"Example Author","protected":false}]},"meta":{"result_count":1}}`, id, author, f.at.Format(time.RFC3339), text, text, refs, author, name)
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
	args := []string{"--source-id", strconv.FormatInt(f.source.ID, 10), "--archive", "--after=2020-01-01"}
	out, _, err := f.run(t, f.clientConfig(t), append(args, extra...)...)
	return out, err
}
func TestSyncXArchivePreviewUsesVerifiedCacheWithoutNetwork(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	_testRequire.NoError(f.st.UpdateSourceDisplayName(f.source.ID, "Synthetic editable source label"))
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	model, revoked := store.XModelErrorResourceShape, "revoked"
	state, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &model, CredentialError: &revoked})
	_testRequire.NoError(err)
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	f.cfg.X.Accounts[0].OwnedReads = true
	_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
	f.cfg.Analytics.AutoBuildCache = true
	out, err := f.runArchive(t, "--json", "--build-cache")
	_testRequire.NoError(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testRequire.NotNil(summary.Archive)
	a := summary.Archive
	_testAssert.True(a.Preview)
	_testAssert.Empty(a.JobID)
	_testAssert.Nil(a.CapMicros)
	_testAssert.Nil(a.RemainingMicros)
	_testAssert.Equal("example_owner", a.Username)
	_testAssert.Equal("from:example_owner -is:retweet", a.OwnQuery)
	_testAssert.Equal("to:example_owner -is:retweet", a.MentionsQuery)
	_testAssert.Equal("2020-01-01", a.After.Format("2006-01-02"))
	_testAssert.False(a.Before.IsZero())
	_testAssert.Equal(int64(1_500_000), a.PageReservationMicros)
	_testAssert.True(a.TotalUnknown)
	_testAssert.Zero(summary.SyncRunID)
	human, err := f.runArchive(t)
	_testRequire.NoError(err)
	for _, part := range []string{"from:example_owner -is:retweet", "to:example_owner -is:retweet", "After (exclusive): 2020-01-01", "Before (frozen):", "Conservative page reservation (100 results): USD 1.500000", "Total estimate: unknown", "historical handles are unverified", "old bare mentions, quotes and nested replies may be absent", "Unavailable content cannot be retrieved"} {
		_testAssert.Contains(human, part)
	}
	_testAssert.NotContains(human, "Synthetic editable source label")
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(state, after)
	_testAssert.Zero(f.calls.Load())
	_testAssert.Zero(f.cacheCalls.Load())
	_, err = os.Stat(f.cfg.TokensDir())
	_testRequire.ErrorIs(err, os.ErrNotExist)
	empty := ""
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, after.Revision, store.XControlPatch{OwnerID: "1001", Username: &empty})
	_testRequire.NoError(err)
	_, err = f.runArchive(t)
	_testRequire.ErrorContains(err, "reconnect")
	_testAssert.Zero(f.calls.Load())
}
func TestSyncXArchiveLifetimeCapResumeRenameAndFreshJob(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	liveOwn := store.XWalk{Endpoint: "timeline", Fingerprint: "saved-live-own", Watermark: "99", Complete: true}
	liveMentions := store.XWalk{Endpoint: "mentions", Fingerprint: "saved-live-mentions", Watermark: "88", Complete: true}
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Own: &liveOwn, Mentions: &liveMentions})
	_testRequire.NoError(err)
	raw, err := f.runArchive(t, "--max-spend=0.15", "--json", "--build-cache")
	_testRequire.Error(err)
	var first xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &first))
	_testRequire.NotNil(first.Archive)
	a := first.Archive
	_testRequire.NotEmpty(a.JobID)
	_testAssert.False(a.Preview)
	_testAssert.Equal("budget_exhausted", first.Code)
	_testAssert.Equal(int64(1), first.MessagesAdded)
	_testAssert.Equal(int64(150_000), *a.CapMicros)
	_testAssert.Equal(int64(15_000), a.SpentMicros)
	_testAssert.Zero(a.ReservedMicros)
	_testAssert.Equal(int64(135_000), *a.RemainingMicros)
	_testAssert.True(a.OwnComplete)
	_testAssert.False(a.MentionsComplete)
	_testAssert.True(a.TotalUnknown)
	ctx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	_testRequire.True(f.adapter.cacheJobs.waitContext(ctx))
	if f.st.IsPostgreSQL() {
		_testAssert.Zero(f.cacheCalls.Load())
	} else {
		_testAssert.Equal(int32(1), f.cacheCalls.Load())
	}
	f.mu.Lock()
	requests := append([]xArchiveCommandRequest{}, f.searches...)
	f.mu.Unlock()
	_testRequire.Len(requests, 1)
	_testAssert.Equal(a.JobID, requests[0].JobID)
	_testAssert.Equal(a.OwnQuery, requests[0].Query)
	_testAssert.Equal("10", requests[0].MaxResults)
	_testAssert.Equal("recency", requests[0].Order)
	_testAssert.Equal(a.After.Format(time.RFC3339), requests[0].After)
	_testAssert.Equal(a.Before.Format(time.RFC3339), requests[0].Before)
	beforeRepeat := f.calls.Load()
	raw, err = f.runArchive(t, "--max-spend=0.15", "--json")
	_testRequire.Error(err)
	var repeat xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &repeat))
	_testAssert.Equal(a.JobID, repeat.Archive.JobID)
	_testAssert.Equal(a.Before, repeat.Archive.Before)
	_testAssert.Equal(a.SpentMicros, repeat.Archive.SpentMicros)
	_testAssert.Equal(a.RemainingMicros, repeat.Archive.RemainingMicros)
	_testAssert.Equal(beforeRepeat, f.calls.Load())
	f.mu.Lock()
	f.username = "renamed_owner"
	f.mu.Unlock()
	_, err = registerXAccountWithDeps(t.Context(), f.st, f.cfg, "1001", f.deps)
	_testRequire.NoError(err)
	raw, err = f.runArchive(t, "--max-spend=0.30", "--json")
	_testRequire.NoError(err)
	var resumed xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &resumed))
	_testRequire.NotNil(resumed.Archive)
	r := resumed.Archive
	_testAssert.Equal(a.JobID, r.JobID)
	_testAssert.Equal(a.Before, r.Before)
	_testAssert.Equal("example_owner", r.Username)
	_testAssert.Equal(int64(300_000), *r.CapMicros)
	_testAssert.Equal(int64(30_000), r.SpentMicros)
	_testAssert.Equal(int64(270_000), *r.RemainingMicros)
	_testAssert.True(r.OwnComplete)
	_testAssert.True(r.MentionsComplete)
	_testAssert.Equal(int64(1), resumed.MessagesAdded)
	f.mu.Lock()
	resumedRequests := append([]xArchiveCommandRequest{}, f.searches...)
	f.mu.Unlock()
	_testRequire.Len(resumedRequests, 2)
	_testAssert.Equal(r.JobID, resumedRequests[1].JobID)
	_testAssert.Equal(r.MentionsQuery, resumedRequests[1].Query)
	_testAssert.Equal(a.Before.Format(time.RFC3339), resumedRequests[1].Before)

	beforeRepeat = f.calls.Load()
	human, err := f.runArchive(t, "--job-id="+r.JobID, "--max-spend=0.30")
	_testRequire.NoError(err)
	_testAssert.Equal(beforeRepeat, f.calls.Load())
	for _, part := range []string{"Job ID: " + r.JobID, "Lifetime cap: USD 0.300000", "Lifetime estimated spent: USD 0.030000", "Lifetime unsettled reserved: USD 0.000000", "Remaining allowance: USD 0.270000", "Archive own walk complete: true", "Archive mentions walk complete: true"} {
		_testAssert.Contains(human, part)
	}
	raw, err = f.runArchive(t, "--new-job", "--max-spend=0.30", "--json")
	_testRequire.NoError(err)
	var fresh xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &fresh))
	_testAssert.NotEqual(r.JobID, fresh.Archive.JobID)
	_testAssert.Equal("renamed_owner", fresh.Archive.Username)
	_testAssert.Equal("from:renamed_owner -is:retweet", fresh.Archive.OwnQuery)
	_testAssert.False(fresh.Archive.Before.Before(r.Before))
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(after.Jobs, 2)
	_testAssert.Equal(&liveOwn, after.Own)
	_testAssert.Equal(&liveMentions, after.Mentions)
	// Selecting an existing job without a cap is still a local preview under holds.
	model, revoked := store.XModelErrorResourceShape, "revoked"
	_, err = f.st.UpdateXControlContext(t.Context(), f.source.ID, after.Revision, store.XControlPatch{OwnerID: "1001", ModelError: &model, CredentialError: &revoked})
	_testRequire.NoError(err)
	f.cfg.X.Accounts[0].MonthlyBudgetMicros = 0
	_testRequire.NoError(os.RemoveAll(f.cfg.TokensDir()))
	beforeRepeat = f.calls.Load()
	raw, err = f.runArchive(t, "--job-id="+r.JobID, "--json")
	_testRequire.NoError(err)
	var preview xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &preview))
	_testAssert.True(preview.Archive.Preview)
	_testAssert.Equal(r.JobID, preview.Archive.JobID)
	_testAssert.Equal(r.Before, preview.Archive.Before)
	_testAssert.Equal(r.Username, preview.Archive.Username)
	_testAssert.Equal(r.CapMicros, preview.Archive.CapMicros)
	_testAssert.Equal(r.SpentMicros, preview.Archive.SpentMicros)
	_testAssert.Equal(beforeRepeat, f.calls.Load())
}
func TestSyncXArchiveZeroCapAndBelowMinimumMakeNoRequest(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	raw, err := f.runArchive(t, "--max-spend=0", "--json")
	_testRequire.Error(err)
	var zero xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &zero))
	_testRequire.NotNil(zero.Archive)
	_testAssert.False(zero.Archive.Preview)
	_testRequire.NotEmpty(zero.Archive.JobID)
	_testRequire.NotNil(zero.Archive.CapMicros)
	_testAssert.Zero(*zero.Archive.CapMicros)
	_testAssert.Zero(f.calls.Load())
	raw, err = f.runArchive(t, "--max-spend=0.149999", "--json")
	_testRequire.Error(err)
	var low xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &low))
	_testAssert.Equal(zero.Archive.JobID, low.Archive.JobID)
	_testAssert.Equal(int64(149_999), *low.Archive.CapMicros)
	_testAssert.Zero(low.Archive.SpentMicros)
	_testAssert.Zero(low.Archive.ReservedMicros)
	_testAssert.Zero(f.calls.Load())
}

func TestSyncXArchiveProviderErrorKeepsLifetimeReservationAndCache(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	f.mu.Lock()
	f.rejectMentions = true
	f.mu.Unlock()
	out, err := f.runArchive(t, "--max-spend=5", "--build-cache")
	_testRequire.Error(err)
	for _, text := range []string{"X archive partial:", "messages_added: 1", "Lifetime cap: USD 5.000000", "Lifetime estimated spent: USD 0.015000", "Lifetime unsettled reserved: USD 1.500000", "Remaining allowance: USD 3.485000", "Archive own walk complete: true", "Archive mentions walk complete: false", "Total estimate: unknown", "Resolve the reported provider, credential or coverage error before retrying", "unsettled requests remain reserved", "repeating a cap never renews it"} {
		_testAssert.Contains(out, text)
	}
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(state.Jobs, 1)
	_testAssert.Equal(int64(15_000), state.Jobs[0].SpentMicros)
	_testAssert.Equal(int64(1_500_000), state.Jobs[0].ReservedMicros)
	_testAssert.Equal(int32(2), f.calls.Load())
	ctx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	_testRequire.True(f.adapter.cacheJobs.waitContext(ctx))
	if f.st.IsPostgreSQL() {
		_testAssert.Zero(f.cacheCalls.Load())
	} else {
		_testAssert.Equal(int32(1), f.cacheCalls.Load())
	}
}

func TestSyncXArchiveOwnAuthorBoundaryStillChargesOriginalResources(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	f.mu.Lock()
	f.foreignOwn = true
	f.mu.Unlock()
	raw, err := f.runArchive(t, "--max-spend=5", "--json")
	_testRequire.NoError(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(raw), &summary))
	_testRequire.NotNil(summary.Archive)
	_testAssert.Equal(int64(1), summary.MessagesAdded)
	_testAssert.Equal(int64(30_000), summary.Archive.SpentMicros)
	var ownPosts, replies int
	_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind("SELECT count(*) FROM messages WHERE source_id=? AND source_message_id='301'"), f.source.ID).Scan(&ownPosts))
	_testAssert.Zero(ownPosts)
	_testRequire.NoError(f.st.DB().QueryRow(f.st.Rebind("SELECT count(*) FROM messages WHERE source_id=? AND source_message_id='302'"), f.source.ID).Scan(&replies))
	_testAssert.Equal(1, replies, "mixed-author to: results stay eligible")
	state, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(state.Jobs, 1)
	_testAssert.Empty(state.Jobs[0].Own.Watermark, "a foreign author cannot publish owner progress")
	_testAssert.Equal("302", state.Jobs[0].Mentions.Watermark)
}

func TestSyncXArchiveScopeFailureKeepsArchiveModeAndAdvice(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	out, err := f.runArchive(t, "--job-id=synthetic-missing-job", "--max-spend=1.00")
	_testRequire.Error(err)
	_testAssert.Contains(out, "X archive partial:")
	_testAssert.Contains(out, "archive_scope")
	_testAssert.NotContains(out, "X collection")
	_testAssert.Contains(out, "select it with --job-id")
	out, err = f.runArchive(t, "--job-id=synthetic-missing-job", "--max-spend=1.00", "--json")
	_testRequire.Error(err)
	var summary xapi.ImportSummary
	_testRequire.NoError(json.Unmarshal([]byte(out), &summary))
	_testAssert.Equal("archive_scope", summary.Code)
	_testAssert.Zero(summary.SyncRunID)
	_testAssert.Zero(f.calls.Load())
}

func TestSyncXOrdinaryCollectionInstallsActualCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXCollectionFixture(t)
	now := time.Now().UTC()
	saved, err := f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "201", AuthorID: "1002", ConversationID: "101", EditHistory: []string{"201"}, Text: "synthetic protected content", CreatedAt: f.at, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
	_testRequire.NoError(err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2/users/1001/mentions" || r.URL.Query().Get("start_time") != "" {
			_, writeErr := fmt.Fprint(w, `{"data":[]}`)
			assert.NoError(t, writeErr)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"201","author_id":"1002","conversation_id":"101","created_at":%q,"text":"synthetic protected content"}],"includes":{"users":[{"id":"1002","username":"example_author","protected":true}]}}`, f.at.Format(time.RFC3339Nano))
	}))
	defer provider.Close()
	f.deps.apiBaseURL = provider.URL + "/2"
	f.deps.httpClient = provider.Client()
	_, _, err = f.run(t, f.clientConfig(t), "--source-id", strconv.FormatInt(f.source.ID, 10), "--json", "--no-build-cache")
	_testRequire.NoError(err)
	body, err := f.st.GetMessageBodyText(t.Context(), saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.False(pending)
}

func TestSyncXArchivePreviewDoesNotResumePendingCleanup(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXArchiveCommandFixture(t)
	_, err := f.st.RemoveXPostContentContext(t.Context(), f.source.ID, "999", "removed")
	_testRequire.NoError(err)
	cleaner := NewXContentCleanup(f.st, f.cfg, nil)
	var callbacks atomic.Int32
	cleaner.afterStore = func() error { callbacks.Add(1); return errors.New("synthetic pending cleanup") }
	f.adapter.xCleanup = cleaner
	before, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_, err = f.runArchive(t, "--json")
	_testRequire.NoError(err)
	_testAssert.Zero(callbacks.Load())
	pending, err := f.st.XCleanupPendingContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.True(pending)
	after, err := f.st.LoadXControlContext(t.Context(), f.source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(before, after)
	_testAssert.Zero(f.calls.Load())
}

func TestXSyncSummaryUsesCanonicalRefreshHint(t *testing.T) {
	_testAssert := assert.New(t)

	summary := xSyncSummaryText(&xapi.ImportSummary{
		Mode: "archive", SourceID: 42, Code: "refresh_required",
		Archive: &xapi.ArchiveSummary{JobID: "job-1"},
	})
	_testAssert.Contains(summary, "hint: run 'msgvault source x sync --source-id 42' without --archive before resuming this job.")
	_testAssert.Contains(summary, "Resume job job-1 with --job-id and an explicit --max-spend")
	_testAssert.NotContains(summary, "Resolve the reported provider, credential or coverage error")
}
