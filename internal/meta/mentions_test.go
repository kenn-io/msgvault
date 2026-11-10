package meta

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mentionEvent(t *testing.T, value string) FeedEvent {
	t.Helper()
	var ev FeedEvent
	require.NoError(t, json.Unmarshal([]byte(value), &ev))
	ev.Time, ev.ReceiptOrder = 10, 1
	return ev
}

func applyMention(t *testing.T, imp *EventImporter, ev ...FeedEvent) []EventOutcome {
	t.Helper()
	return applyEvents(t, imp, EventOptions{Mention: true}, ev...)
}

func TestApplyMentionPayloadPost(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	ev := mentionEvent(t, `{"item":"post","verb":"add","post_id":"400_500","message":"Foreign searchable mention"}`)
	f.paths = nil
	out := applyMention(t, imp, ev)[0]
	assert.Equal(t, "applied", out.Class)
	assert.True(t, out.ArchiveChanged)
	assert.Empty(t, f.paths)
	root := message(t, st, src.ID, "400_500")
	assert.False(t, root.SenderID.Valid)
	assert.False(t, root.ReplyToMessageID.Valid)
	body, err := st.GetMessageBodyText(root.ID)
	require.NoError(t, err)
	assert.Equal(t, "Foreign searchable mention", body)
	for _, verb := range []string{"remove", "delete", "hide", "unhide", "edited", "edit"} {
		ev.Verb = verb
		assert.Equal(t, "ignored", applyMention(t, imp, ev)[0].Class)
	}
	assert.False(t, tombstoned(t, st, src.ID, "400_500"))
	ev.Verb, ev.Time, ev.Message = "add", 20, "Updated searchable mention"
	require.Equal(t, "applied", applyMention(t, imp, ev)[0].Class)
	ev.Time, ev.Message = 10, "Older text"
	ev.From, ev.CreatedTime = &Actor{ID: "300", Name: "Example Customer"}, "2026-10-01T12:00:00Z"
	assert.Equal(t, "ignored", applyMention(t, imp, ev)[0].Class)
	root = message(t, st, src.ID, "400_500")
	assert.True(t, root.SenderID.Valid)
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, rawFormat, []string{"400_500"})
	require.NoError(t, err)
	assert.True(t, rows["400_500"].SentAt.Valid)
	body, err = st.GetMessageBodyText(root.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated searchable mention", body)
	assert.Empty(t, f.paths)
}

func TestApplyMentionSparseTargets(t *testing.T) {
	for _, tc := range []struct {
		name, value, want   string
		nodeParent, replyTo string
		paths               []string
	}{
		{
			name: "null post message", value: `{"item":"post","verb":"add","post_id":"100_200","message":null}`, want: "Example post",
			paths: []string{"/v26.0/100_200"},
		},
		{
			name: "absent comment message", value: `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","parent_id":"100_200"}`, want: "Hydrated comment",
			replyTo: "100_200", paths: []string{"/v26.0/100_201", "/v26.0/100_200"},
		},
		{
			name: "missing comment parent", value: `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","message":"Delivered top-level mention"}`, want: "Delivered top-level mention",
			replyTo: "100_200", paths: []string{"/v26.0/100_201", "/v26.0/100_200"},
		},
		{
			name: "missing parent chain", value: `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","message":"Delivered reply"}`, want: "Delivered reply",
			nodeParent: "100_210", replyTo: "100_210", paths: []string{"/v26.0/100_201", "/v26.0/100_200", "/v26.0/100_210"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f, imp, src := eventImporter(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Hydrated comment")}
			f.comments["100_200"][0].Parent = nil
			if tc.nodeParent != "" {
				f.comments["100_200"][0].Parent = &Actor{ID: tc.nodeParent}
				ancestor := comment(tc.nodeParent, "100_200", "Parent comment")
				ancestor.Parent = nil
				f.comments["100_200"] = append(f.comments["100_200"], *ancestor)
			}
			f.paths = nil
			ev := mentionEvent(t, tc.value)
			assert.Equal(t, "applied", applyMention(t, imp, ev)[0].Class)
			assert.Equal(t, tc.paths, f.paths)
			id := ev.CommentID
			if id == "" {
				id = ev.PostID
			}
			row := message(t, st, src.ID, id)
			if tc.replyTo != "" {
				assert.Equal(t, message(t, st, src.ID, tc.replyTo).ID, row.ReplyToMessageID.Int64)
			}
			body, err := st.GetMessageBodyText(row.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, body)
			metadata, err := st.GetMessageMetadataContext(t.Context(), row.ID)
			require.NoError(t, err)
			var saved objectMetadata
			require.NoError(t, json.Unmarshal([]byte(metadata.String), &saved))
			assert.Equal(t, int64(10), saved.BodyTime)
			assert.Equal(t, uint64(1), saved.BodyOrder)
		})
	}
}

func TestApplyMentionReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name, value   string
		budget        int
		deny          string
		class, reason string
	}{
		{name: "inaccessible target", value: `{"item":"post","verb":"add","post_id":"100_200"}`, deny: "100_200", class: "retry", reason: "permission_missing"},
		{name: "shared budget", value: `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201"}`, budget: 1, class: "retry", reason: "request_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f, imp, src := eventImporter(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Readable target")}
			f.comments["100_200"][0].Parent = nil
			if tc.deny != "" {
				f.denied[tc.deny] = true
			}
			out, err := imp.ApplyFeedEvents(t.Context(), "9", "100", []FeedEvent{mentionEvent(t, tc.value)}, EventOptions{Mention: true, MaxRequests: tc.budget})
			require.NoError(t, err)
			require.Len(t, out, 1)
			assert.Equal(t, tc.class, out[0].Class)
			assert.Equal(t, tc.reason, out[0].ErrorClass)
			ids, err := st.MessageExistsBatch(src.ID, []string{"100_200", "100_201"})
			require.NoError(t, err)
			assert.Empty(t, ids, "reject incomplete evidence before writing ancestors")
		})
	}
}

func TestApplyMentionSparseBudgetRetries(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.posts, f.paths = nil, nil
	ids := []string{"400_500", "400_501", "400_502"}
	events := make([]FeedEvent, len(ids))
	for i, id := range ids {
		f.posts = append(f.posts, Object{ID: id, Message: "Hydrated budget mention"})
		events[i] = FeedEvent{Time: 10, ReceiptOrder: uint64(i + 1), Item: "post", Verb: "add", PostID: id}
	}
	apply := func(wantArchived, wantReads int) []EventOutcome {
		t.Helper()
		out, err := imp.ApplyFeedEvents(t.Context(), "9", "100", events, EventOptions{Mention: true, MaxRequests: 2})
		require.NoError(t, err)
		archived, err := st.MessageExistsBatchContext(t.Context(), src.ID, ids)
		require.NoError(t, err)
		assert.Len(t, archived, wantArchived)
		assert.Len(t, f.paths, wantReads)
		return out
	}
	assert.Equal(t, []EventOutcome{
		{Class: "applied", ArchiveChanged: true},
		{Class: "applied", ArchiveChanged: true},
		{Class: "retry", ErrorClass: "request_budget"},
	}, apply(2, 2))
	assert.Equal(t, []EventOutcome{
		{Class: "ignored"},
		{Class: "ignored"},
		{Class: "applied", ArchiveChanged: true},
	}, apply(3, 3))

	// A new receipt hydrates again even at the same provider timestamp.
	for i := range events {
		events[i].ReceiptOrder += uint64(len(events))
	}
	assert.Equal(t, []EventOutcome{
		{Class: "applied"},
		{Class: "applied"},
		{Class: "retry", ErrorClass: "request_budget"},
	}, apply(3, 5))
	assert.Equal(t, []EventOutcome{
		{Class: "ignored"},
		{Class: "ignored"},
		{Class: "applied"},
	}, apply(3, 6))
}

func TestApplyMentionStaleSparseDoesNotNeedTarget(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	ev := feedEvent("edited", 20)
	ev.Message = "Newer feed body"
	require.Equal(t, "applied", applyFeed(t, imp, ev)[0].Class)
	f.denied["100_201"] = true
	for _, parent := range []string{`,"parent_id":"100_200"`, ""} {
		old := mentionEvent(t, `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201"`+parent+`}`)
		f.paths = nil
		out := applyMention(t, imp, old)[0]
		assert.Equal(t, "ignored", out.Class, "%s", out.ErrorClass)
		assert.Empty(t, f.paths)
		assert.Equal(t, "Newer feed body", feedBody(t, st, src.ID))
	}
}

func TestApplyMentionArchivedParentNeedsNoTargetRead(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	require.Equal(t, "applied", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	f.denied["100_201"] = true
	f.paths = nil
	ev := mentionEvent(t, `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","message":"New delivered mention"}`)
	ev.Time = 20
	out := applyMention(t, imp, ev)[0]
	require.Equal(t, "applied", out.Class, "%s", out.ErrorClass)
	assert.Empty(t, f.paths)
	assert.Equal(t, "New delivered mention", feedBody(t, st, src.ID))
}

func TestApplyMentionKeepsDeliveredParent(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_210", "Hydrated text")}
	ev := mentionEvent(t, `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","parent_id":"100_200"}`)
	require.Equal(t, "applied", applyMention(t, imp, ev)[0].Class)
	assert.Equal(t, message(t, st, src.ID, "100_200").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
	assert.Equal(t, "Hydrated text", feedBody(t, st, src.ID))
}
