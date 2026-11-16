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

func commitmentTestConfig() (peoplesweep.Config, peoplesweep.ProviderProfile) {
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.Enabled = true
	return cfg, peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
}

func TestApplyCommitmentOnlySweepWritesClaims(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	ctx := t.Context()
	f := newPersonSweepApplyFixture(t, "commitments", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	const first, second = "I will send the draft by August 31st.", "I will check the venue."
	text := first + " " + second
	id := persist("two-promises", text, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	start, end := int64(len(first)+1), int64(len(text))
	parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send the draft","due_on":"2026-08-31","due_phrase":"by August 31st"},
		{"source_id":%q,"direction":"owed_to","quote":%q,"start":%d,"end":%d,"summary":"Check the venue","due_on":null,"due_phrase":null}
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
	r.NotNil(items[0].DueOn)
	a.Equal("2026-08-31", *items[0].DueOn)
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
}

func TestCommitmentListPages(t *testing.T) {
	t.Run("open list skips closed history", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-closed-history", false)
		_, persist := newCommitmentTestConversation(t, f)
		persist("closed", "I will send report 0.", -time.Hour)
		persist("open", "I will send report 1.", -time.Hour+time.Second)
		cfg, _ := commitmentTestConfig()
		applyCommitmentTestSources(t, f, cfg.Commitments)
		items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 2, true)
		r.NoError(err)
		r.Len(items, 2)
		_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, personFactLedgerNow)
		r.NoError(err)
		items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, false)
		r.NoError(err)
		r.Len(items, 1)
		r.Equal("I will send report 1.", items[0].Quote)
		until := personFactLedgerNow.Add(time.Hour)
		openID := items[0].ID
		_, err = f.store.DecidePersonCommitmentContext(ctx, openID, LoopStateSnoozed, &until, personFactLedgerNow)
		r.NoError(err)
		items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, false)
		r.NoError(err)
		r.Empty(items)
		reopened, err := f.store.DecidePersonCommitmentContext(ctx, openID, LoopStateOpen, nil, personFactLedgerNow)
		r.NoError(err)
		r.Nil(reopened.SnoozedUntil)
		items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, false)
		r.NoError(err)
		r.Len(items, 1)
		r.Equal(openID, items[0].ID)
		r.Equal(LoopStateOpen, items[0].State)
	})
	t.Run("fresh before limit", func(t *testing.T) {
		r, a, ctx := require.New(t), assert.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-order", false)
		st := f.store
		_, persist := newCommitmentTestConversation(t, f)
		persist("promise-0", "I will send the draft.", -time.Hour)
		persist("promise-1", "I will send the report.", -100*24*time.Hour)
		cfg, _ := commitmentTestConfig()
		cfg.Commitments.Window = 200 * 24 * time.Hour
		applyCommitmentTestSources(t, f, cfg.Commitments)
		items, err := st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, false)
		r.NoError(err)
		r.Len(items, 1)
		a.Equal("I will send the draft.", items[0].Quote)
		items, err = st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, false)
		r.NoError(err)
		r.Len(items, 2)
		a.Equal(personfacts.StatusStale, items[1].Freshness)
	})
	t.Run("overdue before older undated promises", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-overdue-order", false)
		_, persist := newCommitmentTestConversation(t, f)
		for i := range 5 {
			persist(fmt.Sprintf("undated-%d", i), fmt.Sprintf("I will send report %d.", i), -48*time.Hour+time.Duration(i)*time.Minute)
		}
		const quote = "I will call by 2026-08-21."
		persist("overdue", quote, -24*time.Hour)
		cfg, profile := commitmentTestConfig()
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Len(window.Sources, 6)
		output := peoplesweep.ParsedCommitments{}
		for _, source := range window.Sources {
			promise := peoplesweep.CommitmentPromise{SourceID: source.ID, Direction: source.Direction, Quote: source.Text, Start: source.Start, End: source.Start + int64(len([]rune(source.Text))), Summary: "Keep the promise"}
			if source.Text == quote {
				promise.DueOn = new("2026-08-21")
				promise.DuePhrase = new("by 2026-08-21")
			}
			output.Promises = append(output.Promises, promise)
		}
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, output))
		r.NoError(err)
		items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, false)
		r.NoError(err)
		r.Len(items, 5)
		r.Equal(quote, items[0].Quote)
		r.NotNil(items[0].DueOn)
		r.Equal("2026-08-21", *items[0].DueOn)
	})
	t.Run("submillisecond times", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-page-times", false)
		_, persist := newCommitmentTestConversation(t, f)
		cfg, _ := commitmentTestConfig()
		for i := range 40 {
			persist(fmt.Sprintf("promise-%d", i), fmt.Sprintf("I will send report %d.", i), -time.Hour+time.Duration(i)*time.Microsecond)
		}
		applyCommitmentTestSources(t, f, cfg.Commitments)
		items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 40, false)
		r.NoError(err)
		r.Len(items, 40)
		seen := make(map[int64]bool)
		for _, item := range items {
			r.False(seen[item.ID], "a commitment must occur only once")
			seen[item.ID] = true
		}
	})
}

func applyCommitmentTestSources(t *testing.T, f personSweepApplyFixture, cfg peoplesweep.CommitmentConfig) {
	t.Helper()
	r := require.New(t)
	_, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(t.Context(), f.personID, profile, cfg, personFactLedgerNow)
	r.NoError(err)
	r.NotEmpty(window.Sources)
	output := peoplesweep.ParsedCommitments{}
	for _, source := range window.Sources {
		output.Promises = append(output.Promises, peoplesweep.CommitmentPromise{
			SourceID: source.ID, Direction: source.Direction, Quote: source.Text,
			Start: source.Start, End: source.Start + int64(len([]rune(source.Text))), Summary: "Keep the promise",
		})
	}
	_, err = f.store.ApplyPersonSweep(t.Context(), commitmentApplyRequest(t, f, window, cfg, output))
	r.NoError(err)
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
	request := f.request
	request.CommitmentOnly = true
	request.Generation.Policy.AllowSensitive = true
	request.Budget = personSweepApplyBudget()
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg, Output: parsed}
	if len(window.Sources) == 0 {
		return request
	}
	st := f.store
	profile := f.request.Generation.Policy.ProviderPolicyFingerprint
	reservation, err := st.ReservePersonSweepBudget(ctx, peoplesweep.BudgetReservationRequest{
		RunID: f.runID, AttemptID: f.attemptID, BatchOrdinal: 0, CallOrdinal: 0,
		Purpose: peoplesweep.ProviderCallPurposeCommitment, PersonID: f.personID,
		ProviderFingerprint: profile, UTCDate: personFactLedgerNow.Format(time.DateOnly), InputHash: strings.Repeat("c", 64),
		ItemCount: 1, EstimatedRequests: 1, EstimatedInputTokens: 3,
		EstimatedOutputTokens: 2, EstimatedCostMicroUSD: 5, Budget: request.Budget,
	})
	r.NoError(err)
	r.NoError(st.MarkPersonSweepBudgetStarted(ctx, reservation, f.lease))
	request.Generation.Provider, request.Generation.ProviderVersion = "fixture-provider", "provider-v1"
	request.Generation.Model, request.Generation.ModelVersion = "fixture-model", "model-v1"
	request.Batches = []peoplesweep.CompletedBatch{{
		Purpose: peoplesweep.ProviderCallPurposeCommitment, ReservationID: reservation.ID,
		InputHash: reservation.Request.InputHash, ProviderRequestID: f.attemptID,
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage: peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: 1}, UsageKnown: true,
		ActualCostMicroUSD: 3, Latency: time.Second,
	}}
	request.Usage = peoplesweep.Usage{Requests: 1, InputTokens: 3, OutputTokens: 2, EstimatedCostMicroUSD: 5}
	return request
}

func TestCommitmentDiscoveryOversizedMessages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		journal bool
	}{{"backfill", false}, {"journal", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-byte-boundary", false)
			_, persist := newCommitmentTestConversation(t, f)
			large := persist("large", strings.Repeat("x", 90<<10)+" I will call.", -2*time.Hour)
			var secondLarge int64
			if tc.journal {
				secondLarge = persist("second-large", strings.Repeat("y", 90<<10), -90*time.Minute)
			}
			small := persist("small", "I will call.", -time.Hour)
			second := persist("second", "I will write.", -time.Hour)
			_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ? OR id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-2*time.Hour)), large, secondLarge)
			r.NoError(err)
			_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ? OR message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-2*time.Hour)), large, secondLarge)
			r.NoError(err)
			if !tc.journal {
				_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM person_sweep_changes WHERE person_id = ?`), f.personID)
				r.NoError(err)
			}
			cfg, profile := commitmentTestConfig()
			cfg.Commitments.MaxBytes = 20
			cfg.Commitments.MaxItems = 1
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(small, window.Sources[0].MessageID)
			parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":0,"end":12,"summary":"Call","due_on":null,"due_phrase":null}]}`, window.Sources[0].ID)), window)
			r.NoError(err)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			due, err := f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.True(due, "an incomplete batch resumes before the minimum interval")
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, false)
			r.NoError(err)
			r.Len(items, 1)
			a.Equal(small, items[0].AnchorMessageID)
			_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.False(complete)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(second, window.Sources[0].MessageID)
			f = nextCommitmentTestAttempt(t, f, "-page")
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{}}))
			r.NoError(err)
			later := persist("later", "I will reply.", 0)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(later, window.Sources[0].MessageID)
			f = nextCommitmentTestAttempt(t, f, "-later")
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
			r.NoError(err)
			_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.False(complete)
			if !tc.journal {
				due, err := f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
				r.NoError(err)
				a.False(due, "unchanged limits leave an oversized message pending")
			}
			cfg.Commitments.MaxBytes = 100 << 10
			due, err = f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.True(due, "a raised byte limit schedules a retry immediately")
			laterNow := personFactLedgerNow.Add(31 * 24 * time.Hour)
			var foundLarge, foundSecond bool
			for i := range 10 {
				window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow)
				r.NoError(err)
				parsed := peoplesweep.ParsedCommitments{}
				for _, source := range window.Sources {
					if source.MessageID == large {
						a.True(source.MatchesQuote("I will call.", 92161, 92173))
						parsed, err = peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":92161,"end":92173,"summary":"Call","due_on":null,"due_phrase":null}]}`, source.ID)), window)
						r.NoError(err)
						foundLarge = true
					}
					if source.MessageID == secondLarge && tc.journal {
						foundSecond = true
					}
				}
				f = nextCommitmentTestAttempt(t, f, fmt.Sprintf("-larger-%d", i))
				request = commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
				request.Generation.ResolvedAt = laterNow
				_, err = f.store.ApplyPersonSweep(ctx, request)
				r.NoError(err)
				_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
				r.NoError(err)
				if complete {
					break
				}
			}
			a.True(foundLarge)
			if tc.journal {
				a.True(foundSecond)
			}

			items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, false)
			r.NoError(err)
			r.Len(items, 2)
			since, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			r.NotNil(since)
			a.Equal(laterNow.Add(-cfg.Commitments.Window), *since)
			a.True(complete)
			due, err = f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.False(due, "a successful retry clears the scheduling trigger")
		})
	}

	t.Run("recent change to old oversized message", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "oversized-mutation", false)
		_, persist := newCommitmentTestConversation(t, f)
		age := -40 * 24 * time.Hour
		large := persist("large", strings.Repeat("x", 200)+" I will call.", age)
		_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(age)), large)
		r.NoError(err)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-time.Hour)), large)
		r.NoError(err)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxBytes, cfg.Commitments.MaxItems = 30, 1
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Empty(window.Sources)
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
		r.NoError(err)
		_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
		r.NoError(err)
		r.False(complete)
	})
}

// A paid optional result is discarded after revocation without losing its accounting.
func TestCommitmentApplyRevokedConsentRetainsPaidAccounting(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-revoked", false)
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft."
	persist("promise", quote, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send the draft","due_on":null,"due_phrase":null}]}`, window.Sources[0].ID, quote, len(quote))), window)
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

func TestCommitmentDiscoveryCoalescesBackfillAndJournal(t *testing.T) {
	t.Run("narrow unfinished window", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "narrow-window", false)
		_, persist := newCommitmentTestConversation(t, f)
		persist("recent", "I will call.", -time.Hour)
		persist("old", "I will write.", -48*time.Hour)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxItems = 1
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Len(window.Sources, 1)
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
		r.NoError(err)
		cfg.Commitments.Window = 24 * time.Hour
		window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		for _, source := range window.Sources {
			r.NotEqual("I will write.", source.Text)
		}
	})
	t.Run("two pages", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "journal-repeat", false)
		_, persist := newCommitmentTestConversation(t, f)
		body := "I will call."
		first := persist("first", body, -2*time.Hour)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxItems = 1
		second := persist("second", "I will write.", -time.Hour)
		_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), body+" tomorrow.", first)
		r.NoError(err)
		var got []int64
		for i := range 2 {
			if i > 0 {
				f = nextCommitmentTestAttempt(t, f, fmt.Sprintf("-next-%d", i))
			}
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			got = append(got, window.Sources[0].MessageID)
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
			r.NoError(err)
			if i == 0 {
				_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
				r.NoError(err)
				r.False(complete)
			}
		}
		r.Equal([]int64{second, first}, got)
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Empty(window.Sources)
	})
	t.Run("1001 journal rows", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "journal-many", false)
		_, persist := newCommitmentTestConversation(t, f)
		id := persist("repeated", "I will call.", -time.Hour)
		for range 1001 {
			_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_sweep_changes
			(sequence, person_id, source_lane, change_kind, message_id, recorded_at)
			SELECT COALESCE(MAX(sequence), 0) + 1, ?, 'conversation_text', 'upsert', ?, ? FROM person_sweep_changes`),
				f.personID, id, f.store.dialect.TimestampParam(personFactLedgerNow))
			r.NoError(err)
		}
		_, err := f.store.db.ExecContext(ctx, `UPDATE person_sweep_change_clock SET sequence = (SELECT MAX(sequence) FROM person_sweep_changes) WHERE singleton = TRUE`)
		r.NoError(err)
		cfg, profile := commitmentTestConfig()
		var ids []int64
		for i := range 2 {
			if i > 0 {
				f = nextCommitmentTestAttempt(t, f, "-next")
			}
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			for _, source := range window.Sources {
				ids = append(ids, source.MessageID)
			}
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
		}
		r.Equal([]int64{id}, ids)
	})
}

func TestEditedCommitmentDecision(t *testing.T) {
	type promiseSpec struct {
		quote, summary string
		start          int64
	}
	type expectedPromise struct{ quote, state string }
	type rescanSpec struct {
		text          string
		promises      []promiseSpec
		want          []expectedPromise
		includeClosed bool
		mutation      string
	}
	type testCase struct {
		name, initialText string
		initialPromises   []promiseSpec
		rescans           []rescanSpec
		initialStates     []string
		sqliteOnly        bool
	}
	const quote = "I will send it."
	const first, second = "I will send it.", "I will call."
	longer := quote + " Thanks!"
	combined := first + " " + second
	cases := []testCase{
		{
			name: "omitted promise remains open", initialText: quote, initialStates: []string{LoopStateOpen},
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans:         []rescanSpec{{text: quote, want: []expectedPromise{{quote, LoopStateOpen}}}},
		},
		{
			name: "deleted repetition", initialText: quote + " " + quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}, {quote: quote, summary: "Send", start: int64(len(quote) + 1)}},
			rescans:         []rescanSpec{{text: quote, want: []expectedPromise{{quote, LoopStateOpen}}, includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send"}}}},
		},
		{
			name: "prepended text", initialText: quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans:         []rescanSpec{{text: "Note. " + quote, want: []expectedPromise{{quote, LoopStateDone}}, includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send", start: 6}}}},
		},
		{
			name: "unique, duplicated, then unique again clears decision", initialText: quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans: []rescanSpec{
				{text: quote + " " + quote},
				{text: quote, want: []expectedPromise{{quote, LoopStateOpen}}, includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send"}}},
			},
		},
		{
			name: "longer quote keeps decision", initialText: longer,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans:         []rescanSpec{{text: longer, want: []expectedPromise{{longer, LoopStateDone}}, includeClosed: true, promises: []promiseSpec{{quote: longer, summary: "Send"}}}},
		},
		{
			name: "ambiguous overlap retains decisions", initialText: combined, initialStates: []string{LoopStateDone, LoopStateSnoozed},
			initialPromises: []promiseSpec{{quote: first, summary: "Send"}, {quote: second, summary: "Call", start: int64(len(first) + 1)}},
			rescans:         []rescanSpec{{text: combined, want: []expectedPromise{{first, LoopStateDone}, {second, LoopStateSnoozed}}, includeClosed: true, promises: []promiseSpec{{quote: combined, summary: "Send and call"}}}},
		},
		{
			name: "new archive arrival", initialText: combined, initialStates: []string{LoopStateDone, LoopStateSnoozed},
			initialPromises: []promiseSpec{{quote: first, summary: "Send"}, {quote: second, summary: "Call", start: int64(len(first) + 1)}},
			rescans:         []rescanSpec{{text: combined, mutation: "new arrival", promises: []promiseSpec{{quote: combined, summary: "Send and call"}}, want: []expectedPromise{{combined, LoopStateOpen}}}},
		},
		{
			name: "same second reused ID", initialText: quote, sqliteOnly: true,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans: []rescanSpec{
				{text: quote, mutation: "correct sent time", includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send"}}, want: []expectedPromise{{quote, LoopStateDone}}},
				{text: quote, mutation: "reuse ID", promises: []promiseSpec{{quote: quote, summary: "Send"}}, want: []expectedPromise{{quote, LoopStateOpen}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "edited-promise", false)
			if tc.sqliteOnly && f.store.IsPostgreSQL() {
				t.Skip("SQLite reuses the highest row ID")
			}
			_, persist := newCommitmentTestConversation(t, f)
			id := persist("promise", tc.initialText, -time.Hour)
			cfg, profile := commitmentTestConfig()
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			initial := peoplesweep.ParsedCommitments{}
			for _, promise := range tc.initialPromises {
				initial.Promises = append(initial.Promises, peoplesweep.CommitmentPromise{
					SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
					Quote: promise.quote, Start: promise.start, End: promise.start + int64(len([]rune(promise.quote))), Summary: promise.summary,
				})
			}
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, initial)
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, false)
			r.NoError(err)
			r.Len(items, len(initial.Promises))
			states := tc.initialStates
			if len(states) == 0 {
				states = []string{LoopStateDone}
			}
			for i, state := range states {
				if state == LoopStateOpen {
					until := request.CompletedAt.Add(time.Hour)
					_, err = f.store.DecidePersonCommitmentContext(ctx, items[i].ID, LoopStateSnoozed, &until, request.CompletedAt)
					r.NoError(err)
					_, err = f.store.DecidePersonCommitmentContext(ctx, items[i].ID, LoopStateOpen, nil, request.CompletedAt)
					r.NoError(err)
					continue
				}
				var until *time.Time
				if state == LoopStateSnoozed {
					value := request.CompletedAt.Add(24 * time.Hour)
					until = &value
				}
				_, err = f.store.DecidePersonCommitmentContext(ctx, items[i].ID, state, until, request.CompletedAt)
				r.NoError(err)
			}
			recorded := window.Sources[0].RecordedTime

			currentText := tc.initialText
			for i, rescan := range tc.rescans {
				switch rescan.mutation {
				case "correct sent time":
					_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET sent_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-2*time.Hour)), id)
					r.NoError(err)
				case "new arrival", "reuse ID":
					if !f.store.IsPostgreSQL() {
						_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM messages WHERE id = ?`), id)
						r.NoError(err)
						r.Equal(id, persist("replacement", currentText, -time.Hour))
					}
					arrival := recorded
					if rescan.mutation == "new arrival" {
						arrival = arrival.Add(time.Hour)
					}
					_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(arrival), id)
					r.NoError(err)
				}
				if rescan.text != currentText {
					_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), rescan.text, id)
					r.NoError(err)
					currentText = rescan.text
				}
				_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitment_frontiers SET scope_fingerprint = '' WHERE person_id = ?`), f.personID)
				r.NoError(err)
				f = nextCommitmentTestAttempt(t, f, fmt.Sprintf("-rescan-%d", i))
				window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
				r.NoError(err)
				r.Len(window.Sources, 1)
				output := peoplesweep.ParsedCommitments{}
				for _, promise := range rescan.promises {
					output.Promises = append(output.Promises, peoplesweep.CommitmentPromise{
						SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo,
						Quote: promise.quote, Start: promise.start, End: promise.start + int64(len([]rune(promise.quote))), Summary: promise.summary,
					})
				}
				_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, output))
				r.NoError(err)
				if len(rescan.want) == 0 {
					continue
				}
				items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 10, rescan.includeClosed)
				r.NoError(err)
				for _, expected := range rescan.want {
					var found bool
					for _, item := range items {
						if item.Quote == expected.quote {
							found = true
							r.Equal(expected.state, item.State)
						}
					}
					r.True(found, "missing quote %q", expected.quote)
				}
				r.Len(items, len(rescan.want))
			}
		})
	}
}

func TestCommitmentScopeChanges(t *testing.T) {
	for _, expanded := range []string{"dates", "window", "batch"} {
		t.Run(expanded, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "scope-"+expanded, false)
			st := f.store
			_, persist := newCommitmentTestConversation(t, f)
			id := persist("old-promise", "I will send the report.", -10*24*time.Hour)
			old := personFactLedgerNow.Add(-10 * 24 * time.Hour)
			_, err := st.db.ExecContext(ctx, st.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), st.dialect.TimestampParam(old), id)
			r.NoError(err)
			_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), st.dialect.TimestampParam(old), id)
			r.NoError(err)
			cfg, profile := commitmentTestConfig()
			profile.SourceSince = personFactLedgerNow.Add(-24 * time.Hour).Format(time.DateOnly)
			if expanded != "dates" {
				profile.SourceSince = "2026-01-01"
				cfg.Commitments.Window = 24 * time.Hour
			}
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Empty(window.Sources)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
			_, err = st.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			_, complete, err := st.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
			r.NoError(err)
			a.True(complete)
			var through, observed int64
			err = st.db.QueryRowContext(ctx, `SELECT through_sequence, observed_sequence FROM person_commitment_frontiers WHERE person_id = ?`, f.personID).Scan(&through, &observed)
			r.NoError(err)
			a.Zero(through)
			a.Equal(window.ThroughSequence, observed)
			if expanded == "dates" {
				due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
				r.NoError(err)
				a.False(due)
			}
			due, err := st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.Window), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.True(due)
			switch expanded {
			case "dates":
				profile.SourceSince = "2026-01-01"
			case "window":
				cfg.Commitments.Window = 30 * 24 * time.Hour
			default:
				cfg.Commitments.MaxItems++
			}
			due, err = st.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			if expanded == "batch" {
				a.False(due, "batch sizing does not invalidate discovery progress")
			} else {
				a.True(due, "scope changes do not wait out the old minimum interval")
			}
			_, complete, err = st.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.Equal(expanded == "batch", complete)
			window, err = st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			if expanded == "batch" {
				a.Empty(window.Sources, "batch sizing preserves progress without widening eligibility")
			} else {
				r.Len(window.Sources, 1)
				a.Equal(id, window.Sources[0].MessageID)
			}
		})
	}
}

func TestCommitmentScopeRetrack(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "restart-retrack", false)
	_, persist := newCommitmentTestConversation(t, f)

	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Empty(window.Sources)
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})

	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)

	_, err = f.store.SetPersonTrackingContext(ctx, f.personID, false)
	r.NoError(err)
	persist("promise", "I will call.", -time.Hour)
	_, err = f.store.SetPersonTrackingContext(ctx, f.personID, true)
	r.NoError(err)
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	r.Equal("I will call.", window.Sources[0].Text)
}

func TestCommitmentDiscoveryRestartsWhenOwnershipChangesDuringScan(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "identity-during-discovery", false)
	source, persist := newCommitmentTestConversation(t, f)
	_, err := f.store.RemoveAccountIdentity(source, "owner@example.test")
	r.NoError(err)
	persist("promise", "I will call.", -time.Hour)
	persist("second", "I will write.", -time.Minute)
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.MaxItems = 1
	scope := peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments)
	priorSince := personFactLedgerNow.Add(-60 * 24 * time.Hour)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, -1, ?, ?)`), f.personID, f.store.dialect.TimestampParam(personFactLedgerNow), scope, f.store.dialect.TimestampParam(priorSince))
	r.NoError(err)
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Empty(window.Sources)
	r.NoError(f.store.AddAccountIdentity(source, "owner@example.test", "manual"))
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
	result, err := f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	r.Equal(peoplesweep.FailureArchiveGap, result.CommitmentFailureClass)
	r.NoError(f.store.db.QueryRowContext(ctx, f.store.Rebind(`SELECT scope_fingerprint FROM person_commitment_frontiers WHERE person_id = ?`), f.personID).Scan(&scope))
	r.Empty(scope)
	since, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
	r.NoError(err)
	r.NotNil(since)
	r.Equal(priorSince, *since)
	r.False(complete)
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	r.Equal("I will call.", window.Sources[0].Text)
	r.Zero(window.BackfillMessageID)
	r.Equal(personFactLedgerNow, window.SelectionAt)
}

func TestCommitmentOwnershipChangeWaitsForFrontierCommit(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "identity-frontier-lock", false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL row locks")
	}
	source, _ := newCommitmentTestConversation(t, f)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_commitment_frontiers (person_id, through_sequence, processed_at, backfill_message_id, scope_fingerprint, discovery_since) VALUES (?, 0, ?, -1, ?, ?)`), f.personID, f.store.dialect.TimestampParam(personFactLedgerNow), window.ScopeFingerprint, f.store.dialect.TimestampParam(personFactLedgerNow.Add(-cfg.Commitments.Window)))
	r.NoError(err)
	tx, err := f.store.db.BeginTx(ctx, nil)
	r.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var blockerPID int
	r.NoError(tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	request := f.request
	request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
	prepared := &preparedCommitmentSweep{snapshot: window.SnapshotSequence, through: window.ThroughSequence, backfillMessageID: window.BackfillMessageID, scope: window.ScopeFingerprint, selectionAt: window.SelectionAt}
	current, err := f.store.commitmentWindowCurrentTx(ctx, tx, request, prepared)
	r.NoError(err)
	r.True(current)
	done := make(chan error, 1)
	go func() { done <- f.store.AddAccountIdentity(source, "another@example.test", "manual") }()
	r.Eventually(func() bool {
		var waiting bool
		err := f.store.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE state = 'active' AND wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	r.NoError(f.store.saveCommitmentFrontierTx(ctx, tx, request, prepared))
	r.NoError(tx.Commit())
	r.NoError(<-done)
	var scope string
	r.NoError(f.store.db.QueryRowContext(ctx, f.store.Rebind(`SELECT scope_fingerprint FROM person_commitment_frontiers WHERE person_id = ?`), f.personID).Scan(&scope))
	r.Empty(scope)
}
