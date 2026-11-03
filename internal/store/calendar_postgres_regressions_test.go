package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalDAVEventUIDFallsBackToProviderIDPostgres(t *testing.T) {
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\n" +
		"UID:fallback@example.org\r\nDTSTART:20270302T170000Z\r\nDTEND:20270302T180000Z\r\n" +
		"SUMMARY:Fallback UID\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	_, messageID := caldavEventFixture(t, st, "fallback-uid", raw)

	rows := calendarRows(t, st, messageID)
	require.Len(t, rows, 1)
	assert.Equal(t, "fallback@example.org", rows[0].ICalUID)
}

func TestReadLoopMessageEvidenceSnapshotRebindsPostgresPlaceholders(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)
	source, err := st.GetOrCreateSource("gmail", "snapshot-evidence@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "snapshot-evidence", "email_thread", "Synthetic")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&Message{
		SourceID: source.ID, SourceMessageID: "snapshot-evidence",
		ConversationID: conversationID, MessageType: "email",
	})
	requirements.NoError(err)
	_, token, found, err := st.LoopMessageEvidenceContext(t.Context(), messageID, 1)
	requirements.NoError(err)
	assertions.False(found)
	want := LoopMessageEvidence{Asks: true}
	requirements.NoError(st.SaveLoopMessageEvidenceContext(t.Context(), messageID, 1, token, want))

	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	got, found, err := st.ReadLoopMessageEvidenceSnapshotContext(ctx, messageID, 1)
	requirements.NoError(err)
	requirements.True(found)
	assertions.Equal(want, got)
}
