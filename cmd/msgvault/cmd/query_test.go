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
	"go.kenn.io/msgvault/internal/query"
)

// setupQueryTestParquet creates a minimal set of Parquet files in a
// temp directory for testing executeQuery. Returns the analytics dir
// and a cleanup function.
func setupQueryTestParquet(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "msgvault-query-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("open duckdb: %v", err)
	}
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
		if err := os.MkdirAll(filepath.Join(tmpDir, d), 0755); err != nil {
			_ = os.RemoveAll(tmpDir)
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Helper to write a Parquet file from a COPY query
	write := func(path, copySQL string) {
		t.Helper()
		escaped := strings.ReplaceAll(path, "'", "''")
		q := fmt.Sprintf(
			"COPY (%s) TO '%s' (FORMAT PARQUET)",
			copySQL, escaped,
		)
		if _, err := db.Exec(q); err != nil {
			_ = os.RemoveAll(tmpDir)
			t.Fatalf("write %s: %v", path, err)
		}
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

	return tmpDir, func() { _ = os.RemoveAll(tmpDir) }
}

func TestQueryCommand_JSON(t *testing.T) {
	analyticsDir, cleanup := setupQueryTestParquet(t)
	defer cleanup()

	var buf bytes.Buffer
	err := executeQuery(
		analyticsDir,
		"SELECT subject FROM messages",
		"json",
		&buf,
	)
	if err != nil {
		t.Fatalf("executeQuery: %v", err)
	}

	var result query.QueryResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.RowCount != 1 {
		t.Errorf("row_count = %d, want 1", result.RowCount)
	}
}

func TestQueryCommand_CSV(t *testing.T) {
	analyticsDir, cleanup := setupQueryTestParquet(t)
	defer cleanup()

	var buf bytes.Buffer
	err := executeQuery(
		analyticsDir,
		"SELECT subject FROM messages",
		"csv",
		&buf,
	)
	if err != nil {
		t.Fatalf("executeQuery: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "subject") {
		t.Errorf("CSV missing header 'subject': %s", output)
	}
	if !strings.Contains(output, "Hello") {
		t.Errorf("CSV missing data 'Hello': %s", output)
	}
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
	if err == nil {
		t.Fatal("expected error for missing cache, got nil")
	}
}
