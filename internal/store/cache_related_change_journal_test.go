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
