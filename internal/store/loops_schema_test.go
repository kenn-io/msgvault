package store_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func loopExec(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	_, err := s.DB().ExecContext(t.Context(), s.Rebind(query), args...)
	require.NoError(t, err, query)
}

func loopCount(t *testing.T, s *store.Store, table string) int {
	t.Helper()
	var n int
	require.NoError(t, s.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
	return n
}

func insertSchemaLoop(t *testing.T, f *storetest.Fixture, key, kind, detector string) int64 {
	t.Helper()
	var id int64
	err := f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO open_loops
 (turn_key,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version)
 VALUES (?,?,?,?,?,'open','email',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'[]',1) RETURNING id`), key, f.ConvID, f.Source.ID, kind, detector).Scan(&id)
	require.NoError(t, err)
	return id
}

func TestLoopSchemaConstraints(t *testing.T) {
	assertions := assert.New(t)
	f := storetest.New(t)
	s := f.Store
	assertions.Equal(1, loopCount(t, s, "loop_derivation_state"))
	for _, table := range []string{"open_loops", "loop_suppressions", "loop_conversation_queue", "message_automation_signals"} {
		assertions.Zero(loopCount(t, s, table), "bootstrap must not backfill %s", table)
	}
	id := insertSchemaLoop(t, f, "turn-a", "reply_owed", "turn_state")
	_, err := s.DB().Exec(s.Rebind(`INSERT INTO open_loops (turn_key,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version) SELECT 'turn-b',conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version FROM open_loops WHERE id=?`), id)
	assertions.Error(err, "one live turn-state loop per conversation and kind")
	commit := insertSchemaLoop(t, f, "commit-a", "commitment_owed_by_me", "commitment_extraction")
	_, err = s.DB().Exec(s.Rebind(`INSERT INTO open_loops (turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version) SELECT turn_key,2,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version FROM open_loops WHERE id=?`), commit)
	assertions.Error(err, "one live occurrence per turn, including commitments")
	insertSchemaLoop(t, f, "commit-b", "commitment_owed_by_me", "commitment_extraction")
	insertSchemaLoop(t, f, "await-a", "awaiting_reply", "turn_state")
	for _, assignment := range []string{"status='closed'", "closed_at=CURRENT_TIMESTAMP", "closed_reason='done'", "closed_by='user'", "cycle=0", "kind='invalid'", "detector='invalid'", "status='invalid'", "channel='invalid'", "agenda_state='invalid'", "closed_message_id=-1", "fact_claim_id=-1"} {
		t.Run(assignment, func(t *testing.T) {
			_, err := s.DB().Exec(s.Rebind("UPDATE open_loops SET "+assignment+" WHERE id=?"), id)
			assert.Error(t, err)
		})
	}
	loopExec(t, s, "UPDATE open_loops SET status='closed',closed_at=CURRENT_TIMESTAMP,closed_reason='done',closed_by='user' WHERE id=?", id)
	for _, assignment := range []string{"closed_at=NULL", "closed_reason=NULL", "closed_by=NULL", "closed_reason='invalid'", "closed_by='invalid'"} {
		_, err = s.DB().Exec(s.Rebind("UPDATE open_loops SET "+assignment+" WHERE id=?"), id)
		assertions.Error(err)
	}
	_, err = s.DB().Exec(s.Rebind(`INSERT INTO open_loops (turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version,closed_at,closed_reason,closed_by) SELECT turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version,closed_at,closed_reason,closed_by FROM open_loops WHERE id=?`), id)
	assertions.Error(err, "turn/cycle identity remains unique for closed history")
	loopExec(t, s, `INSERT INTO open_loops (turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version) SELECT turn_key,2,conversation_id,source_id,kind,detector,'held',channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version FROM open_loops WHERE id=?`, id)
	for _, cycle := range []int{1, 3} {
		_, err := s.DB().Exec(s.Rebind(`INSERT INTO open_loops (turn_key,cycle,conversation_id,source_id,kind,detector,status,channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version) SELECT turn_key,?,conversation_id,source_id,kind,detector,'held',channel,owed_since_at,last_activity_at,opens_at,evidence_json,ruleset_version FROM open_loops WHERE id=?`), cycle, id)
		assertions.Error(err, "unique historical cycle or live key")
	}
	_, err = s.DB().Exec("INSERT INTO loop_derivation_state(singleton) VALUES(2)")
	assertions.Error(err)
	_, err = s.DB().Exec("INSERT INTO loop_derivation_state(singleton) VALUES(1)")
	assertions.Error(err)
	for _, q := range []string{"INSERT INTO loop_conversation_queue(conversation_id,revision) VALUES(?,0)", "INSERT INTO loop_conversation_queue(conversation_id,processed_revision) VALUES(?,-1)", "INSERT INTO loop_conversation_queue(conversation_id,processed_revision) VALUES(?,2)"} {
		_, err = s.DB().Exec(s.Rebind(q), f.ConvID)
		assertions.Error(err)
	}
}

func TestLoopSchemaAcceptsReservedJevNotExpectedReason(t *testing.T) {
	f := storetest.New(t)
	id := insertSchemaLoop(t, f, "reserved-jev-reason", "awaiting_reply", "turn_state")

	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE open_loops
		SET status='closed',closed_at=CURRENT_TIMESTAMP,closed_reason=?,closed_by='system'
		WHERE id=?`), "jev_not_expected", id)
	require.NoError(t, err)

	var reason string
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(),
		f.Store.Rebind("SELECT closed_reason FROM open_loops WHERE id=?"), id).Scan(&reason))
	assert.Equal(t, "jev_not_expected", reason)
}

func TestLoopSchemaDurableIDsAreNotReused(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	firstLoop := insertSchemaLoop(t, f, "original-turn", "reply_owed", "turn_state")
	loopExec(t, f.Store, "DELETE FROM conversations WHERE id=?", f.ConvID)
	conversation, err := f.Store.EnsureConversation(f.Source.ID, "replacement-thread", "Replacement thread")
	requirements.NoError(err)
	f.ConvID = conversation
	replacementLoop := insertSchemaLoop(t, f, "replacement-turn", "reply_owed", "turn_state")
	assertions.Greater(replacementLoop, firstLoop, "an old confirmed loop ID must never identify a replacement obligation")

	var firstSuppression, replacementSuppression int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("INSERT INTO loop_suppressions(target_kind,conversation_id) VALUES('conversation',?) RETURNING id"), conversation).Scan(&firstSuppression))
	loopExec(t, f.Store, "DELETE FROM loop_suppressions WHERE id=?", firstSuppression)
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("INSERT INTO loop_suppressions(target_kind,conversation_id) VALUES('conversation',?) RETURNING id"), conversation).Scan(&replacementSuppression))
	assertions.Greater(replacementSuppression, firstSuppression, "an old confirmed mute ID must never identify a recreated mute")
}

func TestLoopSchemaSuppressionsAndForeignKeys(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	s := f.Store
	pid := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	var person int64
	requirements.NoError(s.DB().QueryRow("INSERT INTO persons(vcard_uid) VALUES('synthetic-loop-person') RETURNING id").Scan(&person))
	var generation, claim int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`INSERT INTO person_fact_generations(person_id,generation_key,source_cursors_json,program_id,program_version,program_fingerprint,catalog_fingerprint,provider,provider_version,model,model_version,provider_policy_fingerprint,resolved_at) VALUES(?,'loop-gen','{}','test','1',?,'catalog','test','1','test','1','policy',CURRENT_TIMESTAMP) RETURNING id`), person, strings.Repeat("a", 64)).Scan(&generation))
	requirements.NoError(s.DB().QueryRow(s.Rebind(`INSERT INTO person_fact_claims(person_id,generation_id,claim_key,target_kind,target_key,target_revision,relation,submitted_value_json,origin,confidence_json) VALUES(?,?,'loop-claim','attribute','test','1','support','{}','system','{}') RETURNING id`), person, generation).Scan(&claim))
	mid := f.CreateMessage("loop-message")
	id := insertSchemaLoop(t, f, "turn-a", "reply_owed", "turn_state")
	loopExec(t, s, "UPDATE open_loops SET counterparty_participant_id=?,counterparty_person_id=?,anchor_message_id=?,last_activity_message_id=?,closed_message_id=? WHERE id=?", pid, person, mid, mid, mid, id)
	for _, tc := range []struct {
		kind, col string
		target    int64
	}{{"participant", "participant_id", pid}, {"person", "person_id", person}, {"conversation", "conversation_id", f.ConvID}} {
		q := "INSERT INTO loop_suppressions(target_kind," + tc.col + ") VALUES(?,?)"
		loopExec(t, s, q, tc.kind, tc.target)
		_, err := s.DB().Exec(s.Rebind(q), tc.kind, tc.target)
		requirements.Error(err, "unique suppression target")
	}
	for _, q := range []string{"INSERT INTO loop_suppressions(target_kind) VALUES('person')", "INSERT INTO loop_suppressions(target_kind,participant_id,person_id) VALUES('participant',?,?)", "INSERT INTO loop_suppressions(target_kind,person_id) VALUES('participant',?)"} {
		args := []any{}
		if strings.Count(q, "?") == 2 {
			args = []any{pid, person}
		} else if strings.Count(q, "?") == 1 {
			args = []any{person}
		}
		_, err := s.DB().Exec(s.Rebind(q), args...)
		requirements.Error(err, "exactly one matching target")
	}
	loopExec(t, s, "INSERT INTO message_automation_signals(message_id,parser_version,calendar_part) VALUES(?,1,TRUE)", mid)
	_, err := s.DB().Exec(s.Rebind("INSERT INTO message_automation_signals(message_id,parser_version) VALUES(?,1)"), mid)
	assertions.Error(err)
	loopExec(t, s, "UPDATE open_loops SET fact_claim_id=? WHERE id=?", claim, id)
	loopExec(t, s, "DELETE FROM person_fact_claims WHERE id=?", claim)
	var claimRef sql.NullInt64
	requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT fact_claim_id FROM open_loops WHERE id=?"), id).Scan(&claimRef))
	assertions.False(claimRef.Valid)
	loopExec(t, s, "DELETE FROM participants WHERE id=?", pid)
	loopExec(t, s, "DELETE FROM persons WHERE id=?", person)
	loopExec(t, s, "DELETE FROM messages WHERE id=?", mid)
	var participant, personRef, anchor, last, closed sql.NullInt64
	requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT counterparty_participant_id,counterparty_person_id,anchor_message_id,last_activity_message_id,closed_message_id FROM open_loops WHERE id=?"), id).Scan(&participant, &personRef, &anchor, &last, &closed))
	for _, v := range []sql.NullInt64{participant, personRef, anchor, last, closed} {
		assertions.False(v.Valid)
	}
	assertions.Zero(loopCount(t, s, "message_automation_signals"))
	assertions.Equal(1, loopCount(t, s, "loop_suppressions"))
	loopExec(t, s, "INSERT INTO loop_conversation_queue(conversation_id) VALUES(?) ON CONFLICT(conversation_id) DO NOTHING", f.ConvID)
	loopExec(t, s, "DELETE FROM conversations WHERE id=?", f.ConvID)
	assertions.Zero(loopCount(t, s, "open_loops"))
	assertions.Zero(loopCount(t, s, "loop_suppressions"))
	assertions.Zero(loopCount(t, s, "loop_conversation_queue"))
	// Source deletion must also cascade loops even without deleting their conversation first.
	conv, err := s.EnsureConversation(f.Source.ID, "source-cascade", "Source cascade")
	requirements.NoError(err)
	f.ConvID = conv
	insertSchemaLoop(t, f, "source-turn", "reply_owed", "turn_state")
	loopExec(t, s, "DELETE FROM sources WHERE id=?", f.Source.ID)
	assertions.Zero(loopCount(t, s, "open_loops"))
}

func queueRevision(t *testing.T, s *store.Store, conv int64) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, s.DB().QueryRow(s.Rebind("SELECT revision FROM loop_conversation_queue WHERE conversation_id=?"), conv).Scan(&revision))
	return revision
}

func TestLoopSchemaAssociationTriggers(t *testing.T) {
	for _, association := range []string{"labels", "recipients"} {
		t.Run(association, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			s := f.Store
			loopExec(t, s, "DELETE FROM loop_conversation_queue")
			old := f.CreateMessage("old")
			assertions.Zero(loopCount(t, s, "loop_conversation_queue"), "message insert has no loop trigger")
			oldConv := f.ConvID
			conv, err := s.EnsureConversation(f.Source.ID, "new-thread", "New thread")
			requirements.NoError(err)
			f.ConvID = conv
			next := f.CreateMessage("new")
			assertions.Zero(loopCount(t, s, "loop_conversation_queue"), "message insert has no loop trigger")
			frontier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			loopExec(t, s, "UPDATE loop_derivation_state SET feed_frontier=?", frontier)
			table := "message_" + association
			var target int64
			if association == "labels" {
				target = f.EnsureLabels(map[string]string{"INBOX": "Inbox"}, "system")["INBOX"]
			} else {
				target = f.EnsureParticipant("peer@example.test", "Peer", "example.test")
			}
			insert := "INSERT INTO " + table + "(message_id,label_id) VALUES(?,?)"
			if association == "recipients" {
				insert = "INSERT INTO " + table + "(message_id,participant_id,recipient_type) VALUES(?,?,'to')"
			}
			loopExec(t, s, insert, old, target)
			assertions.Equal(int64(1), queueRevision(t, s, oldConv))
			loopExec(t, s, "UPDATE loop_conversation_queue SET epoch='scan-epoch',reason='rescan' WHERE conversation_id=?", oldConv)
			loopExec(t, s, "UPDATE loop_derivation_state SET feed_frontier=?", frontier.Add(time.Hour))
			loopExec(t, s, "UPDATE "+table+" SET message_id=? WHERE message_id=?", next, old)
			assertions.Equal(int64(2), queueRevision(t, s, oldConv), "OLD association invalidated")
			assertions.Equal(int64(1), queueRevision(t, s, conv), "NEW association invalidated")
			var bound time.Time
			var epoch, reason string
			requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT feed_bound,epoch,reason FROM loop_conversation_queue WHERE conversation_id=?"), oldConv).Scan(&bound, &epoch, &reason))
			assertions.True(frontier.Equal(bound))
			assertions.Equal("scan-epoch", epoch)
			assertions.Equal(association, reason)
			loopExec(t, s, "DELETE FROM "+table+" WHERE message_id=?", next)
			assertions.Equal(int64(2), queueRevision(t, s, conv))
			loopExec(t, s, "UPDATE loop_conversation_queue SET processed_revision=revision,feed_bound=NULL,epoch='' WHERE conversation_id=?", conv)
			loopExec(t, s, insert, next, target)
			requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT feed_bound FROM loop_conversation_queue WHERE conversation_id=?"), conv).Scan(&bound))
			assertions.True(frontier.Add(time.Hour).Equal(bound), "acknowledged row takes a fresh bound")
			// Same-message updates visit both sides, and NULL pending bounds stay unknown.
			loopExec(t, s, "UPDATE loop_conversation_queue SET feed_bound=NULL WHERE conversation_id=?", conv)
			loopExec(t, s, "UPDATE "+table+" SET message_id=message_id WHERE message_id=?", next)
			assertions.Equal(int64(5), queueRevision(t, s, conv))
			var nullBound sql.NullTime
			requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT feed_bound FROM loop_conversation_queue WHERE conversation_id=?"), conv).Scan(&nullBound))
			assertions.False(nullBound.Valid)
			// The message trigger queues its conversation once; association cascades
			// cannot look up the already-deleted parent message and add revisions.
			loopExec(t, s, "DELETE FROM messages WHERE id=?", next)
			assertions.Equal(int64(6), queueRevision(t, s, conv), "message deletion invalidates once")
			loopExec(t, s, "DELETE FROM conversations WHERE id=?", conv)
			assertions.Equal(1, loopCount(t, s, "loop_conversation_queue"))
		})
	}
}

func TestLoopSchemaMessageMoveOwnershipTriggers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	s := f.Store
	old := f.ConvID
	mid := f.CreateMessage("moving")
	id := insertSchemaLoop(t, f, "closed-history", "reply_owed", "turn_state")
	loopExec(t, s, "UPDATE open_loops SET status='closed',closed_at=CURRENT_TIMESTAMP,closed_reason='done',closed_by='user' WHERE id=?", id)
	next, err := s.EnsureConversation(f.Source.ID, "move-target", "Move target")
	requirements.NoError(err)
	loopExec(t, s, "DELETE FROM loop_conversation_queue")
	loopExec(t, s, "UPDATE messages SET subject='Bookkeeping',conversation_id=conversation_id,source_is_from_me=source_is_from_me,identity_is_from_me=identity_is_from_me WHERE id=?", mid)
	assertions.Zero(loopCount(t, s, "loop_conversation_queue"))
	loopExec(t, s, "UPDATE messages SET conversation_id=? WHERE id=?", next, mid)
	assertions.Equal(int64(1), queueRevision(t, s, old), "old closed-history conversation still invalidated")
	assertions.Equal(int64(1), queueRevision(t, s, next))
	for _, column := range []string{"source_is_from_me", "identity_is_from_me"} {
		before := queueRevision(t, s, next)
		loopExec(t, s, "UPDATE messages SET "+column+"=TRUE WHERE id=?", mid)
		assertions.Equal(before+2, queueRevision(t, s, next), "both OLD and NEW visited for ownership")
		var reason string
		requirements.NoError(s.DB().QueryRow(s.Rebind("SELECT reason FROM loop_conversation_queue WHERE conversation_id=?"), next).Scan(&reason))
		assertions.Equal("ownership", reason)
	}
	loopExec(t, s, "UPDATE messages SET conversation_id=? WHERE id=?", old, mid)
	assertions.Equal(int64(6), queueRevision(t, s, next))
	assertions.Equal(int64(2), queueRevision(t, s, old))
}

func TestLoopSchemaMessageDeleteQueuesOldConversation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	s := f.Store
	messageID := f.CreateMessage("deleted-message")
	loopExec(t, s, "DELETE FROM loop_conversation_queue")

	loopExec(t, s, "DELETE FROM messages WHERE id=?", messageID)

	assertions.Equal(int64(1), queueRevision(t, s, f.ConvID))
	var reason string
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT reason FROM loop_conversation_queue
		WHERE conversation_id=?`), f.ConvID).Scan(&reason))
	assertions.Equal("message_deleted", reason)
}

func TestLoopSchemaMessageDeleteTriggerUpgrade(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	s := f.Store
	loopExec(t, s, "DELETE FROM applied_migrations WHERE name=?", "loop_projection_triggers_v2")
	requirements.NoError(s.MarkMigrationApplied("loop_projection_triggers_v1"))
	if s.IsPostgreSQL() {
		loopExec(t, s, "DROP TRIGGER IF EXISTS trg_loop_queue_messages_delete ON messages")
	} else {
		loopExec(t, s, "DROP TRIGGER IF EXISTS trg_loop_queue_messages_delete")
	}
	requirements.NoError(s.InitSchemaContext(t.Context()))

	messageID := f.CreateMessage("deleted-message-upgrade")
	loopExec(t, s, "DELETE FROM loop_conversation_queue")
	loopExec(t, s, "DELETE FROM messages WHERE id=?", messageID)
	assertions.Equal(int64(1), queueRevision(t, s, f.ConvID))
}

// Rebuilding the legacy recipient table must restore loop triggers even when
// the separate loop-trigger migration is already marked complete.
func TestLoopSchemaRecipientEnvelopeRebuild(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite table rebuild; PostgreSQL drops the legacy constraint in place")
	f := storetest.New(t)
	s := f.Store
	mid := f.CreateMessage("envelope-rebuild")
	pid := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	for _, statement := range []string{
		"DELETE FROM applied_migrations WHERE name='message_recipients_envelope_unique_index'",
		"ALTER TABLE message_recipients RENAME TO message_recipients_current",
		`CREATE TABLE message_recipients(id INTEGER PRIMARY KEY,message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,recipient_type TEXT NOT NULL,display_name TEXT,email_address TEXT,UNIQUE(message_id,participant_id,recipient_type))`,
		"DROP TABLE message_recipients_current",
	} {
		loopExec(t, s, statement)
	}
	require.NoError(t, s.InitSchemaContext(t.Context()))
	loopExec(t, s, "DELETE FROM loop_conversation_queue")
	loopExec(t, s, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", mid, pid)
	assert.Equal(t, int64(1), queueRevision(t, s, f.ConvID))
}

func TestLoopSchemaQueueObservedRevisionAcknowledgement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	s := f.Store
	mid := f.CreateMessage("queue-cas")
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loopExec(t, s, "INSERT INTO loop_conversation_queue(conversation_id,revision,feed_bound,epoch,reason) VALUES(?,3,?,'rescan-epoch','rescan')", f.ConvID, old)
	observed := queueRevision(t, s, f.ConvID)
	// An enqueue interleaves after loading the revision and before acknowledgment.
	loopExec(t, s, "UPDATE messages SET source_is_from_me=TRUE WHERE id=?", mid)
	acknowledge := func(revision int64) {
		requirements.NoError(s.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
			ConversationID: f.ConvID, ObservedRevision: revision,
			Epoch: "rescan-epoch", Now: old.Add(time.Hour),
		}))
	}
	acknowledge(observed)
	var revision, processed int64
	var bound sql.NullTime
	var epoch string
	read := func() {
		require.NoError(t, s.DB().QueryRow(s.Rebind("SELECT revision,processed_revision,feed_bound,epoch FROM loop_conversation_queue WHERE conversation_id=?"), f.ConvID).Scan(&revision, &processed, &bound, &epoch))
	}
	read()
	assertions.Equal(int64(5), revision)
	assertions.Equal(int64(3), processed)
	requirements.True(bound.Valid)
	assertions.True(old.Equal(bound.Time))
	assertions.Equal("rescan-epoch", epoch)
	acknowledge(revision)
	read()
	assertions.Equal(revision, processed)
	assertions.False(bound.Valid)
	assertions.Empty(epoch)
	frontier := old.Add(time.Hour)
	loopExec(t, s, "UPDATE loop_derivation_state SET feed_frontier=?", frontier)
	loopExec(t, s, "UPDATE messages SET identity_is_from_me=TRUE WHERE id=?", mid)
	read()
	assertions.Equal(int64(7), revision)
	assertions.Equal(int64(5), processed)
	requirements.True(bound.Valid)
	assertions.True(frontier.Equal(bound.Time))
}
