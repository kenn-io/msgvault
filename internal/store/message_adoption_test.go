package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// adoptionFixture seeds one IMAP message with an old composite key, two
// labels, and a running sync generation.
func adoptionFixture(t *testing.T) (*store.Store, int64, []int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "adopt-atomic@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Adoption")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv,
		SourceMessageID: "INBOX|7", MessageType: "email",
	})
	require.NoError(t, err)
	labelA, err := st.EnsureLabel(src.ID, "INBOX", "INBOX", "system")
	require.NoError(t, err)
	labelB, err := st.EnsureLabel(src.ID, "Archive", "Archive", "user")
	require.NoError(t, err)
	for _, labelID := range []int64{labelA, labelB} {
		_, err = st.DB().Exec(st.Rebind(
			"INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)"),
			messageID, labelID)
		require.NoError(t, err)
	}
	run, err := st.StartSync(src.ID, "full")
	require.NoError(t, err)
	return st.ScopedToSync(src.ID, run), messageID, []int64{labelA, labelB}
}

func adoptionLabelNames(t *testing.T, st *store.Store, messageID int64) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(
		"SELECT l.name FROM message_labels ml JOIN labels l ON ml.label_id = l.id"+
			" WHERE ml.message_id = ? ORDER BY l.name"), messageID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

// A failing label reconciliation must roll the guarded rekey back with the
// labels: the old composite key stays put, so the adoption stays retryable.
// The label write fails through a real constraint — a nonexistent label ID
// violates the foreign key on both SQLite and PostgreSQL.
func TestAdoptMessageSourceIDContextRollsBackLabelFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, messageID, labelIDs := adoptionFixture(t)

	changed, err := st.AdoptMessageSourceIDContext(
		t.Context(), messageID, "INBOX|7", "Archive|9",
		true, append(append([]int64{}, labelIDs...), 999999), true)
	require.Error(err)
	assert.False(changed)
	var sourceMessageID string
	require.NoError(st.DB().QueryRow(st.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), messageID,
	).Scan(&sourceMessageID))
	assert.Equal("INBOX|7", sourceMessageID, "the old composite key must survive a failed label step")
	assert.ElementsMatch([]string{"Archive", "INBOX"}, adoptionLabelNames(t, st, messageID))

	// The retry after the fault clears adopts key and labels atomically.
	changed, err = st.AdoptMessageSourceIDContext(
		t.Context(), messageID, "INBOX|7", "Archive|9", true, labelIDs[:1], true)
	require.NoError(err)
	assert.True(changed)
	require.NoError(st.DB().QueryRow(st.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), messageID,
	).Scan(&sourceMessageID))
	assert.Equal("Archive|9", sourceMessageID)
	assert.ElementsMatch([]string{"INBOX"}, adoptionLabelNames(t, st, messageID))
}

// Deferred authoritative reconciliation rekeys alone and never touches
// labels; the command finalizer owns them.
func TestAdoptMessageSourceIDContextDeferredLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, messageID, labelIDs := adoptionFixture(t)

	changed, err := st.AdoptMessageSourceIDContext(
		t.Context(), messageID, "INBOX|7", "Trash|3", false, nil, true)
	require.NoError(err)
	assert.True(changed)
	var sourceMessageID string
	require.NoError(st.DB().QueryRow(st.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), messageID,
	).Scan(&sourceMessageID))
	assert.Equal("Trash|3", sourceMessageID)
	assert.ElementsMatch([]string{"Archive", "INBOX"}, adoptionLabelNames(t, st, messageID),
		"deferred reconciliation leaves labels untouched")
	assert.NotEmpty(labelIDs)
}

// The expected-old-source guard must reject a lost race without touching the
// row, and a canceled context must fail before any write commits.
func TestAdoptMessageSourceIDContextGuardAndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, messageID, _ := adoptionFixture(t)

	_, err := st.AdoptMessageSourceIDContext(
		t.Context(), messageID, "INBOX|8", "Trash|3", true, nil, false)
	require.ErrorContains(err, "identity guard mismatch")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.AdoptMessageSourceIDContext(
		ctx, messageID, "INBOX|7", "Trash|3", true, nil, false)
	require.Error(err)

	var sourceMessageID string
	require.NoError(st.DB().QueryRow(st.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), messageID,
	).Scan(&sourceMessageID))
	assert.Equal("INBOX|7", sourceMessageID,
		"a lost race and cancellation must both leave the old key in place")
}
