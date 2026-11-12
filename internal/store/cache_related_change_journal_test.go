package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCacheRelatedChangeJournalTracksUnlinkedLabelRename(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	_, err := f.Store.EnsureLabelsBatch(t.Context(), f.Source.ID, map[string]store.LabelInfo{
		"remote-label": {Name: "Before", Type: "user"},
	})
	require.NoError(err)
	var baseline int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	_, err = f.Store.EnsureLabelsBatch(t.Context(), f.Source.ID, map[string]store.LabelInfo{
		"remote-label": {Name: "After", Type: "user"},
	})
	require.NoError(err)
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal
		WHERE seq > ? AND dataset = 'labels' AND message_id = 0`, baseline).Scan(&count))
	assert.Positive(t, count)
}

func TestCacheMessageSizeTriggerUpgradesExistingArchive(t *testing.T) {
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	req := require.New(t)
	f := storetest.New(t)
	s := f.Store
	messageID, err := s.UpsertMessage(f.NewMessage().Build())
	req.NoError(err)
	_, err = s.DB().ExecContext(t.Context(), `DROP TRIGGER trg_cache_message_facts_update;
		CREATE TRIGGER trg_cache_message_facts_update AFTER UPDATE OF sender_id ON messages
		BEGIN INSERT INTO cache_related_change_journal (dataset, message_id) VALUES ('message_facts', NEW.id); END;
		UPDATE applied_migrations SET version = 3 WHERE name = 'cache_message_source_attribution';
		DELETE FROM cache_related_change_journal`)
	req.NoError(err)
	req.NoError(s.InitSchema())
	var repaired int
	req.NoError(s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE dataset = 'message_facts' AND message_id = 0`).Scan(&repaired))
	assert.Equal(t, 1, repaired, "upgrade repairs facts missed by the old trigger")
	req.NoError(s.InitSchema())
	req.NoError(s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE dataset = 'message_facts' AND message_id = 0`).Scan(&repaired))
	assert.Equal(t, 1, repaired, "repair runs once")
	_, err = s.DB().ExecContext(t.Context(), `UPDATE messages SET size_estimate = 42 WHERE id = ?`, messageID)
	req.NoError(err)
	_, err = s.DB().ExecContext(t.Context(), `UPDATE messages SET size_estimate = 42 WHERE id = ?`, messageID)
	req.NoError(err)
	var changed int
	req.NoError(s.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE dataset = 'message_facts' AND message_id = ?`, messageID).Scan(&changed))
	assert.Equal(t, 1, changed, "only changed sizes invalidate cached facts")
}

func TestCacheRelatedChangeJournalTracksChildMutations(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	st := f.Store
	first, err := st.UpsertMessage(f.NewMessage().WithSourceMessageID("first").Build())
	require.NoError(err)
	second, err := st.UpsertMessage(f.NewMessage().WithSourceMessageID("second").Build())
	require.NoError(err)
	participant := f.EnsureParticipant("recipient@example.com", "Recipient", "example.com")
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO labels (id, name) VALUES (1, 'synthetic')`)
	require.NoError(err)

	var baseline int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO message_recipients (message_id, participant_id, recipient_type)
		VALUES (?, ?, 'to')`, first, participant)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE message_recipients SET message_id = ? WHERE message_id = ?`, second, first)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM message_recipients WHERE message_id = ?`, second)
	require.NoError(err)

	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, first)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE message_labels SET message_id = ? WHERE message_id = ?`, second, first)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM message_labels WHERE message_id = ?`, second)
	require.NoError(err)

	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO attachments (id, message_id, storage_path) VALUES (1, ?, 'synthetic')`, first)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE attachments SET message_id = ? WHERE id = 1`, second)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM attachments WHERE id = 1`)
	require.NoError(err)

	rows, err := st.DB().QueryContext(t.Context(), `SELECT dataset, message_id FROM cache_related_change_journal WHERE seq > ? ORDER BY seq`, baseline)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	type change struct {
		dataset   string
		messageID int64
	}
	var got []change
	for rows.Next() {
		var entry change
		require.NoError(rows.Scan(&entry.dataset, &entry.messageID))
		got = append(got, entry)
	}
	require.NoError(rows.Err())
	assert.Equal(t, []change{
		{"message_recipients", first}, {"message_recipients", first},
		{"message_recipients", second}, {"message_recipients", second},
		{"message_labels", first}, {"message_labels", first},
		{"message_labels", second}, {"message_labels", second},
		{"attachments", first}, {"attachments", first},
		{"attachments", second}, {"attachments", second},
	}, got)
}

func TestCacheRelatedChangeJournalRollsBackWithMutation(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	st := f.Store
	messageID, err := st.UpsertMessage(f.NewMessage().Build())
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO labels (id, name) VALUES (1, 'synthetic')`)
	require.NoError(err)
	var baseline int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, messageID)
	require.NoError(err)
	require.NoError(tx.Rollback())
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ?`, baseline).Scan(&count))
	assert.Zero(t, count)
}

func TestCacheRelatedChangeJournalInstallsOnExistingArchive(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	st := f.Store
	for _, name := range []string{
		"trg_cache_recipients_insert", "trg_cache_recipients_update", "trg_cache_recipients_delete",
		"trg_cache_labels_insert", "trg_cache_labels_update", "trg_cache_labels_delete",
		"trg_cache_attachments_insert", "trg_cache_attachments_update", "trg_cache_attachments_delete",
	} {
		_, err := st.DB().ExecContext(t.Context(), `DROP TRIGGER IF EXISTS `+name)
		require.NoError(err)
	}
	_, err := st.DB().ExecContext(t.Context(), `DROP TABLE cache_related_change_journal`)
	require.NoError(err)
	require.NoError(st.InitSchema())
	messageID, err := st.UpsertMessage(f.NewMessage().Build())
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO labels (id, name) VALUES (1, 'synthetic')`)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, messageID)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal WHERE dataset = 'message_labels' AND message_id = ?`, messageID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestCacheRelatedChangeJournalTracksHiddenTextFlag(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	st := f.Store
	messageID, err := st.UpsertMessage(f.NewMessage().Build())
	require.NoError(err)
	var baseline int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET hidden_text_present = 1 WHERE id = ?`, messageID)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cache_related_change_journal
		WHERE seq > ? AND dataset = 'message_facts' AND message_id = ?`, baseline, messageID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestMeetingOccurrenceJournalUpgradesSchema42(t *testing.T) {
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	s := f.Store
	ctx := t.Context()
	first, err := s.UpsertMessage(f.NewMessage().WithSourceMessageID("event").Build())
	require.NoError(t, err)
	second, err := s.UpsertMessage(f.NewMessage().WithSourceMessageID("note").Build())
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `DROP TRIGGER trg_cache_meeting_events_insert;
		DROP TRIGGER trg_cache_meeting_sources_update;
		DROP TRIGGER trg_cache_meeting_source_deletion_update;
		DROP TRIGGER trg_cache_meeting_events_update;
		DROP TRIGGER trg_cache_meeting_events_delete;
		DROP TRIGGER trg_cache_meeting_details_update;
		DROP TABLE meeting_link_decision_dependencies;
		DROP TABLE meeting_link_decisions;
		UPDATE archive_metadata SET value = '42' WHERE key = 'schema_version'`)
	require.NoError(t, err)
	require.NoError(t, s.InitSchemaUnlessCurrentContext(ctx))
	version, err := s.SchemaVersionContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, version)
	_, err = s.DB().ExecContext(ctx, `UPDATE messages SET message_type='calendar_event' WHERE id=?`, first)
	require.NoError(t, err)
	var baseline int64
	require.NoError(t, s.DB().QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	_, err = s.DB().ExecContext(ctx, `UPDATE sources SET sync_config='{"calendar_id":"owner@example.com"}' WHERE id=?`, f.Source.ID)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `INSERT INTO calendar_events (message_id, projection_version, content_hash) VALUES (?, 1, 'first')`, first)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `UPDATE calendar_events SET content_hash='second' WHERE message_id=?`, first)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `DELETE FROM calendar_events WHERE message_id=?`, first)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `INSERT INTO meeting_details (message_id, projection_version, content_hash, content_json, action_coverage, transcript_state) VALUES (?, 1, 'first', '{}', '', 'empty')`, second)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `UPDATE meeting_details SET transcript_state='available' WHERE message_id=?`, second)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `INSERT INTO meeting_link_decisions (left_message_id, left_kind, left_key, left_anchor, right_message_id, right_kind, right_key, right_anchor, decision, choice_key) VALUES (?, 'evt', '', '', ?, 'msg', '', '', 'link', 'choice')`, first, second)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `UPDATE meeting_link_decisions SET decision='split' WHERE choice_key='choice'`)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE choice_key='choice'`)
	require.NoError(t, err)
	var changes int
	require.NoError(t, s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset='meeting_occurrences' AND message_id=0`, baseline).Scan(&changes))
	assert.Equal(t, 8, changes)
	_, err = s.DB().ExecContext(ctx, `UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`, first)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `UPDATE messages SET deleted_from_source_at=NULL WHERE id=?`, first)
	require.NoError(t, err)
	require.NoError(t, s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset='message_facts' AND message_id=?`, baseline, first).Scan(&changes))
	assert.Equal(t, 2, changes)
}
