package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func attachmentBytesHashMessage(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("imap", "archive@example.com")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: id, MessageType: "email",
	})
	require.NoError(t, err)
	return messageID
}

func casPath(hash string) string { return hash[:2] + "/" + hash }

func TestInitSchemaRepairsAttachmentsClaimingBytesWithoutHash(t *testing.T) {
	st := storetest.New(t).Store
	first := attachmentBytesHashMessage(t, st, "first")
	second := attachmentBytesHashMessage(t, st, "second")
	hash := strings.Repeat("ab", 32)
	thumb := strings.Repeat("cd", 32)
	other := strings.Repeat("ef", 32)

	// Archives created before the guard can hold rows that break the rule.
	drop := []string{
		"DROP TRIGGER IF EXISTS trg_attachments_archived_bytes_hash_insert",
		"DROP TRIGGER IF EXISTS trg_attachments_archived_bytes_hash_update",
	}
	if st.IsPostgreSQL() {
		drop = []string{"ALTER TABLE attachments DROP CONSTRAINT attachments_archived_bytes_hash"}
	}
	for _, stmt := range drop {
		_, err := st.DB().ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
	type row struct {
		message                     int64
		hash, path, state, reason   string
		partKey, sourceAttachmentID string
		thumbHash, thumbPath        string
	}
	seed := map[string]row{
		"stored":                 {message: first, hash: hash, path: casPath(hash), state: "stored", thumbHash: thumb, thumbPath: casPath(thumb)},
		"stored-without-hash":    {message: first, path: "legacy/report.pdf", state: "stored"},
		"locator":                {message: first, path: "https://files.example.com/a.png", state: "pending"},
		"thumbnail-without-hash": {message: first, hash: other, path: casPath(other), thumbPath: "thumbs/a.jpg"},
		// Duplicate bytes in one message were kept as hashless aliases at
		// the content-addressed path.
		"keyed-alias":   {message: first, path: casPath(hash), state: "stored", sourceAttachmentID: "slack:F1"},
		"unkeyed-alias": {message: first, path: casPath(hash), state: "stored"},
		"legacy-alias":  {message: second, path: casPath(other)},
		// A source merge copies an alias onto the surviving message as
		// unavailable historical evidence.
		"merged-alias": {message: second, path: casPath(hash), state: "unavailable", reason: "historical-source"},
	}
	for name, r := range seed {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
			INSERT INTO attachments
				(message_id, filename, content_hash, storage_path, attachment_state, attachment_skip_reason,
				 source_attachment_id, thumbnail_hash, thumbnail_path)
			VALUES (?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))
		`), r.message, name, r.hash, r.path, r.state, r.reason, r.sourceAttachmentID, r.thumbHash, r.thumbPath)
		require.NoError(t, err, name)
	}

	var unkeyedID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(),
		`SELECT id FROM attachments WHERE filename = 'unkeyed-alias'`).Scan(&unkeyedID))

	_, err := st.DB().ExecContext(t.Context(), st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "attachment_bytes_hash_v1")
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()))

	want := map[string]row{
		"stored":                 seed["stored"],
		"stored-without-hash":    {message: first, path: "legacy/report.pdf", state: "failed", reason: "fetch_failure"},
		"locator":                seed["locator"],
		"thumbnail-without-hash": {message: first, hash: other, path: casPath(other)},
		"keyed-alias": {message: first, hash: hash, path: casPath(hash), state: "stored",
			partKey: "slack:F1", sourceAttachmentID: "slack:F1"},
		"unkeyed-alias": {message: first, hash: hash, path: casPath(hash), state: "stored"},
		"legacy-alias":  {message: second, hash: other, path: casPath(other)},
		"merged-alias":  {message: second, hash: hash, path: casPath(hash), state: "unavailable", reason: "historical-source"},
	}
	// An alias without a usable source attachment ID is keyed by its row.
	w := want["unkeyed-alias"]
	w.partKey = fmt.Sprintf("legacy-alias:%d", unkeyedID)
	want["unkeyed-alias"] = w
	for name, w := range want {
		var got row
		err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
			SELECT message_id, COALESCE(content_hash, ''), storage_path, COALESCE(attachment_state, ''),
			       COALESCE(attachment_skip_reason, ''), COALESCE(source_part_key, ''),
			       COALESCE(source_attachment_id, ''), COALESCE(thumbnail_hash, ''), COALESCE(thumbnail_path, '')
			FROM attachments WHERE filename = ?
		`), name).Scan(&got.message, &got.hash, &got.path, &got.state, &got.reason,
			&got.partKey, &got.sourceAttachmentID, &got.thumbHash, &got.thumbPath)
		require.NoError(t, err, name)
		assert.Equal(t, w, got, name)
	}
}

func TestAttachmentsRejectStoredBytesWithoutHash(t *testing.T) {
	st := storetest.New(t).Store
	messageID := attachmentBytesHashMessage(t, st, "message")
	hash := strings.Repeat("ab", 32)

	err := st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "claims-bytes", StoragePath: "legacy/report.pdf", State: attachmentpolicy.StateStored,
	})
	require.Error(t, err)

	// A hashless row is a source locator, whatever its path looks like.
	require.NoError(t, st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "locator", StoragePath: "https://files.example.com/a.png", State: attachmentpolicy.StatePending,
	}))
	require.NoError(t, st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "stored", StoragePath: casPath(hash), ContentHash: hash, State: attachmentpolicy.StateStored,
	}))

	for _, stmt := range []string{
		`UPDATE attachments SET content_hash = NULL WHERE filename = 'stored'`,
		`UPDATE attachments SET attachment_state = 'stored' WHERE filename = 'locator'`,
		`UPDATE attachments SET thumbnail_path = 'thumbs/a.jpg' WHERE filename = 'stored'`,
	} {
		_, err := st.DB().ExecContext(t.Context(), stmt)
		require.Error(t, err, stmt)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ? WHERE filename = 'stored'`),
		hash, casPath(hash))
	assert.NoError(t, err)
}
