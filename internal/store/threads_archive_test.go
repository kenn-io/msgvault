package store_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestThreadsArchiveMessageMetadataContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s := testutil.NewTestStore(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		require.True(s.IsPostgreSQL())
	}
	add := func(sourceIdentifier, root, media, metadata string) (int64, int64) {
		source, err := s.GetOrCreateSource("threads", sourceIdentifier)
		require.NoError(err)
		conversation, err := s.EnsureConversationWithTypeContext(t.Context(), source.ID, root, "channel", "root")
		require.NoError(err)
		id, err := s.UpsertMessageContext(t.Context(), &store.Message{SourceID: source.ID, SourceMessageID: media, ConversationID: conversation, MessageType: "threads"})
		require.NoError(err)
		require.NoError(s.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: metadata, Valid: true}))
		return source.ID, id
	}
	metadata := `{"root_id":"999","parent_id":"100"}`
	sourceID, id := add("threads:123:456", "100", "101", metadata)
	otherSource, otherID := add("threads:123:789", "200", "101", `[]`)
	add("threads:123:456", "300", "301", `[]`)
	add("threads:123:789", "200", "201", `{}`)
	record, found, err := s.ThreadsArchiveMessageMetadataContext(t.Context(), sourceID, "101")
	require.NoError(err)
	require.True(found)
	assert.Equal(id, record.ID)
	assert.Equal(sql.NullString{String: "100", Valid: true}, record.SourceConversationID, "physical archive conversation must not come from provider metadata")
	assert.True(record.Metadata.Valid)
	assert.JSONEq(metadata, record.Metadata.String)
	record, found, err = s.ThreadsArchiveMessageMetadataContext(t.Context(), otherSource, "101")
	require.NoError(err)
	require.True(found)
	assert.Equal(otherID, record.ID)
	assert.Equal("200", record.SourceConversationID.String)
	assert.JSONEq(`[]`, record.Metadata.String, "lookup preserves metadata without decoding it")
	for _, missing := range []string{"404", "201"} {
		_, found, err = s.ThreadsArchiveMessageMetadataContext(t.Context(), sourceID, missing)
		require.NoError(err)
		assert.False(found, "an ID in another source is not a local target")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = s.ThreadsArchiveMessageMetadataContext(ctx, sourceID, "101")
	assert.ErrorIs(err, context.Canceled)
}
