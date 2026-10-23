package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestReplaceAttachmentsKeepsRetainedOccurrenceIDs(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		replace      func(*store.Store, int64, []store.AttachmentRef) error
	}{
		{"discord", "discord:", (*store.Store).ReplaceMessageDiscordAttachments},
		{"slack", "slack:", (*store.Store).ReplaceMessageSlackAttachments},
		{"beeper", "beeper:", (*store.Store).ReplaceMessageBeeperAttachments},
		{"inline", "inline:", (*store.Store).ReplaceMessageInlineProviderAttachments},
		{"teams-inline", "teams:inline:", func(st *store.Store, messageID int64, refs []store.AttachmentRef) error {
			return st.ReplaceMessageInlineAttachments(messageID, refs, true)
		}},
		{"teams-links", "teams:link:", (*store.Store).ReplaceMessageLinkAttachments},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			sourceMessageID := "attachment-identity"
			if tc.name == "inline" {
				source, err := f.Store.GetOrCreateSource("inline", "api.inline.chat:user:42")
				requirements.NoError(err)
				conversationID, err := f.Store.EnsureConversationWithType(source.ID, "chat:123", "group_chat", "Example group")
				requirements.NoError(err)
				f.Source, f.ConvID = source, conversationID
				sourceMessageID = "chat:123:message:7"
			}
			messageID := f.CreateMessage(sourceMessageID)
			keep := store.AttachmentRef{
				Filename: "keep.txt", MimeType: "text/plain", Size: 12,
				StoragePath: "https://files.example/keep.txt", SourceAttachmentID: tc.prefix + "keep",
			}
			drop := store.AttachmentRef{
				Filename: "drop.txt", StoragePath: "https://files.example/drop.txt",
				SourceAttachmentID: tc.prefix + "drop", SourcePartKey: tc.prefix + "drop-part",
			}
			if tc.name == "inline" {
				keep.SourceAttachmentID, keep.SourcePartKey = "inline:document:7", "inline:document:7"
				keep.StoragePath = "inline:pending:document:7"
				drop.SourceAttachmentID, drop.SourcePartKey = "inline:document:8", "inline:document:8"
				drop.StoragePath = "inline:pending:document:8"
			}
			requirements.NoError(tc.replace(f.Store, messageID, []store.AttachmentRef{keep, drop}))
			var keepID, dropID int64
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
				`SELECT id FROM attachments WHERE message_id = ? AND source_attachment_id = ?`),
				messageID, keep.SourceAttachmentID).Scan(&keepID))
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
				`SELECT id FROM attachments WHERE message_id = ? AND source_attachment_id = ?`),
				messageID, drop.SourceAttachmentID).Scan(&dropID))
			// A later row also prevents SQLite from reusing the deleted maximum ID.
			requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
				Filename: "unrelated.txt", StoragePath: "local/unrelated", SourcePartKey: "other:part",
			}))

			keep.Filename = "renamed.txt"
			keep.Size = 24
			requirements.NoError(tc.replace(f.Store, messageID, []store.AttachmentRef{keep}))
			retained, err := f.Store.GetFileMetadata(t.Context(), keepID)
			requirements.NoError(err)
			requirements.NotNil(retained, "resync must retain the identity of the same source occurrence")
			assertions.Equal("renamed.txt", retained.Filename)
			assertions.Equal(int64(24), retained.Size)
			removed, err := f.Store.GetFileMetadata(t.Context(), dropID)
			requirements.NoError(err)
			assertions.Nil(removed)

			requirements.NoError(tc.replace(f.Store, messageID, nil))
			message, err := f.Store.GetMessage(messageID)
			requirements.NoError(err)
			requirements.Len(message.Attachments, 1)
			assertions.Equal("unrelated.txt", message.Attachments[0].Filename)
		})
	}
}

func TestAttachmentResyncInvalidatesThumbnailsAndReconcilesDocument(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("attachment-content-change")
	ref := store.AttachmentRef{
		Filename: "report.txt", MimeType: "text/plain", Size: 12,
		ContentHash: strings.Repeat("a", 64), SourceAttachmentID: "discord:report",
		Role:       store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}
	ref.StoragePath = "aa/" + ref.ContentHash
	requirements.NoError(f.Store.ReplaceMessageDiscordAttachments(
		messageID, []store.AttachmentRef{ref},
	))
	var attachmentID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentID))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ? WHERE id = ?`),
		strings.Repeat("c", 64), "cc/"+strings.Repeat("c", 64), attachmentID)
	requirements.NoError(err)
	reconciler, err := documentindex.NewReconciler(f.Store, documentindex.ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	requirements.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	requirements.NoError(err)
	var occurrenceKey, occurrenceHash string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT occurrence_key, canonical_blob_hash FROM document_occurrences
		WHERE attachment_id = ?`), attachmentID).Scan(&occurrenceKey, &occurrenceHash))
	assert.Equal(t, strings.Repeat("a", 64), occurrenceHash)

	for _, tc := range []struct {
		name, hash    string
		wantThumbnail bool
	}{
		{"same bytes", strings.Repeat("a", 64), true},
		{"changed bytes", strings.Repeat("b", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			ref.Filename = "renamed.txt"
			ref.ContentHash = tc.hash
			ref.StoragePath = tc.hash[:2] + "/" + tc.hash
			requirements.NoError(f.Store.ReplaceMessageDiscordAttachments(
				messageID, []store.AttachmentRef{ref},
			))
			var thumbnailHash, thumbnailPath sql.NullString
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
				SELECT thumbnail_hash, thumbnail_path FROM attachments WHERE id = ?`),
				attachmentID).
				Scan(&thumbnailHash, &thumbnailPath))
			assertions.Equal(tc.wantThumbnail, thumbnailHash.Valid)
			assertions.Equal(tc.wantThumbnail, thumbnailPath.Valid)
			if tc.wantThumbnail {
				assertions.Equal(strings.Repeat("c", 64), thumbnailHash.String)
				assertions.Equal("cc/"+strings.Repeat("c", 64), thumbnailPath.String)
			}
			// The update journal refreshes document occurrences without
			// deleting the attachment or changing its stable occurrence key.
			result, err := reconciler.Reconcile(t.Context())
			requirements.NoError(err)
			assertions.Positive(result.ChangesConsumed)
			var updatedKey string
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
				SELECT occurrence_key, canonical_blob_hash FROM document_occurrences
				WHERE attachment_id = ?`), attachmentID).Scan(&updatedKey, &occurrenceHash))
			assertions.Equal(occurrenceKey, updatedKey)
			assertions.Equal(tc.hash, occurrenceHash)
		})
	}
}

func TestReplaceAttachmentsRetainsProviderIDWhenAssigningPartKey(t *testing.T) {
	for _, partKey := range []string{"", "discord:part:report"} {
		t.Run("part="+partKey, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := storetest.New(t)
			messageID := f.CreateMessage("provider-identity")
			// Provider rows written before source-part keys retain their
			// source attachment ID when the schema adds the nullable column.
			requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID,
				store.AttachmentWrite{
					Filename: "before.txt", StoragePath: "aa/" + strings.Repeat("a", 64),
					ContentHash: strings.Repeat("a", 64), SourceAttachmentID: "discord:report",
				}))
			var attachmentID int64
			var initialKey sql.NullString
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
				SELECT id, source_part_key FROM attachments WHERE message_id = ?`), messageID).
				Scan(&attachmentID, &initialKey))
			requirements.False(initialKey.Valid)
			// Prevent SQLite's maximum-row-ID reuse from hiding a delete/insert.
			requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID,
				store.AttachmentWrite{Filename: "other.txt", SourcePartKey: "other:part"}))
			ref := store.AttachmentRef{
				Filename: "after.txt", StoragePath: "bb/" + strings.Repeat("b", 64),
				ContentHash: strings.Repeat("b", 64), SourceAttachmentID: "discord:report",
				SourcePartKey: partKey,
			}
			requirements.NoError(f.Store.ReplaceMessageDiscordAttachments(
				messageID, []store.AttachmentRef{ref},
			))
			file, err := f.Store.GetFileMetadata(t.Context(), attachmentID)
			requirements.NoError(err)
			requirements.NotNil(file, "the same provider occurrence keeps its file ID")
			assertions.Equal("after.txt", file.Filename)
			assertions.Equal(strings.Repeat("b", 64), file.ContentHash)
		})
	}
}
