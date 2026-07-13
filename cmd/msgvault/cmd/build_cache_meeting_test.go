package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// TestBuildCache_IncludesMeetingTranscripts verifies meeting messages and
// their related searchable data are exported to Parquet.
func TestBuildCache_IncludesMeetingTranscripts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")

	// An ordinary email: sender alice → recipient carol.
	emailSrc, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	emailConvID, err := st.EnsureConversationWithType(emailSrc.ID, "thread-1", "email_thread", "Hi")
	require.NoError(err)
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	carolID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err)
	emailID, err := st.UpsertMessage(&store.Message{
		ConversationID:  emailConvID,
		SourceID:        emailSrc.ID,
		SourceMessageID: "m1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: aliceID, Valid: true},
		Subject:         sql.NullString{String: "Hi", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(emailID, "from", []int64{aliceID}, []string{""}))
	require.NoError(st.ReplaceMessageRecipients(emailID, "to", []int64{carolID}, []string{""}))

	// A meeting transcript: organizer dave, attendee bob.
	meetSrc, err := st.GetOrCreateSource("granola", "dave@example.com")
	require.NoError(err)
	meetConvID, err := st.EnsureConversationWithType(meetSrc.ID, "meeting:not_abc", "meeting", "Planning")
	require.NoError(err)
	daveID, err := st.EnsureParticipant("dave@example.com", "Dave", "example.com")
	require.NoError(err)
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	meetingID, err := st.UpsertMessage(&store.Message{
		ConversationID:  meetConvID,
		SourceID:        meetSrc.ID,
		SourceMessageID: "not_abc",
		MessageType:     "meeting_transcript",
		SentAt:          sql.NullTime{Time: time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC), Valid: true},
		SenderID:        sql.NullInt64{Int64: daveID, Valid: true},
		Subject:         sql.NullString{String: "Planning", Valid: true},
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(meetingID, "from", []int64{daveID}, []string{""}))
	require.NoError(st.ReplaceMessageRecipients(meetingID, "to", []int64{bobID}, []string{""}))
	meetingLabelID, err := st.EnsureLabel(meetSrc.ID, "MEETING", "Meeting", "system")
	require.NoError(err)
	require.NoError(st.ReplaceMessageLabels(meetingID, []int64{meetingLabelID}))
	require.NoError(st.UpsertAttachment(meetingID, "transcript.txt", "text/plain", "", "", 42))

	require.NoError(st.Close())

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")
	require.False(result.Skipped, "buildCache unexpectedly skipped")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()

	// Both messages are searchable from the cache.
	msgPattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var msgCount int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`, msgPattern).Scan(&msgCount))
	assert.Equal(2, msgCount, "email and meeting transcript should be exported")

	var meetCount int64
	var cachedConversationID sql.NullInt64
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*), MAX(conversation_id) FROM read_parquet(?, hive_partitioning=true) WHERE message_type = 'meeting_transcript'`,
		msgPattern).Scan(&meetCount, &cachedConversationID))
	assert.Equal(int64(1), meetCount, "meeting_transcript row in messages Parquet")
	assert.Equal(sql.NullInt64{Int64: meetConvID, Valid: true}, cachedConversationID, "meeting message keeps its conversation junction")

	// Meeting attendee and email recipient junction rows are both searchable.
	recPattern := filepath.Join(analyticsDir, "message_recipients", "*.parquet")
	var bobRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, bobID).Scan(&bobRows))
	assert.Equal(1, bobRows, "meeting attendee should be exported")

	var carolRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE participant_id = ?`, recPattern, carolID).Scan(&carolRows))
	assert.Equal(1, carolRows, "email recipient must still be exported")

	var meetingConversationRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE id = ? AND conversation_type = 'meeting'`,
		filepath.Join(analyticsDir, "conversations", "*.parquet"), meetConvID).Scan(&meetingConversationRows))
	assert.Equal(1, meetingConversationRows, "meeting conversation should be exported")

	var meetingLabelRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = ? AND label_id = ?`,
		filepath.Join(analyticsDir, "message_labels", "*.parquet"), meetingID, meetingLabelID).Scan(&meetingLabelRows))
	assert.Equal(1, meetingLabelRows, "meeting label junction should be exported")

	var meetingAttachmentRows int
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = ?`,
		filepath.Join(analyticsDir, "attachments", "*.parquet"), meetingID).Scan(&meetingAttachmentRows))
	assert.Equal(1, meetingAttachmentRows, "meeting attachment junction should be exported")
}
