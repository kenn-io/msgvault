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
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestApplyCommitmentOnlySweepWritesClaimsAndDecisions(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	ctx := t.Context()
	f := newPersonSweepApplyFixture(t, "commitments", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const first, second = "I will send the draft.", "I will check the venue."
	text := first + " " + second
	id := persist("two-promises", text, -time.Hour)
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	cfg.Commitments.MaxItems = 1
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
		AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
	}, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	start, end := int64(len(first)+1), int64(len(text))
	parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send the draft","due_on":null},
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":%d,"end":%d,"summary":"Check the venue","due_on":null}
	]}`, window.Sources[0].ID, first, len(first), window.Sources[0].ID, second, start, end)), window)
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
	for _, claim := range claims {
		a.Nil(claim.ValidUntil)
		var value personfacts.CommitmentValue
		r.NoError(json.Unmarshal(claim.Normalized.JSON, &value))
		a.Equal("owed_to", value.Direction)
		a.NotEmpty(value.Summary)
	}

	decision, err := st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	a.Equal(LoopStateDone, decision.State)
	decisionID := decision.ID
	f = nextCommitmentTestAttempt(t, f, "-repeat")
	cfg.Commitments.MaxBytes++
	window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{
		AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
	}, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	parsed.Promises[0].Quote = strings.TrimSuffix(first, ".")
	parsed.Promises[0].End--
	_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, parsed))
	r.NoError(err)
	repeated, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt.Add(120*24*time.Hour), 5, true)
	r.NoError(err)
	r.Len(repeated, 2)
	a.Equal(decisionID, repeated[0].ID)
	a.Equal(LoopStateDone, repeated[0].State)
}

func TestCommitmentDecisionSurvivesShiftedQuote(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		ambiguous   bool
	}{
		{"done", LoopStateDone, false},
		{"snoozed", LoopStateSnoozed, false},
		{"ambiguous", LoopStateDone, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "shifted-"+tc.name, false)
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the draft."
			id := persist("promise", quote, -time.Hour)
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			first := peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}}}
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, first)
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, false)
			r.NoError(err)
			r.Len(items, 1)
			var until *time.Time
			if tc.state == LoopStateSnoozed {
				value := request.CompletedAt.Add(24 * time.Hour)
				until = &value
			}
			decision, err := f.store.DecidePersonCommitmentContext(ctx, items[0].ID, tc.state, until, request.CompletedAt)
			r.NoError(err)
			body := "Update: " + quote
			if tc.ambiguous {
				body += " " + quote
			}
			_, err = f.store.db.ExecContext(ctx, f.store.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), body, id)
			r.NoError(err)
			f = nextCommitmentTestAttempt(t, f, "-edited")
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			promises := []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", Start: 8, End: 8 + int64(len(quote))}}
			if tc.ambiguous {
				start := int64(9 + len(quote))
				promises = append(promises, peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send another draft", Start: start, End: start + int64(len(quote))})
			}
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: promises}))
			r.NoError(err)
			items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, true)
			r.NoError(err)
			if tc.ambiguous {
				r.Len(items, 2)
				for _, item := range items {
					a.Equal(LoopStateOpen, item.State)
					a.NotEqual(decision.ID, item.ID)
				}
			} else {
				r.Len(items, 1)
				a.Equal(decision.ID, items[0].ID)
				a.Equal(tc.state, items[0].State)
				if until != nil {
					r.NotNil(items[0].SnoozedUntil)
					a.True(items[0].SnoozedUntil.Equal(*until))
				}
			}
		})
	}
}

func TestCommitmentListPrioritizesFreshBeforeLimit(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "promise-order", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	cfg.Commitments.Window = 200 * 24 * time.Hour
	for i, message := range []struct {
		quote string
		age   time.Duration
	}{{"I will send the draft.", -time.Hour}, {"I will send the report.", -100 * 24 * time.Hour}, {"I will call.", 48 * time.Hour}} {
		id := persist(fmt.Sprintf("promise-%d", i), message.quote, message.age)
		if i > 0 {
			f = nextCommitmentTestAttempt(t, f, "-older")
		}
		window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
		r.NoError(err)
		r.NotEmpty(commitmentSourceID(window, id))
		_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
			Promises: []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, id), Direction: peoplesweep.CommitmentOwedTo, Quote: message.quote, Summary: message.quote, End: int64(len(message.quote))}},
		}))
		r.NoError(err)
	}
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, false)
	r.NoError(err)
	r.Len(items, 1)
	r.Equal("I will send the draft.", items[0].Quote)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, false)
	r.NoError(err)
	r.Len(items, 3)
	var future *PersonCommitment
	for i := range items {
		if items[i].Quote == "I will call." {
			future = &items[i]
		}
	}
	r.NotNil(future)
	r.Equal(personfacts.StatusStale, future.Freshness)
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

func commitmentApplyRequest(t *testing.T, f personSweepApplyFixture, window peoplesweep.CommitmentWindow, cfg peoplesweep.CommitmentConfig, parsed peoplesweep.ParsedCommitments) peoplesweep.ApplyRequest {
	t.Helper()
	ctx, r := t.Context(), require.New(t)
	encoded, err := json.Marshal(parsed)
	r.NoError(err)
	parsed, err = peoplesweep.ParseCommitments(encoded, window)
	r.NoError(err)
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

func TestCommitmentDiscoverySkipsOversizeAndRetriesWithLargerLimit(t *testing.T) {
	for _, journal := range []bool{false, true} {
		t.Run(fmt.Sprintf("journal=%t", journal), func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-byte-boundary", false)
			_, persist := newCommitmentTestConversation(t, f)
			large := persist("large", strings.Repeat("x", 2100)+" I will call.", -2*time.Hour)
			small := persist("small", "I will call.", -time.Hour)
			second := persist("second", "I will write.", -time.Hour)
			if !journal {
				_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM person_sweep_changes WHERE person_id = ?`), f.personID)
				r.NoError(err)
			}
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			cfg.Commitments.MaxBytes = 20
			cfg.Commitments.MaxItems = 1
			profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(small, window.Sources[0].MessageID)
			parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":0,"end":12,"summary":"Call","due_on":null}]}`, window.Sources[0].ID)), window)
			r.NoError(err)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, false)
			r.NoError(err)
			r.Len(items, 1)
			a.Equal(small, items[0].AnchorMessageID)
			_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID)
			r.NoError(err)
			a.False(complete)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(second, window.Sources[0].MessageID)
			f = nextCommitmentTestAttempt(t, f, "-page")
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{}}))
			r.NoError(err)
			later := persist("later", "I will reply.", 0)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(later, window.Sources[0].MessageID)
			f = nextCommitmentTestAttempt(t, f, "-later")
			empty, err := peoplesweep.ParseCommitments([]byte(`{"promises":[]}`), window)
			r.NoError(err)
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, empty))
			r.NoError(err)
			_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID)
			r.NoError(err)
			a.False(complete)
			cfg.Commitments.MaxBytes = 2200
			laterNow := personFactLedgerNow.Add(31 * 24 * time.Hour)
			cfg.Commitments.MaxItems = 10
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 4)
			a.Equal(personFactLedgerNow, window.SelectionAt)
			var largeSource string
			for _, source := range window.Sources {
				if source.MessageID == large {
					a.True(source.MatchesQuote("I will call.", 2101, 2113))
					largeSource = source.ID
				}
			}
			r.NotEmpty(largeSource)
			f = nextCommitmentTestAttempt(t, f, "-larger")
			empty, err = peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":2101,"end":2113,"summary":"Call","due_on":null}]}`, largeSource)), window)
			r.NoError(err)
			request = commitmentApplyRequest(t, f, window, cfg.Commitments, empty)
			request.Generation.ResolvedAt = laterNow
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, false)
			r.NoError(err)
			r.Len(items, 2)
			since, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID)
			r.NoError(err)
			r.NotNil(since)
			a.Equal(laterNow.Add(-cfg.Commitments.Window), *since)
			a.True(complete)
			for i := range 11 {
				persist(fmt.Sprintf("pending-%d", i), "I will reply.", 0)
			}
			f = nextCommitmentTestAttempt(t, f, "-journal-page")
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 10)
			request = commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{}})
			request.Generation.ResolvedAt = laterNow
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID)
			r.NoError(err)
			a.False(complete)
		})
	}
}

func commitmentSourceID(window peoplesweep.CommitmentWindow, id int64) string {
	for _, source := range window.Sources {
		if source.MessageID == id {
			return source.ID
		}
	}
	return ""
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
	parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send the draft","due_on":null}]}`, window.Sources[0].ID, quote, len(quote))), window)
	r.NoError(err)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
	revoked, err := f.store.RevokePersonCommitmentConsent(ctx, request.Generation.Policy.ProviderPolicyFingerprint, "test")
	r.NoError(err)
	r.True(revoked)
	result, err := f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	a.Equal(1, result.Mutations.BatchRowsReconciled)
	var commitments, frontiers, requests int
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM person_commitments").Scan(&commitments))
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM person_commitment_frontiers").Scan(&frontiers))
	r.NoError(f.store.db.QueryRowContext(ctx, "SELECT actual_requests FROM person_sweep_batches WHERE attempt_id = ?", f.attemptID).Scan(&requests))
	a.Zero(commitments)
	a.Zero(frontiers)
	a.Equal(1, requests)
}
