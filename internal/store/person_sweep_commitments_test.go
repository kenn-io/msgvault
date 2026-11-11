package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestApplyCommitmentOnlySweepPreservesDecisionAcrossRequote(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	ctx := t.Context()
	f := newPersonSweepApplyFixture(t, "commitments", false)
	st := f.store
	sourceID, persist := newCommitmentTestConversation(t, f)
	const first, second = "I will send the draft.", "I will check the venue."
	text := first + " " + second
	id := persist("two-promises", text, -time.Hour)
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	active, err := st.HasActivePersonInferenceConsent(ctx, profile)
	r.NoError(err)
	a.False(active)

	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
		AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
	}, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	start, end := int64(len(first)+1), int64(len(text))
	parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send the draft","due_on":null},
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":%d,"end":%d,"summary":"Check the venue","due_on":null}
	],"fulfilments":[]}`, window.Sources[0].ID, first, len(first), window.Sources[0].ID, second, start, end)), window)
	r.NoError(err)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
	result, err := st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Zero(result.Mutations.CursorRowsAdvanced)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, false)
	r.NoError(err)
	r.Len(items, 2)
	a.Equal(first, items[0].Quote)
	a.Equal(second, items[1].Quote)
	a.Equal(id, items[0].AnchorMessageID)
	claims, err := st.ListPersonFactClaimsContext(ctx, f.personID, personfacts.ClaimFilter{Limit: 10})
	r.NoError(err)
	r.Len(claims, 2)

	decision, err := st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	a.Equal(LoopStateDone, decision.State)
	// Both ordinary journal consumers can read the ledger after commitment
	// evidence has been added.
	change := peoplesweep.ArchiveChange{PersonID: f.personID, SourceID: sourceID, MessageID: id,
		SourceLane: peoplesweep.SourceConversationText, EvidenceEffect: peoplesweep.EvidenceEffectSourceEdited}
	changes, err := st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, []peoplesweep.ArchiveChange{change})
	r.NoError(err)
	r.Len(changes, 2)
	for _, change := range changes {
		a.False(change.Supported)
	}
	tombstones, err := st.personSweepTombstones(ctx, f.personID, change)
	r.NoError(err)
	a.Empty(tombstones)

	f = nextCommitmentTestAttempt(t, f, "-requote")
	// Shortening a valid citation keeps the existing done decision. The other
	// disjoint promise in the same original message stays open.
	requote := strings.TrimSuffix(first, ".")
	parsed, err = peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send the draft","due_on":null}
	],"fulfilments":[]}`, window.Sources[0].ID, requote, len(requote))), window)
	r.NoError(err)
	request = commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
	_, err = st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, false)
	r.NoError(err)
	r.Len(items, 1)
	a.Equal(second, items[0].Quote)
	history, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, true)
	r.NoError(err)
	r.Len(history, 2)
	a.Equal(decision.ID, history[0].ID)
	a.Equal(LoopStateDone, history[0].State)
	a.Equal(requote, history[0].Quote)

	_, err = st.DecidePersonCommitmentContext(ctx, decision.ID, LoopStateSnoozed, nil, request.CompletedAt)
	a.ErrorIs(err, ErrInvalidActivityRequest)

	t.Run("agenda receipt refreshes until attempted", func(t *testing.T) {
		testCommitmentAgendaReceipt(t, st, items[0], request.CompletedAt)
	})
}

func newCommitmentTestConversation(t *testing.T, f personSweepApplyFixture) (int64, func(string, string, time.Duration) int64) {
	t.Helper()
	r, ctx, st := require.New(t), t.Context(), f.store
	source, err := st.GetOrCreateSource("whatsapp", "commitment-test")
	r.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
	r.NoError(err)
	alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	r.NoError(err)
	r.NoError(st.AddAccountIdentity(source.ID, "owner@example.test", "manual"))
	thread, err := st.EnsureConversationWithType(source.ID, "direct", "direct_chat", "Planning")
	r.NoError(err)
	r.NoError(st.EnsureConversationParticipant(thread, owner, "member"))
	r.NoError(st.EnsureConversationParticipant(thread, alice, "member"))
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	_, err = st.db.ExecContext(ctx, `UPDATE person_inference_profiles SET allow_sensitive = TRUE,
		allowed_sources = '["conversation_text"]', source_since = '2026-01-01' WHERE fingerprint = ?`, profile)
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile, "test")
	r.NoError(err)
	return source.ID, func(key, text string, offset time.Duration) int64 {
		t.Helper()
		id, err := st.PersistMessageContext(ctx, &MessagePersistData{
			Message: &Message{
				SourceID: source.ID, ConversationID: thread, MessageType: "whatsapp", SourceMessageID: key,
				SenderID: sql.NullInt64{Int64: alice, Valid: true},
				SentAt:   sql.NullTime{Time: personFactLedgerNow.Add(offset), Valid: true},
			},
			BodyText: sql.NullString{String: text, Valid: true},
		})
		r.NoError(err)
		return id
	}
}

func nextCommitmentTestAttempt(t *testing.T, f personSweepApplyFixture, suffix string) personSweepApplyFixture {
	t.Helper()
	r, ctx, st := require.New(t), t.Context(), f.store
	_, err := st.EnsurePersonSweepWork(ctx, f.personID, true)
	r.NoError(err)
	lease, err := st.ClaimPersonSweep(ctx, peoplesweep.ClaimRequest{
		PersonID: f.personID, WorkerID: "commitment-page-worker", LeaseDuration: time.Hour,
	})
	r.NoError(err)
	r.NotNil(lease)
	f.lease, f.request.Lease = *lease, *lease
	f.attemptID += suffix
	f.request.AttemptID = f.attemptID
	envelopeHash, err := personSweepRawEnvelopeHash(f.request.CursorEnvelope)
	r.NoError(err)
	r.NoError(st.StartPersonSweepAttempt(ctx, peoplesweep.StartAttempt{
		ID: f.attemptID, RunID: f.runID, PersonID: f.personID, LeaseFence: lease.Fence,
		Mode: peoplesweep.RunIncremental, CursorEnvelope: f.request.CursorEnvelope,
		EnvelopeHash: envelopeHash, StartedAt: f.request.Generation.ResolvedAt,
	}))
	return f
}

func TestCommitmentWindowReplaysNewEvidenceForNinthOpenPromise(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-anchor-pages", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	var body strings.Builder
	var promises []peoplesweep.CommitmentPromise
	for i := range 9 {
		quote := fmt.Sprintf("I will send report %d.", i+1)
		start := int64(body.Len())
		body.WriteString(quote)
		promises = append(promises, peoplesweep.CommitmentPromise{
			Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: quote, Start: start, End: int64(body.Len()),
		})
		body.WriteByte(' ')
	}
	dueDate := personFactLedgerNow.AddDate(0, 0, 120).Format(time.DateOnly)
	promises[8].DueOn = &dueDate
	persist("nine-promises", strings.TrimSpace(body.String()), -2*time.Hour)
	initial, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(initial.Sources, 1)
	for i := range promises {
		promises[i].SourceID = initial.Sources[0].ID
	}
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, initial, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: promises, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
	r.NoError(err)

	const completed = "I sent report 9."
	persist("ninth-fulfilment", completed, -time.Hour)
	first, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(first.Anchors, 8)
	r.Len(first.Sources, 1)
	a.Equal(completed, first.Sources[0].Text)
	a.Equal(first.Anchors[7].ID, first.AnchorAfterID)
	f = nextCommitmentTestAttempt(t, f, "-first")
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, first, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)

	due, err := st.PersonCommitmentsDue(ctx, f.personID, f.request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window)
	r.NoError(err)
	a.True(due)
	persist("new-activity", "A newer message.", -30*time.Minute)
	second, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(second.Anchors, 1)
	r.Len(second.Sources, 1)
	a.Equal(promises[8].Quote, second.Anchors[0].Quote)
	a.Equal(first.Sources[0].MessageID, second.Sources[0].MessageID)
	a.Equal(first.ThroughSequence, second.ThroughSequence)
	a.Zero(second.AnchorAfterID)
	f = nextCommitmentTestAttempt(t, f, "-second")
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, second, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{},
		Fulfilments: []peoplesweep.CommitmentFulfilment{{
			CommitmentID: second.Anchors[0].ID, SourceID: second.Sources[0].ID, Quote: completed, End: int64(len(completed)),
		}},
	}))
	r.NoError(err)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 9, false)
	r.NoError(err)
	r.Len(items, 9)
	r.NotNil(items[8].Fulfilment)
	a.Equal(completed, items[8].Fulfilment.Quote)
	current, err := st.DecidePersonCommitmentContext(ctx, items[8].ID, LoopStateOpen, nil, f.request.CompletedAt)
	r.NoError(err)
	a.Nil(current.ReopenedAt)
	a.NotNil(current.Fulfilment)

	duringDatedPromise, err := st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow.AddDate(0, 0, 100), 9, false)
	r.NoError(err)
	r.Len(duringDatedPromise, 1)
	a.Equal(items[8].ID, duringDatedPromise[0].ID)
	_, err = st.DecidePersonCommitmentContext(ctx, items[8].ID, LoopStateDone, nil, items[8].ExpiresAt)
	a.ErrorIs(err, sql.ErrNoRows)
}
func TestCommitmentWindowReplaysSameBatchReplyForNewSnoozedPromise(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "new-snoozed-anchor", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const promise, reply = "I will send the report.", "I sent the report."
	promiseID := persist("new-promise", promise, -2*time.Hour)
	replyID := persist("same-batch-reply", reply, -time.Hour)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	first, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Empty(first.Anchors)
	r.Len(first.Sources, 2)
	sourceByMessage := func(window peoplesweep.CommitmentWindow, messageID int64) string {
		for _, source := range window.Sources {
			if source.MessageID == messageID {
				return source.ID
			}
		}
		return ""
	}
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, first, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{
			SourceID: sourceByMessage(first, promiseID), Direction: peoplesweep.CommitmentOwedTo,
			Quote: promise, Summary: "Send the report", End: int64(len(promise)),
		}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 2, false)
	r.NoError(err)
	r.Len(items, 1)
	until := f.request.CompletedAt.Add(48 * time.Hour)
	_, err = st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateSnoozed, &until, f.request.CompletedAt)
	r.NoError(err)
	next, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(next.Anchors, 1)
	a.Equal(items[0].ID, next.Anchors[0].ID)
	a.Equal(first.ThroughSequence, next.ThroughSequence)
	f = nextCommitmentTestAttempt(t, f, "-fulfilment")
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, next, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{},
		Fulfilments: []peoplesweep.CommitmentFulfilment{{
			CommitmentID: next.Anchors[0].ID, SourceID: sourceByMessage(next, replyID), Quote: reply, End: int64(len(reply)),
		}},
	}))
	r.NoError(err)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 2, true)
	r.NoError(err)
	r.Len(items, 1)
	r.NotNil(items[0].Fulfilment)
	a.Equal(LoopStateSnoozed, items[0].State)
	a.Equal(reply, items[0].Fulfilment.Quote)
}

func commitmentApplyRequest(t *testing.T, f personSweepApplyFixture, window peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, parsed peoplesweep.ParsedCommitments) peoplesweep.ApplyRequest {
	t.Helper()
	ctx, r := t.Context(), require.New(t)
	st := f.store
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	budget := personSweepApplyBudget()
	reservation, err := st.ReservePersonSweepBudget(ctx, peoplesweep.BudgetReservationRequest{
		RunID: f.runID, AttemptID: f.attemptID, BatchOrdinal: 0, CallOrdinal: 0,
		Purpose: peoplesweep.ProviderCallPurposeCommitment, PersonID: f.personID,
		ProviderFingerprint: profile, UTCDate: personFactLedgerNow.Format(time.DateOnly), InputHash: strings.Repeat("c", 64),
		ItemCount: 1, EstimatedRequests: 1, EstimatedInputTokens: 3,
		EstimatedOutputTokens: 2, EstimatedCostMicroUSD: 5, Budget: budget,
	})
	r.NoError(err)
	r.NoError(st.MarkPersonSweepBudgetStarted(ctx, reservation, f.lease))
	request := f.request
	request.Generation.Policy.AllowSensitive = true
	request.Generation.Provider, request.Generation.ProviderVersion = "fixture-provider", "provider-v1"
	request.Generation.Model, request.Generation.ModelVersion = "fixture-model", "model-v1"
	request.Budget = budget
	request.Batches = []peoplesweep.CompletedBatch{{
		Purpose: peoplesweep.ProviderCallPurposeCommitment, ReservationID: reservation.ID,
		InputHash: reservation.Request.InputHash, ProviderRequestID: f.attemptID,
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage: peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1}, UsageKnown: true,
		ActualCostMicroUSD: 3, Latency: time.Second,
	}}
	request.Usage = peoplesweep.Usage{Requests: 1, InputTokens: 3, OutputTokens: 2, EstimatedCostMicroUSD: 5}
	request.CommitmentOnly = true
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg, Output: parsed}
	return request
}

func TestEmptyCommitmentScanKeepsProviderFrontierAndWaitsForNewWork(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	ctx := t.Context()
	f := newPersonSweepApplyFixture(t, "empty-commitment-scan", false)
	st := f.store
	request := f.request
	profile := request.Generation.Policy.ProviderPolicyFingerprint
	_, err := st.db.ExecContext(ctx, `UPDATE person_inference_profiles SET allow_sensitive = TRUE,
		allowed_sources = '["conversation_text"]', source_since = '2026-01-01' WHERE fingerprint = ?`, profile)
	r.NoError(err)
	_, _, err = st.GrantPersonCommitmentConsent(ctx, profile, "test")
	r.NoError(err)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
		AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
	}, cfg.Commitments, request.Generation.ResolvedAt, "UTC")
	r.NoError(err)
	r.Empty(window.Sources)
	request.Generation.Policy.AllowSensitive = true
	request.Budget = personSweepApplyBudget()
	request.CommitmentOnly = true
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
	_, err = st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	var through, observed int64
	err = st.db.QueryRowContext(ctx, `SELECT through_sequence, observed_sequence FROM person_commitment_frontiers WHERE person_id = ?`, f.personID).Scan(&through, &observed)
	r.NoError(err)
	a.Zero(through)
	a.Equal(window.ThroughSequence, observed)
	due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window)
	r.NoError(err)
	a.False(due)
	due, err = st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.Window), cfg.Commitments.MinInterval, cfg.Commitments.Window)
	r.NoError(err)
	a.True(due)
}

func testCommitmentAgendaReceipt(t *testing.T, st *Store, commitment PersonCommitment, now time.Time) {
	t.Helper()
	r, a := require.New(t), assert.New(t)
	ctx := t.Context()
	start, end := int(commitment.Start), int(commitment.End)
	evidence, err := kataevidence.New(st).Prepare(ctx, []kataevidence.Selector{{
		Kind: "message", MessageID: commitment.AnchorMessageID, StartRune: &start, EndRune: &end,
	}})
	r.NoError(err)
	r.Len(evidence, 1)
	item := CommitmentAgendaSuggestion{
		CommitmentID: commitment.ID, TargetKey: commitment.TargetKey,
		DestinationKey: "first", Quote: evidence[0].Excerpt,
		Request: kataissues.CreateInput{Title: commitment.Summary, List: "commitments", PersonUID: "synthetic-uid",
			Evidence: []kataevidence.Reference{evidence[0].Reference}},
	}
	pending, err := st.EnqueueCommitmentAgendaContext(ctx, item)
	r.NoError(err)
	a.Equal(CommitmentAgendaPending, pending.State)
	key := pending.IdempotencyKey
	a.NotEmpty(key)
	item.DestinationKey = "second"
	pending, err = st.EnqueueCommitmentAgendaContext(ctx, item)
	r.NoError(err)
	a.Equal(item.DestinationKey, pending.DestinationKey)
	a.Equal(key, pending.IdempotencyKey)
	until := now.Add(time.Hour)
	_, err = st.DecidePersonCommitmentContext(ctx, commitment.ID, LoopStateSnoozed, &until, now)
	r.NoError(err)
	batch, err := st.ListPendingCommitmentAgendaContext(ctx, "second", now, 8)
	r.NoError(err)
	a.Empty(batch)
	marked, err := st.MarkCommitmentAgendaAttemptContext(ctx, pending, now, false)
	r.NoError(err)
	r.True(marked)
	item.DestinationKey = "third"
	frozen, err := st.EnqueueCommitmentAgendaContext(ctx, item)
	r.NoError(err)
	a.Equal("second", frozen.DestinationKey)
	a.Equal(key, frozen.IdempotencyKey)
	batch, err = st.ListPendingCommitmentAgendaContext(ctx, "third", until, 8)
	r.NoError(err)
	a.Empty(batch)
	_, err = st.DecidePersonCommitmentContext(ctx, commitment.ID, LoopStateDone, nil, now)
	r.NoError(err)
	r.NoError(st.RetireCommitmentAgendaContext(ctx, key))
	reopened, err := st.ReopenCommitmentAgendaContext(ctx, key)
	r.NoError(err)
	a.Equal(key, reopened.IdempotencyKey)
	a.Equal(CommitmentAgendaPending, reopened.State)
	_, err = st.ReopenCommitmentAgendaContext(ctx, key)
	r.NoError(err)
	r.NoError(st.RetireCommitmentAgendaContext(ctx, key))
	linked, err := st.LinkCommitmentAgendaContext(ctx, key, "issue-uid", "synthetic#7")
	r.NoError(err)
	a.Equal(CommitmentAgendaLinked, linked.State)
	a.Equal("synthetic#7", linked.IssueRef)
}
