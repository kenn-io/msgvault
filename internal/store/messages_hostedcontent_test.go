package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestForEachTeamsHostedContentBody verifies that the iterator streams only the
// message_bodies rows whose body_html contains a hostedContents URL for the
// given source, and skips rows without one (and NULL/empty bodies).
func TestForEachTeamsHostedContentBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)

	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "oneOnOne", "DM")
	require.NoError(err)

	// Message WITH a hostedContents URL.
	withID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m_with",
		MessageType:     "teams",
	})
	require.NoError(err)
	hostedHTML := `<div><img src="https://graph.microsoft.com/v1.0/chats/19:x@thread.v2/messages/m_with/hostedContents/1/$value"></div>`
	require.NoError(st.UpsertMessageBody(withID,
		sql.NullString{String: "with image", Valid: true},
		sql.NullString{String: hostedHTML, Valid: true}))

	// Message WITHOUT a hostedContents URL.
	withoutID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m_without",
		MessageType:     "teams",
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(withoutID,
		sql.NullString{String: "plain", Valid: true},
		sql.NullString{String: "<div>no images here</div>", Valid: true}))

	// Message with NULL body_html — should be skipped.
	nullID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "m_null",
		MessageType:     "teams",
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(nullID,
		sql.NullString{String: "text only", Valid: true},
		sql.NullString{}))

	var seen []int64
	var seenBodies []string
	err = st.ForEachTeamsHostedContentBody(src.ID, func(messageID int64, bodyHTML string) error {
		seen = append(seen, messageID)
		seenBodies = append(seenBodies, bodyHTML)
		return nil
	})
	require.NoError(err)

	require.Len(seen, 1, "only the hostedContents row should be streamed")
	assert.Equal(withID, seen[0])
	assert.Equal(hostedHTML, seenBodies[0])
}

// TestForEachTeamsIncompleteHostedContentBody verifies the iterator yields only
// messages whose hostedContents reference count exceeds their stored on-disk
// inline images — i.e. messages still missing media — and skips fully-downloaded
// ones.
func TestForEachTeamsIncompleteHostedContentBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "oneOnOne", "DM")
	require.NoError(err)

	mk := func(smid, html string) int64 {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID: convID, SourceID: src.ID, SourceMessageID: smid, MessageType: "teams",
		})
		require.NoError(err)
		require.NoError(st.UpsertMessageBody(id,
			sql.NullString{String: "x", Valid: true}, sql.NullString{String: html, Valid: true}))
		return id
	}

	oneRef := `<img src="https://g/v1.0/chats/x/messages/a/hostedContents/1/$value">`
	// Complete: one hostedContents ref, one stored on-disk image.
	complete := mk("m_complete", oneRef)
	require.NoError(st.UpsertAttachment(complete, "", "", "ab/abc", "abc123", 10))
	// Complete with duplicate HTML references to the same hostedContent URL:
	// the repair path should compare distinct hosted refs, not raw occurrences.
	duplicateComplete := mk("m_duplicate_complete", oneRef+oneRef)
	require.NoError(st.UpsertAttachment(duplicateComplete, "", "", "de/def", "def456", 10))
	// Incomplete: one hostedContents ref, no stored image.
	incomplete := mk("m_incomplete", oneRef)

	var seen []int64
	require.NoError(st.ForEachTeamsIncompleteHostedContentBody(src.ID, func(id int64, _ string) error {
		seen = append(seen, id)
		return nil
	}))
	require.Len(seen, 1, "only the message still missing media should be yielded")
	assert.Equal(incomplete, seen[0])
	assert.NotContains(seen, duplicateComplete)
}

// TestForEachTeamsHostedContentBody_WriteInsideCallback verifies that the
// callback can write to the store without the iterator's read cursor causing
// contention — the iterator must read all matching rows and close the cursor
// before invoking callbacks, since callers write (UpsertAttachment) inside fn.
func TestForEachTeamsHostedContentBody_WriteInsideCallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "oneOnOne", "DM")
	require.NoError(err)

	// Seed several hostedContents messages so the callback writes many times
	// while iterating (the pattern that previously deadlocked on a live DB).
	ids := make([]int64, 0, 5)
	for _, smid := range []string{"a", "b", "c", "d", "e"} {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: "m_" + smid,
			MessageType:     "teams",
		})
		require.NoError(err)
		html := `<img src="https://graph.microsoft.com/v1.0/chats/x/messages/m_` + smid + `/hostedContents/1/$value">`
		require.NoError(st.UpsertMessageBody(id,
			sql.NullString{String: "x", Valid: true},
			sql.NullString{String: html, Valid: true}))
		ids = append(ids, id)
	}

	err = st.ForEachTeamsHostedContentBody(src.ID, func(messageID int64, bodyHTML string) error {
		// Write inside the callback — must not error/deadlock. Use the message
		// id in the content hash so each row is distinct.
		hash := fmt.Sprintf("hash-%d", messageID)
		return st.UpsertAttachment(messageID, "img", "image/png", "abc/"+hash, hash, 1)
	})
	require.NoError(err)

	for _, id := range ids {
		var n int
		require.NoError(st.DB().QueryRow(
			st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`),
			id,
		).Scan(&n))
		assert.Equal(1, n, "callback write should have persisted for message %d", id)
	}
}
