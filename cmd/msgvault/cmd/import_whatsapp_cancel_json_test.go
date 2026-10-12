package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportWhatsAppJSONRetainsRunIDWhenInterrupted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, parent := importCommandCaptureFixture(t, false)
	fixture := createSyntheticAndroidWhatsAppCancelFixture(t)
	ctx, cancel := context.WithCancel(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	defer cancel()

	command := cloneImportCommandForTest(t, importWhatsappCmd)
	command.SetContext(ctx)
	command.SetErr(io.Discard)
	command.SetArgs([]string{fixture, "--phone", "+12025550123", "--json"})
	finish := captureStdout(t)
	runDone := make(chan error, 1)
	go func() { runDone <- command.Execute() }()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	var observedRunID string
	var queryErr error
	var runErr error
	observedRun := false
	finishedBeforeRun := false
	timedOut := false
	for !observedRun && !finishedBeforeRun && !timedOut {
		queryErr = parent.DB().QueryRow(
			`SELECT id FROM import_runs WHERE import_format='whatsapp'`,
		).Scan(&observedRunID)
		if queryErr == nil {
			observedRun = true
			cancel()
			break
		}
		if queryErr != sql.ErrNoRows {
			cancel()
			break
		}

		select {
		case runErr = <-runDone:
			finishedBeforeRun = true
		case <-ticker.C:
		case <-timeout.C:
			timedOut = true
			cancel()
		}
	}
	if !finishedBeforeRun {
		select {
		case runErr = <-runDone:
		case <-time.After(10 * time.Second):
			cancel()
			output := finish()
			require.FailNow("command did not stop after cancellation", "stdout: %s", output)
		}
	}
	output := finish()
	require.NoError(queryErr)
	require.True(observedRun, "cancellation must follow durable logical import run creation")
	require.False(finishedBeforeRun, "command must still be running when its durable run is observed")
	require.False(timedOut, "the durable logical import run must be created before timeout")
	require.NoError(runErr)

	var result struct {
		ImportRunIDs []string `json:"import_run_ids"`
		Completed    bool     `json:"completed"`
	}
	require.NoError(json.Unmarshal([]byte(output), &result), "stdout must contain one JSON result: %q", output)

	var persistedID string
	var persistedComplete bool
	require.NoError(parent.DB().QueryRow(
		`SELECT id, completed FROM import_runs WHERE import_format='whatsapp'`,
	).Scan(&persistedID, &persistedComplete))
	assert.NotEmpty(persistedID)
	assert.Equal(observedRunID, persistedID)
	assert.False(persistedComplete, "the interrupted logical import run must remain resumable")
	assert.False(result.Completed, "interrupted work must not be reported complete")
	assert.Len(result.ImportRunIDs, 1, "JSON must report the durable interrupted run ID")
	if len(result.ImportRunIDs) == 1 {
		assert.Equal(persistedID, result.ImportRunIDs[0])
	}
}

func TestImportWhatsAppJSONRetainsRunIDWhenReplyPersistenceFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg, parent := importCommandCaptureFixture(t, false)
	fixture := createSyntheticAndroidWhatsAppReplyFailureFixture(t)
	_, err := parent.DB().Exec(`CREATE TRIGGER synthetic_whatsapp_reply_abort
		BEFORE UPDATE OF reply_to_message_id ON messages
		WHEN NEW.source_message_id='synthetic-whatsapp-child' AND NEW.reply_to_message_id IS NOT NULL
		BEGIN SELECT RAISE(ABORT,'synthetic required reply failure'); END`)
	require.NoError(err)

	command := cloneImportCommandForTest(t, importWhatsappCmd)
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	command.SetErr(io.Discard)
	command.SilenceUsage = true
	command.SetArgs([]string{fixture, "--phone", "+12025550123", "--json"})
	finish := captureStdout(t)
	runErr := command.Execute()
	output := finish()
	require.ErrorContains(runErr, "link reply: reply persistence failed")

	var result struct {
		ImportRunIDs []string `json:"import_run_ids"`
		Completed    bool     `json:"completed"`
	}
	require.NoError(json.Unmarshal([]byte(output), &result), "stdout must contain one JSON result: %q", output)
	var persistedID string
	var persistedComplete bool
	require.NoError(parent.DB().QueryRow(
		`SELECT id, completed FROM import_runs WHERE import_format='whatsapp'`,
	).Scan(&persistedID, &persistedComplete))
	assert.False(persistedComplete, "the failed import run must remain resumable")
	assert.False(result.Completed, "failed work must not be reported complete")
	assert.Len(result.ImportRunIDs, 1, "JSON must report the durable failed run ID")
	if len(result.ImportRunIDs) == 1 {
		assert.Equal(persistedID, result.ImportRunIDs[0])
	}
}

func createSyntheticAndroidWhatsAppReplyFailureFixture(t *testing.T) string {
	t.Helper()
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "msgstore.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = db.Exec(`
		CREATE TABLE jid (
			_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT
		);
		CREATE TABLE chat (
			_id INTEGER PRIMARY KEY, jid_row_id INTEGER UNIQUE, hidden INTEGER,
			subject TEXT, sort_timestamp INTEGER
		);
		CREATE TABLE message (
			_id INTEGER PRIMARY KEY, chat_row_id INTEGER, from_me INTEGER,
			key_id TEXT, sender_jid_row_id INTEGER, timestamp INTEGER,
			message_type INTEGER, text_data TEXT, status INTEGER, starred INTEGER
		);
		CREATE TABLE message_media (
			message_row_id INTEGER PRIMARY KEY, mime_type TEXT, media_caption TEXT,
			file_size INTEGER, file_path TEXT, width INTEGER, height INTEGER,
			media_duration INTEGER
		);
		CREATE TABLE message_quoted (message_row_id INTEGER, key_id TEXT);
		INSERT INTO jid VALUES
			(1, '12025550124', 's.whatsapp.net', '12025550124@s.whatsapp.net'),
			(2, '12025550123', 's.whatsapp.net', '12025550123@s.whatsapp.net');
		INSERT INTO chat VALUES (10, 1, 0, 'Synthetic Chat', 2000);
		INSERT INTO message VALUES
			(100, 10, 0, 'synthetic-whatsapp-parent', 1, 1700000000000, 1, 'Synthetic parent', 0, 0),
			(101, 10, 0, 'synthetic-whatsapp-child', 1, 1700000001000, 1, 'Synthetic reply', 0, 0);
		INSERT INTO message_quoted VALUES (101, 'synthetic-whatsapp-parent');
	`)
	require.NoError(err)
	require.NoError(db.Close())
	return path
}

func createSyntheticAndroidWhatsAppCancelFixture(t *testing.T) string {
	t.Helper()
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "msgstore.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = db.Exec(`
		CREATE TABLE jid (
			_id INTEGER PRIMARY KEY, user TEXT, server TEXT, raw_string TEXT
		);
		CREATE TABLE chat (
			_id INTEGER PRIMARY KEY, jid_row_id INTEGER UNIQUE, hidden INTEGER,
			subject TEXT, sort_timestamp INTEGER
		);
		CREATE TABLE message (
			_id INTEGER PRIMARY KEY, chat_row_id INTEGER, from_me INTEGER,
			key_id TEXT, sender_jid_row_id INTEGER, timestamp INTEGER,
			message_type INTEGER, text_data TEXT, status INTEGER, starred INTEGER
		);
		CREATE TABLE message_media (
			message_row_id INTEGER PRIMARY KEY, mime_type TEXT, media_caption TEXT,
			file_size INTEGER, file_path TEXT, width INTEGER, height INTEGER,
			media_duration INTEGER
		);
		INSERT INTO jid VALUES
			(1, '12025550124', 's.whatsapp.net', '12025550124@s.whatsapp.net'),
			(2, '12025550123', 's.whatsapp.net', '12025550123@s.whatsapp.net');
		INSERT INTO chat VALUES (10, 1, 0, 'Synthetic Chat', 2000);
	`)
	require.NoError(err)
	tx, err := db.Begin()
	require.NoError(err)
	func() {
		statement, err := tx.Prepare(`INSERT INTO message VALUES (?, 10, 0, ?, 1, ?, 1, 'Synthetic body', 0, 0)`)
		require.NoError(err)
		defer func() { require.NoError(statement.Close()) }()
		for i := range 10000 {
			_, err = statement.Exec(100+i, fmt.Sprintf("synthetic-whatsapp-message-%d", i), 1700000000000+int64(i)*1000)
			require.NoError(err)
		}
	}()
	require.NoError(tx.Commit())
	require.NoError(db.Close())
	return path
}
