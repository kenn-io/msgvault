package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestListMessagesSourceScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	var sources []int64
	var wanted []int64
	for i := range 2 {
		src, err := st.GetOrCreateSource("test", fmt.Sprintf("scope-%d@example.test", i))
		requirements.NoError(err)
		sources = append(sources, src.ID)
		conv, err := st.EnsureConversation(src.ID, "thread", "Synthetic")
		requirements.NoError(err)
		for j := range 3 {
			id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(j), MessageType: "whatsapp", Subject: sql.NullString{String: "Synthetic", Valid: true}})
			requirements.NoError(err)
			if j == 2 {
				requirements.NoError(st.MarkMessageDeleted(src.ID, strconv.Itoa(j)))
			} else if i == 0 {
				wanted = append(wanted, id)
			}
		}
	}
	for page := range 2 {
		rows, total, err := st.ListMessagesContext(t.Context(), page, 1, sources[:1])
		requirements.NoError(err)
		requirements.Len(rows, 1)
		assertions.Equal(int64(2), total)
		assertions.Equal(wanted[1-page], rows[0].ID)
		assertions.Equal(sources[0], rows[0].SourceID)
	}
	for _, ids := range [][]int64{{}, {-1}} {
		rows, total, err := st.ListMessagesContext(t.Context(), 0, 20, ids)
		requirements.NoError(err)
		assertions.Empty(rows)
		assertions.Zero(total)
	}
	rows, total, err := st.ListMessagesContext(t.Context(), 0, 20, nil)
	requirements.NoError(err)
	assertions.Len(rows, 4)
	assertions.Equal(int64(4), total)
}

func TestReadSnapshotContextHydration(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "original body", Valid: true}, sql.NullString{}))
	original, err := st.EnsureParticipant("original@example.test", "Original", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{original}, nil))
	image := store.AttachmentWrite{SourcePartKey: "remote-image:chart", SourceAttachmentID: "remote-image:chart", ContentID: "remote-image:chart", ContentHash: "original", StoragePath: "original.png", MIMEType: "image/png"}
	requirements.NoError(st.UpsertRemoteImageAttachment(t.Context(), id, image))
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	requirements.NoError(store.ReadDBContext(ctx, st.DB()).QueryRowContext(ctx, st.Rebind("SELECT source_id FROM messages WHERE id=?"), id).Scan(new(int64)))
	replacement, err := st.EnsureParticipant("replacement@example.test", "Replacement", "example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "replacement body", Valid: true}, sql.NullString{}))
	requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{replacement}, nil))
	image.ContentHash, image.StoragePath = "replacement", "replacement.png"
	requirements.NoError(st.UpsertRemoteImageAttachment(t.Context(), id, image))
	refs, err := st.MessageRemoteImagesContext(ctx, id)
	requirements.NoError(err)
	assertions.Equal("original", refs[image.SourceAttachmentID].ContentHash)
	assertions.Equal("original.png", refs[image.SourceAttachmentID].StoragePath)
	detail, err := query.NewEngine(st.DB(), st.IsPostgreSQL()).GetMessage(ctx, id)
	requirements.NoError(err)
	requirements.NotNil(detail)
	assertions.Equal("original body", detail.BodyText)
	requirements.Len(detail.To, 1)
	assertions.Equal("original@example.test", detail.To[0].Email)
	fallback, err := st.GetMessageContext(ctx, id)
	requirements.NoError(err)
	assertions.Equal("original body", fallback.BodyText)
	assertions.Contains(fallback.To[0], "original@example.test")
	release()
	ctx = store.WithoutReadSnapshotContext(ctx)
	requirements.NoError(store.ReadDBContext(ctx, st.DB()).QueryRowContext(ctx, "SELECT 1").Scan(new(int)))
	detail, err = query.NewEngine(st.DB(), st.IsPostgreSQL()).GetMessage(ctx, id)
	requirements.NoError(err)
	refs, err = st.MessageRemoteImagesContext(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("replacement", refs[image.SourceAttachmentID].ContentHash)
	assertions.Equal("replacement body", detail.BodyText)
	assertions.Equal("replacement@example.test", detail.To[0].Email)
	canceled, cancel := context.WithCancel(t.Context())
	ctx, release, err = st.BeginReadSnapshotContext(canceled)
	requirements.NoError(err)
	cancel()
	_, err = st.ListSourcesContext(ctx, "")
	requirements.Error(err)
	release()
	requirements.NoError(st.UpdateSourceDisplayName(src.ID, "After cancellation"))
}
