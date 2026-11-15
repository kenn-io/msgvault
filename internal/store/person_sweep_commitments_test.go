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
	const first, second = "I will send the draft.", "I will check the venue."
	text := first + " " + second
	id := persist("two-promises", text, -time.Hour)
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.MaxItems = 1
	window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
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
	items, err := st.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, 0, false)
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
}

func TestCommitmentDecisionSurvivesShiftedQuote(t *testing.T) {
	for _, tc := range []struct {
		name, state            string
		ambiguous, sameVersion bool
		resweep                string
	}{
		{"done", LoopStateDone, false, false, ""},
		{"snoozed", LoopStateSnoozed, false, false, ""},
		{"ambiguous", LoopStateDone, true, false, ""},
		{"shorter_quote", LoopStateDone, false, true, ""},
		{"split", LoopStateDone, false, true, "split"},
		{"merged", LoopStateDone, false, true, "merged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "shifted-"+tc.name, false)
			_, persist := newCommitmentTestConversation(t, f)
			const quote = "I will send the draft."
			const second, third = "I will check the venue.", "I will call."
			combined := quote + " " + second
			text := quote
			if tc.resweep != "" {
				text = combined + " " + third
			}
			id := persist("promise", text, -time.Hour)
			cfg, profile := commitmentTestConfig()
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			first := peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}}}
			firstPart := first.Promises[0]
			secondPart := peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: second, Summary: "Check", Start: int64(len(quote) + 1), End: int64(len(combined))}
			combinedPromise := peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: combined, Summary: "Send and check", End: int64(len(combined))}
			if tc.resweep == "split" {
				first.Promises = []peoplesweep.CommitmentPromise{combinedPromise}
			}
			if tc.resweep == "merged" {
				first.Promises = []peoplesweep.CommitmentPromise{firstPart, secondPart}
			}
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, first)
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, false)
			r.NoError(err)
			r.Len(items, len(first.Promises))
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
			if tc.sameVersion {
				cfg.Commitments.Window += time.Second
			} else {
				_, err = f.store.db.ExecContext(ctx, f.store.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), body, id)
				r.NoError(err)
			}
			f = nextCommitmentTestAttempt(t, f, "-edited")
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			promises := []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", Start: 8, End: 8 + int64(len(quote))}}
			if tc.sameVersion {
				promises[0].Quote = strings.TrimSuffix(quote, ".")
				promises[0].Start = 0
				promises[0].End = int64(len(quote) - 1)
			}
			if tc.ambiguous {
				start := int64(9 + len(quote))
				promises = append(promises, peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send another draft", Start: start, End: start + int64(len(quote))})
			}
			switch tc.resweep {
			case "split":
				promises = []peoplesweep.CommitmentPromise{firstPart, secondPart, {SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: third, Summary: "Call", Start: int64(len(combined) + 1), End: int64(len(text))}}
			case "merged":
				promises = []peoplesweep.CommitmentPromise{combinedPromise}
			}

			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: promises}))
			r.NoError(err)
			items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, true)
			r.NoError(err)
			if tc.resweep != "" {
				r.Len(items, 2)
				states := map[string]string{}
				for _, item := range items {
					states[item.Quote] = item.State
				}
				if tc.resweep == "split" {
					a.Equal(LoopStateDone, states[combined])
					a.Equal(LoopStateOpen, states[third])
				} else {
					a.Equal(LoopStateDone, states[quote])
					a.Equal(LoopStateOpen, states[second])
				}
			} else if tc.ambiguous {
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

func TestCommitmentDecisionDoesNotFollowReusedMessageID(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-reused-message", false)
	if f.store.IsPostgreSQL() {
		t.Skip("SQLite can reuse the highest message ID")
	}
	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will send the draft."
	id := persist("first", quote, -2*time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	promise := peoplesweep.CommitmentPromise{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send draft", End: int64(len(quote))}
	request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{promise}})
	_, err = f.store.ApplyPersonSweep(ctx, request)
	r.NoError(err)
	items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, false)
	r.NoError(err)
	r.Len(items, 1)
	decision, err := f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateDone, nil, request.CompletedAt)
	r.NoError(err)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM messages WHERE id = ?`), id)
	r.NoError(err)
	replacement := persist("replacement", quote, -time.Hour)
	r.Equal(id, replacement)
	f = nextCommitmentTestAttempt(t, f, "-replacement")
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	promise.SourceID = window.Sources[0].ID
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{promise}}))
	r.NoError(err)
	items, err = f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 10, 0, false)
	r.NoError(err)
	r.Len(items, 1)
	r.NotEqual(decision.ID, items[0].ID)
	r.Equal(LoopStateOpen, items[0].State)
}

func TestCommitmentListPages(t *testing.T) {
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
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
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
	t.Run("closed decisions", func(t *testing.T) {
		r, ctx := require.New(t), t.Context()
		f := newPersonSweepApplyFixture(t, "closed-pages", false)
		_, persist := newCommitmentTestConversation(t, f)
		var body strings.Builder
		var promises []peoplesweep.CommitmentPromise
		for i := range 201 {
			quote := fmt.Sprintf("I will complete task %d.", i)
			start := int64(body.Len())
			body.WriteString(quote)
			body.WriteByte(' ')
			promises = append(promises, peoplesweep.CommitmentPromise{Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: quote, Start: start, End: start + int64(len(quote))})
		}
		persist("many-promises", body.String(), -time.Hour)
		cfg, profile := commitmentTestConfig()
		window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
		r.NoError(err)
		r.Len(window.Sources, 1)
		for i := range promises {
			promises[i].SourceID = window.Sources[0].ID
		}
		request := commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: promises})
		_, err = f.store.ApplyPersonSweep(ctx, request)
		r.NoError(err)
		all, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 201, 0, true)
		r.NoError(err)
		r.Len(all, 201)
		for _, item := range all {
			_, err = f.store.DecidePersonCommitmentContext(ctx, item.ID, LoopStateDone, nil, request.CompletedAt)
			r.NoError(err)
		}
		page, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 200, 200, true)
		r.NoError(err)
		r.Len(page, 1)
		r.Equal(all[200].ID, page[0].ID)
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
	for _, tc := range []struct {
		name    string
		journal bool
	}{
		{"backfill", false}, {"journal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-byte-boundary", false)
			_, persist := newCommitmentTestConversation(t, f)
			large := persist("large", strings.Repeat("x", 90<<10)+" I will call.", -2*time.Hour)
			small := persist("small", "I will call.", -time.Hour)
			second := persist("second", "I will write.", -time.Hour)
			if !tc.journal {
				_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM person_sweep_changes WHERE person_id = ?`), f.personID)
				r.NoError(err)
			}
			cfg, profile := commitmentTestConfig()
			cfg.Commitments.MaxBytes = 20
			cfg.Commitments.MaxItems = 1
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			a.Equal(small, window.Sources[0].MessageID)
			parsed, err := peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":0,"end":12,"summary":"Call","due_on":null}]}`, window.Sources[0].ID)), window)
			r.NoError(err)
			request := commitmentApplyRequest(t, f, window, cfg.Commitments, parsed)
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
			items, err := f.store.ListPersonCommitmentsContext(ctx, f.personID, request.CompletedAt, 5, 0, false)
			r.NoError(err)
			r.Len(items, 1)
			a.Equal(small, items[0].AnchorMessageID)
			_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
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
			_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
			r.NoError(err)
			a.False(complete)
			if !tc.journal {
				due, err := f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt.Add(cfg.Commitments.MinInterval), cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
				r.NoError(err)
				a.False(due, "unchanged limits leave an oversized message pending")
			}
			cfg.Commitments.MaxBytes = 100 << 10
			due, err := f.store.PersonCommitmentsDue(ctx, f.personID, request.CompletedAt, cfg.Commitments.MinInterval, cfg.Commitments.Window, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments), cfg.Commitments.MaxBytes)
			r.NoError(err)
			a.True(due, "a raised byte limit schedules a retry immediately")
			laterNow := personFactLedgerNow.Add(24 * time.Hour)
			cfg.Commitments.MaxItems = 10
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, laterNow, "UTC")
			r.NoError(err)
			if !tc.journal {
				r.Len(window.Sources, 1, "raising batch limits should retry only the oversized message")
				a.Equal(laterNow, window.SelectionAt)
			} else {
				a.Equal(personFactLedgerNow, window.SelectionAt)
			}
			var largeSource string
			for _, source := range window.Sources {
				if source.MessageID == large {
					a.True(source.MatchesQuote("I will call.", 92161, 92173))
					largeSource = source.ID
				}
			}
			r.NotEmpty(largeSource)
			f = nextCommitmentTestAttempt(t, f, "-larger")
			fits, err := peoplesweep.CommitmentInputFits(window)
			r.NoError(err)
			a.True(fits)
			empty, err = peoplesweep.ParseCommitments([]byte(fmt.Sprintf(`{"promises":[{"source_id":%q,"direction":"owed_to","quote":"I will call.","start":92161,"end":92173,"summary":"Call","due_on":null}]}`, largeSource)), window)
			r.NoError(err)
			request = commitmentApplyRequest(t, f, window, cfg.Commitments, empty)
			request.Generation.ResolvedAt = laterNow
			_, err = f.store.ApplyPersonSweep(ctx, request)
			r.NoError(err)
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
}

func TestCommitmentRetryBatchKeepsDiscoveryProgress(t *testing.T) {
	for _, journal := range []bool{false, true} {
		name := "backfill"
		if journal {
			name = "journal"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := require.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "commitment-retry-progress", false)
			_, persist := newCommitmentTestConversation(t, f)
			first := persist("first-large", strings.Repeat("x", 90<<10), -2*time.Hour)
			second := persist("second-large", strings.Repeat("y", 90<<10), -time.Hour)
			history := persist("history", "I will call.", -time.Hour)
			if !journal {
				_, err := f.store.db.ExecContext(ctx, f.store.Rebind(`DELETE FROM person_sweep_changes WHERE person_id = ?`), f.personID)
				r.NoError(err)
			}
			cfg, profile := commitmentTestConfig()
			cfg.Commitments.MaxBytes = 20
			window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			r.Equal(history, window.Sources[0].MessageID)
			_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
			r.NoError(err)

			cfg.Commitments.MaxBytes = 100 << 10
			cfg.Commitments.MaxItems = 1
			for i, want := range []int64{first, second} {
				window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
				r.NoError(err)
				r.Len(window.Sources, 1)
				r.Equal(want, window.Sources[0].MessageID)
				f = nextCommitmentTestAttempt(t, f, fmt.Sprintf("-retry-%d", i))
				_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
				r.NoError(err)
			}
			window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 0, "completed history must not be sent again")
		})
	}
}

func TestCommitmentDiscoveryRetriesEditedOversizedMessage(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-shrink", false)
	_, persist := newCommitmentTestConversation(t, f)
	large := persist("large", strings.Repeat("x", 200)+" I will call.", -2*time.Hour)
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.MaxBytes, cfg.Commitments.MaxItems = 30, 1
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
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
	a.False(complete)
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind("UPDATE message_bodies SET body_text = ? WHERE message_id = ?"), "I will call.", large)
	r.NoError(err)
	f = nextCommitmentTestAttempt(t, f, "-short")
	window, err = f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 1)
	a.Equal(large, window.Sources[0].MessageID)
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
	r.NoError(err)
	_, complete, err = f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, window.ScopeFingerprint)
	r.NoError(err)
	a.True(complete)
}

func TestCommitmentDiscoveryMaxItemsLeavesJournalIncomplete(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "commitment-journal-page", false)
	_, persist := newCommitmentTestConversation(t, f)
	for i := range 11 {
		persist(fmt.Sprintf("pending-%d", i), "I will reply.", 0)
	}
	cfg, profile := commitmentTestConfig()
	cfg.Commitments.MaxItems = 10
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
	r.NoError(err)
	r.Len(window.Sources, 10)
	_, err = f.store.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{}))
	r.NoError(err)
	_, complete, err := f.store.PersonCommitmentDiscoveryContext(ctx, f.personID, peoplesweep.CommitmentScopeFingerprint(profile, cfg.Commitments))
	r.NoError(err)
	r.False(complete)
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
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
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

func TestCommitmentDecisionReportsConcurrentChange(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "decision-conflict", false)

	_, persist := newCommitmentTestConversation(t, f)
	const quote = "I will call."
	persist("promise", quote, -time.Hour)
	cfg, profile := commitmentTestConfig()
	window, err := f.store.LoadPersonCommitmentWindow(ctx, f.personID, profile, cfg.Commitments, personFactLedgerNow, "UTC")
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
	t.Run("reopens a snoozed promise", func(t *testing.T) {
		reopened, err := f.store.DecidePersonCommitmentContext(ctx, items[0].ID, LoopStateOpen, nil, request.CompletedAt)
		require.NoError(t, err)
		assert.Equal(t, LoopStateOpen, reopened.State)
		assert.Nil(t, reopened.SnoozedUntil)
	})
	_, err = f.store.db.ExecContext(ctx, f.store.Rebind(`INSERT INTO person_fact_evidence_status_events
		(person_id, generation_id, evidence_id, evidence_key, source_version, supported, reason)
		VALUES (?, ?, ?, ?, ?, FALSE, 'source-edited')`), f.personID, result.Generation.GenerationID,
		candidate.promise.ID, candidate.promise.Key, candidate.promise.Input.SourceVersion)
	r.NoError(err)
	_, err = f.store.updatePersonCommitmentDecision(ctx, candidate, LoopStateSnoozed, new(request.CompletedAt.Add(time.Hour)))
	r.ErrorIs(err, ErrCommitmentChanged)
}
