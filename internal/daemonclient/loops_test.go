package daemonclient_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
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

func TestLoopDaemonClientActionsAndFreshness(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Now().UTC()
	participant := f.EnsureParticipant("person@example.test", "Example Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	message := f.CreateMessage("loop-client-anchor")
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Open: &store.LoopOpenDecision{
			TurnKey: fmt.Sprintf("turn:%d:reply_owed:%d", f.ConvID, message), Kind: loops.ReplyOwed,
			Status: "open", Channel: "email", ConversationID: f.ConvID, SourceID: f.Source.ID,
			AnchorMessageID: message, LastActivityMessageID: message, CounterpartyParticipantID: &participant,
			CounterpartyPersonID: &person.ID, OwedSinceAt: now.Add(-48 * time.Hour), LastActivityAt: now.Add(-48 * time.Hour),
			OpensAt: now.Add(-24 * time.Hour), Summary: "A synthetic question", EvidenceJSON: []byte(`[]`), RulesetVersion: 1, TurnMessageIDs: []int64{message},
		}},
	}))
	var stale atomic.Bool
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "loop-client-test-key"
	cfg.Accounts = []config.AccountSchedule{{Email: f.Source.Identifier, Enabled: false}}
	service, err := loops.NewService(f.Store, cfg, func(context.Context) (freshness.ProjectionState, error) {
		if stale.Load() {
			return freshness.ProjectionState{}, nil
		}
		return freshness.ProjectionState{ComputedAt: &now}, nil
	})
	requirements.NoError(err)
	runner, err := loops.NewRunner(loops.RunnerOptions{Context: t.Context(), Run: func(context.Context) error { return nil }, Rebuild: func(context.Context) error { return nil }, StartScheduled: func() (scheduler.JobDisposition, error) { return scheduler.JobStarted, nil }})
	requirements.NoError(err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, LoopService: service, LoopRunner: runner, LoopState: f.Store.LoopDerivationStateContext, Logger: slog.New(slog.DiscardHandler)})
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: httpServer.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, Context: t.Context()})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	list, err := client.ListOpenLoops(t.Context(), loops.ListOptions{PersonID: person.ID, Limit: 1})
	requirements.NoError(err)
	requirements.Len(list.Items, 1)
	id := list.Items[0].ID
	detail, err := client.GetOpenLoop(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("A synthetic question", detail.Loop.Summary)
	page, err := client.ListPersonOpenLoops(t.Context(), person.ID, 5)
	requirements.NoError(err)
	assertions.Len(page.Loops, 1)
	assertions.Equal(now, page.ComputedAt)
	waiting, err := client.GetWaitingLoops(t.Context(), loops.WaitingOptions{})
	requirements.NoError(err)
	assertions.Equal(1, waiting.Count)
	_, err = client.SnoozeOpenLoop(t.Context(), id, now.Add(time.Hour))
	requirements.NoError(err)
	list, err = client.ListOpenLoops(t.Context(), loops.ListOptions{})
	requirements.NoError(err)
	assertions.Empty(list.Items)
	_, err = client.UnsnoozeOpenLoop(t.Context(), id)
	requirements.NoError(err)
	preview, err := client.CreateLoopSuppression(t.Context(), loops.SuppressionRequest{TargetKind: "person", PersonID: &person.ID, DryRun: true})
	requirements.NoError(err)
	assertions.Equal(int64(1), preview.WouldClose)
	detail, err = client.CloseOpenLoop(t.Context(), id, "Handled elsewhere")
	requirements.NoError(err)
	assertions.Equal("closed", detail.Loop.Status)
	_, err = client.CloseOpenLoop(t.Context(), id, "")
	var apiError *daemonclient.APIError
	requirements.ErrorAs(err, &apiError)
	assertions.Equal("loop_already_closed", apiError.Code)
	muted, err := client.CreateLoopSuppression(t.Context(), loops.SuppressionRequest{TargetKind: "person", PersonID: &person.ID})
	requirements.NoError(err)
	requirements.NotNil(muted.Suppression)
	mutes, err := client.ListLoopSuppressions(t.Context())
	requirements.NoError(err)
	assertions.Len(mutes, 1)
	requirements.NoError(client.DeleteLoopSuppression(t.Context(), muted.Suppression.ID))
	for _, rebuild := range []bool{false, true} {
		run, err := client.RunLoops(t.Context(), rebuild)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobStarted, run.Disposition)
	}
	status, err := client.GetLoopsStatus(t.Context())
	requirements.NoError(err)
	assertions.False(status.Freshness.Stale)
	stale.Store(true)
	_, err = client.GetWaitingLoops(t.Context(), loops.WaitingOptions{})
	assertions.ErrorIs(err, daemonclient.ErrLoopsStale)
	var staleError *daemonclient.LoopsStaleError
	requirements.ErrorAs(err, &staleError)
	assertions.Equal("loops_stale", staleError.Response.Error)
	assertions.True(staleError.Response.Freshness.Stale)
	assertions.Contains(staleError.Response.Freshness.StaleReasons, "projection_missing:open_loops")
	waiting, err = client.GetWaitingLoops(t.Context(), loops.WaitingOptions{AllowStale: true})
	requirements.NoError(err)
	assertions.True(waiting.Freshness.Stale)
	list, err = client.ListOpenLoops(t.Context(), loops.ListOptions{})
	requirements.NoError(err)
	assertions.True(list.Freshness.Stale)
}
