package muesli

import (
	"database/sql"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// currentSchemaDDL is the meeting subset of Muesli 0.8.4's fresh-database
// schema (DictationStore.migrateIfNeeded), including folder_id, which the
// app adds with ALTER TABLE.
const currentSchemaDDL = `
CREATE TABLE meeting_folders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    sort_order INTEGER NOT NULL DEFAULT 0,
    parent_id INTEGER REFERENCES meeting_folders(id),
    created_at TEXT DEFAULT (datetime('now'))
);
CREATE TABLE meetings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL,
    calendar_event_id TEXT,
    calendar_occurrence_key TEXT,
    calendar_source TEXT,
    calendar_id TEXT,
    calendar_series_id TEXT,
    calendar_occurrence_start REAL,
    start_time TEXT NOT NULL,
    end_time TEXT,
    duration_seconds REAL,
    raw_transcript TEXT,
    formatted_notes TEXT,
    mic_audio_path TEXT,
    system_audio_path TEXT,
    saved_recording_path TEXT,
    meeting_status TEXT NOT NULL DEFAULT 'completed',
    manual_notes TEXT NOT NULL DEFAULT '',
    word_count INTEGER NOT NULL DEFAULT 0,
    selected_template_id TEXT,
    selected_template_name TEXT,
    selected_template_kind TEXT,
    selected_template_prompt TEXT,
    source TEXT NOT NULL DEFAULT 'meeting',
    updated_at REAL NOT NULL DEFAULT 0,
    deleted_at REAL,
    cloud_record_name TEXT,
    cloud_change_tag TEXT,
    cloud_system_fields BLOB,
    cloud_transcript_record_name TEXT,
    last_synced_at REAL,
    sync_dirty INTEGER NOT NULL DEFAULT 1,
    follow_up_to_id INTEGER REFERENCES meetings(id) ON DELETE SET NULL,
    follow_up_to_record_name TEXT,
    visual_context TEXT,
    created_at TEXT DEFAULT (datetime('now')),
    folder_id INTEGER REFERENCES meeting_folders(id)
);
CREATE TABLE meeting_participants (
    meeting_id INTEGER NOT NULL REFERENCES meetings(id) ON DELETE CASCADE,
    participant_identifier TEXT NOT NULL,
    display_name TEXT NOT NULL,
    email_address TEXT,
    insertion_order INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT 'manual',
    is_suppressed INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (meeting_id, participant_identifier)
);
`

// legacySchemaDDL models an early Muesli database: no status, manual notes,
// deletion, folder, or template columns; suppressions in their own table.
const legacySchemaDDL = `
CREATE TABLE meetings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL,
    calendar_event_id TEXT,
    start_time TEXT NOT NULL,
    end_time TEXT,
    duration_seconds REAL,
    raw_transcript TEXT,
    formatted_notes TEXT,
    word_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT DEFAULT (datetime('now'))
);
CREATE TABLE meeting_participants (
    meeting_id INTEGER NOT NULL REFERENCES meetings(id) ON DELETE CASCADE,
    participant_identifier TEXT NOT NULL,
    display_name TEXT NOT NULL,
    email_address TEXT,
    insertion_order INTEGER NOT NULL,
    PRIMARY KEY (meeting_id, participant_identifier)
);
CREATE TABLE meeting_participant_suppressions (
    meeting_id INTEGER NOT NULL,
    participant_identifier TEXT NOT NULL,
    PRIMARY KEY (meeting_id, participant_identifier)
);
`

// newFixtureDB creates a WAL-mode Muesli-shaped database at path and returns
// a read-write connection the test can keep mutating.
func newFixtureDB(t *testing.T, path, ddl string) *sql.DB {
	t.Helper()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	dsn := (&url.URL{Scheme: "file", Path: sqliteURIPath(abs), RawQuery: "_journal_mode=WAL&_busy_timeout=5000"}).String()
	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(ddl)
	require.NoError(t, err)
	return db
}

// insertRow inserts one row with the given column values and returns its
// rowid.
func insertRow(t *testing.T, db *sql.DB, table string, values map[string]any) int64 {
	t.Helper()
	columns := make([]string, 0, len(values))
	for column := range values {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	args := make([]any, len(columns))
	for i, column := range columns {
		args[i] = values[column]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
	result, err := db.Exec(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, strings.Join(columns, ","), placeholders), args...)
	require.NoError(t, err)
	id, err := result.LastInsertId()
	require.NoError(t, err)
	return id
}

// completedMeeting returns column values for an ordinary finished meeting.
func completedMeeting(overrides map[string]any) map[string]any {
	values := map[string]any{
		"title":            "Weekly sync",
		"start_time":       "2026-09-01T14:00:00Z",
		"end_time":         "2026-09-01T14:45:00Z",
		"duration_seconds": 2700.0,
		"raw_transcript":   "[10:00:01] You: hello\n[10:00:04] Speaker 1: hi there",
		"formatted_notes":  "## Decisions\nShip it",
		"manual_notes":     "typed note",
		"word_count":       5,
		"meeting_status":   "completed",
		"created_at":       "2026-09-01 14:00:03",
	}
	maps.Copy(values, overrides)
	return values
}
