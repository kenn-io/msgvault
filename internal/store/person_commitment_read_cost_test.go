package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Measure actual read paths over supported exact archive citations. Seed the
// ledger directly so provider/ingestion throughput is outside this measurement.
// There is deliberately no machine-dependent timing assertion.
func TestCommitmentListRepresentativeReadCost(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := storetest.New(t)
	st, now := f.Store, loopNow
	r.NoError(st.AddAccountIdentity(f.Source.ID, loopOwner, "manual"))
	owner := f.EnsureParticipant(loopOwner, "", "example.com")
	profile := peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}
	cfg := peoplesweep.Config{}
	cfg.ApplyDefaults()
	cfg.Commitments.MaxItems = 100
	for personIndex := range 10 {
		peer := f.EnsureParticipant(fmt.Sprintf("read-cost-%d@example.test", personIndex), "Peer", "example.test")
		person, _, err := st.CreatePersonFromParticipant(peer)
		r.NoError(err)
		_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
		r.NoError(err)
		thread, err := st.EnsureConversationWithType(f.Source.ID, fmt.Sprintf("read-cost-%d", personIndex), "direct_chat", "Planning")
		r.NoError(err)
		r.NoError(st.EnsureConversationParticipant(thread, owner, "member"))
		r.NoError(st.EnsureConversationParticipant(thread, peer, "member"))
		for message := range 62 {
			text := fmt.Sprintf("I will send draft %d.", message)
			at := now.Add(-2*time.Hour + time.Duration(message)*time.Second)
			if message == 61 {
				text, at = "I sent all sixty earlier drafts.", now.Add(-time.Hour)
			}
			_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
				Message: &store.Message{SourceID: f.Source.ID, ConversationID: thread, MessageType: "whatsapp",
					SourceMessageID: fmt.Sprintf("read-cost-%d-%d", personIndex, message),
					SenderID:        sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}},
				BodyText: sql.NullString{String: text, Valid: true},
			})
			r.NoError(err)
		}
		window, err := st.LoadPersonCommitmentWindow(ctx, person.ID, profile, cfg.Commitments, now, "UTC")
		r.NoError(err)
		r.Len(window.Sources, 62)
		seedPersonFactClaimLedger(t, st, person.ID, fmt.Sprintf("read-cost-%d", personIndex))
		var completion peoplesweep.CommitmentSource
		for _, source := range window.Sources {
			if source.Text == "I sent all sixty earlier drafts." {
				completion = source
			}
		}
		r.NotZero(completion.MessageID)
		fulfilment := insertReadCostEvidence(t, st, person.ID, completion)
		for _, source := range window.Sources {
			if source.MessageID == completion.MessageID {
				continue
			}
			key := fmt.Sprintf("read-cost-%d", source.MessageID)
			evidence := insertReadCostEvidence(t, st, person.ID, source)
			var claim int64
			r.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`INSERT INTO person_fact_claims
				(person_id,generation_id,claim_key,target_kind,target_key,target_revision,relation,submitted_value_json,origin,confidence_json)
				SELECT ?,MIN(id),?,'commitment',?,'v1','support','"Send draft"','commitment','{}'
				FROM person_fact_generations WHERE person_id = ? RETURNING id`), person.ID, key, key, person.ID).Scan(&claim))
			_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_fact_claim_evidence (claim_id,evidence_id) VALUES (?,?)`), claim, evidence)
			r.NoError(err)
			var confirmed, completionID any = 1, fulfilment
			if source.EventTime.Equal(now.Add(-2*time.Hour + 60*time.Second)) {
				confirmed, completionID = 0, nil
			}
			_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_commitments
				(person_id,anchor_message_id,current_claim_id,target_key,direction,quote,quote_start,quote_end,summary,promised_at,expires_at,fulfilment_evidence_id,fulfilment_confirmed)
				VALUES (?,?,?,?,'owed_to',?,0,?,'Send draft',?,?,?,?)`), person.ID, source.MessageID, claim, key,
				source.Text, len([]rune(source.Text)), source.EventTime, now.Add(24*time.Hour), completionID, confirmed)
			r.NoError(err)
		}
	}
	started := time.Now()
	items, err := st.ListPersonCommitmentsContext(ctx, 0, now, 51, false)
	elapsed := time.Since(started)
	r.NoError(err)
	a.Len(items, 10)
	t.Logf("ListPersonCommitmentsContext: 10 people, 600 confirmed fulfilled + 10 open, elapsed=%s (SQL query instrumentation unavailable)", elapsed)
	started = time.Now()
	result, err := loops.List(ctx, st, loops.Options{Limit: 50}, now)
	elapsed = time.Since(started)
	r.NoError(err)
	a.Len(result.Commitments, 10)
	t.Logf("loops.List same archive: elapsed=%s", elapsed)
}

func insertReadCostEvidence(t *testing.T, st *store.Store, personID int64, source peoplesweep.CommitmentSource) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_fact_evidence
		(person_id,evidence_key,source_class,directness,authority,source_ref,span_start,span_end,excerpt,content_sha256,source_version,event_time,recorded_time,identity_score)
		VALUES (?,?,'archive','direct-self','authoritative',?,0,?,?,?,?,?,?,1000) RETURNING id`),
		personID, fmt.Sprintf("read-cost-evidence-%d", source.MessageID), source.SourceRef, len([]rune(source.Text)),
		source.Text, source.ContentSHA256, source.SourceVersion, source.EventTime, source.RecordedTime).Scan(&id))
	return id
}
