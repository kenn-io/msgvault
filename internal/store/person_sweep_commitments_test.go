package store

import (
	"database/sql"
	"encoding/json/v2"
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
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) { testNinthCommitmentPage(t, deleted) })
	}
}

func testNinthCommitmentPage(t *testing.T, deleted bool) {
	t.Helper()
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-anchor-pages", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	cfg.Commitments.Window = 24 * time.Hour
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

	// Complete the initial promise-discovery replay before importing later replies.
	f = nextCommitmentTestAttempt(t, f, "-initial-replay")
	replay, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, replay, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
	r.NoError(err)

	const completed = "I sent report 9."
	completionID := persist("ninth-fulfilment", completed, -time.Hour)
	otherID := persist("other-source", "Another update.", -time.Hour)
	first, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(first.Anchors, 8)
	r.Len(first.Sources, 2)
	a.NotEmpty(commitmentSourceID(first, completionID))
	a.Equal(first.Anchors[7].ID, first.AnchorAfterID)
	f = nextCommitmentTestAttempt(t, f, "-first")
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, first, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)

	due, err := st.PersonCommitmentsDue(ctx, f.personID, f.request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, "")
	r.NoError(err)
	a.True(due)
	newPromiseID := persist("new-activity", "I will send tomorrow's report.", 24*time.Hour)
	if deleted {
		_, err := st.db.ExecContext(ctx, st.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), otherID)
		r.NoError(err)
	}
	later := personFactLedgerNow.Add(25 * time.Hour)
	second, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, later, "UTC")
	r.NoError(err)
	r.Len(second.Anchors, 1)
	if deleted {
		r.Len(second.Sources, 1)
	} else {
		r.Len(second.Sources, 2)
	}
	a.Empty(commitmentSourceID(second, newPromiseID), "a newer promise cannot restart the pending ninth-anchor page")
	a.Equal(promises[8].Quote, second.Anchors[0].Quote)
	a.NotEmpty(commitmentSourceID(second, completionID))
	a.Equal(first.SelectionAt, second.SelectionAt)
	a.Equal(first.ThroughSequence, second.ThroughSequence)
	a.Zero(second.AnchorAfterID)
	f = nextCommitmentTestAttempt(t, f, "-second")
	f.request.Generation.ResolvedAt = later
	f.request.CompletedAt = later.Add(time.Minute)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, second, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{},
		Fulfilments: []peoplesweep.CommitmentFulfilment{{
			CommitmentID: second.Anchors[0].ID, SourceID: commitmentSourceID(second, completionID), Quote: completed, End: int64(len(completed)),
		}},
	}))
	r.NoError(err)
	var selection nullableTimestamp
	r.NoError(st.db.QueryRowContext(ctx, st.Rebind(`SELECT selection_at FROM person_commitment_frontiers WHERE person_id = ?`), f.personID).Scan(&selection))
	a.False(selection.Valid, "completed bounded cycles release their cutoff")
	next, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, later, "UTC")
	r.NoError(err)
	a.NotEmpty(commitmentSourceID(next, newPromiseID), "the next cycle admits newer promise evidence")
	a.Equal(later, next.SelectionAt)
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

func TestCommitmentByteLimitRetainsUnprocessedSourceBoundary(t *testing.T) {
	for _, journal := range []bool{false, true} {
		t.Run(fmt.Sprintf("journal=%t", journal), func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-byte-boundary", false)
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the report."
			persist("byte-bounded-promise", quote, -time.Hour)
			if !journal {
				_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM person_sweep_changes WHERE person_id = ?`), f.personID)
				r.NoError(err)
			}
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			cfg.Commitments.MaxBytes = len(quote) - 1
			r.NoError(cfg.Commitments.Validate(personSweepApplyBudget()))
			profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
				AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
			r.NoError(err)
			a.Empty(window.Sources)
			a.Zero(window.BackfillMessageID, "an excluded source must stay ahead of the historical cursor")
			if journal {
				r.Positive(window.SnapshotSequence)
				a.Less(window.ThroughSequence, window.SnapshotSequence, "an excluded source must stay ahead of the journal frontier")
			}
			request := f.request
			request.Generation.Policy.AllowSensitive = true
			request.Budget = personSweepApplyBudget()
			request.CommitmentOnly = true
			request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			next, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, request.CompletedAt, "UTC")
			r.NoError(err)
			a.Empty(next.Sources)
			a.Zero(next.BackfillMessageID, "saving an empty scan must preserve the excluded historical source")
			if journal {
				a.Less(next.ThroughSequence, next.SnapshotSequence, "saving an empty scan must preserve the excluded journal source")
			}
		})
	}
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
	due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, "")
	r.NoError(err)
	a.False(due)
	due, err = st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.Window), cfg.Commitments.MinInterval, cfg.Commitments.Window, "")
	r.NoError(err)
	a.True(due)
}

func TestCommitmentAgendaReceiptLifecycle(t *testing.T) {
	f, commitment := newCommitmentAgendaReceiptFixture(t)
	st, now := f.store, f.request.CompletedAt
	r, a := require.New(t), assert.New(t)
	ctx := t.Context()
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	// Finish the first extraction's frozen replay before importing new work.
	f = nextCommitmentTestAttempt(t, f, "-initial-agenda-replay")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	candidates, err := st.ListCommitmentAgendaCandidatesContext(ctx, now, 8)
	r.NoError(err)
	r.Len(candidates, 1)
	_, err = st.SetPersonTrackingContext(ctx, f.personID, false)
	r.NoError(err)
	candidates, err = st.ListCommitmentAgendaCandidatesContext(ctx, now, 8)
	r.NoError(err)
	a.Empty(candidates, "untracked promises are not automatic candidates")
	_, err = st.SetPersonTrackingContext(ctx, f.personID, true)
	r.NoError(err)
	r.NoError(st.DeferCommitmentAgendaContext(ctx, commitment, now))
	candidates, err = st.ListCommitmentAgendaCandidatesContext(ctx, now, 1)
	r.NoError(err)
	a.Empty(candidates, "a deferred promise waits for its retry interval")
	_, persist := newCommitmentTestConversation(t, f)
	freshMessage := persist("fresh-agenda-promise", "I will book the venue.", -30*time.Minute)
	f = nextCommitmentTestAttempt(t, f, "-fresh-agenda")
	applyCommitmentTestSources(t, f, cfg.Commitments)
	candidates, err = st.ListCommitmentAgendaCandidatesContext(ctx, now, 1)
	r.NoError(err)
	r.Len(candidates, 1)
	a.Equal(freshMessage, candidates[0].AnchorMessageID, "fresh promises precede deferred retries")
	fresh := candidates[0]
	candidates, err = st.ListCommitmentAgendaCandidatesContext(ctx, now.Add(commitmentAgendaDeferFor), 2)
	r.NoError(err)
	r.Len(candidates, 2)
	a.Equal(fresh.ID, candidates[0].ID)
	a.Equal(commitment.ID, candidates[1].ID, "a mature deferred promise returns after fresh work")
	_, err = st.DecidePersonCommitmentContext(ctx, fresh.ID, LoopStateDone, nil, now)
	r.NoError(err)
	start, end := int(commitment.Start), int(commitment.End)
	evidence, err := kataevidence.New(st).Prepare(ctx, []kataevidence.Selector{{
		Kind: "message", MessageID: commitment.AnchorMessageID, StartRune: &start, EndRune: &end,
	}})
	r.NoError(err)
	r.Len(evidence, 1)
	item := CommitmentAgendaSuggestion{
		CommitmentID: commitment.ID, TargetKey: commitment.TargetKey,
		Quote: evidence[0].Excerpt,
		Request: kataissues.CreateInput{Title: commitment.Summary, List: "commitments", PersonUID: "synthetic-uid",
			Evidence: []kataevidence.Reference{evidence[0].Reference}},
	}
	pending, err := st.EnqueueCommitmentAgendaContext(ctx, item)
	r.NoError(err)
	a.Equal(CommitmentAgendaPending, pending.State)
	key := pending.IdempotencyKey
	a.NotEmpty(key)
	item.Request.Title = "Updated before attempt"
	pending, err = st.EnqueueCommitmentAgendaContext(ctx, item)
	r.NoError(err)
	a.Equal(item.Request.Title, pending.Request.Title)
	a.Equal(key, pending.IdempotencyKey)
	marked, err := st.MarkCommitmentAgendaAttemptContext(ctx, pending, now, true)
	r.NoError(err)
	r.True(marked, "tracked promises remain eligible for automatic filing")
	_, err = st.SetPersonTrackingContext(ctx, f.personID, false)
	r.NoError(err)
	marked, err = st.MarkCommitmentAgendaAttemptContext(ctx, pending, now, true)
	r.NoError(err)
	a.False(marked, "automatic retries recheck enrollment before external creation")
	batch, err := st.ListPendingCommitmentAgendaContext(ctx, now, 8)
	r.NoError(err)
	a.Empty(batch, "untracked receipts do not occupy an automatic batch")
	marked, err = st.MarkCommitmentAgendaAttemptContext(ctx, pending, now, false)
	r.NoError(err)
	a.True(marked, "explicit manual filing remains available after untracking")
	_, err = st.SetPersonTrackingContext(ctx, f.personID, true)
	r.NoError(err)
	until := now.Add(time.Hour)
	_, err = st.DecidePersonCommitmentContext(ctx, commitment.ID, LoopStateSnoozed, &until, now)
	r.NoError(err)
	batch, err = st.ListPendingCommitmentAgendaContext(ctx, now, 8)
	r.NoError(err)
	a.Empty(batch)
	marked, err = st.MarkCommitmentAgendaAttemptContext(ctx, pending, now, false)
	r.NoError(err)
	r.True(marked)
	item.Request.Title = "Changed after attempt"
	frozen, err := st.EnqueueCommitmentAgendaContext(ctx, item)
	r.NoError(err)
	a.Equal(pending.Request.Title, frozen.Request.Title)
	a.Equal(key, frozen.IdempotencyKey)
	batch, err = st.ListPendingCommitmentAgendaContext(ctx, until, 8)
	r.NoError(err)
	r.Len(batch, 1)
	_, err = st.DecidePersonCommitmentContext(ctx, commitment.ID, LoopStateDone, nil, now)
	r.NoError(err)
	r.NoError(st.RetireCommitmentAgendaContext(ctx, key))
	reopened, err := st.ReopenCommitmentAgendaContext(ctx, key)
	r.NoError(err)
	a.Equal(key, reopened.IdempotencyKey)
	a.Equal(CommitmentAgendaPending, reopened.State)
	reopened, err = st.ReopenCommitmentAgendaContext(ctx, key)
	r.NoError(err)
	a.Equal(key, reopened.IdempotencyKey)
	a.Equal(CommitmentAgendaPending, reopened.State)
	r.NoError(st.RetireCommitmentAgendaContext(ctx, key))
	linked, err := st.LinkCommitmentAgendaContext(ctx, key, "issue-uid", "synthetic#7")
	r.NoError(err)
	a.Equal(CommitmentAgendaLinked, linked.State)
	a.Equal("synthetic#7", linked.IssueRef)
	again, err := st.LinkCommitmentAgendaContext(ctx, key, "issue-uid", "synthetic#7")
	r.NoError(err)
	a.Equal(linked, again)
}

func commitmentSourceID(window peoplesweep.CommitmentWindow, id int64) string {
	for _, source := range window.Sources {
		if source.MessageID == id {
			return source.ID
		}
	}
	return ""
}

func TestCommitmentWindowReplaysEarlierConsumedCompletion(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(fmt.Sprintf("detached=%t", detached), func(t *testing.T) {
			testCommitmentConsumedCompletion(t, detached)
		})
	}
}

func testCommitmentConsumedCompletion(t *testing.T, detached bool) {
	t.Helper()
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "late-promise", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	const quote, completed = "I will send the report.", "I sent the report."
	completion := persist("completion-first", completed, -time.Hour)
	first, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, first, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
	r.NoError(err)
	promise := persist("older-promise-later", quote, -2*time.Hour)
	f = nextCommitmentTestAttempt(t, f, "-promise")
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, promise), Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send report", End: int64(len(quote))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	if detached {
		// This is the detached lifecycle state left by an absorbed identity.
		_, err := st.db.ExecContext(ctx, st.Rebind(`UPDATE person_commitments SET current_claim_id = NULL WHERE person_id = ?`), f.personID)
		r.NoError(err)
		f = nextCommitmentTestAttempt(t, f, "-reattach")
		window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
		r.NoError(err)
		_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
			Promises: []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, promise), Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send report", End: int64(len(quote))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
		}))
		r.NoError(err)
	}
	f = nextCommitmentTestAttempt(t, f, fmt.Sprintf("-complete-%t", detached))
	window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Anchors, 1)
	r.NotEmpty(commitmentSourceID(window, completion))
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{{CommitmentID: window.Anchors[0].ID, SourceID: commitmentSourceID(window, completion), Quote: completed, End: int64(len(completed))}},
	}))
	r.NoError(err)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, true)
	r.NoError(err)
	r.Len(items, 1)
	r.NotNil(items[0].Fulfilment)
	a.Equal(completed, items[0].Fulfilment.Quote)
}

func newCommitmentAgendaReceiptFixture(t *testing.T) (personSweepApplyFixture, PersonCommitment) {
	t.Helper()
	r := require.New(t)
	f := newPersonSweepApplyFixture(t, "agenda-receipt", false)
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the report."
	persist("agenda-promise", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	window, err := f.store.LoadPersonCommitmentWindow(t.Context(), f.personID, peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	_, err = f.store.ApplyPersonSweep(t.Context(), commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send report", End: int64(len(quote))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
	r.NoError(err)
	items, err := f.store.ListPersonCommitmentsContext(t.Context(), f.personID, f.request.CompletedAt, 5, false)
	r.NoError(err)
	r.Len(items, 1)
	return f, items[0]
}

func TestCommitmentRequoteReconcilesLocators(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "split-promise", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const first, second = "I will send the report.", "I will book the venue."
	body := first + " " + second
	messageID := persist("two-promises", body, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: body, Summary: "Send report and book venue", End: int64(len(body))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
	r.NoError(err)
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, false)
	r.NoError(err)
	r.Len(items, 1)
	originalID := items[0].ID
	_, err = st.DecidePersonCommitmentContext(ctx, originalID, LoopStateDone, nil, f.request.CompletedAt)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-split")
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{
		{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: first, Summary: "Send report", End: int64(len(first))},
		{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: second, Summary: "Book venue", Start: int64(len(first) + 1), End: int64(len(body))},
	}, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
	r.NoError(err)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, f.request.CompletedAt, 5, true)
	r.NoError(err)
	r.Len(items, 2)
	a.Equal(originalID, items[0].ID)
	a.Equal(LoopStateDone, items[0].State)
	a.Equal(first, items[0].Quote)
	a.NotEqual(originalID, items[1].ID)
	a.Equal(LoopStateOpen, items[1].State)
	a.Equal(second, items[1].Quote)

	// Complete the frozen anchor replay before admitting the later edit.
	f = nextCommitmentTestAttempt(t, f, "-finish-replay")
	window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{}, Fulfilments: []peoplesweep.CommitmentFulfilment{},
	}))
	r.NoError(err)
	frontier := func() [5]int64 {
		var values [5]int64
		r.NoError(st.db.QueryRowContext(ctx, `SELECT through_sequence,backfill_message_id,anchor_after_id,anchor_through_sequence,anchor_backfill_id
 FROM person_commitment_frontiers WHERE person_id = ?`, f.personID).Scan(&values[0], &values[1], &values[2], &values[3], &values[4]))
		return values
	}
	previous := frontier()
	const edited = "I will send the revised report after the meeting."
	sequence, err := st.LatestPersonSweepChangeSequence(ctx)
	r.NoError(err)
	r.Equal(messageID, persist("two-promises", edited, -time.Hour))
	journal, err := st.ScanPersonSweepChanges(ctx, f.personID, sequence, 100)
	r.NoError(err)
	r.NotEmpty(journal)
	changes, err := st.BuildPersonSweepEvidenceStatusChanges(ctx, f.personID, journal)
	r.NoError(err)
	r.Len(changes, 3)
	f.request.Generation.ResolvedAt = f.request.Generation.ResolvedAt.Add(time.Minute)
	f.request.CompletedAt = f.request.Generation.ResolvedAt.Add(time.Minute)
	f = nextCommitmentTestAttempt(t, f, "-edited")
	window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, f.request.Generation.ResolvedAt, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
			Quote: edited, Summary: "Send revised report", End: int64(len(edited))}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	})
	request.Generation.EvidenceStatusChanges = changes
	result, err := st.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(peoplesweep.FailureInvalidOutput, result.CommitmentFailureClass)
	a.Equal(previous, frontier(), "unrepresented ambiguous output cannot consume source or anchor progress")
}

func TestCommitmentCitationIgnoresOrdinaryReferenceBeforeParsing(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	f := newPersonSweepApplyFixture(t, "ordinary-citation", false)
	_, live, err := f.store.currentCommitmentCitation(t.Context(), f.personID, peoplesweep.CommitmentOwedTo, personfacts.EvidenceInput{SourceRef: "message:42"})
	r.NoError(err)
	a.False(live)
}

// A paid optional result is discarded after revocation without losing its accounting.
func TestCommitmentApplyRevokedConsentRetainsPaidAccounting(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-revoked", false)
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft."
	persist("promise", quote, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
		AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
	}, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
		Promises:    []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send the draft", End: int64(len(quote))}},
		Fulfilments: []peoplesweep.CommitmentFulfilment{},
	})
	revoked, err := f.store.RevokePersonCommitmentConsent(ctx, request.Generation.Policy.ProviderPolicyFingerprint, "test")
	r.NoError(err)
	r.True(revoked)
	result, err := f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(1, result.Mutations.BatchRowsReconciled)
	var commitments, frontiers, requests int
	var failure string
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM person_commitments").Scan(&commitments))
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM person_commitment_frontiers").Scan(&frontiers))
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT actual_requests FROM person_sweep_batches WHERE attempt_id = ?", f.attemptID).Scan(&requests))
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT commitment_failure_class FROM person_sweep_attempts WHERE id = ?", f.attemptID).Scan(&failure))
	a.Zero(commitments)
	a.Zero(frontiers)
	a.Equal(1, requests)
	a.Equal(string(peoplesweep.FailurePolicy), failure)
}

// A format-valid date must not leave an active promise that its public readers
// cannot return or serialize. Rejection belongs to the optional lane.
func TestCommitmentExpiryRemainsRepresentableToReaders(t *testing.T) {
	for _, tc := range []struct {
		date       string
		wantStored int
		wantClass  peoplesweep.FailureClass
	}{
		{"2026-12-01", 1, ""},
		{"9999-12-31", 0, peoplesweep.FailureInvalidOutput},
	} {
		t.Run(tc.date, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-expiry-range", false)
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the draft."
			persist("promise", quote, -time.Hour)
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
				AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			}, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			due := tc.date
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
				Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
					Quote: quote, Summary: "Send the draft", End: int64(len(quote)), DueOn: &due}},
				Fulfilments: []peoplesweep.CommitmentFulfilment{},
			})
			result, err := f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			var stored int
			r.NoError(f.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM person_commitments").Scan(&stored))
			a.Equal(tc.wantStored, stored)
			a.Equal(tc.wantClass, result.CommitmentFailureClass)
			if tc.wantStored > 0 {
				items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, true)
				r.NoError(err)
				r.Len(items, tc.wantStored)
				_, err = json.Marshal(items)
				a.NoError(err, "public time fields must remain JSON representable")
			}
		})
	}
}
