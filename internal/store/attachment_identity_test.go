package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{"teams-inline", "teams:inline:", func(st *store.Store, messageID int64, refs []store.AttachmentRef) error {
			return st.ReplaceMessageInlineAttachments(messageID, refs, true)
		}},
		{"teams-links", "teams:link:", (*store.Store).ReplaceMessageLinkAttachments},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			messageID := f.CreateMessage("attachment-identity")
			keep := store.AttachmentRef{
				Filename: "keep.txt", MimeType: "text/plain", Size: 12,
				StoragePath: "https://files.example/keep.txt", SourceAttachmentID: tc.prefix + "keep",
			}
			drop := store.AttachmentRef{
				Filename: "drop.txt", StoragePath: "https://files.example/drop.txt",
				SourceAttachmentID: tc.prefix + "drop", SourcePartKey: tc.prefix + "drop-part",
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
