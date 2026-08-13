package whatsapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestHandleMediaFile(t *testing.T) {
	content := []byte("whatsapp media bytes")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	newOpts := func(t *testing.T) ImportOptions {
		t.Helper()
		mediaDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(mediaDir, "photo.jpg"), content, 0o600))
		return ImportOptions{MediaDir: mediaDir, AttachmentsDir: t.TempDir()}
	}
	media := func(rel string) waMedia {
		return waMedia{FilePath: sql.NullString{String: rel, Valid: true}}
	}
	imp := &Importer{}

	t.Run("stores media at canonical content-addressed path", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		opts := newOpts(t)
		rel, hash := imp.handleMediaFile(media("photo.jpg"), opts)
		// Storage paths are slash-separated on every platform.
		assert.Equal(wantHash[:2]+"/"+wantHash, rel)
		assert.Equal(wantHash, hash)

		got, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, wantHash[:2], wantHash))
		require.NoError(err)
		assert.Equal(content, got)
	})

	t.Run("returns empty for missing media file", func(t *testing.T) {
		opts := newOpts(t)
		rel, hash := imp.handleMediaFile(media("nope.jpg"), opts)
		assert.Empty(t, rel)
		assert.Empty(t, hash)
	})

	t.Run("returns empty for oversized media file", func(t *testing.T) {
		opts := newOpts(t)
		opts.MaxMediaFileSize = 4
		rel, hash := imp.handleMediaFile(media("photo.jpg"), opts)
		assert.Empty(t, rel)
		assert.Empty(t, hash)
	})
}

func TestImportClassifiesStoredMediaAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mediaDir := t.TempDir()
	attachmentsDir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(mediaDir, "report.pdf"), []byte("document bytes"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(mediaDir, "sticker.webp"), []byte("sticker bytes"), 0o600))

	waDBPath := filepath.Join(t.TempDir(), "msgstore.db")
	waDB, err := sql.Open("sqlite3", waDBPath)
	require.NoError(err)
	_, err = waDB.Exec(`
		PRAGMA journal_mode=WAL;
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
			message_row_id INTEGER PRIMARY KEY, mime_type TEXT, file_size INTEGER,
			file_path TEXT, width INTEGER, height INTEGER, media_duration INTEGER
		);
		INSERT INTO jid VALUES
			(1, '15555550101', 's.whatsapp.net', '15555550101@s.whatsapp.net');
		INSERT INTO chat VALUES (10, 1, 0, NULL, 2000);
		INSERT INTO message VALUES
			(100, 10, 1, 'document-message', NULL, 1000, 13, NULL, 0, 0),
			(101, 10, 1, 'sticker-message', NULL, 2000, 90, NULL, 0, 0);
		INSERT INTO message_media VALUES
			(100, 'application/pdf', 14, 'report.pdf', NULL, NULL, NULL),
			(101, 'image/webp', 13, 'sticker.webp', NULL, NULL, NULL);
	`)
	require.NoError(err)
	require.NoError(waDB.Close())

	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	summary, err := imp.Import(context.Background(), waDBPath, ImportOptions{
		Phone: "+15555550100", MediaDir: mediaDir, AttachmentsDir: attachmentsDir,
	})
	require.NoError(err)
	assert.Equal(int64(2), summary.AttachmentsFound)
	assert.Equal(int64(2), summary.MediaCopied)

	rows, err := st.DB().Query(`
		SELECT a.filename, a.attachment_role, a.role_source,
		       COALESCE(a.source_part_key, '')
		FROM attachments a
		ORDER BY a.filename`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()

	type attachmentRole struct {
		filename, role, roleSource, sourcePartKey string
	}
	var got []attachmentRole
	for rows.Next() {
		var item attachmentRole
		require.NoError(rows.Scan(&item.filename, &item.role, &item.roleSource, &item.sourcePartKey))
		got = append(got, item)
	}
	require.NoError(rows.Err())
	require.Len(got, 2)
	assert.Equal(attachmentRole{
		filename: "report.pdf", role: string(store.AttachmentRoleStandalone),
		roleSource:    string(store.AttachmentRoleSourceImporterSemantics),
		sourcePartKey: "whatsapp:media",
	}, got[0])
	assert.Equal(attachmentRole{
		filename: "sticker.webp", role: string(store.AttachmentRoleSticker),
		roleSource:    string(store.AttachmentRoleSourceProviderExplicit),
		sourcePartKey: "whatsapp:media",
	}, got[1])
}
