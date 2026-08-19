package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
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

	convID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "direct_chat", "DM")
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
	require.NoError(st.ForEachTeamsIncompleteHostedContentBody(src.ID, attachmentpolicy.Policy{}, func(id int64, _ string) error {
		seen = append(seen, id)
		return nil
	}))
	require.Len(seen, 1, "only the message still missing media should be yielded")
	assert.Equal(incomplete, seen[0])
	assert.NotContains(seen, duplicateComplete)
}

func TestForEachTeamsIncompleteHostedContentBodyReevaluatesPolicySkips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "chat", "direct_chat", "DM")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID,
		SourceMessageID: "skipped", MessageType: "teams",
	})
	require.NoError(err)
	body := `<img src="https://g/v1.0/chats/x/messages/a/hostedContents/1/$value">`
	require.NoError(st.UpsertMessageBody(messageID, sql.NullString{}, sql.NullString{String: body, Valid: true}))
	require.NoError(st.ReplaceMessageInlineAttachments(messageID, []store.AttachmentRef{{
		SourceAttachmentID: "teams:inline:/chats/x/messages/a/hostedContents/1/$value",
		StoragePath:        "excluded:teams:inline:/chats/x/messages/a/hostedContents/1/$value",
		Size:               10,
		State:              attachmentpolicy.StateSkipped,
		SkipReason:         attachmentpolicy.SkipPolicyScope,
	}}, false))

	var unchanged []int64
	require.NoError(st.ForEachTeamsIncompleteHostedContentBody(src.ID, attachmentpolicy.Policy{
		Scope: attachmentpolicy.ScopeNone,
	}, func(id int64, _ string) error {
		unchanged = append(unchanged, id)
		return nil
	}))
	assert.Empty(unchanged)

	var relaxed []int64
	require.NoError(st.ForEachTeamsIncompleteHostedContentBody(src.ID, attachmentpolicy.Policy{}, func(id int64, _ string) error {
		relaxed = append(relaxed, id)
		return nil
	}))
	assert.Equal([]int64{messageID}, relaxed)
}

// TestForEachTeamsIncompleteHostedContentBodyYieldsUnresolvedRosterSkips pins
// the backfill contract for a roster the sync could not read: the accumulated
// participant rows are not authoritative, so a participant-threshold skip must
// still reach the importer, which re-resolves membership before it evaluates
// the policy. Other exclusions keep applying so a scope-excluded marker does
// not trigger a roster fetch on every run.
func TestForEachTeamsIncompleteHostedContentBodyYieldsUnresolvedRosterSkips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "team/channel", "channel", "Releases")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`), 20, conversationID)
	require.NoError(err)
	require.NoError(st.MarkConversationMemberCountUnknown(conversationID))
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID,
		SourceMessageID: "skipped", MessageType: "teams",
	})
	require.NoError(err)
	body := `<img src="https://g/v1.0/teams/t/channels/c/messages/a/hostedContents/1/$value">`
	require.NoError(st.UpsertMessageBody(messageID, sql.NullString{}, sql.NullString{String: body, Valid: true}))
	skipped := func(reason attachmentpolicy.SkipReason) {
		require.NoError(st.ReplaceMessageInlineAttachments(messageID, []store.AttachmentRef{{
			SourceAttachmentID: "teams:inline:/teams/t/channels/c/messages/a/hostedContents/1/$value",
			StoragePath:        "excluded:teams:inline:/teams/t/channels/c/messages/a/hostedContents/1/$value",
			Size:               10,
			State:              attachmentpolicy.StateSkipped,
			SkipReason:         reason,
		}}, false))
	}
	yielded := func(policy attachmentpolicy.Policy) []int64 {
		var ids []int64
		require.NoError(st.ForEachTeamsIncompleteHostedContentBody(src.ID, policy, func(id int64, _ string) error {
			ids = append(ids, id)
			return nil
		}))
		return ids
	}
	limited := attachmentpolicy.Policy{MaxParticipants: 4}

	skipped(attachmentpolicy.SkipParticipantThreshold)
	assert.Equal([]int64{messageID}, yielded(limited),
		"an unresolved roster must reach the importer even though the accumulated count exceeds the limit")

	skipped(attachmentpolicy.SkipPolicyScope)
	assert.Empty(yielded(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeNone, MaxParticipants: 4}),
		"other exclusions still apply while the roster is unresolved")

	// Once the roster is read, the archived count is authoritative again.
	skipped(attachmentpolicy.SkipParticipantThreshold)
	require.NoError(st.SetConversationMemberCount(conversationID, 20))
	assert.Empty(yielded(limited))
	require.NoError(st.SetConversationMemberCount(conversationID, 3))
	assert.Equal([]int64{messageID}, yielded(limited))

	// A failed read after a successful one leaves the roster unresolved again,
	// so the importer gets to re-resolve it whatever the stale count says.
	require.NoError(st.SetConversationMemberCount(conversationID, 20))
	require.NoError(st.MarkConversationMemberCountUnknown(conversationID))
	assert.Equal([]int64{messageID}, yielded(limited))
}

func TestForEachTeamsIncompleteHostedContentBodyFindsMarkersBesideLegacyMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "chat", "direct_chat", "DM")
	require.NoError(err)
	body := `<img src="https://g/v1.0/chats/x/messages/a/hostedContents/1/$value">`
	seed := func(sourceMessageID string, state attachmentpolicy.DownloadState, reason attachmentpolicy.SkipReason) int64 {
		messageID, seedErr := st.UpsertMessage(&store.Message{
			ConversationID: conversationID, SourceID: src.ID,
			SourceMessageID: sourceMessageID, MessageType: "teams",
		})
		require.NoError(seedErr)
		require.NoError(st.UpsertMessageBody(messageID, sql.NullString{}, sql.NullString{String: body, Valid: true}))
		require.NoError(st.UpsertAttachment(messageID, "", "", "legacy/"+sourceMessageID, "legacy-"+sourceMessageID, 12))
		require.NoError(st.ReplaceMessageInlineAttachments(messageID, []store.AttachmentRef{{
			SourceAttachmentID: "teams:inline:/chats/x/messages/a/hostedContents/1/$value",
			StoragePath:        "https://g/v1.0/chats/x/messages/a/hostedContents/1/$value",
			State:              state,
			SkipReason:         reason,
		}}, true))
		return messageID
	}
	failedID := seed("failed", attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure)
	skippedID := seed("skipped", attachmentpolicy.StateSkipped, attachmentpolicy.SkipPolicyScope)

	var seen []int64
	require.NoError(st.ForEachTeamsIncompleteHostedContentBody(src.ID, attachmentpolicy.Policy{}, func(id int64, _ string) error {
		seen = append(seen, id)
		return nil
	}))
	assert.ElementsMatch([]int64{failedID, skippedID}, seen,
		"legacy stored media must not hide retryable or newly allowed markers")
}

// TestReplaceMessageLinkAttachmentsPreservesTeamsInlineMarkers verifies that
// replacing a message's link attachments leaves Teams inline markers alone.
// Those markers carry a URL storage path too, but it records a durable
// pending/skipped/failed outcome rather than a link the importer re-derives
// from the message — deleting them loses the outcome and re-fetches excluded
// media on the next sync.
func TestReplaceMessageLinkAttachmentsPreservesTeamsInlineMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("teams", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(src.ID, "19:x@thread.v2", "direct_chat", "DM")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: src.ID,
		SourceMessageID: "m1", MessageType: "teams",
	})
	require.NoError(err)

	const hostedURL = "https://graph.microsoft.com/v1.0/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value"
	const markerID = "teams:inline:/chats/19:x@thread.v2/messages/m1/hostedContents/1/$value"

	// A stale link row from an earlier import, which the replacement must drop.
	require.NoError(st.ReplaceMessageLinkAttachments(messageID, []store.AttachmentRef{{
		Filename: "old.pdf", StoragePath: "https://example.com/old.pdf",
	}}))
	require.NoError(st.ReplaceMessageInlineAttachments(messageID, []store.AttachmentRef{{
		SourceAttachmentID: markerID,
		StoragePath:        hostedURL,
		Size:               4096,
		State:              attachmentpolicy.StateSkipped,
		SkipReason:         attachmentpolicy.SkipSizeCap,
	}}, false))

	require.NoError(st.ReplaceMessageLinkAttachments(messageID, []store.AttachmentRef{{
		Filename: "new.pdf", StoragePath: "https://example.com/new.pdf",
	}}))

	markers, err := st.MessageTeamsInlineAttachments(messageID)
	require.NoError(err)
	require.Contains(markers, markerID, "the inline marker must survive link replacement")
	assert.Equal(attachmentpolicy.StateSkipped, markers[markerID].State)
	assert.Equal(attachmentpolicy.SkipSizeCap, markers[markerID].SkipReason)
	assert.Equal(4096, markers[markerID].Size, "the observed size must survive so the skip stays decidable")

	rows, err := st.DB().Query(st.Rebind(`
		SELECT storage_path FROM attachments WHERE message_id = ? ORDER BY storage_path
	`), messageID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var path string
		require.NoError(rows.Scan(&path))
		paths = append(paths, path)
	}
	require.NoError(rows.Err())
	assert.Equal([]string{"https://example.com/new.pdf", hostedURL}, paths,
		"the new link replaces the stale one while the marker is untouched")
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
