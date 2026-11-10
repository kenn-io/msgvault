package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/metawebhook"
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
	assert.Equal(t, "ignored", out.Class)
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

func TestApplyMentionWorkerCompletesSparseBudgetRetries(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.posts, f.paths = nil, nil
	ids := make([]string, 51)
	changes := make([]map[string]any, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("400_%d", 500+i)
		f.posts = append(f.posts, Object{ID: ids[i], Message: "Hydrated budget mention"})
		changes[i] = map[string]any{"field": "mention", "value": map[string]string{"item": "post", "verb": "add", "post_id": ids[i]}}
	}
	body, err := json.Marshal(map[string]any{"object": "page", "entry": []map[string]any{{"id": "100", "time": 10, "changes": changes}}})
	require.NoError(t, err)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	spool, err := metawebhook.OpenSpool(t.TempDir(), 1<<20, func() time.Time { return now })
	require.NoError(t, err)
	_, duplicate, err := spool.Accept("9", body)
	require.NoError(t, err)
	require.False(t, duplicate)
	due, err := spool.Due(1)
	require.NoError(t, err)
	require.Len(t, due, 1)
	receipt := due[0].ReceiptOrder
	require.NotZero(t, receipt)
	reg := metawebhook.NewRegistry()
	reg.Register("page", "mention", func(ctx context.Context, app string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		events := make([]FeedEvent, len(items))
		for i, item := range items {
			require.NoError(t, json.Unmarshal(item.Value, &events[i]))
			events[i].Time, events[i].ReceiptOrder = item.Time, item.ReceiptOrder
		}
		results, err := imp.ApplyFeedEvents(ctx, app, items[0].EntryID, events, EventOptions{Mention: true})
		require.NoError(t, err)
		out := make([]metawebhook.ItemOutcome, len(results))
		for i, result := range results {
			out[i] = metawebhook.ItemOutcome{Class: metawebhook.Class(result.Class), ErrorClass: result.ErrorClass, CountAttempt: result.CountAttempt, ArchiveChanged: result.ArchiveChanged}
		}
		return out
	})
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: reg})
	check := func(wantArchived, wantReads, wantPending, wantDone int) {
		t.Helper()
		archived, err := st.MessageExistsBatchContext(t.Context(), src.ID, ids)
		require.NoError(t, err)
		assert.Len(t, archived, wantArchived)
		assert.Len(t, f.paths, wantReads)
		stats, err := spool.Stats()
		require.NoError(t, err)
		assert.Equal(t, wantPending, stats["9"].Pending)
		assert.Equal(t, wantDone, stats["9"].Done)
	}
	_, err = worker.DrainOnce(t.Context())
	require.NoError(t, err)
	check(50, 50, 1, 0)
	now = now.Add(time.Minute)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(t, err)
	check(51, 51, 0, 1)
	noop, err := worker.DrainOnce(t.Context())
	require.NoError(t, err)
	assert.Zero(t, noop.Deliveries)
	assert.Len(t, f.paths, 51)

	// Explicit replay gets a new receipt and must hydrate every target again.
	count, err := spool.Requeue(metawebhook.RequeueFilter{States: []metawebhook.State{metawebhook.StateDone}}, false)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	due, err = spool.Due(1)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Greater(t, due[0].ReceiptOrder, receipt)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(t, err)
	check(51, 101, 1, 0)
	now = now.Add(time.Minute)
	_, err = worker.DrainOnce(t.Context())
	require.NoError(t, err)
	check(51, 102, 0, 1)
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
