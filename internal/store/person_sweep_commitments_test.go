package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
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
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, 0, false)
	r.NoError(err)
	r.Len(items, 2)
	a.Equal(first, items[0].Quote)
	a.Equal(second, items[1].Quote)
	a.Equal(id, items[0].AnchorMessageID)
	r.NotNil(items[0].DueOn)
	a.Equal("2026-08-31", *items[0].DueOn)
	_, err = st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	_, err = st.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateOpen, nil, request.CompletedAt)
	r.NoError(err)
	items, err = st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, 0, false)
	r.NoError(err)
	r.Len(items, 2)
	a.Equal(LoopStateOpen, items[0].State)
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

func TestCommitmentYearlessNumericDeadline(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "numeric-deadline", false)
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft by 8/24."
	persist("promise", quote, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":%q,"start":0,"end":%d,"summary":"Send","due_on":"2026-08-24","due_phrase":"by 8/24"}]}`, window.Sources[0].ID, quote, len(quote))), window)
	r.NoError(err)
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, parsed))
	r.NoError(err)
	items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 10, 0, false)
	r.NoError(err)
	r.Len(items, 1)
	r.NotNil(items[0].DueOn)
	r.Equal("2026-08-24", *items[0].DueOn)
}

func TestCommitmentListPages(t *testing.T) {
	t.Run("closed history reuses owner lookup", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-closed-history", false)
		_, persist := newCommitmentTestConversation(t, f)
		for i := range 150 {
			persist(fmt.Sprintf("promise-%d", i), fmt.Sprintf("I will send report %d.", i), -time.Hour+time.Duration(i)*time.Second)
		}
		cfg, _ := commitmentTestConfig()
		cfg.Commitments.MaxItems = 150
		applyCommitmentTestSources(t, f, cfg.Commitments)
		items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 150, 0, true)
		r.NoError(err)
		r.Len(items, 150)
		for _, item := range items[:145] {
			_, err = f.store.DecidePersonCommitmentContext(ctx, item.ID, LoopStateDone, nil, personFactLedgerNow)
			r.NoError(err)
		}
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		ConfigureSQLLogging(SQLLogOptions{FullTrace: true, MaxStmtChars: 10000})
		t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}); slog.SetDefault(previous) })
		items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, 0, false)
		r.NoError(err)
		r.Len(items, 1)
		r.Equal("I will send report 145.", items[0].Quote)
		r.Equal(146, items[0].PagePosition)
		r.Equal(1, strings.Count(logs.String(), "SELECT p.id FROM account_identities ai"))
	})
	t.Run("decision between pages", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-page-decision", false)
		_, persist := newCommitmentTestConversation(t, f)
		for i := range 5 {
			persist(fmt.Sprintf("promise-%d", i), fmt.Sprintf("I will send report %d.", i), -time.Hour+time.Duration(i)*time.Second)
		}
		cfg, _ := commitmentTestConfig()
		applyCommitmentTestSources(t, f, cfg.Commitments)
		first, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 2, 0, false)
		r.NoError(err)
		r.Len(first, 2)
		_, err = f.store.DecidePersonCommitmentContext(ctx, first[0].ID, LoopStateDone, nil, personFactLedgerNow)
		r.NoError(err)
		second, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 2, first[1].PagePosition, false)
		r.NoError(err)
		r.Len(second, 2)
		r.Equal("I will send report 2.", second[0].Quote)
		r.Equal("I will send report 3.", second[1].Quote)
		first, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 2, 0, false)
		r.NoError(err)
		r.Len(first, 2)
		r.Equal(3, first[1].PagePosition)
		second, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 2, first[1].PagePosition, false)
		r.NoError(err)
		r.Len(second, 2)
		r.Equal("I will send report 3.", second[0].Quote)
		r.Equal("I will send report 4.", second[1].Quote)
	})
	t.Run("fresh before limit", func(t *testing.T) {
		r, a, ctx := require.New(t), assert.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "promise-order", false)
		st := f.store
		_, persist := newCommitmentTestConversation(t, f)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.Window = 200 * 24 * time.Hour
		for i, message := range []struct {
			quote string
			age   time.Duration
		}{{"I will send the draft.", -time.Hour}, {"I will send the report.", -100 * 24 * time.Hour}} {
			id := persist(fmt.Sprintf("promise-%d", i), message.quote, message.age)
			if i > 0 {
				f = nextCommitmentTestAttempt(t, f, "-older")
			}
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.NotEmpty(commitmentSourceID(window, id))
			_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{
				Promises: []peoplesweep.CommitmentPromise{{SourceID: commitmentSourceID(window, id), Direction: peoplesweep.CommitmentOwedTo, Quote: message.quote, Summary: message.quote, End: int64(len(message.quote))}},
			}))
			r.NoError(err)
		}
		items, err := st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 1, 0, false)
		r.NoError(err)
		r.Len(items, 1)
		a.Equal("I will send the draft.", items[0].Quote)
		items, err = st.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, 0, false)
		r.NoError(err)
		r.Len(items, 2)
		a.Equal(personfacts.StatusStale, items[1].Freshness)
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
		items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 40, 0, false)
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
	if len(window.Sources) == 0 {
		request := f.request
		request.CommitmentOnly = true
		request.Generation.Policy.AllowSensitive = true
		request.Budget = personSweepApplyBudget()
		request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg, Output: parsed}
		return request
	}
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

func TestCommitmentDiscoveryExpiresOversizedMessages(t *testing.T) {
	t.Run("expired outside window", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "commitment-retry-progress", false)
		_, persist := newCommitmentTestConversation(t, f)
		large := persist("large", strings.Repeat("x", 90<<10), -2*time.Hour)
		history := persist("history", "I will call.", 0)
		_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-2*time.Hour)), large)
		r.NoError(err)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-2*time.Hour)), large)
		r.NoError(err)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxBytes = 20
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Len(window.Sources, 1)
		r.Equal(history, window.Sources[0].MessageID)
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
		r.NoError(err)
		cfg.Commitments.MaxBytes = 100 << 10
		cfg.Commitments.MaxItems = 1
		window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow.Add(30*24*time.Hour-90*time.Minute))
		r.NoError(err)
		r.Empty(window.Sources, "expired messages must not be sent to the provider")
	})
	t.Run("recent change to old oversized message", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "commitment-old-edit", false)
		_, persist := newCommitmentTestConversation(t, f)
		id := persist("old-large", strings.Repeat("x", 200), -40*24*time.Hour)
		_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-40*24*time.Hour)), id)
		r.NoError(err)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-time.Hour)), id)
		r.NoError(err)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxBytes = 30
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Empty(window.Sources)
		request := f.request
		request.CommitmentOnly = true
		request.Generation.Policy.AllowSensitive = true
		request.Budget = personSweepApplyBudget()
		request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
		_, err = f.store.ApplyPersonSweep(ctx, request)
		r.NoError(err)
		_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
		r.NoError(err)
		r.False(complete)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_sweep_changes SET recorded_at = ? WHERE message_id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-40*24*time.Hour)), id)
		r.NoError(err)
		_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
		r.NoError(err)
		r.True(complete)
	})
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
			due, err := f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
			r.NoError(err)
			a.True(due, "an incomplete batch resumes before the minimum interval")
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, 0, false)
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
			empty, err := peoplesweep.ParseCommitments([]byte(`{"promises":[]}`), window)
			r.NoError(err)
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, empty))
			r.NoError(err)
			_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.False(complete)
			if !tc.journal {
				due, err := f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
				r.NoError(err)
				a.False(due, "unchanged limits leave an oversized message pending")
			}
			cfg.Commitments.MaxBytes = 100 << 10
			due, err = f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
			r.NoError(err)
			a.True(due, "a raised byte limit schedules a retry immediately")
			laterNow := personFactLedgerNow.Add(24 * time.Hour)
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow)
			r.NoError(err)
			r.Len(window.Sources, 1, "raising the byte limit retries oversized messages one at a time")
			a.Equal(laterNow, window.SelectionAt)
			var largeSource string
			for _, source := range window.Sources {
				if source.MessageID == large {
					a.True(source.MatchesQuote("I will call.", 92161, 92173))
					largeSource = source.ID
				}
			}
			r.NotEmpty(largeSource)
			if tc.journal {
				retry, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow)
				r.NoError(err)
				r.Len(retry.Sources, 1)
				a.Equal(large, retry.Sources[0].MessageID)
			}
			f = nextCommitmentTestAttempt(t, f, "-larger")
			fits, err := peoplesweep.CommitmentInputFits(window)
			r.NoError(err)
			a.True(fits)
			empty, err = peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":92161,"end":92173,"summary":"Call","due_on":null,"due_phrase":null}]}`, largeSource)), window)
			r.NoError(err)
			request = commitmentApplyRequest(t, f, window, cfg.Commitments, empty)
			request.Generation.ResolvedAt = laterNow
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			if tc.journal {
				window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow)
				r.NoError(err)
				r.Len(window.Sources, 1)
				a.Equal(secondLarge, window.Sources[0].MessageID)
				f = nextCommitmentTestAttempt(t, f, "-second-larger")
				request = commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
				request.Generation.ResolvedAt = laterNow
				_, err = f.store.ApplyPersonSweep(ctx, request)
				r.NoError(err)
			}
			items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 5, 0, false)
			r.NoError(err)
			r.Len(items, 2)
			since, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			r.NotNil(since)
			a.Equal(laterNow.Add(-cfg.Commitments.Window), *since)
			a.True(complete)
			due, err = f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
			r.NoError(err)
			a.False(due, "a successful retry clears the scheduling trigger")
		})
	}

	t.Run("edited oversized message", func(t *testing.T) {
		r, a, ctx := require.New(t), assert.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "commitment-shrink", false)
		_, persist := newCommitmentTestConversation(t, f)
		large := persist("large", strings.Repeat("x", 200)+" I will call.", -2*time.Hour)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxBytes, cfg.Commitments.MaxItems = 30, 1
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Empty(window.Sources)
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
		r.NoError(err)
		_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
		r.NoError(err)
		a.False(complete)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), "I will call.", large)
		r.NoError(err)
		f = nextCommitmentTestAttempt(t, f, "-short")
		window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		r.Len(window.Sources, 1)
		a.Equal(large, window.Sources[0].MessageID)
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
		r.NoError(err)
		_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
		r.NoError(err)
		a.True(complete)
	})
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

func TestCommitmentDecisionReportsConcurrentChange(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "decision-conflict", false)

	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will call."
	persist("promise", quote, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	promise := peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Call", End: int64(len(quote))}
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{promise}})
	result, err := f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, true)
	r.NoError(err)
	r.Len(items, 1)
	load := func() commitmentCandidate {
		var candidate commitmentCandidate
		r.NoError(f.store.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
			c, err := scanPersonCommitment(tx.QueryRowContext(ctx, `SELECT `+commitmentColumns+` FROM person_commitments WHERE id = ?`, items[0].ID))
			if err != nil {
				return err
			}
			candidate.commitment = c
			return f.store.loadCommitmentCandidateEvidenceTx(ctx, tx, &candidate)
		}))
		return candidate
	}
	candidate := load()
	_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	_, err = f.store.updatePersonCommitmentDecision(ctx, candidate, LoopStateSnoozed, new(request.CompletedAt.Add(time.Hour)))
	r.ErrorIs(err, ErrCommitmentChanged)
	// A changed snooze deadline is also a concurrent change.
	first := request.CompletedAt.Add(time.Hour)
	_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateSnoozed, &first, request.CompletedAt)
	r.NoError(err)
	snoozed := load()
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitments SET snoozed_until = ? WHERE id = ?`), f.store.dialect.TimestampParam(first.Add(time.Hour)), items[0].ID)
	r.NoError(err)
	_, err = f.store.updatePersonCommitmentDecision(ctx, snoozed, LoopStateSnoozed, new(first.Add(2*time.Hour)))
	r.ErrorIs(err, ErrCommitmentChanged)

	reopened, err := f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateOpen, nil, request.CompletedAt)
	r.NoError(err)
	r.Equal(LoopStateOpen, reopened.State)
	r.Nil(reopened.SnoozedUntil)
	candidate = load()

	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_fact_evidence_status_events
		(person_id, generation_id, evidence_id, evidence_key, source_version, supported, reason)
		VALUES (?, ?, ?, ?, ?, FALSE, 'source-edited')`), f.personID, result.Generation.GenerationID,
		candidate.promise.ID, candidate.promise.Key, candidate.promise.Input.SourceVersion)
	r.NoError(err)
	_, err = f.store.updatePersonCommitmentDecision(ctx, candidate, LoopStateSnoozed, new(request.CompletedAt.Add(time.Hour)))
	r.ErrorIs(err, ErrCommitmentChanged)
}

func TestCommitmentDiscoveryCoalescesBackfillAndJournal(t *testing.T) {
	t.Run("two pages", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "journal-repeat", false)
		_, persist := newCommitmentTestConversation(t, f)
		first := persist("first", "I will call.", -time.Hour)
		second := persist("second", "I will write.", -time.Hour)
		_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "I will call tomorrow.", first)
		r.NoError(err)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxItems = 1
		var got []int64
		for i := range 2 {
			if i > 0 {
				f = nextCommitmentTestAttempt(t, f, "-next")
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
	t.Run("pending edit waits for journal", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "oversized-pending-edit", false)
		_, persist := newCommitmentTestConversation(t, f)
		large := persist("large", strings.Repeat("x", 200), -2*time.Hour)
		cfg, profile := commitmentTestConfig()
		cfg.Commitments.MaxBytes, cfg.Commitments.MaxItems = 30, 1
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
		r.NoError(err)
		_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
		r.NoError(err)
		newer := persist("newer", "I will call.", -time.Hour)
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_sweep_changes (sequence, person_id, source_lane, change_kind, message_id, recorded_at) SELECT COALESCE(MAX(sequence), 0) + 1, ?, 'conversation_text', 'upsert', ?, ? FROM person_sweep_changes`), f.personID, large, f.store.dialect.TimestampParam(personFactLedgerNow))
		r.NoError(err)
		_, err = f.store.db.ExecContext(ctx, `UPDATE person_sweep_change_clock SET sequence = (SELECT MAX(sequence) FROM person_sweep_changes) WHERE singleton = TRUE`)
		r.NoError(err)
		cfg.Commitments.MaxBytes = 1000
		var seen []int64
		for i := range 2 {
			f = nextCommitmentTestAttempt(t, f, fmt.Sprintf("-retry-%d", i))
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
			r.NoError(err)
			r.Len(window.Sources, 1)
			seen = append(seen, window.Sources[0].MessageID)
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
			r.NoError(err)
		}
		r.Equal([]int64{newer, large}, seen)
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
			request := f.request
			request.CommitmentOnly = true
			request.Generation.Policy.AllowSensitive = true
			request.Budget = personSweepApplyBudget()
			request.Commitment = &peoplesweep.CommitmentResult{Window: window, Config: cfg.Commitments}
			if len(window.Sources) > 0 {
				request = commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{})
			}
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
	type rescanSpec struct {
		text, wantQuote, wantState string
		promises                   []promiseSpec
		includeClosed              bool
	}
	type testCase struct {
		name, initialText string
		initialPromises   []promiseSpec
		rescans           []rescanSpec
	}
	const quote = "I will send it."
	const first, second = "I will send it.", "I will call."
	longer := quote + " Thanks!"
	combined := first + " " + second
	cases := []testCase{
		{
			name: "omitted promise remains open", initialText: quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans:         []rescanSpec{{text: quote, wantQuote: quote, wantState: LoopStateOpen}},
		},
		{
			name: "deleted repetition", initialText: quote + " " + quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}, {quote: quote, summary: "Send", start: int64(len(quote) + 1)}},
			rescans:         []rescanSpec{{text: quote, wantQuote: quote, wantState: LoopStateOpen, includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send"}}}},
		},
		{
			name: "prepended text", initialText: quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans:         []rescanSpec{{text: "Note. " + quote, wantQuote: quote, wantState: LoopStateDone, includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send", start: 6}}}},
		},
		{
			name: "unique, duplicated, then unique again stays open", initialText: quote,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans: []rescanSpec{
				{text: quote + " " + quote},
				{text: quote, wantQuote: quote, wantState: LoopStateOpen, includeClosed: true, promises: []promiseSpec{{quote: quote, summary: "Send"}}},
			},
		},
		{
			name: "longer quote keeps decision", initialText: longer,
			initialPromises: []promiseSpec{{quote: quote, summary: "Send"}},
			rescans:         []rescanSpec{{text: longer, wantQuote: longer, wantState: LoopStateDone, includeClosed: true, promises: []promiseSpec{{quote: longer, summary: "Send"}}}},
		},
		{
			name: "ambiguous overlap stays open", initialText: combined,
			initialPromises: []promiseSpec{{quote: first, summary: "Send"}, {quote: second, summary: "Call", start: int64(len(first) + 1)}},
			rescans:         []rescanSpec{{text: combined, wantQuote: combined, wantState: LoopStateOpen, includeClosed: true, promises: []promiseSpec{{quote: combined, summary: "Send and call"}}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "edited-promise", false)
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
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, false)
			r.NoError(err)
			r.Len(items, len(initial.Promises))
			_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
			r.NoError(err)
			if tc.name == "omitted promise remains open" {
				_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateOpen, nil, request.CompletedAt)
				r.NoError(err)
			}
			if tc.name == "ambiguous overlap stays open" {
				until := request.CompletedAt.Add(24 * time.Hour)
				_, err = f.store.DecidePersonCommitmentContext(ctx, items[1].ID, LoopStateSnoozed, &until, request.CompletedAt)
				r.NoError(err)
			}

			currentText := tc.initialText
			for i, rescan := range tc.rescans {
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
				if rescan.wantState == "" {
					continue
				}
				items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 10, 0, rescan.includeClosed)
				r.NoError(err)
				if tc.name == "ambiguous overlap stays open" {
					r.Len(items, 2)
					r.Equal(LoopStateDone, items[0].State)
					r.Equal(LoopStateSnoozed, items[1].State)
					continue
				}
				var found bool
				for _, item := range items {
					if item.Quote == rescan.wantQuote {
						found = true
						r.Equal(rescan.wantState, item.State)
					}
				}
				r.True(found)
			}
		})
	}
}

func TestCommitmentDecisionDoesNotCrossArchiveArrival(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "promise-new-arrival", false)
	_, persist := newCommitmentTestConversation(t, f)
	const quote, second, text = "I will send it.", "I will call.", "I will send it. I will call."
	id := persist("first", text, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	output := peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{
		{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, End: int64(len(quote)), Summary: "Send"},
		{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: second, Start: int64(len(quote) + 1), End: int64(len(text)), Summary: "Call"},
	}}
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, output)
	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, false)
	r.NoError(err)
	r.Len(items, 2)
	_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	until := request.CompletedAt.Add(24 * time.Hour)
	_, err = f.store.DecidePersonCommitmentContext(ctx, items[1].ID, LoopStateSnoozed, &until, request.CompletedAt)
	r.NoError(err)
	recorded := window.Sources[0].RecordedTime
	if !f.store.IsPostgreSQL() {
		_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM messages WHERE id = ?`), id)
		r.NoError(err)
		r.Equal(id, persist("second", text, -time.Hour))
	}
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(recorded.Add(time.Hour)), id)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitment_frontiers SET scope_fingerprint = '' WHERE person_id = ?`), f.personID)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-new-arrival")
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	output.Promises = output.Promises[:1]
	output.Promises[0].SourceID, output.Promises[0].Quote, output.Promises[0].End = window.Sources[0].ID, text, int64(len(text))
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, output))
	r.NoError(err)
	items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 10, 0, false)
	r.NoError(err)
	r.Len(items, 1)
	r.Equal(text, items[0].Quote)
	r.Equal(LoopStateOpen, items[0].State)
}

func TestCommitmentDecisionDoesNotCrossReusedMessageID(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "reused-message", false)
	if f.store.IsPostgreSQL() {
		t.Skip("SQLite reuses the highest row ID")
	}
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send it."
	id := persist("first", quote, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	output := peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, End: int64(len(quote)), Summary: "Send"}}}
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, output)
	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, false)
	r.NoError(err)
	r.Len(items, 1)
	_, err = f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET sent_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(personFactLedgerNow.Add(-2*time.Hour)), id)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitment_frontiers SET scope_fingerprint = '' WHERE person_id = ?`), f.personID)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-corrected-time")
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	output.Promises[0].SourceID = window.Sources[0].ID
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, output))
	r.NoError(err)
	items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 10, true)
	r.NoError(err)
	r.Len(items, 1)
	r.Equal(LoopStateDone, items[0].State)
	recorded := window.Sources[0].RecordedTime
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM messages WHERE id = ?`), id)
	r.NoError(err)
	r.Equal(id, persist("second", quote, -time.Hour))
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE messages SET archived_at = ? WHERE id = ?`), f.store.dialect.TimestampParam(recorded), id)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`UPDATE person_commitment_frontiers SET scope_fingerprint = '' WHERE person_id = ?`), f.personID)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-reused")
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow)
	r.NoError(err)
	r.Len(window.Sources, 1)
	output.Promises[0].SourceID = window.Sources[0].ID
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, output))
	r.NoError(err)
	items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, personFactLedgerNow, 10, false)
	r.NoError(err)
	r.Len(items, 1)
	r.Equal(LoopStateOpen, items[0].State)
}
