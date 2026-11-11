package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommitmentAgendaReceiptFollowsMessageSurvivor(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "agenda-message-merge", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
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
	a.ErrorIs(err, sql.ErrNoRows)
	_, err = st.db.ExecContext(ctx, st.Rebind(`DELETE FROM messages WHERE id = ?`), survivorMessage)
	r.NoError(err)
	_, err = st.GetCommitmentAgendaContext(ctx, survivor)
	a.ErrorIs(err, sql.ErrNoRows)
	_, err = st.LinkCommitmentAgendaContext(ctx, "absorbed-key", "in-flight-issue", "agenda#4")
	a.ErrorIs(err, sql.ErrNoRows)
}

func TestCommitmentAgendaReceiptFollowsPersonSurvivor(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "agenda-person-merge", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
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
	a.ErrorIs(err, sql.ErrNoRows)
}

func TestCommitmentAgendaReceiptPurgesWithPerson(t *testing.T) {
	r, a, ctx := require.New(t), assert.New(t), t.Context()
	f := newPersonSweepApplyFixture(t, "agenda-person-purge", false)
	st := f.store
	_, persist := newCommitmentTestConversation(t, f)
	linked := insertAgendaMergeFixture(t, st, f.personID, persist("linked", "I will send the report.", -time.Hour), "linked", true, true)
	retired := insertAgendaMergeFixture(t, st, f.personID, persist("retired", "I will send the report.", -time.Hour), "retired", false, false)
	r.NoError(st.RetireCommitmentAgendaContext(ctx, "retired"))
	person, err := st.GetPersonContext(ctx, f.personID)
	r.NoError(err)
	r.NoError(st.DeletePersonContext(ctx, person.ID, person.Revision))
	_, err = st.GetCommitmentAgendaContext(ctx, linked)
	a.ErrorIs(err, sql.ErrNoRows)
	_, err = st.GetCommitmentAgendaContext(ctx, retired)
	a.ErrorIs(err, sql.ErrNoRows)
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
	_, err = st.db.ExecContext(ctx, st.Rebind(`INSERT INTO person_commitment_agenda
		(commitment_id, target_key, idempotency_key, destination_key, request_json, quote, state, attempted_at, issue_uid, issue_ref)
		VALUES (?, ?, ?, 'destination', `+st.dialect.JSONBindExpr()+`, ?, ?, ?, ?, ?)`),
		id, "target:"+key, key, "{}", quote, state, attemptedAt, uid, ref)
	r.NoError(err)
	return id
}
