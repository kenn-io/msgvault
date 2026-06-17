package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

// setupQueryTestParquet creates a minimal set of Parquet files in a
// temp directory for testing executeQuery. Returns the analytics dir.
func setupQueryTestParquet(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()

	db, err := sql.Open("duckdb", "")
	require.NoError(t, err, "open duckdb")
	defer func() { _ = db.Close() }()

	// Create subdirectories matching required Parquet layout
	dirs := []string{
		"messages/year=2024",
		"sources",
		"participants",
		"message_recipients",
		"labels",
		"message_labels",
		"attachments",
		"conversations",
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, d), 0755), "mkdir %s", d)
	}

	// Helper to write a Parquet file from a COPY query
	write := func(path, copySQL string) {
		t.Helper()
		escaped := strings.ReplaceAll(path, "'", "''")
		q := fmt.Sprintf(
			"COPY (%s) TO '%s' (FORMAT PARQUET)",
			copySQL, escaped,
		)
		_, err := db.Exec(q)
		require.NoError(t, err, "write %s", path)
	}

	// Messages
	write(filepath.Join(tmpDir, "messages/year=2024/data.parquet"),
		`SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT, 'msg1', 200::BIGINT,
			 'Hello', 'Preview', TIMESTAMP '2024-01-15 10:00:00',
			 1000::BIGINT, false, 0,
			 NULL::TIMESTAMP, NULL::BIGINT, 'email', 2024, 1)
		) AS t(id, source_id, source_message_id, conversation_id,
			subject, snippet, sent_at, size_estimate,
			has_attachments, attachment_count,
			deleted_from_source_at, sender_id, message_type,
			year, month)`)

	// Sources
	write(filepath.Join(tmpDir, "sources/sources.parquet"),
		`SELECT * FROM (VALUES
			(1::BIGINT, 'test@gmail.com', 'gmail')
		) AS t(id, account_email, source_type)`)

	// Participants
	write(filepath.Join(tmpDir, "participants/participants.parquet"),
		`SELECT * FROM (VALUES
			(1::BIGINT, 'alice@example.com', 'example.com',
			 'Alice', '')
		) AS t(id, email_address, domain, display_name,
			phone_number)`)

	// Message recipients
	write(filepath.Join(tmpDir, "message_recipients/message_recipients.parquet"),
		`SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT, 'from', 'Alice')
		) AS t(message_id, participant_id, recipient_type,
			display_name)`)

	// Labels
	write(filepath.Join(tmpDir, "labels/labels.parquet"),
		`SELECT * FROM (VALUES
			(1::BIGINT, 'INBOX')
		) AS t(id, name)`)

	// Message labels
	write(filepath.Join(tmpDir, "message_labels/message_labels.parquet"),
		`SELECT * FROM (VALUES
			(1::BIGINT, 1::BIGINT)
		) AS t(message_id, label_id)`)

	// Attachments (empty with schema)
	write(filepath.Join(tmpDir, "attachments/attachments.parquet"),
		`SELECT * FROM (VALUES
			(0::BIGINT, 0::BIGINT, '')
		) AS t(message_id, size, filename) WHERE false`)

	// Conversations
	write(filepath.Join(tmpDir, "conversations/conversations.parquet"),
		`SELECT * FROM (VALUES
			(200::BIGINT, 'thread200', '')
		) AS t(id, source_conversation_id, title)`)

	return tmpDir
}

func TestQueryCommand_JSON(t *testing.T) {
	analyticsDir := setupQueryTestParquet(t)

	var buf bytes.Buffer
	err := executeQuery(
		analyticsDir,
		"SELECT subject FROM messages",
		"json",
		&buf,
	)
	require.NoError(t, err, "executeQuery")

	var result query.QueryResult
	require.NoError(t, json.Unmarshal(buf.Bytes(), &result), "unmarshal")
	assert.Equal(t, 1, result.RowCount, "row_count")
}

func TestQueryCommand_CSV(t *testing.T) {
	analyticsDir := setupQueryTestParquet(t)

	var buf bytes.Buffer
	err := executeQuery(
		analyticsDir,
		"SELECT subject FROM messages",
		"csv",
		&buf,
	)
	require.NoError(t, err, "executeQuery")

	output := buf.String()
	assert.Contains(t, output, "subject", "CSV missing header 'subject'")
	assert.Contains(t, output, "Hello", "CSV missing data 'Hello'")
}

func TestQueryCommand_MissingCache(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	err := executeQuery(
		dir,
		"SELECT 1",
		"json",
		&buf,
	)
	require.Error(t, err, "expected error for missing cache")
}
