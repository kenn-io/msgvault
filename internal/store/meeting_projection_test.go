package store

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

const meetingProjectionRaw = `{"summary_text":"Source summary","started_at":"2026-09-01T10:00:00Z","ended_at":"2026-09-01T10:00:01.5Z","action_items":[{"source_id":"a","title":"Send brief","status":"open"}]}`

func projectionFixture(t *testing.T, st *Store, key, format, raw string) (*MessagePersistData, int64) {
	t.Helper()
	source, err := st.GetOrCreateSource("meeting_import", "projection@example.com")
	require.NoError(t, err)
	data := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript", Subject: sql.NullString{String: "Synthetic meeting", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "meeting", Title: "Synthetic meeting"},
		BodyText:     sql.NullString{String: "Rendered body", Valid: true}, RawMIME: []byte(raw), RawFormat: format,
	}
	id, err := st.PersistMessage(data)
	require.NoError(t, err)
	require.NoError(t, st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, id).Scan(&data.Message.ConversationID))
	return data, id
}

func readProjection(t *testing.T, st *Store, id int64) (meetingcontent.Content, string) {
	t.Helper()
	var raw, hash string
	require.NoError(t, st.db.QueryRow(`SELECT content_json, content_hash FROM meeting_details WHERE message_id = ?`, id).Scan(&raw, &hash))
	var content meetingcontent.Content
	require.NoError(t, json.Unmarshal([]byte(raw), &content))
	return content, hash
}

func TestMeetingProjectionPersistRefreshAndRemove(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := projectionFixture(t, st, "snapshot", "meeting_json", meetingProjectionRaw)
	content, hash := readProjection(t, st, id)
	requirements.NotNil(content.DurationSeconds)
	assertions.InDelta(1.5, *content.DurationSeconds, 0)
	assertions.Equal(meetingcontent.CoverageAvailable, content.ActionCoverage)
	requirements.Len(content.Actions, 1)
	assertions.Equal(meetingcontent.StatusPending, content.Actions[0].Status)
	var duration float64
	requirements.NoError(st.db.QueryRow(`SELECT duration_seconds FROM meeting_details WHERE message_id = ?`, id).Scan(&duration))
	assertions.InDelta(1.5, duration, 0)

	data.RawMIME = nil
	data.Metadata = nil
	_, err := st.PersistMessage(data)
	requirements.NoError(err)
	_, preservedHash := readProjection(t, st, id)
	assertions.Equal(hash, preservedHash)

	data.RawMIME = []byte(`{"summary_text":"Source summary","transcript":"Late private transcript","action_items":[]}`)
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	content, updatedHash := readProjection(t, st, id)
	assertions.NotEqual(hash, updatedHash)
	assertions.Equal(meetingcontent.StateAvailable, content.Transcript.State)
	assertions.Empty(content.Transcript.Text)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_action_items WHERE message_id = ?`, id).Scan(&count))
	assertions.Zero(count)

	// Even when compact projection JSON is identical, full transcript changes update the snapshot hash.
	data.RawMIME = []byte(`{"summary_text":"Source summary","transcript":"Changed private transcript","action_items":[]}`)
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	_, newerHash := readProjection(t, st, id)
	assertions.NotEqual(updatedHash, newerHash)

	data.Message.MessageType = "email"
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_details WHERE message_id = ?`, id).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingProjectionDirectWritesAndCascade(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := projectionFixture(t, st, "direct", "meeting_json", meetingProjectionRaw)
	requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(`{"meeting":{"notes":"same body","actionItems":[{"title":"Changed action","status":"done"}]}}`), "circleback_json"))
	content, _ := readProjection(t, st, id)
	requirements.Len(content.Actions, 1)
	assertions.Equal("Changed action", content.Actions[0].Title)
	assertions.Equal(meetingcontent.StatusCompleted, content.Actions[0].Status)
	requirements.NoError(st.UpsertMessageRaw(id, []byte("MIME evidence")))
	content, _ = readProjection(t, st, id)
	assertions.Equal(meetingcontent.CoverageUnavailable, content.ActionCoverage)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"extra":"preserved"}`, Valid: true}))
	data.RawMIME = []byte(meetingProjectionRaw)
	_, err := st.PersistMessage(data)
	requirements.NoError(err)
	metadata, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.JSONEq(`{"extra":"preserved"}`, metadata.String)
	data.Message.MessageType = "email"
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_details WHERE message_id = ?`, id).Scan(&count))
	assertions.Zero(count)
	data.Message.MessageType = "meeting_transcript"
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	content, _ = readProjection(t, st, id)
	requirements.Len(content.Actions, 1)
	_, err = st.db.Exec(`DELETE FROM messages WHERE id = ?`, id)
	requirements.NoError(err)
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_details`).Scan(&count))
	assertions.Zero(count)
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM meeting_action_items`).Scan(&count))
	assertions.Zero(count)
}

func rejectMeetingProjectionWrites(t *testing.T, st *Store) {
	t.Helper()
	var statements []string
	if st.dialect.DriverName() == postgresDriverName {
		statements = []string{`CREATE FUNCTION reject_meeting_projection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'projection blocked'; END $$`, `CREATE TRIGGER reject_meeting_projection BEFORE INSERT OR UPDATE ON meeting_details FOR EACH ROW EXECUTE FUNCTION reject_meeting_projection()`}
	} else {
		statements = []string{`CREATE TRIGGER reject_meeting_projection BEFORE INSERT ON meeting_details BEGIN SELECT RAISE(ABORT, 'projection blocked'); END`}
	}
	for _, statement := range statements {
		_, err := st.db.Exec(statement)
		require.NoError(t, err)
	}
}

func TestMeetingProjectionStorageFailureRollsBackEvidence(t *testing.T) {
	for _, operation := range []string{"persist", "raw", "mime", "metadata", "type"} {
		t.Run(operation, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, id := projectionFixture(t, st, "rollback", "meeting_json", meetingProjectionRaw)
			before, hash := readProjection(t, st, id)
			var compressedBefore []byte
			requirements.NoError(st.db.QueryRow(`SELECT raw_data FROM message_raw WHERE message_id = ?`, id).Scan(&compressedBefore))
			// Force a refresh even for the metadata-only write, whose decoder currently ignores metadata.
			_, err := st.db.Exec(`UPDATE meeting_details SET projection_version = 0 WHERE message_id = ?`, id)
			requirements.NoError(err)
			rejectMeetingProjectionWrites(t, st)
			switch operation {
			case "persist":
				data.RawMIME = []byte(`{"summary_text":"Changed","action_items":[]}`)
				data.BodyText = sql.NullString{String: "Changed body", Valid: true}
				data.Metadata = &sql.NullString{String: `{"changed":true}`, Valid: true}
				_, err = st.PersistMessage(data)
			case "raw":
				err = st.UpsertMessageRawWithFormat(id, []byte(`{"action_items":[]}`), "meeting_json")
			case "mime":
				err = st.UpsertMessageRaw(id, []byte("changed MIME"))
			case "metadata":
				err = st.SetMessageMetadata(id, sql.NullString{String: `{"changed":true}`, Valid: true})
			case "type":
				data.Message.Subject = sql.NullString{String: "Changed subject", Valid: true}
				_, err = st.UpsertMessage(data.Message)
			}
			requirements.ErrorContains(err, "projection blocked")
			after, afterHash := readProjection(t, st, id)
			assertions.Equal(before, after)
			assertions.Equal(hash, afterHash)
			var compressedAfter []byte
			requirements.NoError(st.db.QueryRow(`SELECT raw_data FROM message_raw WHERE message_id = ?`, id).Scan(&compressedAfter))
			assertions.Equal(compressedBefore, compressedAfter)
			var body, subject string
			var metadata sql.NullString
			requirements.NoError(st.db.QueryRow(`SELECT body_text FROM message_bodies WHERE message_id = ?`, id).Scan(&body))
			requirements.NoError(st.db.QueryRow(`SELECT subject, metadata FROM messages WHERE id = ?`, id).Scan(&subject, &metadata))
			assertions.Equal("Rendered body", body)
			assertions.Equal("Synthetic meeting", subject)
			assertions.False(metadata.Valid)
		})
	}
}

func TestMeetingProjectionActionWriteFailureRestoresPriorSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, id := projectionFixture(t, st, "action-rollback", "meeting_json", meetingProjectionRaw)
	_, oldHash := readProjection(t, st, id)
	if st.IsPostgreSQL() {
		_, err := st.db.Exec(`CREATE FUNCTION reject_meeting_action() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'action blocked'; END $$`)
		requirements.NoError(err)
		_, err = st.db.Exec(`CREATE TRIGGER reject_meeting_action BEFORE INSERT ON meeting_action_items FOR EACH ROW EXECUTE FUNCTION reject_meeting_action()`)
		requirements.NoError(err)
	} else {
		_, err := st.db.Exec(`CREATE TRIGGER reject_meeting_action BEFORE INSERT ON meeting_action_items BEGIN SELECT RAISE(ABORT, 'action blocked'); END`)
		requirements.NoError(err)
	}
	data.RawMIME = []byte(`{"summary_text":"Revised summary","action_items":[{"title":"Replacement"}]}`)
	_, err := st.PersistMessage(data)
	requirements.ErrorContains(err, "action blocked")
	content, hash := readProjection(t, st, id)
	assertions.Equal(oldHash, hash)
	assertions.Equal("Source summary", content.Summary.Text)
	var title string
	requirements.NoError(st.db.QueryRow(`SELECT title FROM meeting_action_items WHERE message_id = ?`, id).Scan(&title))
	assertions.Equal("Send brief", title)
}

func TestMeetingProjectionUnavailableRawStillPersists(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for _, tc := range []struct{ name, format, raw, reason string }{
		{"missing", "meeting_json", "", "missing_raw"},
		{"invalid", "meeting_json", "{broken", "invalid_raw"},
		{"unknown", "future_json", "{}", "unsupported_format"},
		{"new schema", "notion_meeting_json", `{"schema_version":99}`, "unsupported_schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, id := projectionFixture(t, st, tc.name, tc.format, tc.raw)
			content, _ := readProjection(t, st, id)
			assert.Equal(t, meetingcontent.CoverageUnavailable, content.ActionCoverage)
			assert.Equal(t, tc.reason, content.ActionReason)
		})
	}
}

func TestMeetingProjectionCompressedRawBoundAndCorruption(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "bounded", "meeting_json", meetingProjectionRaw)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	chunk := bytes.Repeat([]byte("x"), 1024)
	for range 65537 {
		_, err := writer.Write(chunk)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	for _, tc := range []struct {
		name       string
		compressed []byte
		reason     string
	}{
		{"oversized", compressed.Bytes(), "raw_too_large"},
		{"corrupt", []byte("invalid zlib stream"), "invalid_raw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, err := st.db.Exec(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`, tc.compressed, id)
			requirements.NoError(err)
			requirements.NoError(st.SetMessageMetadata(id, sql.NullString{}))
			content, _ := readProjection(t, st, id)
			assertions.Equal(meetingcontent.CoverageUnavailable, content.ActionCoverage)
			assertions.Equal(tc.reason, content.ActionReason)
			assertions.Empty(content.Actions)
		})
	}
}

func TestMeetingProjectionPostgresKeepsWideMessageIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL ID width contract")
	}
	var next int64
	requirements.NoError(st.db.QueryRow(`SELECT setval(pg_get_serial_sequence('messages', 'id'), 2147483648, false)`).Scan(&next))
	_, id := projectionFixture(t, st, "wide", "meeting_json", meetingProjectionRaw)
	assertions.Equal(int64(2147483648), id)
	content, _ := readProjection(t, st, id)
	requirements.Len(content.Actions, 1)
	var storedID int64
	requirements.NoError(st.db.QueryRow(`SELECT message_id FROM meeting_action_items WHERE message_id = ?`, id).Scan(&storedID))
	assertions.Equal(id, storedID)
}
