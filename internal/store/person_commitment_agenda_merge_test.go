package store

import (
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestCommitmentAgendaReceiptFollowsSurvivor(t *testing.T) {
	for _, kind := range []string{"message", "person"} {
		t.Run(kind, func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "agenda-"+kind+"-merge", false)
			st := f.store
			_, persist := newCommitmentTestConversation(t, f)
			if kind == "message" {
				survivorMessage := persist("survivor", "I will send the report.", -time.Hour)
				absorbedMessage := persist("absorbed", "I will send the report.", -time.Hour)
				survivor := insertAgendaMergeFixture(t, st, f.personID, survivorMessage, "survivor-key", false, false)
				absorbed := insertAgendaMergeFixture(t, st, f.personID, absorbedMessage, "absorbed-key", true, false)
				r.NoError(st.withTxContext(ctx, func(tx *loggedTx) error {
					return st.copyLoopDecisionTx(ctx, tx, absorbedMessage, survivorMessage)
				}))
				item, err := st.GetCommitmentAgendaContext(ctx, survivor)
				r.NoError(err)
				a.Equal("absorbed-key", item.IdempotencyKey)
				a.True(item.Attempted)
				item, err = st.LinkCommitmentAgendaContext(ctx, "absorbed-key", "in-flight-issue", "agenda#4")
				r.NoError(err)
				a.Equal(survivor, item.CommitmentID)
				a.Equal("agenda#4", item.IssueRef)
				_, err = st.GetCommitmentAgendaContext(ctx, absorbed)
				r.ErrorIs(err, sql.ErrNoRows)
				_, err = st.db.ExecContext(ctx, st.Rebind(`DELETE FROM messages WHERE id = ?`), survivorMessage)
				r.NoError(err)
				_, err = st.GetCommitmentAgendaContext(ctx, survivor)
				r.ErrorIs(err, sql.ErrNoRows)
				_, err = st.LinkCommitmentAgendaContext(ctx, "absorbed-key", "in-flight-issue", "agenda#4")
				r.ErrorIs(err, sql.ErrNoRows)
			} else {
				messageID := persist("promise", "I will send the report.", -time.Hour)
				person, err := st.GetPersonContext(ctx, f.personID)
				r.NoError(err)
				participant, err := st.EnsureParticipant("duplicate@example.test", "Duplicate", "example.test")
				r.NoError(err)
				absorbedPerson, _, err := st.CreatePersonFromParticipant(participant)
				r.NoError(err)
				survivor := insertAgendaMergeFixture(t, st, person.ID, messageID, "survivor-key", true, false)
				absorbed := insertAgendaMergeFixture(t, st, absorbedPerson.ID, messageID, "absorbed-key", true, true)
				merged, err := st.MergePersonsContext(ctx, PersonMergeRequest{
					SurvivorID: person.ID, AbsorbedID: absorbedPerson.ID, ExpectedSurvivorRevision: person.Revision,
					ExpectedAbsorbedRevision: absorbedPerson.Revision, IdempotencyKey: "agenda-person-merge", Actor: "test",
				})
				r.NoError(err)
				r.NotNil(merged)
				item, err := st.GetCommitmentAgendaContext(ctx, survivor)
				r.NoError(err)
				a.Equal("absorbed-key", item.IdempotencyKey)
				a.Equal("agenda#3", item.IssueRef)
				_, err = st.GetCommitmentAgendaContext(ctx, absorbed)
				r.ErrorIs(err, sql.ErrNoRows)
			}
		})
	}
}

func TestCommitmentAgendaReceiptPurgesWithPerson(t *testing.T) {
	r, ctx := require.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "agenda-person-purge", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	linked := insertAgendaMergeFixture(t, st, f.personID, persist("linked", "I will send the report.", -time.Hour), "linked", true, true)
	retired := insertAgendaMergeFixture(t, st, f.personID, persist("retired", "I will send the report.", -time.Hour), "retired", false, false)
	r.NoError(st.RetireCommitmentAgendaContext(ctx, "retired"))
	// Exercise deletion after the owner has stopped the sweep enrollment.
	_, err := st.SetPersonTrackingContext(ctx, f.personID, false)
	r.NoError(err)
	_, err = st.GetCommitmentAgendaContext(ctx, linked)
	r.NoError(err)
	_, err = st.GetCommitmentAgendaContext(ctx, retired)
	r.NoError(err)
	person, err := st.GetPersonContext(ctx, f.personID)
	r.NoError(err)
	r.NoError(st.DeletePersonContext(ctx, person.ID, person.Revision))
	_, err = st.GetCommitmentAgendaContext(ctx, linked)
	r.ErrorIs(err, sql.ErrNoRows)
	_, err = st.GetCommitmentAgendaContext(ctx, retired)
	r.ErrorIs(err, sql.ErrNoRows)
}

func insertAgendaMergeFixture(t *testing.T, st *Store, personID, messageID int64, key string, attempted, linked bool) int64 {
	t.Helper()
	r, ctx := require.New(t), t.Context()
	const quote = "I will send the report."
	var id int64
	now := st.dialect.TimestampParam(personFactLedgerNow)
	err := st.db.QueryRowContext(ctx, st.Rebind(`INSERT INTO person_commitments
		(person_id, target_key, anchor_message_id, direction, quote, quote_start, quote_end, summary, promised_at, expires_at)
		VALUES (?, ?, ?, 'owed_to', ?, 0, ?, 'Send report', ?, ?) RETURNING id`),
		personID, "target:"+key, messageID, quote, len(quote), now, st.dialect.TimestampParam(personFactLedgerNow.Add(24*time.Hour))).Scan(&id)
	r.NoError(err)
	var attemptedAt, uid, ref any
	state := CommitmentAgendaPending
	if attempted {
		attemptedAt = now
	}
	if linked {
		state, uid, ref = CommitmentAgendaLinked, "issue-uid", "agenda#3"
	}
	start, end := 0, len(quote)
	evidence, err := kataevidence.New(st).Prepare(ctx, []kataevidence.Selector{{Kind: "message", MessageID: messageID, StartRune: &start, EndRune: &end}})
	r.NoError(err)
	r.Len(evidence, 1)
	person, err := st.GetPersonContext(ctx, personID)
	r.NoError(err)
	request, err := json.Marshal(kataissues.CreateInput{Title: "Send report", List: "commitments", PersonUID: person.VCardUID, Evidence: []kataevidence.Reference{evidence[0].Reference}})
	r.NoError(err)
	_, err = st.db.ExecContext(ctx, st.Rebind(`INSERT INTO person_commitment_agenda
		(commitment_id, target_key, idempotency_key, request_json, quote, state, attempted_at, issue_uid, issue_ref)
		VALUES (?, ?, ?, `+st.dialect.JSONBindExpr()+`, ?, ?, ?, ?, ?)`),
		id, "target:"+key, key, string(request), quote, state, attemptedAt, uid, ref)
	r.NoError(err)
	return id
}

func TestCommitmentAgendaReattachesExactPersonMergeCitation(t *testing.T) {
	for _, requote := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "requote"}[requote], func(t *testing.T) {
			r, a, ctx := require.New(t), assert.New(t), t.Context()
			f := newPersonSweepApplyFixture(t, "agenda-reattach", false)
			st := f.store
			_, persist := newCommitmentTestConversation(t, f)
			const original = "I will send the report."
			messageID := persist("promise", original, -time.Hour)
			participant, err := st.EnsureParticipant("duplicate@example.test", "Duplicate", "example.test")
			r.NoError(err)
			absorbed, _, err := st.CreatePersonFromParticipant(participant)
			r.NoError(err)
			// Before the merge this is the absorbed person's direct promise.
			_, err = st.db.ExecContext(ctx, st.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), participant, messageID)
			r.NoError(err)
			var conversationID int64
			r.NoError(st.db.QueryRowContext(ctx, st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), messageID).Scan(&conversationID))
			owner, err := st.EnsureParticipant("owner@example.test", "Owner", "example.test")
			r.NoError(err)
			r.NoError(st.ReplaceConversationParticipants(conversationID, []ConversationParticipantRef{{ParticipantID: owner, Role: "member"}, {ParticipantID: participant, Role: "member"}}))
			id := insertAgendaMergeFixture(t, st, absorbed.ID, messageID, "frozen-before-merge", true, false)
			frozen, err := st.GetCommitmentAgendaContext(ctx, id)
			r.NoError(err)
			person, err := st.GetPersonContext(ctx, f.personID)
			r.NoError(err)
			_, err = st.MergePersonsContext(ctx, PersonMergeRequest{SurvivorID: person.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: person.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "reattach", Actor: "test"})
			r.NoError(err)
			var survivorID int64
			r.NoError(st.db.QueryRowContext(ctx, st.Rebind(`SELECT commitment_id FROM person_commitment_agenda WHERE idempotency_key = ?`), frozen.IdempotencyKey).Scan(&survivorID))
			a.NotEqual(id, survivorID, "the copied survivor receives the existing frozen receipt")
			r.NoError(st.RetireCommitmentAgendaContext(ctx, frozen.IdempotencyKey))
			cfg := peoplesweep.Config{}
			cfg.ApplyDefaults()
			cfg.Commitments.Enabled = true
			window, err := st.LoadPersonCommitmentWindow(ctx, f.personID, peoplesweep.ProviderProfile{AllowSensitive: true, SourceSince: "2026-01-01", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}}, cfg.Commitments, personFactLedgerNow, "UTC")
			r.NoError(err)
			r.Len(window.Sources, 1)
			quote := original
			if requote {
				quote = strings.TrimSuffix(quote, ".")
			}
			_, err = st.ApplyPersonSweep(ctx, commitmentApplyRequest(t, f, window, cfg.Commitments, peoplesweep.ParsedCommitments{Promises: []peoplesweep.CommitmentPromise{{SourceID: window.Sources[0].ID, Direction: peoplesweep.CommitmentOwedTo, Quote: quote, Summary: "Send report", End: int64(len(quote))}}, Fulfilments: []peoplesweep.CommitmentFulfilment{}}))
			r.NoError(err)
			current, err := st.GetCommitmentAgendaContext(ctx, survivorID)
			r.NoError(err)
			a.Equal(frozen.IdempotencyKey, current.IdempotencyKey)
			a.Equal(frozen.Request, current.Request)
			if requote {
				a.Equal(CommitmentAgendaRetired, current.State)
				a.Equal(frozen.TargetKey, current.TargetKey)
			} else {
				a.Equal(CommitmentAgendaPending, current.State)
				a.NotEqual(frozen.TargetKey, current.TargetKey)
				items, err := st.ListPendingCommitmentAgendaContext(ctx, personFactLedgerNow, 8)
				r.NoError(err)
				r.Len(items, 1)
			}
		})
	}
}
