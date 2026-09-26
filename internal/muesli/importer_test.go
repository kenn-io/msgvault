package muesli

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type importerFixture struct {
	st     *store.Store
	muesli *sql.DB
	path   string
	source *store.Source
	imp    *Importer
}

func newImporterFixture(t *testing.T) *importerFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "mac")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "muesli.db")
	return &importerFixture{
		st: st, muesli: newFixtureDB(t, path, currentSchemaDDL), path: path,
		source: source, imp: NewImporter(st),
	}
}

func (f *importerFixture) run(t *testing.T, opts ImportOptions) (*ImportSummary, error) {
	t.Helper()
	if opts.Identifier == "" {
		opts.Identifier = "mac"
	}
	if opts.AccountEmail == "" {
		opts.AccountEmail = "you@example.com"
	}
	if opts.DBPath == "" {
		opts.DBPath = f.path
	}
	return f.imp.Import(context.Background(), opts)
}

func (f *importerFixture) body(t *testing.T, sourceMessageID string) string {
	t.Helper()
	var messageID int64
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(
		`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		f.source.ID, sourceMessageID).Scan(&messageID))
	body, err := f.st.GetMessageBodyText(messageID)
	require.NoError(t, err)
	return body
}

func (f *importerFixture) messageCount(t *testing.T) int {
	t.Helper()
	var count int
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(
		`SELECT count(*) FROM messages WHERE source_id = ?`), f.source.ID).Scan(&count))
	return count
}

func TestImportArchivesMeetingsAndIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	first := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"title": "Planning", "created_at": "2026-09-02 09:00:00", "start_time": "2026-09-02T09:00:00Z",
	}))
	insertRow(t, f.muesli, "meeting_participants", map[string]any{
		"meeting_id": first, "participant_identifier": "email:alice@example.com",
		"display_name": "Alice Example", "email_address": "alice@example.com",
		"insertion_order": 0, "source": "calendar",
	})

	summary, err := f.run(t, ImportOptions{})
	require.NoError(err)
	assert.Equal(int64(2), summary.MeetingsProcessed)
	assert.Equal(int64(2), summary.MeetingsAdded)
	assert.Equal(2, f.messageCount(t))

	var messageType string
	var fromMe bool
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`
		SELECT message_type, is_from_me FROM messages
		WHERE source_id = ? AND source_message_id = ?`),
		f.source.ID, "meeting:1:20260901T140003Z").Scan(&messageType, &fromMe))
	assert.Equal("meeting_transcript", messageType)
	assert.True(fromMe, "the configured account recorded the meeting")
	var recipients int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`
		SELECT count(*) FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_id = ? AND mr.recipient_type = 'to' AND p.email_address = ?`),
		f.source.ID, "alice@example.com").Scan(&recipients))
	assert.Equal(1, recipients)

	again, err := f.run(t, ImportOptions{})
	require.NoError(err)
	assert.Equal(int64(2), again.MeetingsProcessed)
	assert.Equal(int64(0), again.MeetingsAdded)
	assert.Equal(int64(0), again.MeetingsUpdated)
}

func TestImportUpdatesEditedMeetingInPlace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	id := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	_, err := f.run(t, ImportOptions{})
	require.NoError(err)

	_, err = f.muesli.Exec(`UPDATE meetings SET raw_transcript = ? WHERE id = ?`,
		"[10:00:01] You: revised words", id)
	require.NoError(err)
	insertRow(t, f.muesli, "meeting_participants", map[string]any{
		"meeting_id": id, "participant_identifier": "contact:XYZ",
		"display_name": "Carol Example", "insertion_order": 0,
	})
	summary, err := f.run(t, ImportOptions{})
	require.NoError(err)

	assert.Equal(int64(0), summary.MeetingsAdded)
	assert.Equal(int64(1), summary.MeetingsUpdated)
	assert.Equal(1, f.messageCount(t))
	body := f.body(t, "meeting:1:20260901T140003Z")
	assert.Contains(body, "revised words")
	assert.Contains(body, "Attendees: Carol Example")
}

func TestImportSkipsDeletedInProgressAndEmptyMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	archived := insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"created_at": "2026-09-01 15:00:00", "deleted_at": 1788271503.0,
	}))
	recording := insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"created_at": "2026-09-01 16:00:00", "meeting_status": "recording",
	}))
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"created_at": "2026-09-01 17:00:00", "raw_transcript": "", "formatted_notes": "", "manual_notes": "",
	}))

	summary, err := f.run(t, ImportOptions{})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(int64(1), summary.SkippedDeleted)
	assert.Equal(int64(1), summary.SkippedInProgress)
	assert.Equal(int64(1), summary.SkippedEmpty)

	// Muesli's soft delete blanks the row; the archived copy must survive.
	before := f.body(t, "meeting:1:20260901T140003Z")
	_, err = f.muesli.Exec(`UPDATE meetings SET title = 'Deleted Meeting', raw_transcript = NULL,
		formatted_notes = NULL, manual_notes = '', deleted_at = 1788271600 WHERE id = ?`, archived)
	require.NoError(err)
	_, err = f.muesli.Exec(`UPDATE meetings SET meeting_status = 'completed' WHERE id = ?`, recording)
	require.NoError(err)

	next, err := f.run(t, ImportOptions{})
	require.NoError(err)
	assert.Equal(int64(1), next.MeetingsAdded, "the finished recording is archived")
	assert.Equal(int64(2), next.SkippedDeleted)
	assert.Equal(before, f.body(t, "meeting:1:20260901T140003Z"))
}

func TestImportDatabaseResetDoesNotOverwriteArchivedMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"title": "Before reset"}))
	_, err := f.run(t, ImportOptions{})
	require.NoError(err)

	resetPath := filepath.Join(t.TempDir(), "muesli.db")
	reset := newFixtureDB(t, resetPath, currentSchemaDDL)
	insertRow(t, reset, "meetings", completedMeeting(map[string]any{
		"title": "After reset", "created_at": "2026-10-01 08:00:00",
	}))
	summary, err := f.run(t, ImportOptions{DBPath: resetPath})
	require.NoError(err)

	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(2, f.messageCount(t))
	assert.Contains(f.body(t, "meeting:1:20260901T140003Z"), "Before reset")
	assert.Contains(f.body(t, "meeting:1:20261001T080000Z"), "After reset")
}

func TestImportCountsUnkeyableMeetingsAndContinues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"created_at": nil}))
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{"created_at": "2026-09-02 09:00:00"}))

	summary, err := f.run(t, ImportOptions{})

	require.Error(err)
	assert.Contains(err.Error(), "muesli meeting 1")
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(int64(1), summary.Errors)
	run, runErr := f.st.GetLatestSync(f.source.ID)
	require.NoError(runErr)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Equal(int64(1), run.ErrorsCount)
}

func TestImportLimitAndStartedAfter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"created_at": "2026-09-10 09:00:00", "start_time": "2026-09-10T09:00:00Z",
	}))
	insertRow(t, f.muesli, "meetings", completedMeeting(map[string]any{
		"created_at": "2026-09-11 09:00:00", "start_time": "2026-09-11T09:00:00Z",
	}))

	limited, err := f.run(t, ImportOptions{Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), limited.MeetingsProcessed)

	after, err := f.run(t, ImportOptions{StartedAfter: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)})
	require.NoError(err)
	assert.Equal(int64(2), after.MeetingsProcessed)
	assert.Equal(int64(2), after.MeetingsAdded)
}

func TestImportFullRewritesUnchangedMeetings(t *testing.T) {
	require := require.New(t)
	f := newImporterFixture(t)
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))
	_, err := f.run(t, ImportOptions{})
	require.NoError(err)

	summary, err := f.run(t, ImportOptions{Full: true})
	require.NoError(err)

	assert.Equal(t, int64(1), summary.MeetingsUpdated)
}

func TestImportRecordsCursorWithoutDatabasePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	insertRow(t, f.muesli, "meetings", completedMeeting(nil))

	_, err := f.run(t, ImportOptions{})
	require.NoError(err)

	run, err := f.st.GetLastSuccessfulSync(f.source.ID)
	require.NoError(err)
	require.True(run.CursorAfter.Valid)
	var cursor map[string]any
	require.NoError(json.Unmarshal([]byte(run.CursorAfter.String), &cursor))
	assert.InDelta(float64(1), cursor["version"], 0)
	assert.NotEmpty(cursor["scanned_at"])
	assert.NotContains(run.CursorAfter.String, "muesli.db")
}

func TestImportRequiresRegisteredSource(t *testing.T) {
	f := newImporterFixture(t)

	_, err := f.run(t, ImportOptions{Identifier: "laptop"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `muesli source "laptop" is not registered`)
}

func TestImportRecordsFailedSyncWhenDatabaseCannotOpen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newImporterFixture(t)
	missing := filepath.Join(t.TempDir(), "private-folder", "muesli.db")

	_, err := f.run(t, ImportOptions{DBPath: missing})

	require.Error(err)
	assert.Contains(err.Error(), missing, "the CLI error names the path it tried")
	run, runErr := f.st.GetLatestSync(f.source.ID)
	require.NoError(runErr)
	assert.Equal(store.SyncStatusFailed, run.Status)
	require.True(run.ErrorMessage.Valid)
	assert.NotContains(run.ErrorMessage.String, "private-folder", "the archive does not keep local paths")
	assert.Contains(run.ErrorMessage.String, "db_path")
}
