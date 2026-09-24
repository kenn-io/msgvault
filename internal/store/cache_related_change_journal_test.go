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
	_, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{
		"remote-label": {Name: "Before", Type: "user"},
	})
	require.NoError(err)
	var baseline int64
	require.NoError(f.Store.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	_, err = f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{
		"remote-label": {Name: "After", Type: "user"},
	})
	require.NoError(err)
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
		WHERE seq > ? AND dataset = 'labels' AND message_id = 0`, baseline).Scan(&count))
	assert.Positive(t, count)
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
	_, err = st.DB().Exec(`INSERT INTO labels (id, name) VALUES (1, 'synthetic')`)
	require.NoError(err)

	var baseline int64
	require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	_, err = st.DB().Exec(`INSERT INTO message_recipients (message_id, participant_id, recipient_type)
		VALUES (?, ?, 'to')`, first, participant)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE message_recipients SET message_id = ? WHERE message_id = ?`, second, first)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ?`, second)
	require.NoError(err)

	_, err = st.DB().Exec(`INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, first)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE message_labels SET message_id = ? WHERE message_id = ?`, second, first)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM message_labels WHERE message_id = ?`, second)
	require.NoError(err)

	_, err = st.DB().Exec(`INSERT INTO attachments (id, message_id, storage_path) VALUES (1, ?, 'synthetic')`, first)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE attachments SET message_id = ? WHERE id = 1`, second)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM attachments WHERE id = 1`)
	require.NoError(err)

	rows, err := st.DB().Query(`SELECT dataset, message_id FROM cache_related_change_journal WHERE seq > ? ORDER BY seq`, baseline)
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

func TestCacheFromRecipientChangesInvalidateMessageFacts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	st := f.Store
	first, err := st.UpsertMessage(f.NewMessage().WithSourceMessageID("first-from").Build())
	require.NoError(err)
	second, err := st.UpsertMessage(f.NewMessage().WithSourceMessageID("second-from").Build())
	require.NoError(err)
	firstParticipant := f.EnsureParticipant("first@example.test", "First", "example.test")
	secondParticipant := f.EnsureParticipant("second@example.test", "Second", "example.test")
	countFacts := func(messageID int64) int {
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
			WHERE dataset = 'message_facts' AND message_id = ?`, messageID).Scan(&count))
		return count
	}
	var recipientID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO message_recipients
		(message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'from', 'first@example.test') RETURNING id`, first, firstParticipant).Scan(&recipientID))
	assert.Equal(1, countFacts(first), "From insert changes the cached owner")
	_, err = st.DB().Exec(`UPDATE message_recipients SET display_name = 'Renamed' WHERE id = ?`, recipientID)
	require.NoError(err)
	assert.Equal(1, countFacts(first), "display-name edits do not change cached message facts")
	_, err = st.DB().Exec(`UPDATE message_recipients SET email_address = 'alias@example.test' WHERE id = ?`, recipientID)
	require.NoError(err)
	assert.Equal(2, countFacts(first), "envelope change can change the cached owner")
	_, err = st.DB().Exec(`UPDATE message_recipients SET participant_id = ? WHERE id = ?`, secondParticipant, recipientID)
	require.NoError(err)
	assert.Equal(3, countFacts(first), "From participant change invalidates cached facts")
	_, err = st.DB().Exec(`UPDATE message_recipients SET message_id = ? WHERE id = ?`, second, recipientID)
	require.NoError(err)
	assert.Equal(4, countFacts(first), "moving From row invalidates its old message")
	assert.Equal(1, countFacts(second), "moving From row invalidates its new message")
	_, err = st.DB().Exec(`UPDATE message_recipients SET recipient_type = 'to' WHERE id = ?`, recipientID)
	require.NoError(err)
	assert.Equal(2, countFacts(second), "removing From role invalidates cached facts")
	_, err = st.DB().Exec(`UPDATE message_recipients SET recipient_type = 'from' WHERE id = ?`, recipientID)
	require.NoError(err)
	assert.Equal(3, countFacts(second), "adding From role invalidates cached facts")
	_, err = st.DB().Exec(`DELETE FROM message_recipients WHERE id = ?`, recipientID)
	require.NoError(err)
	assert.Equal(4, countFacts(second), "From delete invalidates cached facts")
	_, err = st.DB().Exec(`INSERT INTO message_recipients
		(message_id, participant_id, recipient_type) VALUES (?, ?, 'to')`, first, firstParticipant)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE message_recipients SET participant_id = ?
		WHERE message_id = ? AND recipient_type = 'to'`, secondParticipant, first)
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`, first)
	require.NoError(err)
	assert.Equal(4, countFacts(first), "To edits keep the child-row refresh path")
}

func TestCacheFromFactsUpdateBeforeLegacyEnvelopeColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "SQLite legacy recipient column upgrade")
	f := storetest.New(t)
	st := f.Store
	messageID, err := st.UpsertMessage(f.NewMessage().Build())
	require.NoError(err)
	first := f.EnsureParticipant("legacy-first@example.test", "First", "example.test")
	second := f.EnsureParticipant("legacy-second@example.test", "Second", "example.test")
	_, err = st.DB().Exec(`INSERT INTO message_recipients
		(message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`, messageID, first)
	require.NoError(err)
	var baseline int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
		WHERE dataset = 'message_facts' AND message_id = ?`, messageID).Scan(&baseline))
	_, err = st.DB().Exec(`DROP INDEX idx_message_recipients_envelope`)
	require.NoError(err)
	_, err = st.DB().Exec(`ALTER TABLE message_recipients DROP COLUMN email_address`)
	require.NoError(err, "recreate recipient shape before legacy envelope-column migration")
	_, err = st.DB().Exec(`UPDATE message_recipients SET participant_id = ?
		WHERE message_id = ? AND recipient_type = 'from'`, second, messageID)
	require.NoError(err, "From update must run before email_address column exists")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal
		WHERE dataset = 'message_facts' AND message_id = ?`, messageID).Scan(&count))
	assert.Equal(baseline+1, count)
}

func TestCacheRelatedChangeJournalRollsBackWithMutation(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "analytics cache journal is SQLite-only")
	f := storetest.New(t)
	st := f.Store
	messageID, err := st.UpsertMessage(f.NewMessage().Build())
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO labels (id, name) VALUES (1, 'synthetic')`)
	require.NoError(err)
	var baseline int64
	require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&baseline))
	tx, err := st.DB().Begin()
	require.NoError(err)
	_, err = tx.Exec(`INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, messageID)
	require.NoError(err)
	require.NoError(tx.Rollback())
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ?`, baseline).Scan(&count))
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
		_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS ` + name)
		require.NoError(err)
	}
	_, err := st.DB().Exec(`DROP TABLE cache_related_change_journal`)
	require.NoError(err)
	require.NoError(st.InitSchema())
	messageID, err := st.UpsertMessage(f.NewMessage().Build())
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO labels (id, name) VALUES (1, 'synthetic')`)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO message_labels (message_id, label_id) VALUES (?, 1)`, messageID)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal WHERE dataset = 'message_labels' AND message_id = ?`, messageID).Scan(&count))
	assert.Equal(t, 1, count)
}
