package muesli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReaderReadsCurrentSchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db := newFixtureDB(t, path, currentSchemaDDL)
	clients := insertRow(t, db, "meeting_folders", map[string]any{"name": "Clients"})
	acme := insertRow(t, db, "meeting_folders", map[string]any{"name": "Acme", "parent_id": clients})
	id := insertRow(t, db, "meetings", completedMeeting(map[string]any{
		"folder_id":              acme,
		"calendar_event_id":      "event-1",
		"calendar_source":        "eventKit",
		"calendar_series_id":     "series-1",
		"selected_template_name": "Auto",
		"selected_template_kind": "auto",
		"source":                 "meeting",
		"updated_at":             1788271503.0,
	}))
	followUp := insertRow(t, db, "meetings", completedMeeting(map[string]any{
		"title": "Follow-up", "follow_up_to_id": id, "created_at": "2026-09-02 09:00:00",
	}))
	insertRow(t, db, "meeting_participants", map[string]any{
		"meeting_id": id, "participant_identifier": "email:alice@example.com",
		"display_name": "Alice Example", "email_address": "Alice@Example.com",
		"insertion_order": 1, "source": "calendar",
	})
	insertRow(t, db, "meeting_participants", map[string]any{
		"meeting_id": id, "participant_identifier": "contact:ABC-123",
		"display_name": "Carol Example", "insertion_order": 0, "source": "manual",
	})
	insertRow(t, db, "meeting_participants", map[string]any{
		"meeting_id": id, "participant_identifier": "email:bob@example.com",
		"display_name": "Bob Example", "email_address": "bob@example.com",
		"insertion_order": 2, "source": "calendar", "is_suppressed": 1,
	})
	insertRow(t, db, "meeting_participants", map[string]any{
		"meeting_id": followUp, "participant_identifier": "email:dana@example.com",
		"display_name": " Dana Example ", "insertion_order": 0, "source": "manual",
	})
	var beforeCount int
	var beforeUpdated float64
	require.NoError(db.QueryRow(`SELECT count(*), max(updated_at) FROM meetings`).Scan(&beforeCount, &beforeUpdated))

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)

	require.Len(meetings, 2)
	assert.Equal(Meeting{
		ID: id, Title: "Weekly sync",
		StartTime: "2026-09-01T14:00:00Z", EndTime: "2026-09-01T14:45:00Z",
		CreatedAt: "2026-09-01 14:00:03", DurationSeconds: 2700,
		Status: "completed", Source: "meeting",
		RawTranscript:  "[10:00:01] You: hello\n[10:00:04] Speaker 1: hi there",
		FormattedNotes: "## Decisions\nShip it", ManualNotes: "typed note", WordCount: 5,
		TemplateName: "Auto", TemplateKind: "auto",
		CalendarEventID: "event-1", CalendarSource: "eventKit", CalendarSeriesID: "series-1",
		Folder: "Clients/Acme",
		Participants: []Participant{
			{Name: "Carol Example", Source: "manual", Identifier: "contact:ABC-123"},
			{Name: "Alice Example", Email: "alice@example.com", Source: "calendar", Identifier: "email:alice@example.com"},
		},
	}, meetings[0])
	assert.Equal(id, meetings[1].FollowUpToID)
	assert.Equal([]Participant{{Name: "Dana Example", Email: "dana@example.com", Source: "manual", Identifier: "email:dana@example.com"}},
		meetings[1].Participants, "an email identifier supplies a missing email address")

	var afterCount int
	var afterUpdated float64
	require.NoError(db.QueryRow(`SELECT count(*), max(updated_at) FROM meetings`).Scan(&afterCount, &afterUpdated))
	assert.Equal(beforeCount, afterCount)
	assert.InDelta(beforeUpdated, afterUpdated, 0)
}

func TestReaderReadsLegacySchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db := newFixtureDB(t, path, legacySchemaDDL)
	id := insertRow(t, db, "meetings", map[string]any{
		"title": "Old meeting", "start_time": "2025-01-02T03:04:05Z",
		"raw_transcript": "[03:04:06] You: hi", "created_at": "2025-01-02 03:04:05",
	})
	insertRow(t, db, "meeting_participants", map[string]any{
		"meeting_id": id, "participant_identifier": "email:alice@example.com",
		"display_name": "Alice Example", "email_address": "alice@example.com", "insertion_order": 0,
	})
	insertRow(t, db, "meeting_participants", map[string]any{
		"meeting_id": id, "participant_identifier": "email:hidden@example.com",
		"display_name": "Hidden Example", "email_address": "hidden@example.com", "insertion_order": 1,
	})
	insertRow(t, db, "meeting_participant_suppressions", map[string]any{
		"meeting_id": id, "participant_identifier": "email:hidden@example.com",
	})

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)

	require.Len(meetings, 1)
	got := meetings[0]
	assert.Equal("Old meeting", got.Title)
	assert.Empty(got.Status)
	assert.Empty(got.ManualNotes)
	assert.Empty(got.Folder)
	assert.False(got.Deleted)
	assert.Equal([]Participant{{Name: "Alice Example", Email: "alice@example.com", Identifier: "email:alice@example.com"}}, got.Participants)
}

func TestReaderMarksDeletedMeetings(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db := newFixtureDB(t, path, currentSchemaDDL)
	insertRow(t, db, "meetings", completedMeeting(map[string]any{"deleted_at": 1788271503.0}))

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)

	require.Len(meetings, 1)
	assert.True(t, meetings[0].Deleted)
}

func TestReaderReadsCommittedRowsStillInTheWAL(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db := newFixtureDB(t, path, currentSchemaDDL)
	_, err := db.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(err)
	insertRow(t, db, "meetings", completedMeeting(map[string]any{"title": "Only in WAL"}))
	walInfo, err := os.Stat(path + "-wal")
	require.NoError(err)
	require.Positive(walInfo.Size(), "the fixture row must still be in the WAL")

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)

	require.Len(meetings, 1)
	assert.Equal(t, "Only in WAL", meetings[0].Title)
}

func TestReaderOpensWALWithoutSharedMemorySidecar(t *testing.T) {
	require := require.New(t)
	const writerDBEnv = "MSGVAULT_TEST_WAL_WRITER_DB"
	if path := os.Getenv(writerDBEnv); path != "" {
		db := newFixtureDB(t, path, currentSchemaDDL)
		_, err := db.Exec(`PRAGMA wal_autocheckpoint=0`)
		require.NoError(err)
		insertRow(t, db, "meetings", completedMeeting(map[string]any{"title": "Only in WAL"}))
		// Exit without closing the database so the committed WAL remains for the
		// parent process to open after removing the shared-memory sidecar.
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "muesli.db")
	executable, err := os.Executable()
	require.NoError(err)
	cmd := exec.Command(executable, "-test.run=^TestReaderOpensWALWithoutSharedMemorySidecar$")
	cmd.Env = append(os.Environ(), writerDBEnv+"="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(err, string(output))
	require.FileExists(path + "-wal")
	require.FileExists(path + "-shm")
	require.NoError(os.Remove(path + "-shm"))

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)
	require.Len(meetings, 1)
	assert.Equal(t, "Only in WAL", meetings[0].Title)
}

func TestReaderConnectionRejectsWriteQueries(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "muesli.db")
	db := newFixtureDB(t, path, currentSchemaDDL)
	insertRow(t, db, "meetings", completedMeeting(nil))

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	_, err = reader.db.ExecContext(context.Background(), `UPDATE meetings SET title = 'Changed'`)
	require.Error(err)

	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)
	require.Len(meetings, 1)
	assert.Equal(t, "Weekly sync", meetings[0].Title)
}

func TestReaderOpensPathsWithURISpecialCharacters(t *testing.T) {
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "Application Support #1%41")
	require.NoError(os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "muesli.db")
	db := newFixtureDB(t, path, currentSchemaDDL)
	insertRow(t, db, "meetings", completedMeeting(nil))

	reader, err := Open(context.Background(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reader.Close() })
	meetings, err := reader.ListMeetings(context.Background())
	require.NoError(err)

	assert.Len(t, meetings, 1)
}

func TestReaderRejectsMissingOrForeignDatabases(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "foreign.db")
	newFixtureDB(t, foreign, `CREATE TABLE notes (id INTEGER PRIMARY KEY)`)
	incomplete := filepath.Join(dir, "incomplete.db")
	newFixtureDB(t, incomplete, `CREATE TABLE meetings (id INTEGER PRIMARY KEY, title TEXT)`)

	for _, tt := range []struct {
		name string
		path string
		want string
	}{
		{name: "missing file", path: filepath.Join(dir, "absent.db"), want: "open Muesli database"},
		{name: "no meetings table", path: foreign, want: "not a Muesli database"},
		{name: "missing start_time", path: incomplete, want: "not a Muesli database"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := Open(context.Background(), tt.path)
			if reader != nil {
				_ = reader.Close()
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
