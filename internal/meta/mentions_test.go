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
	out = applyMention(t, imp, ev)[0]
	assert.Equal(t, "applied", out.Class)
	assert.False(t, out.ArchiveChanged)
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
		name, value, want string
		comment           bool
		reads             int
	}{
		{"null post message", `{"item":"post","verb":"add","post_id":"100_200","message":null}`, "Example post", false, 1},
		{"absent comment message", `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","parent_id":"100_200"}`, "Hydrated comment", true, 2},
		{"missing comment parent", `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","message":"Delivered top-level mention"}`, "Delivered top-level mention", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, f, imp, src := eventImporter(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Hydrated comment")}
			f.comments["100_200"][0].Parent = nil
			f.paths = nil
			ev := mentionEvent(t, tc.value)
			assert.Equal(t, "applied", applyMention(t, imp, ev)[0].Class)
			assert.Len(t, f.paths, tc.reads)
			id := "100_200"
			if tc.comment {
				id = "100_201"
			}
			row := message(t, st, src.ID, id)
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

func TestApplyMentionMissingParentChain(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.comments["100_200"] = []Object{*comment("100_210", "100_200", "Parent comment"), *comment("100_201", "100_210", "Hydrated reply")}
	f.comments["100_200"][0].Parent = nil
	ev := mentionEvent(t, `{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","message":"Delivered reply"}`)
	f.paths = nil
	assert.Equal(t, "applied", applyMention(t, imp, ev)[0].Class)
	assert.Equal(t, []string{"/v26.0/100_201", "/v26.0/100_200", "/v26.0/100_210"}, f.paths)
	assert.Equal(t, message(t, st, src.ID, "100_210").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
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
