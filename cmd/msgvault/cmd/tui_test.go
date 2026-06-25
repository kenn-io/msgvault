package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
)

func TestTUIDuckDBOptionsDisableSQLiteScannerByDefault(t *testing.T) {
	opts := tuiDuckDBOptions()

	assert.True(t, opts.DisableSQLiteScanner,
		"TUI must not attach the live SQLite DB through DuckDB sqlite_scanner")
}

func TestOpenLocalTUIStoreMigratesBeforeCacheBuild(t *testing.T) {
	savedCfg := cfg
	savedForceSQL := forceSQL
	savedSkipCacheBuild := skipCacheBuild
	t.Cleanup(func() {
		cfg = savedCfg
		forceSQL = savedForceSQL
		skipCacheBuild = savedSkipCacheBuild
	})
	cfg = &config.Config{}
	forceSQL = false
	skipCacheBuild = false

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "legacy.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")
	setupLegacyTUIDBMissingDeletedAt(t, dbPath)

	st, err := openLocalTUIStore(dbPath, analyticsDir)
	require.NoError(t, err, "openLocalTUIStore")
	t.Cleanup(func() { _ = st.Close() })

	assert.True(t, query.HasCompleteParquetData(analyticsDir),
		"TUI startup should build cache after legacy columns are migrated")
}

func setupLegacyTUIDBMissingDeletedAt(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open sqlite")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE sources (
			id INTEGER PRIMARY KEY,
			source_type TEXT NOT NULL DEFAULT 'gmail',
			identifier TEXT NOT NULL,
			display_name TEXT,
			google_user_id TEXT UNIQUE,
			last_sync_at DATETIME,
			sync_cursor TEXT,
			sync_config JSON,
			oauth_app TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(source_type, identifier)
		);
		CREATE TABLE conversations (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			source_conversation_id TEXT,
			conversation_type TEXT NOT NULL DEFAULT 'email_thread',
			title TEXT,
			participant_count INTEGER DEFAULT 0,
			message_count INTEGER DEFAULT 0,
			unread_count INTEGER DEFAULT 0,
			last_message_at DATETIME,
			last_message_preview TEXT,
			metadata JSON,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE participants (
			id INTEGER PRIMARY KEY,
			email_address TEXT,
			phone_number TEXT,
			display_name TEXT,
			domain TEXT,
			canonical_id TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			source_message_id TEXT NOT NULL,
			conversation_id INTEGER,
			subject TEXT,
			snippet TEXT,
			sent_at DATETIME,
			received_at DATETIME,
			internal_date DATETIME,
			size_estimate INTEGER,
			has_attachments BOOLEAN DEFAULT FALSE,
			is_from_me BOOLEAN DEFAULT FALSE,
			archived_at DATETIME,
			rfc822_message_id TEXT,
			sender_id INTEGER,
			message_type TEXT NOT NULL DEFAULT 'email',
			attachment_count INTEGER DEFAULT 0,
			deleted_from_source_at DATETIME,
			UNIQUE(source_id, source_message_id)
		);
		CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL,
			participant_id INTEGER NOT NULL,
			recipient_type TEXT NOT NULL,
			display_name TEXT
		);
		CREATE TABLE labels (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			source_label_id TEXT,
			name TEXT NOT NULL,
			label_type TEXT
		);
		CREATE TABLE message_labels (
			message_id INTEGER NOT NULL,
			label_id INTEGER NOT NULL,
			PRIMARY KEY (message_id, label_id)
		);
		CREATE TABLE attachments (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL,
			filename TEXT,
			mime_type TEXT,
			size INTEGER,
			content_hash TEXT,
			storage_path TEXT NOT NULL DEFAULT '',
			media_type TEXT,
			width INTEGER,
			height INTEGER,
			duration_ms INTEGER,
			thumbnail_hash TEXT,
			thumbnail_path TEXT,
			source_attachment_id TEXT,
			attachment_metadata JSON,
			encryption_version INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		INSERT INTO sources (id, source_type, identifier, display_name)
		VALUES (1, 'gmail', 'test@example.com', 'Test Account');
		INSERT INTO conversations (id, source_id, source_conversation_id, title)
		VALUES (1, 1, 'thread-1', 'Legacy Thread');
		INSERT INTO participants (id, email_address, domain, display_name)
		VALUES (1, 'alice@example.com', 'example.com', 'Alice'),
		       (2, 'bob@example.com', 'example.com', 'Bob');
		INSERT INTO messages (
			id, source_id, source_message_id, conversation_id, subject, snippet,
			sent_at, size_estimate, has_attachments
		) VALUES (
			1, 1, 'msg-1', 1, 'Legacy message', 'Preview',
			'2024-01-02 03:04:05', 1234, 0
		);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name)
		VALUES (1, 1, 'from', 'Alice'), (1, 2, 'to', 'Bob');
		INSERT INTO labels (id, source_id, source_label_id, name, label_type)
		VALUES (1, 1, 'INBOX', 'INBOX', 'system');
		INSERT INTO message_labels (message_id, label_id) VALUES (1, 1);
	`)
	require.NoError(t, err, "create legacy TUI fixture")
}
