package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type loopCLISetup struct {
	ctx                         context.Context
	fixture                     *storetest.Fixture
	service                     *loops.Service
	id, personID, participantID int64
	now                         time.Time
	stale                       atomic.Bool
	mu                          sync.Mutex
	queries                     map[string]url.Values
}

func newLoopCLISetup(t *testing.T) *loopCLISetup {
	t.Helper()
	f := storetest.New(t)
	s := &loopCLISetup{fixture: f, now: time.Now().UTC(), queries: map[string]url.Values{}}
	s.participantID = f.EnsureParticipant("reader@example.test", "Example Reader", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(s.participantID)
	require.NoError(t, err)
	s.personID = person.ID
	message := f.CreateMessage("cli-loop-anchor")
	evidence, err := json.Marshal([]loops.Evidence{{MessageID: message, ConversationID: f.ConvID, OccurredAt: s.now.Add(-48 * time.Hour), Subject: "A synthetic question"}})
	require.NoError(t, err)
	require.NoError(t, f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, Now: s.now, Verdict: store.LoopVerdict{Open: &store.LoopOpenDecision{
		TurnKey: fmt.Sprintf("turn:%d:reply_owed:%d", f.ConvID, message), Kind: loops.ReplyOwed, Status: "open", Channel: "email", ConversationID: f.ConvID, SourceID: f.Source.ID,
		AnchorMessageID: message, LastActivityMessageID: message, CounterpartyParticipantID: &s.participantID, CounterpartyPersonID: &s.personID,
		OwedSinceAt: s.now.Add(-48 * time.Hour), LastActivityAt: s.now.Add(-48 * time.Hour), OpensAt: s.now.Add(-24 * time.Hour), Summary: "Please answer the synthetic question", EvidenceJSON: evidence, RulesetVersion: 1, TurnMessageIDs: []int64{message},
	}}}))
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "loops-cli-test-key"
	cfg.Accounts = []config.AccountSchedule{{Email: f.Source.Identifier, Enabled: false}}
	s.service, err = loops.NewService(f.Store, cfg, func(context.Context) (freshness.ProjectionState, error) {
		if s.stale.Load() {
			return freshness.ProjectionState{}, nil
		}
		return freshness.ProjectionState{ComputedAt: &s.now}, nil
	})
	require.NoError(t, err)
	list, err := s.service.List(t.Context(), loops.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	s.id = list.Items[0].ID
	runner, err := loops.NewRunner(loops.RunnerOptions{Context: t.Context(), Run: func(context.Context) error { return nil }, Rebuild: func(context.Context) error { return nil }, StartScheduled: func() (scheduler.JobDisposition, error) { return scheduler.JobStarted, nil }})
	require.NoError(t, err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, LoopService: s.service, LoopRunner: runner, LoopState: f.Store.LoopDerivationStateContext, Logger: slog.New(slog.DiscardHandler)})
	router := server.Router()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries[r.URL.Path] = r.URL.Query()
		s.mu.Unlock()
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	s.ctx = withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpServer.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true}})
	return s
}

func executeLoopCLI(ctx context.Context, args ...string) (string, string, error) {
	command := newLoopsCommand()
	ensureSilenceUsageWrapped(command)
	var out, stderr bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs(args)
	err := command.ExecuteContext(ctx)
	return out.String(), stderr.String(), err
}

func TestLoopsCLIReadsAndFreshness(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s := newLoopCLISetup(t)
	id := strconv.FormatInt(s.id, 10)
	out, stderr, err := executeLoopCLI(s.ctx, "list", "--json")
	requirements.NoError(err)
	assertions.Contains(stderr, "WARNING: Sync is disabled for gmail source test@example.com.")
	var list loops.ListResponse
	requirements.NoError(json.Unmarshal([]byte(out), &list))
	requirements.Len(list.Items, 1)
	assertions.Equal("Please answer the synthetic question", list.Items[0].Summary)
	s.mu.Lock()
	query := s.queries["/api/v1/loops"]
	s.mu.Unlock()
	assertions.Equal("50", query.Get("limit"))
	assertions.Equal("open", query.Get("status"))
	out, _, err = executeLoopCLI(s.ctx, "show", id)
	requirements.NoError(err)
	assertions.Contains(out, "A synthetic question")
	assertions.Contains(out, s.now.Add(-48*time.Hour).Format(time.RFC3339))
	out, _, err = executeLoopCLI(s.ctx, "waiting")
	requirements.NoError(err)
	assertions.Contains(out, "Example Reader")
	assertions.Contains(out, "Please answer the synthetic question")
	assertions.Contains(out, "2d")
	s.mu.Lock()
	query = s.queries["/api/v1/loops/waiting"]
	s.mu.Unlock()
	assertions.Equal("10", query.Get("top"))
	out, _, err = executeLoopCLI(s.ctx, "waiting", "--top", "1", "--as-of", s.now.Add(time.Hour).Format(time.RFC3339), "--json")
	requirements.NoError(err)
	var waiting loops.WaitingResponse
	requirements.NoError(json.Unmarshal([]byte(out), &waiting))
	assertions.Equal(1, waiting.Count)
	assertions.Equal(s.now.Add(time.Hour).Truncate(time.Second), waiting.Freshness.AsOf)
	out, _, err = executeLoopCLI(s.ctx, "list", "--person", strconv.FormatInt(s.personID, 10), "--source-id", strconv.FormatInt(s.fixture.Source.ID, 10), "--kind", "reply_owed", "--limit", "1", "--include-snoozed", "--json")
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal([]byte(out), &list))
	assertions.Len(list.Items, 1)
	s.stale.Store(true)
	out, stderr, err = executeLoopCLI(s.ctx, "waiting", "--json")
	requirements.ErrorIs(err, loops.ErrLoopsStale)
	var stale *daemonclient.LoopsStaleError
	requirements.ErrorAs(err, &stale)
	var envelope loops.LoopsStaleResponse
	requirements.NoError(json.Unmarshal([]byte(out), &envelope))
	assertions.Equal(stale.Response, envelope)
	assertions.Contains(stderr, "WARNING: archive may be stale")
	assertions.Contains(stderr, "msgvault loops run")
	out, stderr, err = executeLoopCLI(s.ctx, "waiting", "--allow-stale", "--json")
	requirements.NoError(err)
	assertions.Contains(stderr, "WARNING: archive may be stale")
	requirements.NoError(json.Unmarshal([]byte(out), &waiting))
	assertions.True(waiting.Freshness.Stale)
	assertions.Equal(1, waiting.Count)
	for _, args := range [][]string{{"list", "--json"}, {"show", id, "--json"}, {"status", "--json"}} {
		out, stderr, err = executeLoopCLI(s.ctx, args...)
		requirements.NoError(err)
		assertions.True(json.Valid([]byte(out)))
		assertions.Contains(stderr, "WARNING: archive may be stale")
	}
}

func TestLoopsCLIActionsUseDaemonState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s := newLoopCLISetup(t)
	id := strconv.FormatInt(s.id, 10)
	for _, until := range []string{"2h", time.Now().Add(48 * time.Hour).Format("2006-01-02"), time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)} {
		before := time.Now()
		_, _, err := executeLoopCLI(s.ctx, "snooze", id, "--until", until)
		requirements.NoError(err)
		detail, err := s.service.Get(t.Context(), s.id)
		requirements.NoError(err)
		requirements.NotNil(detail.Loop.SnoozedUntil)
		minimumFuture := before.UTC().Truncate(time.Millisecond)
		assertions.False(detail.Loop.SnoozedUntil.Before(minimumFuture))
		if until == "2h" {
			// SQLite stores loop timestamps at the dialect's millisecond precision.
			minimum := before.Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
			assertions.False(detail.Loop.SnoozedUntil.Before(minimum))
			assertions.False(detail.Loop.SnoozedUntil.After(time.Now().Add(2 * time.Hour)))
		}
		_, _, err = executeLoopCLI(s.ctx, "unsnooze", id)
		requirements.NoError(err)
		detail, err = s.service.Get(t.Context(), s.id)
		requirements.NoError(err)
		assertions.Nil(detail.Loop.SnoozedUntil)
	}
	out, _, err := executeLoopCLI(s.ctx, "mute", "person", strconv.FormatInt(s.personID, 10), "--note", "Preview only", "--dry-run")
	requirements.NoError(err)
	assertions.Contains(out, "1")
	mutes, err := s.service.ListSuppressions(t.Context())
	requirements.NoError(err)
	assertions.Empty(mutes)
	_, _, err = executeLoopCLI(s.ctx, "done", id, "--note", "Answered elsewhere")
	requirements.NoError(err)
	detail, err := s.service.Get(t.Context(), s.id)
	requirements.NoError(err)
	assertions.Equal("closed", detail.Loop.Status)
	requirements.Len(detail.Loop.Evidence, 1)
	assertions.Equal("Answered elsewhere", detail.Loop.Evidence[0].OwnerNote)
	_, _, err = executeLoopCLI(s.ctx, "done", id)
	var apiErr *daemonclient.APIError
	requirements.ErrorAs(err, &apiErr)
	assertions.Equal("loop_already_closed", apiErr.Code)
	// Ensure the durable suppression ID differs from its target ID.
	dummy := s.fixture.EnsureParticipant("other@example.test", "Other Reader", "example.test")
	_, err = s.service.Suppress(t.Context(), loops.SuppressionRequest{TargetKind: "participant", ParticipantID: &dummy})
	requirements.NoError(err)
	for _, target := range []struct {
		kind string
		id   int64
	}{{"person", s.personID}, {"participant", s.participantID}, {"conversation", s.fixture.ConvID}} {
		_, _, err = executeLoopCLI(s.ctx, "mute", target.kind, strconv.FormatInt(target.id, 10), "--note", "No follow-up")
		requirements.NoError(err)
		out, _, err = executeLoopCLI(s.ctx, "mutes", "--json")
		requirements.NoError(err)
		var entries loops.SuppressionListResponse
		requirements.NoError(json.Unmarshal([]byte(out), &entries))
		requirements.Len(entries.Items, 2)
		assertions.Equal("No follow-up", entries.Items[1].Note)
		if target.kind == "person" {
			assertions.NotEqual(target.id, entries.Items[1].ID)
		}
		_, _, err = executeLoopCLI(s.ctx, "unmute", target.kind, strconv.FormatInt(target.id, 10))
		requirements.NoError(err)
		mutes, err = s.service.ListSuppressions(t.Context())
		requirements.NoError(err)
		requirements.Len(mutes, 1)
		assertions.Equal(&dummy, mutes[0].ParticipantID)
	}
	for _, command := range []string{"run", "rebuild"} {
		out, _, err = executeLoopCLI(s.ctx, command)
		requirements.NoError(err)
		assertions.Contains(out, "started")
	}
	out, _, err = executeLoopCLI(s.ctx, "status")
	requirements.NoError(err)
	assertions.Contains(out, "Queue")
	assertions.Contains(out, "Applied through")
}

func TestLoopsCLIRejectsInvalidInputBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"show", "0"}, {"show", "9223372036854775808"}, {"done", "-1"}, {"snooze", "1"}, {"snooze", "1", "--until", "0s"}, {"snooze", "1", "--until", "-1h"}, {"snooze", "1", "--until", "2020-02-30"}, {"snooze", "1", "--until", "2000-01-01"},
		{"waiting", "--top", "0"}, {"waiting", "--top", "51"}, {"waiting", "--as-of", "tomorrow"}, {"list", "--person", "0"}, {"list", "--source-id", "-1"}, {"list", "--limit", "201"}, {"list", "--limit", "0"}, {"list", "--kind", "guess"}, {"list", "--status", "guess"}, {"mute", "person", "0"}, {"unmute", "conversation", "x"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			_, _, err := executeLoopCLI(t.Context(), args...)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "invocation state is required")
		})
	}
}

func TestLoopsCLITrackRejectsConflictingFlagsBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{{"track", "12", "--issue", "example#ab12", "--title", "Override"}, {"track", "12", "--issue", "example#ab12", "--list", "agenda"}} {
		_, _, err := executeLoopCLI(t.Context(), args...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--issue cannot be combined")
	}
}

func TestLoopsCLITrackChecksDaemonVersion(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.25.0"}`))
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	_, _, err := executeLoopCLI(ctx, "track", "12")
	require.ErrorContains(t, err, "3.26.0")
	assert.Zero(t, requests.Load())
}

// The root supports repeated execution in one process. Owner choices must be
// scoped to one invocation even when parsing or argument validation fails.
func TestLoopsCLIRepeatedRootExecutionResetsOwnerChoices(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	s := newLoopCLISetup(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	remote := invocationFromContext(s.ctx).cfg.Remote
	requirements.NoError(os.WriteFile(configPath, []byte(fmt.Sprintf("[remote]\nurl = %q\napi_key = %q\nallow_insecure = true\n[loops]\nenabled = false\n", remote.URL, remote.APIKey)), 0o600))
	root := newRootCommand()
	root.AddCommand(newLoopsCommand())
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	execute := func(args ...string) error {
		stdout.Reset()
		stderr.Reset()
		root.SetArgs(append([]string{"--home", home, "--config", configPath, "--no-log-file", "--log-level=warn", "loops"}, args...))
		return executeRootContext(t.Context(), root)
	}
	s.stale.Store(true)
	requirements.NoError(execute("waiting", "--allow-stale", "--json", "--top", "1", "--as-of", s.now.Format(time.RFC3339)))
	requirements.ErrorIs(execute("waiting"), loops.ErrLoopsStale)
	assertions.Empty(stdout.String(), "the previous JSON output flag must not carry over")
	s.mu.Lock()
	query := s.queries["/api/v1/loops/waiting"]
	s.mu.Unlock()
	assertions.Equal("false", query.Get("allow_stale"))
	assertions.Equal("10", query.Get("top"))
	assertions.Empty(query.Get("as_of"))
	for _, badArgs := range [][]string{
		{"waiting", "--allow-stale", "--json", "--top", "51"},
		{"waiting", "--allow-stale", "--json", "--top", "invalid"},
		{"waiting", "unexpected", "--allow-stale", "--json"},
	} {
		requirements.Error(execute(badArgs...))
		requirements.ErrorIs(execute("waiting"), loops.ErrLoopsStale)
		assertions.Empty(stdout.String())
	}
	s.stale.Store(false)
	requirements.Error(execute("list", "--person", strconv.FormatInt(s.personID, 10), "--source-id", strconv.FormatInt(s.fixture.Source.ID, 10), "--kind", "awaiting_reply", "--status", "closed", "--include-snoozed", "--limit", "1", "--cursor", "invalid", "--json"))
	requirements.NoError(execute("list"))
	assertions.Contains(stdout.String(), "Please answer the synthetic question")
	s.mu.Lock()
	query = s.queries["/api/v1/loops"]
	s.mu.Unlock()
	assertions.Equal("50", query.Get("limit"))
	assertions.Equal("open", query.Get("status"))
	for _, field := range []string{"person_id", "source_id", "kind", "include_snoozed", "cursor"} {
		assertions.Empty(query.Get(field))
	}
	id := strconv.FormatInt(s.id, 10)
	requirements.Error(execute("done", "invalid", "--note", "Old note"))
	requirements.NoError(execute("done", id))
	detail, err := s.service.Get(t.Context(), s.id)
	requirements.NoError(err)
	requirements.Len(detail.Loop.Evidence, 1)
	assertions.Empty(detail.Loop.Evidence[0].OwnerNote)
	requirements.NoError(execute("mute", "person", strconv.FormatInt(s.personID, 10), "--dry-run", "--note", "Old preview note"))
	requirements.NoError(execute("mute", "participant", strconv.FormatInt(s.participantID, 10)))
	items, err := s.service.ListSuppressions(t.Context())
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal("participant", items[0].TargetKind)
	assertions.Empty(items[0].Note)
	requirements.Error(execute("snooze", id, "--until", "invalid"))
	requirements.ErrorContains(execute("snooze", id), "required flag")
}

// This fake is the daemon HTTP boundary: output must retain fields from the
// daemon receipt, including cached status when live Kata is unavailable.
func TestLoopsCLITrackOutputAndCachedViews(t *testing.T) {
	checked := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	item := loops.Loop{ID: 12, Kind: loops.ReplyOwed, Summary: "Review draft", Status: "closed", ClosedBy: new("system"), Evidence: []loops.Evidence{}, Counterparty: loops.Counterparty{DisplayName: "Example Person"}, Kata: &loops.LoopKata{QualifiedRef: "example#ab12", Title: "Review draft", Status: "closed", CheckedAt: &checked, ClosedLoopAt: &checked, LastError: "Kata offline"}}
	detail := loops.DetailResponse{Loop: item, Freshness: freshness.Report{StaleReasons: []string{}, Warnings: []string{}, Sources: []freshness.SourceFreshness{}, Projections: map[string]*freshness.ProjectionFreshness{}}}
	result := loops.TrackResponse{DetailResponse: detail, Issue: loops.KataIssue{QualifiedRef: "example#ab12", Title: "Review draft", Status: "closed"}, Replayed: true, Note: "Evidence window used"}
	var tracked loops.TrackRequest
	var untracked atomic.Bool
	var conflict atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": "3.26.0"})
		case "/api/v1/loops/12/kata":
			if r.Method == http.MethodDelete {
				untracked.Store(true)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&tracked)) {
				return
			}
			if conflict.Load() {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "loop_already_tracked", "message": "Already tracked", "kata": item.Kata})
				return
			}
			_ = json.NewEncoder(w).Encode(result)
		case "/api/v1/loops/12":
			_ = json.NewEncoder(w).Encode(result.DetailResponse)
		case "/api/v1/loops":
			_ = json.NewEncoder(w).Encode(loops.ListResponse{Items: []loops.Loop{item}})
		case "/api/v1/loops/waiting":
			_ = json.NewEncoder(w).Encode(loops.WaitingResponse{Groups: []loops.WaitingGroup{{Counterparty: item.Counterparty, Loops: []loops.Loop{item}, LoopCount: 1}}})
		case "/api/v1/loops/status":
			_ = json.NewEncoder(w).Encode(loops.StatusResponse{State: store.LoopDerivationState{KataLastRunAt: &checked, KataLastSuccessAt: &checked, KataLastError: "Kata offline"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	out, stderr, err := executeLoopCLI(ctx, "track", "12", "--title", "Review draft", "--list", "agenda")
	require.NoError(t, err)
	assert.Contains(t, out, "example#ab12\tclosed\tReview draft (already tracked)")
	assert.Contains(t, out, "Loop 12")
	assert.Contains(t, stderr, "Evidence window used")
	assert.Equal(t, loops.TrackRequest{Title: "Review draft", List: "agenda"}, tracked)
	out, _, err = executeLoopCLI(ctx, "track", "12", "--json")
	require.NoError(t, err)
	var receipt loops.TrackResponse
	require.NoError(t, json.Unmarshal([]byte(out), &receipt))
	assert.Equal(t, result, receipt)
	for _, command := range []string{"show", "list", "waiting"} {
		args := []string{command}
		if command == "show" {
			args = append(args, "12")
		}
		out, _, err = executeLoopCLI(ctx, args...)
		require.NoError(t, err)
		assert.Contains(t, out, "example#ab12")
		assert.Contains(t, out, "closed")
		if command == "show" {
			assert.Contains(t, out, "Closed by: system (Kata example#ab12 closed)")
			assert.Contains(t, out, "checked 2026-10-17T10:00:00Z")
			assert.Contains(t, out, "Kata offline")
		} else {
			assert.Contains(t, out, "KATA")
		}
	}
	out, _, err = executeLoopCLI(ctx, "status")
	require.NoError(t, err)
	assert.Contains(t, out, "Kata last run: 2026-10-17T10:00:00Z")
	assert.Contains(t, out, "Kata last success:")
	assert.Contains(t, out, "Kata last error: Kata offline")
	conflict.Store(true)
	out, stderr, err = executeLoopCLI(ctx, "track", "12", "--issue", "example#zz99")
	require.Error(t, err)
	assert.Contains(t, stderr+out+err.Error(), "example#ab12")
	out, _, err = executeLoopCLI(ctx, "untrack", "12")
	require.NoError(t, err)
	assert.True(t, untracked.Load())
	assert.Contains(t, out, "Untracked loop 12")
}
