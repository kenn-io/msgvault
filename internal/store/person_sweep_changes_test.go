package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type personSweepJournalFixture struct {
	store          *store.Store
	sourceID       int64
	conversationID int64
	aliceID        int64
	bobID          int64
	alicePersonID  int64
	bobPersonID    int64
}

func newPersonSweepJournalFixture(t *testing.T, trackAlice, trackBob bool) personSweepJournalFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "person-sweep-journal")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "person-sweep-chat", "direct_chat", "Synthetic chat")
	require.NoError(t, err)
	aliceID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	bobID, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(t, err)
	alice, _, err := st.CreatePersonFromParticipant(aliceID)
	require.NoError(t, err)
	bob, _, err := st.CreatePersonFromParticipant(bobID)
	require.NoError(t, err)
	if trackAlice {
		_, err = st.SetPersonTrackingContext(t.Context(), alice.ID, true)
		require.NoError(t, err)
	}
	if trackBob {
		_, err = st.SetPersonTrackingContext(t.Context(), bob.ID, true)
		require.NoError(t, err)
	}
	return personSweepJournalFixture{
		store: st, sourceID: source.ID, conversationID: conversationID,
		aliceID: aliceID, bobID: bobID,
		alicePersonID: alice.ID, bobPersonID: bob.ID,
	}
}

func (f personSweepJournalFixture) insertMessage(
	t *testing.T, sourceMessageID, messageType string, senderID int64, sentAt time.Time,
) int64 {
	t.Helper()
	messageID, err := f.store.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: sourceMessageID,
		ConversationID: f.conversationID, MessageType: messageType,
		SenderID: sql.NullInt64{Int64: senderID, Valid: true},
		SentAt:   sql.NullTime{Time: sentAt, Valid: true},
		Subject:  sql.NullString{String: "Synthetic subject", Valid: true},
	})
	require.NoError(t, err)
	return messageID
}

func latestPersonSweepSequence(t *testing.T, st *store.Store) int64 {
	t.Helper()
	sequence, err := st.LatestPersonSweepChangeSequence(t.Context())
	require.NoError(t, err)
	return sequence
}

func personSweepChangesAfter(
	t *testing.T, st *store.Store, personID, after int64,
) []peoplesweep.ArchiveChange {
	t.Helper()
	changes, err := st.ScanPersonSweepChanges(t.Context(), personID, after, 1_000)
	require.NoError(t, err)
	return changes
}

func TestPersonSweepJournalCapturesLateOldMessage(t *testing.T) {
	checks := assert.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	newerID := f.insertMessage(t, "newer", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	highWater := latestPersonSweepSequence(t, f.store)
	olderID := f.insertMessage(t, "older-late", "email", f.aliceID,
		time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, highWater)
	require.Len(t, changes, 1)
	checks.Greater(changes[0].Sequence, highWater)
	checks.Equal(olderID, changes[0].MessageID)
	checks.NotEqual(newerID, changes[0].MessageID)
}

func TestPersonSweepJournalCapturesBodyAndSourceDeletion(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "body-source-delete", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`),
		messageID, "Original synthetic body")
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
		"Edited synthetic body", messageID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	requirements.NoError(err)

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 2)
	checks.Equal(peoplesweep.EvidenceEffectSourceEdited, changes[0].EvidenceEffect)
	checks.Equal(peoplesweep.ChangeUpsert, changes[0].Kind)
	checks.Equal(peoplesweep.EvidenceEffectSourceDeleted, changes[1].EvidenceEffect)
	checks.Equal(peoplesweep.ChangeDelete, changes[1].Kind)
}

func TestPersonSweepJournalPreservesDeleteCoordinates(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "hard-delete", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	before := latestPersonSweepSequence(t, f.store)

	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM messages WHERE id = ?`), messageID)
	requirements.NoError(err)

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 1)
	checks.Equal(f.alicePersonID, changes[0].PersonID)
	checks.Equal(f.sourceID, changes[0].SourceID)
	checks.Equal(messageID, changes[0].MessageID)
	checks.Equal(peoplesweep.SourceConversationText, changes[0].SourceLane)
	checks.Equal(peoplesweep.ChangeDelete, changes[0].Kind)
	checks.Equal(peoplesweep.EvidenceEffectSourceDeleted, changes[0].EvidenceEffect)
}

func TestPersonSweepJournalCapturesRecipientAndRosterScope(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	messageID := f.insertMessage(t, "recipient-roster", "email", f.bobID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	beforeAlice := latestPersonSweepSequence(t, f.store)

	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), messageID, f.aliceID, "alice@example.test")
	requirements.NoError(err)
	recipientChanges := personSweepChangesAfter(t, f.store, f.alicePersonID, beforeAlice)
	requirements.Len(recipientChanges, 1)
	checks.Equal(peoplesweep.EvidenceEffectScopeRelinked, recipientChanges[0].EvidenceEffect)
	checks.Equal(messageID, recipientChanges[0].MessageID)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM message_recipients WHERE message_id = ? AND participant_id = ?`),
		messageID, f.aliceID)
	requirements.NoError(err)
	beforeRoster := latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO conversation_participants (conversation_id, participant_id, role)
		VALUES (?, ?, 'member')`), f.conversationID, f.aliceID)
	requirements.NoError(err)
	rosterChanges := personSweepChangesAfter(t, f.store, f.alicePersonID, beforeRoster)
	requirements.Len(rosterChanges, 1)
	checks.Equal(peoplesweep.EvidenceEffectScopeRelinked, rosterChanges[0].EvidenceEffect)
	checks.Equal(messageID, rosterChanges[0].MessageID)
}

func TestPersonSweepJournalCapturesPersonParticipantBinding(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "binding", "email", f.bobID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_participants WHERE person_id = ? AND participant_id = ?`),
		f.bobPersonID, f.bobID)
	requirements.NoError(err)
	_, err = f.store.SetPersonTrackingContext(t.Context(), f.bobPersonID, true)
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`),
		f.bobPersonID, f.bobID)
	requirements.NoError(err)
	linked := personSweepChangesAfter(t, f.store, f.bobPersonID, before)
	requirements.Len(linked, 1)
	checks.Equal(peoplesweep.EvidenceEffectScopeRelinked, linked[0].EvidenceEffect)
	checks.Equal(messageID, linked[0].MessageID)

	before = latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_participants WHERE person_id = ? AND participant_id = ?`),
		f.bobPersonID, f.bobID)
	requirements.NoError(err)
	unlinked := personSweepChangesAfter(t, f.store, f.bobPersonID, before)
	requirements.Len(unlinked, 1)
	checks.Equal(peoplesweep.EvidenceEffectScopeUnlinked, unlinked[0].EvidenceEffect)
	checks.Equal(messageID, unlinked[0].MessageID)
}

func TestPersonSweepJournalClassifiesEvidenceEffects(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	messageID := f.insertMessage(t, "effects", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`), messageID, "Original")
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "Edited", messageID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET deleted_from_source_at = NULL WHERE id = ?`), messageID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), messageID, f.bobID, "bob@example.test")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM message_recipients WHERE message_id = ? AND participant_id = ?`),
		messageID, f.bobID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET sender_id = ? WHERE id = ?`), f.bobID, messageID)
	requirements.NoError(err)

	aliceChanges := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	bobChanges := personSweepChangesAfter(t, f.store, f.bobPersonID, before)
	checks.Equal([]peoplesweep.EvidenceChangeEffect{
		peoplesweep.EvidenceEffectSourceEdited,
		peoplesweep.EvidenceEffectSourceDeleted,
		peoplesweep.EvidenceEffectSourceReimported,
		peoplesweep.EvidenceEffectIdentityReassigned,
	}, archiveEffects(aliceChanges))
	checks.Equal([]peoplesweep.EvidenceChangeEffect{
		peoplesweep.EvidenceEffectScopeRelinked,
		peoplesweep.EvidenceEffectScopeUnlinked,
		peoplesweep.EvidenceEffectIdentityReassigned,
	}, archiveEffects(bobChanges))
}

func archiveEffects(changes []peoplesweep.ArchiveChange) []peoplesweep.EvidenceChangeEffect {
	effects := make([]peoplesweep.EvidenceChangeEffect, 0, len(changes))
	for _, change := range changes {
		effects = append(effects, change.EvidenceEffect)
	}
	return effects
}

func TestPersonSweepJournalRollbackIsInvisible(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "rollback", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	before := latestPersonSweepSequence(t, f.store)

	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	_, err = tx.ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "Rolled back", messageID)
	requirements.NoError(err)
	var inside int64
	requirements.NoError(tx.QueryRowContext(t.Context(),
		`SELECT sequence FROM person_sweep_change_clock WHERE singleton = TRUE`).Scan(&inside))
	checks.Greater(inside, before)
	requirements.NoError(tx.Rollback())

	checks.Equal(before, latestPersonSweepSequence(t, f.store))
	checks.Empty(personSweepChangesAfter(t, f.store, f.alicePersonID, before))
}

func TestPersonSweepJournalIgnoresUntrackedPeople(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, false, false)
	before := latestPersonSweepSequence(t, f.store)
	messageID := f.insertMessage(t, "untracked", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`), messageID, "Body")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET subject = ? WHERE id = ?`), "Edited", messageID)
	requirements.NoError(err)

	checks.Equal(before, latestPersonSweepSequence(t, f.store))
	checks.Empty(personSweepChangesAfter(t, f.store, f.alicePersonID, before))
}

func TestPersonSweepChangeScanBoundsAndCoalescingScope(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	f.insertMessage(t, "coalesce-alice", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	f.insertMessage(t, "coalesce-bob", "meeting_transcript", f.bobID,
		time.Date(2026, 8, 23, 13, 0, 0, 0, time.UTC))

	count, err := f.store.CoalescePersonSweepChangesContext(t.Context(), nil)
	requirements.NoError(err)
	checks.Equal(int64(2), count)
	count, err = f.store.CoalescePersonSweepChangesContext(t.Context(), &f.sourceID)
	requirements.NoError(err)
	checks.Equal(int64(2), count)
	missingSource := f.sourceID + 10_000
	count, err = f.store.CoalescePersonSweepChangesContext(t.Context(), &missingSource)
	requirements.NoError(err)
	checks.Zero(count)

	changes, err := f.store.ScanPersonSweepChanges(t.Context(), f.alicePersonID, 0, 2_000)
	requirements.NoError(err)
	requirements.Len(changes, 1)
	changes, err = f.store.ScanPersonSweepChanges(t.Context(), f.alicePersonID, 0, 0)
	requirements.NoError(err)
	checks.Empty(changes)
}

func TestPersonRelinkReactivatesExactEvidence(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	messageID := f.insertMessage(t, "reversible-recipient", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	deletePersonSweepWork(t, f.store, f.alicePersonID, f.bobPersonID)
	syncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients
			(message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), messageID, f.bobID, "bob@example.test")
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		DELETE FROM message_recipients
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
		messageID, f.bobID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients
			(message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), messageID, f.bobID, "bob@example.test")
	requirements.NoError(err)
	requirements.NoError(f.store.CompleteSyncAndUpdateSourceCursorContext(
		t.Context(), syncID, f.sourceID, "relinked-cursor"))

	changes := personSweepChangesAfter(t, f.store, f.bobPersonID, before)
	requirements.Len(changes, 3)
	checks.Equal([]peoplesweep.EvidenceChangeEffect{
		peoplesweep.EvidenceEffectScopeRelinked,
		peoplesweep.EvidenceEffectScopeUnlinked,
		peoplesweep.EvidenceEffectScopeRelinked,
	}, archiveEffects(changes))
	for _, change := range changes {
		checks.Equal(messageID, change.MessageID)
		checks.Equal(f.sourceID, change.SourceID)
		checks.Equal(peoplesweep.SourceConversationText, change.SourceLane)
	}
	rows, dirtyThrough := personSweepWorkState(t, f.store, f.bobPersonID)
	checks.Equal(1, rows)
	checks.Equal(changes[len(changes)-1].Sequence, dirtyThrough)
}

func TestSourceEditDisablesOldVersionAndQueuesReplacement(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "edited-source-version", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`),
		messageID, "Original exact evidence")
	requirements.NoError(err)
	deletePersonSweepWork(t, f.store, f.alicePersonID)
	syncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE message_bodies SET body_text = ? WHERE message_id = ?`),
		"Replacement exact evidence", messageID)
	requirements.NoError(err)
	requirements.NoError(f.store.CompleteSyncAndUpdateSourceCursorContext(
		t.Context(), syncID, f.sourceID, "edited-cursor"))

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 1)
	checks.Equal(peoplesweep.EvidenceEffectSourceEdited, changes[0].EvidenceEffect)
	checks.Equal(peoplesweep.ChangeUpsert, changes[0].Kind)
	checks.Equal(f.sourceID, changes[0].SourceID)
	checks.Equal(messageID, changes[0].MessageID)
	checks.Equal(peoplesweep.SourceConversationText, changes[0].SourceLane)
	rows, dirtyThrough := personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Equal(1, rows)
	checks.Equal(changes[0].Sequence, dirtyThrough)
}

func TestPersonSweepJournalSourceLaneUsesClosedVocabulary(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "meeting", "meeting_transcript", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, 0)
	require.NotEmpty(t, changes)
	last := changes[len(changes)-1]
	assert.Equal(t, messageID, last.MessageID)
	assert.Equal(t, peoplesweep.SourceMeetingText, last.SourceLane)
}

func TestPersonSweepJournalIgnoresMessageMaintenanceUpdates(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "maintenance", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	before := latestPersonSweepSequence(t, f.store)

	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE messages
		SET last_modified = CURRENT_TIMESTAMP,
		    content_changed_at = CURRENT_TIMESTAMP,
		    embed_gen = ?
		WHERE id = ?`), 17, messageID)
	require.NoError(t, err)

	assert.Equal(t, before, latestPersonSweepSequence(t, f.store))
	assert.Empty(t, personSweepChangesAfter(t, f.store, f.alicePersonID, before))
}

func TestPersonSweepJournalCapturesDateCorrection(t *testing.T) {
	dateColumns := []string{"sent_at", "received_at", "internal_date"}
	for _, column := range dateColumns {
		t.Run(column, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newPersonSweepJournalFixture(t, true, false)
			messageID := f.insertMessage(t, "date-"+column, "email", f.aliceID,
				time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC))
			before := latestPersonSweepSequence(t, f.store)

			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
				`UPDATE messages SET `+column+` = ? WHERE id = ?`),
				time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC), messageID)
			requirements.NoError(err)

			changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
			requirements.Len(changes, 1)
			checks.Equal(peoplesweep.ChangeUpsert, changes[0].Kind)
			checks.Equal(peoplesweep.EvidenceEffectSourceEdited, changes[0].EvidenceEffect)
		})
	}
}

func TestPersonSweepJournalCapturesCanonicalRosterScope(t *testing.T) {
	for _, conversationType := range []string{"group_chat", "channel", "unclassified"} {
		t.Run(conversationType, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			f := newPersonSweepJournalFixture(t, true, true)
			conversationID, err := f.store.EnsureConversationWithType(
				f.sourceID, "roster-"+conversationType, conversationType, "Roster scope")
			requirements.NoError(err)
			var messageID int64
			err = f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				INSERT INTO messages
					(source_id, source_message_id, conversation_id, message_type, sender_id, sent_at)
				VALUES (?, ?, ?, 'email', ?, ?) RETURNING id`),
				f.sourceID, "roster-"+conversationType, conversationID, f.bobID,
				time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)).Scan(&messageID)
			requirements.NoError(err)
			before := latestPersonSweepSequence(t, f.store)

			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
				INSERT INTO conversation_participants (conversation_id, participant_id, role)
				VALUES (?, ?, 'member')`), conversationID, f.aliceID)
			requirements.NoError(err)

			changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
			requirements.Len(changes, 1)
			checks.Equal(messageID, changes[0].MessageID)
			checks.Equal(peoplesweep.EvidenceEffectScopeRelinked, changes[0].EvidenceEffect)
		})
	}
}

func TestPersonSweepJournalRecipientRoleScopeBoundaries(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	messageID := f.insertMessage(t, "unknown-recipient", "email", f.bobID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	before := latestPersonSweepSequence(t, f.store)
	var recipientID int64
	err := f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients
			(message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'mention', ?) RETURNING id`),
		messageID, f.aliceID, "alice@example.test").Scan(&recipientID)
	requirements.NoError(err)
	checks.Empty(personSweepChangesAfter(t, f.store, f.alicePersonID, before))

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE message_recipients SET recipient_type = 'to' WHERE id = ?`), recipientID)
	requirements.NoError(err)
	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 1)
	checks.Equal(peoplesweep.ChangeScope, changes[0].Kind)
	checks.Equal(peoplesweep.EvidenceEffectScopeRelinked, changes[0].EvidenceEffect)

	before = latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE message_recipients SET recipient_type = 'mention' WHERE id = ?`), recipientID)
	requirements.NoError(err)
	changes = personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 1)
	checks.Equal(peoplesweep.ChangeScope, changes[0].Kind)
	checks.Equal(peoplesweep.EvidenceEffectScopeUnlinked, changes[0].EvidenceEffect)
}

func TestPersonSweepJournalMetadataEditsUseSourceEdited(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, true)
	messageID := f.insertMessage(t, "metadata-edges", "email", f.bobID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	var recipientID int64
	err := f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		INSERT INTO message_recipients
			(message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?) RETURNING id`),
		messageID, f.aliceID, "alice@example.test").Scan(&recipientID)
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO conversation_participants (conversation_id, participant_id, role)
		VALUES (?, ?, 'member')`), f.conversationID, f.aliceID)
	requirements.NoError(err)
	before := latestPersonSweepSequence(t, f.store)

	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE message_recipients SET recipient_type = 'cc' WHERE id = ?`), recipientID)
	requirements.NoError(err)
	recipientChanges := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(recipientChanges, 1)
	checks.Equal(peoplesweep.ChangeUpsert, recipientChanges[0].Kind)
	checks.Equal(peoplesweep.EvidenceEffectSourceEdited, recipientChanges[0].EvidenceEffect)

	before = latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE conversation_participants
		SET role = 'admin', joined_at = ?
		WHERE conversation_id = ? AND participant_id = ?`),
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), f.conversationID, f.aliceID)
	requirements.NoError(err)
	rosterChanges := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(rosterChanges, 1)
	checks.Equal(peoplesweep.ChangeUpsert, rosterChanges[0].Kind)
	checks.Equal(peoplesweep.EvidenceEffectSourceEdited, rosterChanges[0].EvidenceEffect)
}
