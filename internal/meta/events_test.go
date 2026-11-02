package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func feedEvent(verb string, stamp int64) FeedEvent {
	return FeedEvent{Time: stamp, Item: "comment", Verb: verb, PostID: "100_200", CommentID: "100_201", ParentID: "100_200", Message: "webhook example", CreatedTime: "2026-10-01T12:00:00+0000", From: &Actor{ID: "300", Name: "Example Customer"}}
}
func eventImporter(t *testing.T) (*store.Store, *graphFixture, *EventImporter, *store.Source) {
	t.Helper()
	st, f, imp, src := setupImport(t)
	return st, f, NewEventImporter(st, imp.api.(*Client)), src
}
func applyFeed(t *testing.T, imp *EventImporter, ev ...FeedEvent) []EventOutcome {
	t.Helper()
	out, err := imp.ApplyFeedEvents(t.Context(), "9", "100", ev, EventOptions{})
	require.NoError(t, err)
	require.Len(t, out, len(ev))
	return out
}
func feedMetadata(t *testing.T, st *store.Store, src int64) objectMetadata {
	t.Helper()
	m := message(t, st, src, "100_201")
	saved, err := st.GetMessageMetadataContext(t.Context(), m.ID)
	require.NoError(t, err)
	var metadata objectMetadata
	require.NoError(t, json.Unmarshal([]byte(saved.String), &metadata))
	return metadata
}
func feedBody(t *testing.T, st *store.Store, src int64) string {
	t.Helper()
	body, err := st.GetMessageBodyText(message(t, st, src, "100_201").ID)
	require.NoError(t, err)
	return body
}
func tombstoned(t *testing.T, st *store.Store, src int64, native string) bool {
	t.Helper()
	var deleted sql.NullString
	require.NoError(t, st.DB().QueryRow(`SELECT deleted_from_source_at FROM messages WHERE source_id=? AND source_message_id=?`, src, native).Scan(&deleted))
	return deleted.Valid
}

func TestApplyFeedAddTopLevel(t *testing.T) {
	st, _, imp, src := setupImport(t)
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	out := applyFeed(t, NewEventImporter(st, imp.api.(*Client)), feedEvent("add", 10))
	assert.Equal(t, "applied", out[0].Class)
	got := message(t, st, src.ID, "100_201")
	root := message(t, st, src.ID, "100_200")
	assert.Equal(t, root.ConversationID, got.ConversationID)
	assert.Equal(t, root.ID, got.ReplyToMessageID.Int64)

	other, pf, poll, ps := setupImport(t)
	pf.comments["100_200"] = []Object{*comment("100_201", "100_200", "webhook example")}
	_, err = poll.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	want := message(t, other, ps.ID, "100_201")
	assert.Equal(t, want.ConversationID, got.ConversationID)
	assert.Equal(t, want.ReplyToMessageID, got.ReplyToMessageID)
	assert.Equal(t, want.SenderID, got.SenderID)
	assert.Equal(t, feedBody(t, other, ps.ID), feedBody(t, st, src.ID))
}

func TestApplyFeedFetchesMissingRoot(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.paths = nil
	assert.Equal(t, "applied", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	assert.Equal(t, []string{"/v26.0/100_200"}, f.paths)
	assert.Equal(t, message(t, st, src.ID, "100_200").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
}

func TestApplyFeedFetchesParentChain(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.comments["100_200"] = []Object{*comment("100_210", "100_200", "ancestor one"), *comment("100_211", "100_210", "ancestor two")}
	ev := feedEvent("add", 10)
	ev.ParentID = "100_211"
	assert.Equal(t, "applied", applyFeed(t, imp, ev)[0].Class)
	assert.Equal(t, message(t, st, src.ID, "100_211").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
	assert.Contains(t, f.paths, "/v26.0/100_210")
}

func TestApplyFeedDepthCapRetry(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	parent := "100_200"
	for i := range 9 {
		id := fmt.Sprintf("100_%d", 210+i)
		f.comments["100_200"] = append(f.comments["100_200"], *comment(id, parent, "ancestor"))
		parent = id
	}
	ev := feedEvent("add", 10)
	ev.ParentID = parent
	out := applyFeed(t, imp, ev)
	assert.Equal(t, "retry", out[0].Class)
	assert.True(t, out[0].CountAttempt)
	ids, err := st.MessageExistsBatch(src.ID, []string{ev.CommentID})
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestApplyFeedBudgetRetry(t *testing.T) {
	_, f, imp, _ := eventImporter(t)
	f.comments["100_200"] = []Object{*comment("100_210", "100_200", "ancestor")}
	ev := feedEvent("add", 10)
	ev.ParentID = "100_210"
	before := f.calls
	out, err := imp.ApplyFeedEvents(t.Context(), "9", "100", []FeedEvent{ev}, EventOptions{MaxRequests: 1})
	require.NoError(t, err)
	assert.Equal(t, "retry", out[0].Class)
	assert.Equal(t, 1, f.calls-before)
}

func TestApplyFeedEditedUpdatesBody(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	ev := feedEvent("edited", 20)
	ev.Message = "new body"
	assert.Equal(t, "applied", applyFeed(t, imp, ev)[0].Class)
	assert.Equal(t, "new body", feedBody(t, st, src.ID))
	assert.Equal(t, int64(20), feedMetadata(t, st, src.ID).BodyTime)
}
func TestApplyFeedStaleEditIgnored(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("edited", 20))
	ev := feedEvent("edit", 10)
	ev.Message = "stale body"
	assert.Equal(t, "ignored", applyFeed(t, imp, ev)[0].Class)
	assert.Equal(t, "webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedAddAfterEditKeepsEditedBody(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("edited", 20)
	ev.Message = "edited body"
	applyFeed(t, imp, ev)
	assert.Equal(t, "ignored", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	assert.Equal(t, "edited body", feedBody(t, st, src.ID))
}
func TestApplyFeedHideUnhide(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10), feedEvent("hide", 20))
	assert.True(t, feedMetadata(t, st, src.ID).Hidden)
	applyFeed(t, imp, feedEvent("unhide", 20))
	assert.False(t, feedMetadata(t, st, src.ID).Hidden)
}
func TestApplyFeedStaleHideIgnored(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("hide", 20), feedEvent("unhide", 30))
	assert.Equal(t, "ignored", applyFeed(t, imp, feedEvent("hide", 25))[0].Class)
	assert.False(t, feedMetadata(t, st, src.ID).Hidden)
}
func TestApplyFeedHideUnknownPersistsThenHides(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("hide", 20)
	ev.Message = ""
	applyFeed(t, imp, ev, feedEvent("add", 10))
	metadata := feedMetadata(t, st, src.ID)
	assert.True(t, metadata.Hidden)
	assert.Equal(t, int64(20), metadata.StateTime)
	assert.Equal(t, int64(10), metadata.BodyTime)
	assert.Equal(t, "webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedRemoveTombstonesKeepsRow(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	id := message(t, st, src.ID, "100_201").ID
	applyFeed(t, imp, feedEvent("remove", 20))
	assert.Equal(t, id, message(t, st, src.ID, "100_201").ID)
	assert.True(t, tombstoned(t, st, src.ID, "100_201"))
	assert.Equal(t, "webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedRemoveUnknownPersistsTombstone(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("delete", 20)
	ev.Message = ""
	applyFeed(t, imp, ev, feedEvent("add", 10))
	assert.True(t, tombstoned(t, st, src.ID, "100_201"))
	assert.Equal(t, "webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedAddAfterRemoveKeepsTombstone(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10), feedEvent("remove", 20), feedEvent("add", 10))
	assert.True(t, tombstoned(t, st, src.ID, "100_201"))
}
func TestApplyFeedStateVerbAncestorGoneIgnored(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.posts = nil
	out := applyFeed(t, imp, feedEvent("remove", 20))[0]
	assert.Equal(t, "ignored", out.Class)
	assert.Equal(t, "ancestor_unavailable", out.ErrorClass)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_201"})
	require.NoError(t, err)
	assert.Empty(t, ids)
}
func TestApplyFeedPostRemoveTombstonesRoot(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	ev := feedEvent("remove", 20)
	ev.Item = "post"
	applyFeed(t, imp, ev)
	assert.True(t, tombstoned(t, st, src.ID, "100_200"))
	assert.False(t, tombstoned(t, st, src.ID, "100_201"))
}
func TestApplyFeedIgnoredOnlyStartsNoRun(t *testing.T) {
	st, _, imp, _ := eventImporter(t)
	ev := feedEvent("edit", 20)
	ev.Item = "post"
	assert.Equal(t, "ignored", applyFeed(t, imp, ev)[0].Class)
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM sync_runs`).Scan(&count))
	assert.Zero(t, count)
}
func TestApplyFeedNonCommentIgnored(t *testing.T) {
	_, _, imp, _ := eventImporter(t)
	for _, item := range []string{"reaction", "status", "comment"} {
		ev := feedEvent("follow", 20)
		ev.Item = item
		assert.Equal(t, "ignored", applyFeed(t, imp, ev)[0].Class)
	}
}
func TestApplyFeedPageAuthorIsFromMe(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("add", 10)
	ev.From = &Actor{ID: "100", Name: "Example Page"}
	applyFeed(t, imp, ev)
	assert.True(t, message(t, st, src.ID, "100_201").IsFromMe)
}
func TestApplyFeedCreatedTimeForms(t *testing.T) {
	for _, value := range []string{`1790856000`, `"2026-10-01T12:00:00+0000"`, `"2026-10-01T12:00:00Z"`} {
		var ev FeedEvent
		require.NoError(t, json.Unmarshal([]byte(`{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","created_time":`+value+`}`), &ev))
		st, _, imp, src := eventImporter(t)
		assert.Equal(t, "applied", applyFeed(t, imp, ev)[0].Class)
		var sent time.Time
		require.NoError(t, st.DB().QueryRow(`SELECT sent_at FROM messages WHERE source_id=? AND source_message_id='100_201'`, src.ID).Scan(&sent))
		assert.Equal(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), sent.UTC())
	}
}
func TestApplyFeedConflictingParentInvalid(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	ev := feedEvent("edited", 20)
	ev.ParentID = "100_210"
	ev.Message = "conflicting body"
	assert.Equal(t, "invalid", applyFeed(t, imp, ev)[0].Class)
	assert.Equal(t, "webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedPermissionRetryClass(t *testing.T) {
	_, f, imp, _ := eventImporter(t)
	f.denied["100_200"] = true
	out := applyFeed(t, imp, feedEvent("add", 10))[0]
	assert.Equal(t, "retry", out.Class)
	assert.Equal(t, "permission_missing", out.ErrorClass)
}
func TestApplyFeedLockBusyRetryUncounted(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	exec, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, exec.Release()) }()
	calls := f.calls
	out := applyFeed(t, imp, feedEvent("add", 10))[0]
	assert.Equal(t, "retry", out.Class)
	assert.False(t, out.CountAttempt)
	assert.Equal(t, calls, f.calls)
}
func TestApplyFeedRetiredSourceUnrouted(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	other, err := st.GetOrCreateSource(SourceType, "app:99:page:100")
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	require.NoError(t, err)
	before := f.calls
	assert.Equal(t, "unrouted", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	assert.Equal(t, before, f.calls)
}
func TestApplyFeedRunType(t *testing.T) {
	st, _, imp, src := eventImporter(t)
	before, err := st.GetSourceByIDContext(t.Context(), src.ID)
	require.NoError(t, err)
	applyFeed(t, imp, feedEvent("add", 10))
	var kind, status string
	require.NoError(t, st.DB().QueryRow(`SELECT sync_type,status FROM sync_runs WHERE source_id=?`, src.ID).Scan(&kind, &status))
	assert.Equal(t, WebhookSyncType, kind)
	assert.Equal(t, "completed", status)
	after, err := st.GetSourceByIDContext(t.Context(), src.ID)
	require.NoError(t, err)
	assert.Equal(t, before.SyncCursor, after.SyncCursor)
	assert.Equal(t, before.LastSyncAt, after.LastSyncAt)
}
func TestPollingAfterWebhookKeepsHiddenAndAddsFlag(t *testing.T) {
	st, f, imp, src := setupImport(t)
	applyFeed(t, NewEventImporter(st, imp.api.(*Client)), feedEvent("hide", 20))
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "polled body")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	metadata := feedMetadata(t, st, src.ID)
	assert.True(t, metadata.Hidden)
	assert.True(t, metadata.Organic)
	assert.Equal(t, int64(20), metadata.StateTime)
	assert.Equal(t, "polled body", feedBody(t, st, src.ID))
}

// A canceled read must retain cancellation, while the item remains retryable.
func TestApplyFeedCanceled(t *testing.T) {
	_, _, imp, _ := eventImporter(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := imp.ApplyFeedEvents(ctx, "9", "100", []FeedEvent{feedEvent("add", 10)}, EventOptions{})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Len(t, out, 1)
}

func TestApplyFeedArchiveWriteFailureRetries(t *testing.T) {
	st, _, imp, _ := eventImporter(t)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_feed_write BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'synthetic unavailable'); END`)
	require.NoError(t, err)
	out := applyFeed(t, imp, feedEvent("add", 10))[0]
	assert.Equal(t, "retry", out.Class)
	assert.True(t, out.CountAttempt)
}
