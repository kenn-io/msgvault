package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func fixtureEventClient(t *testing.T, imp *Importer) *Client {
	t.Helper()
	client, ok := imp.api.(*Client)
	require.True(t, ok, "fixture importer must use the Graph client")
	return client
}

func feedEvent(verb string, stamp int64) FeedEvent {
	return FeedEvent{Time: stamp, Item: "comment", Verb: verb, PostID: "100_200", CommentID: "100_201", ParentID: "100_200", Message: "webhook example", CreatedTime: "2026-10-01T12:00:00+0000", From: &Actor{ID: "300", Name: "Example Customer"}}
}
func eventImporter(t *testing.T) (*store.Store, *graphFixture, *EventImporter, *store.Source) {
	t.Helper()
	st, f, imp, src := setupImport(t)
	return st, f, NewEventImporter(st, fixtureEventClient(t, imp)), src
}
func fixtureEventImporter(t *testing.T) *EventImporter {
	t.Helper()
	st, _, imp, _ := setupImport(t)
	return NewEventImporter(st, fixtureEventClient(t, imp))
}

func applyFeed(t *testing.T, imp *EventImporter, ev ...FeedEvent) []EventOutcome {
	t.Helper()
	return applyEvents(t, imp, EventOptions{}, ev...)
}
func applyEvents(t *testing.T, imp *EventImporter, opts EventOptions, ev ...FeedEvent) []EventOutcome {
	t.Helper()
	out, err := imp.ApplyFeedEvents(t.Context(), "9", "100", ev, opts)
	require.NoError(t, err)
	require.Len(t, out, len(ev))
	return out
}
func feedMetadata(t *testing.T, st *store.Store, src int64) objectMetadata {
	t.Helper()
	require := require.New(t)
	m := message(t, st, src, "100_201")
	saved, err := st.GetMessageMetadataContext(t.Context(), m.ID)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
	return metadata
}
func feedBody(t *testing.T, st *store.Store, src int64) string {
	t.Helper()
	require := require.New(t)
	body, err := st.GetMessageBodyText(message(t, st, src, "100_201").ID)
	require.NoError(err)
	return body
}
func tombstoned(t *testing.T, st *store.Store, src int64, native string) bool {
	t.Helper()
	require := require.New(t)
	var deleted sql.NullString
	require.NoError(st.DB().QueryRow(`SELECT deleted_from_source_at FROM messages WHERE source_id=? AND source_message_id=?`, src, native).Scan(&deleted))
	return deleted.Valid
}

func TestApplyFeedAddTopLevel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp, src := setupImport(t)
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	out := applyFeed(t, NewEventImporter(st, fixtureEventClient(t, imp)), feedEvent("add", 10))
	assert.Equal("applied", out[0].Class)
	got := message(t, st, src.ID, "100_201")
	root := message(t, st, src.ID, "100_200")
	assert.Equal(root.ConversationID, got.ConversationID)
	assert.Equal(root.ID, got.ReplyToMessageID.Int64)

	other, pf, poll, ps := setupImport(t)
	pf.comments["100_200"] = []Object{*comment("100_201", "100_200", "webhook example")}
	_, err = poll.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	want := message(t, other, ps.ID, "100_201")
	assert.Equal(want.ConversationID, got.ConversationID)
	assert.Equal(want.ReplyToMessageID, got.ReplyToMessageID)
	assert.Equal(want.SenderID, got.SenderID)
	assert.Equal(feedBody(t, other, ps.ID), feedBody(t, st, src.ID))
}

func TestApplyFeedFetchesMissingRoot(t *testing.T) {
	assert := assert.New(t)
	st, f, imp, src := eventImporter(t)
	f.paths = nil
	assert.Equal("applied", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	assert.Equal([]string{"/v26.0/100_200"}, f.paths)
	assert.Equal(message(t, st, src.ID, "100_200").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
}

func TestApplyFeedFetchesParentChain(t *testing.T) {
	assert := assert.New(t)
	st, f, imp, src := eventImporter(t)
	f.comments["100_200"] = []Object{*comment("100_210", "100_200", "ancestor one"), *comment("100_211", "100_210", "ancestor two")}
	ev := feedEvent("add", 10)
	ev.ParentID = "100_211"
	assert.Equal("applied", applyFeed(t, imp, ev)[0].Class)
	assert.Equal(message(t, st, src.ID, "100_211").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
	assert.Contains(f.paths, "/v26.0/100_210")
}

func TestApplyFeedDepthCapRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	assert.Equal("retry", out[0].Class)
	assert.True(out[0].CountAttempt)
	ids, err := st.MessageExistsBatch(src.ID, []string{ev.CommentID})
	require.NoError(err)
	assert.Empty(ids)
}

func TestApplyFeedBudgetRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, f, imp, _ := eventImporter(t)
	f.comments["100_200"] = []Object{*comment("100_210", "100_200", "ancestor")}
	ev := feedEvent("add", 10)
	ev.ParentID = "100_210"
	before := f.calls
	out, err := imp.ApplyFeedEvents(t.Context(), "9", "100", []FeedEvent{ev}, EventOptions{MaxRequests: 1})
	require.NoError(err)
	assert.Equal("retry", out[0].Class)
	assert.Equal(1, f.calls-before)
	assert.False(out[0].CountAttempt)
}

func TestApplyFeedEditedUpdatesBody(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	ev := feedEvent("edited", 20)
	ev.Message = "new body"
	assert.Equal("applied", applyFeed(t, imp, ev)[0].Class)
	assert.Equal("new body", feedBody(t, st, src.ID))
	assert.Equal(int64(20), feedMetadata(t, st, src.ID).BodyTime)
}
func TestApplyFeedStaleEditIgnored(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("edited", 20))
	ev := feedEvent("edit", 10)
	ev.Message = "stale body"
	assert.Equal("ignored", applyFeed(t, imp, ev)[0].Class)
	assert.Equal("webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedAddAfterEditKeepsEditedBody(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("edited", 20)
	ev.Message = "edited body"
	applyFeed(t, imp, ev)
	assert.Equal("ignored", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	assert.Equal("edited body", feedBody(t, st, src.ID))
}
func TestApplyFeedHideUnhide(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10), feedEvent("hide", 20))
	assert.Equal(new(true), feedMetadata(t, st, src.ID).Hidden)
	applyFeed(t, imp, feedEvent("unhide", 20))
	assert.Equal(new(false), feedMetadata(t, st, src.ID).Hidden)
}
func TestApplyFeedStaleHideIgnored(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("hide", 20), feedEvent("unhide", 30))
	assert.Equal("ignored", applyFeed(t, imp, feedEvent("hide", 25))[0].Class)
	assert.Equal(new(false), feedMetadata(t, st, src.ID).Hidden)
}
func TestApplyFeedHideUnknownPersistsThenHides(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("hide", 20)
	ev.Message = ""
	applyFeed(t, imp, ev, feedEvent("add", 10))
	metadata := feedMetadata(t, st, src.ID)
	assert.Equal(new(true), metadata.Hidden)
	assert.Equal(int64(20), metadata.StateTime)
	assert.Equal(int64(10), metadata.BodyTime)
	assert.Equal("webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedRemoveTombstonesKeepsRow(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	id := message(t, st, src.ID, "100_201").ID
	applyFeed(t, imp, feedEvent("remove", 20))
	assert.Equal(id, message(t, st, src.ID, "100_201").ID)
	assert.True(tombstoned(t, st, src.ID, "100_201"))
	assert.Equal("webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedRemoveUnknownPersistsTombstone(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("delete", 20)
	ev.Message = ""
	applyFeed(t, imp, ev, feedEvent("add", 10))
	assert.True(tombstoned(t, st, src.ID, "100_201"))
	assert.Equal("webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedAddAfterRemoveKeepsTombstone(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10), feedEvent("remove", 20), feedEvent("add", 10))
	assert.True(tombstoned(t, st, src.ID, "100_201"))
}
func TestApplyFeedStateVerbAncestorGoneIgnored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, imp, src := eventImporter(t)
	f.posts = nil
	out := applyFeed(t, imp, feedEvent("remove", 20))[0]
	assert.Equal("ignored", out.Class)
	assert.Equal("ancestor_unavailable", out.ErrorClass)
	ids, err := st.MessageExistsBatch(src.ID, []string{"100_201"})
	require.NoError(err)
	assert.Empty(ids)
}
func TestApplyFeedPostRemoveTombstonesRoot(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	ev := feedEvent("remove", 20)
	ev.Item = "post"
	applyFeed(t, imp, ev)
	assert.True(tombstoned(t, st, src.ID, "100_200"))
	assert.False(tombstoned(t, st, src.ID, "100_201"))
}
func TestApplyFeedIgnoredOnlyStartsNoRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp, _ := eventImporter(t)
	ev := feedEvent("edit", 20)
	ev.Item = "post"
	assert.Equal("ignored", applyFeed(t, imp, ev)[0].Class)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM sync_runs`).Scan(&count))
	assert.Zero(count)
}
func TestApplyFeedNonCommentIgnored(t *testing.T) {
	assert := assert.New(t)
	imp := fixtureEventImporter(t)
	for _, item := range []string{"reaction", "status", "comment"} {
		ev := feedEvent("follow", 20)
		ev.Item = item
		assert.Equal("ignored", applyFeed(t, imp, ev)[0].Class)
	}
}
func TestApplyFeedPageAuthorIsFromMe(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	ev := feedEvent("add", 10)
	ev.From = &Actor{ID: "100", Name: "Example Page"}
	applyFeed(t, imp, ev)
	assert.True(message(t, st, src.ID, "100_201").IsFromMe)
}
func TestApplyFeedCreatedTimeForms(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, value := range []string{`1790856000`, `"2026-10-01T12:00:00+0000"`, `"2026-10-01T12:00:00Z"`} {
		var ev FeedEvent
		require.NoError(json.Unmarshal([]byte(`{"item":"comment","verb":"add","post_id":"100_200","comment_id":"100_201","created_time":`+value+`}`), &ev))
		st, _, imp, src := eventImporter(t)
		assert.Equal("applied", applyFeed(t, imp, ev)[0].Class)
		var sent time.Time
		require.NoError(st.DB().QueryRow(`SELECT sent_at FROM messages WHERE source_id=? AND source_message_id='100_201'`, src.ID).Scan(&sent))
		assert.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), sent.UTC())
	}
}
func TestApplyFeedConflictingParentInvalid(t *testing.T) {
	assert := assert.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	ev := feedEvent("edited", 20)
	ev.ParentID = "100_210"
	ev.Message = "conflicting body"
	assert.Equal("invalid", applyFeed(t, imp, ev)[0].Class)
	assert.Equal("webhook example", feedBody(t, st, src.ID))
}
func TestApplyFeedPermissionRetryClass(t *testing.T) {
	assert := assert.New(t)
	_, f, imp, _ := eventImporter(t)
	f.denied["100_200"] = true
	out := applyFeed(t, imp, feedEvent("add", 10))[0]
	assert.Equal("retry", out.Class)
	assert.Equal("permission_missing", out.ErrorClass)
}
func TestApplyFeedLockBusyRetryUncounted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, imp, src := eventImporter(t)
	exec, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	defer func() {
		require.NoError(exec.Release())
	}()
	calls := f.calls
	out := applyFeed(t, imp, feedEvent("add", 10))[0]
	assert.Equal("retry", out.Class)
	assert.False(out.CountAttempt)
	assert.Equal(calls, f.calls)
}
func TestApplyFeedRetiredSourceUnrouted(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	f.paths = nil
	out, err := imp.ApplyFeedEvents(t.Context(), "99", "100", []FeedEvent{feedEvent("add", 10)}, EventOptions{})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "unrouted", out[0].Class)
	assert.Equal(t, "source_missing", out[0].ErrorClass)
	assert.Empty(t, f.paths)
	other, err := st.GetOrCreateSource(SourceType, "app:99:page:100")
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	require.NoError(t, err)
	before := f.calls
	assert.Equal(t, "unrouted", applyFeed(t, imp, feedEvent("add", 10))[0].Class)
	assert.Equal(t, before, f.calls)
}
func TestApplyFeedRunType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp, src := eventImporter(t)
	before, err := st.GetSourceByIDContext(t.Context(), src.ID)
	require.NoError(err)
	applyFeed(t, imp, feedEvent("add", 10))
	var kind, status string
	require.NoError(st.DB().QueryRow(`SELECT sync_type,status FROM sync_runs WHERE source_id=?`, src.ID).Scan(&kind, &status))
	assert.Equal(WebhookSyncType, kind)
	assert.Equal("completed", status)
	after, err := st.GetSourceByIDContext(t.Context(), src.ID)
	require.NoError(err)
	assert.Equal(before.SyncCursor, after.SyncCursor)
	assert.Equal(before.LastSyncAt, after.LastSyncAt)
}
func TestPollingAfterWebhookKeepsHiddenAndAddsFlag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	applyFeed(t, NewEventImporter(st, fixtureEventClient(t, imp)), feedEvent("hide", 20))
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "polled body")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	metadata := feedMetadata(t, st, src.ID)
	assert.Equal(new(true), metadata.Hidden)
	assert.True(metadata.Organic)
	assert.Equal(int64(20), metadata.StateTime)
	assert.Equal("polled body", feedBody(t, st, src.ID))
}

// A canceled read must retain cancellation, while the item remains retryable.
func TestApplyFeedCanceled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	imp := fixtureEventImporter(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := imp.ApplyFeedEvents(ctx, "9", "100", []FeedEvent{feedEvent("add", 10)}, EventOptions{})
	require.ErrorIs(err, context.Canceled)
	assert.Len(out, 1)
}

func TestApplyFeedArchiveWriteFailureRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp, _ := eventImporter(t)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_feed_write BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'synthetic unavailable'); END`)
	require.NoError(err)
	out := applyFeed(t, imp, feedEvent("add", 10))[0]
	assert.Equal("retry", out.Class)
	assert.True(out.CountAttempt)
}

func TestPollingVisibilityRemainsUnknownUntilWebhookEvidence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, fixture, imp, src := setupImport(t)
	fixture.comments["100_200"] = []Object{*comment("100_201", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	metadata := func() map[string]any {
		saved, err := st.GetMessageMetadataContext(t.Context(), message(t, st, src.ID, "100_201").ID)
		require.NoError(err)
		var fields map[string]any
		require.NoError(json.Unmarshal([]byte(saved.String), &fields))
		return fields
	}
	assert.NotContains(metadata(), "hidden", "polling does not observe moderation visibility")
	webhook := NewEventImporter(st, fixtureEventClient(t, imp))
	applyFeed(t, webhook, feedEvent("hide", 20))
	assert.Equal(true, metadata()["hidden"])
	applyFeed(t, webhook, feedEvent("unhide", 30))
	assert.Equal(false, metadata()["hidden"], "explicit unhide must survive JSON serialization")
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.Equal(false, metadata()["hidden"], "polling preserves observed visibility")
}

func TestPollingCachedCommentDoesNotOverwriteNewerWebhookBody(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(fmt.Sprintf("ancestor=%t", ancestor), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, fixture, poll, src := setupImport(t)
			fixture.comments["100_200"] = []Object{*comment("100_201", "100_200", "first comment"), *comment("100_202", "100_200", "cached body")}
			summary, err := poll.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
			require.NoError(err)
			require.True(summary.Partial)
			webhook := NewEventImporter(st, fixtureEventClient(t, poll))
			event := feedEvent("edited", 20)
			event.CommentID, event.Message = "100_202", "newer webhook body"
			if ancestor {
				fixture.comments["100_200"][1].Message = "newer webhook body"
				event.CommentID, event.ParentID = "100_203", "100_202"
			}
			outcomes, err := webhook.ApplyFeedEvents(t.Context(), "9", "100", []FeedEvent{event}, EventOptions{})
			require.NoError(err)
			require.Equal("applied", outcomes[0].Class)
			_, err = poll.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			row := message(t, st, src.ID, "100_202")
			body, err := st.GetMessageBodyText(row.ID)
			require.NoError(err)
			assert.Equal("newer webhook body", body, "resuming a cached response must preserve a later webhook write")
			fixture.comments["100_200"][1].Message = "fresh polling body"
			_, err = poll.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			body, err = st.GetMessageBodyText(row.ID)
			require.NoError(err)
			assert.Equal("fresh polling body", body, "fresh provider reads must still reconcile the body")
		})
	}
}

func TestPollingCachedCommentFillsMissingWebhookEvidence(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, fixture, poll, src := setupImport(t)
			cached := comment("100_202", "100_200", "old cached body")
			fixture.comments["100_200"] = []Object{*comment("100_201", "100_200", "first comment"), *cached}
			summary, err := poll.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 3})
			require.NoError(err)
			require.True(summary.Partial)
			event := feedEvent("edited", 20)
			event.CommentID, event.Message = cached.ID, "newer webhook body"
			event.From, event.CreatedTime = &Actor{ID: "301", Name: "Newer Example"}, "2026-10-02T12:00:00+0000"
			wantName, wantTime := event.From.Name, event.CreatedTime
			if incomplete {
				event.From, event.CreatedTime = nil, ""
				wantName, wantTime = cached.From.Name, cached.CreatedTime
			}
			webhook := NewEventImporter(st, fixtureEventClient(t, poll))
			outcomes, err := webhook.ApplyFeedEvents(t.Context(), "9", "100", []FeedEvent{event}, EventOptions{})
			require.NoError(err)
			require.Equal("applied", outcomes[0].Class)
			_, err = poll.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			row := message(t, st, src.ID, cached.ID)
			got, err := st.GetMessageContext(t.Context(), row.ID)
			require.NoError(err)
			assert.True(row.SenderID.Valid, "cached evidence fills a missing sender")
			assert.Equal(wantName, got.FromName, "cached evidence never replaces a known newer sender")
			stamp, err := time.Parse("2006-01-02T15:04:05-0700", wantTime)
			require.NoError(err)
			assert.False(got.SentAt.IsZero())
			assert.Equal(stamp.UTC(), got.SentAt.UTC())
			body, err := st.GetMessageBodyText(row.ID)
			require.NoError(err)
			assert.Equal("newer webhook body", body)
			var matches int
			require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '"newer webhook body"'`).Scan(&matches))
			assert.Equal(1, matches)
			metadata, err := st.GetMessageMetadataContext(t.Context(), row.ID)
			require.NoError(err)
			var saved objectMetadata
			require.NoError(json.Unmarshal([]byte(metadata.String), &saved))
			assert.Equal(int64(20), saved.BodyTime)
		})
	}
}

func TestApplyFeedOlderAddFillsMissingAuthorAndCreationTime(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, _, imp, src := eventImporter(t)
	edit := feedEvent("edited", 20)
	edit.From, edit.CreatedTime, edit.Message = nil, "", "newer body"
	applyFeed(t, imp, edit)
	applyFeed(t, imp, feedEvent("hide", 30))
	initial, err := st.StoredMessagesContext(t.Context(), src.ID, rawFormat, []string{edit.CommentID})
	require.NoError(err)
	require.False(initial[edit.CommentID].SenderID.Valid)
	require.False(initial[edit.CommentID].SentAt.Valid)
	outcomes := applyFeed(t, imp, feedEvent("add", 10))
	rows, err := st.StoredMessagesContext(t.Context(), src.ID, rawFormat, []string{edit.CommentID})
	require.NoError(err)
	got := rows[edit.CommentID]
	assert.True(got.SenderID.Valid, "an older add still supplies missing author evidence")
	assert.True(got.SentAt.Valid, "an older add still supplies missing creation time")
	assert.Equal("newer body", feedBody(t, st, src.ID))
	assert.True(outcomes[0].ArchiveChanged)
	metadata := feedMetadata(t, st, src.ID)
	assert.Equal(int64(20), metadata.BodyTime)
	assert.Equal(int64(30), metadata.StateTime)
	assert.Equal(new(true), metadata.Hidden)
}

func TestApplyFeedUnchangedBodyKeepsReceiptOrdering(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, _, imp, src := eventImporter(t)
	event := feedEvent("edit", 30)
	event.ReceiptOrder = 100
	applyFeed(t, imp, event)
	// A second identical provider snapshot still advances the receipt fence.
	event.ReceiptOrder = 300
	outcomes := applyFeed(t, imp, event)
	assert.False(outcomes[0].ArchiveChanged)
	metadata := feedMetadata(t, st, src.ID)
	assert.Equal(uint64(300), metadata.BodyOrder)
	event.ReceiptOrder, event.Message = 200, "older delayed body"
	outcomes = applyFeed(t, imp, event)
	assert.Equal("ignored", outcomes[0].Class)
	assert.Equal("webhook example", feedBody(t, st, src.ID))
	latest, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Zero(latest.MessagesUpdated, "ordering-only metadata does not change the analytics snapshot")
}

func TestApplyFeedEqualTimeStateRetriesKeepReceiptOrder(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, _, imp, src := eventImporter(t)
	applyFeed(t, imp, feedEvent("add", 10))
	newer := feedEvent("hide", 30)
	newer.ReceiptOrder = 200
	applyFeed(t, imp, newer)
	older := feedEvent("unhide", 30)
	older.ReceiptOrder = 100
	outcomes := applyFeed(t, imp, older)
	assert.Equal("ignored", outcomes[0].Class)
	assert.False(outcomes[0].ArchiveChanged)
	assert.Equal(new(true), feedMetadata(t, st, src.ID).Hidden)
	// Explicit replay receives a fresh order and may reapply equal-time state.
	older.ReceiptOrder = 300
	outcomes = applyFeed(t, imp, older)
	require.Equal("applied", outcomes[0].Class)
	assert.Equal(new(false), feedMetadata(t, st, src.ID).Hidden)
}

func TestPollingSubsetIgnoresForeignWebhookReadOrder(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(strconv.FormatBool(full), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			sourcePath := filepath.Join(t.TempDir(), "source.db")
			st := testutil.NewSQLiteTestStoreAt(t, sourcePath)
			f, client := newGraphFixture(t)
			imp := NewImporter(st, client)
			src, err := imp.Register(t.Context(), "9", "100")
			require.NoError(err)
			for range 5 {
				_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
				require.NoError(err)
			}
			ev := feedEvent("add", 10)
			applyFeed(t, NewEventImporter(st, client), ev)
			assert.Greater(feedMetadata(t, st, src.ID).BodySyncID, int64(1))
			dest := filepath.Join(t.TempDir(), "subset")
			_, err = store.CopySubset(sourcePath, dest, 100, false)
			require.NoError(err)
			copied, err := store.Open(filepath.Join(dest, "msgvault.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(copied.Close()) })
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "Edited after subset")}
			_, err = NewImporter(copied, client).Import(t.Context(), "9", "100", ImportOptions{Full: full})
			require.NoError(err)
			body, err := copied.GetMessageBodyText(message(t, copied, src.ID, "100_201").ID)
			require.NoError(err)
			assert.Equal("Edited after subset", body)
		})
	}
}

func TestApplyFeedTopLevelAncestorWithoutParent(t *testing.T) {
	st, f, imp, src := eventImporter(t)
	ancestor := *comment("100_210", "100_200", "Top-level ancestor")
	ancestor.Parent = nil
	f.comments["100_200"] = []Object{ancestor}
	ev := feedEvent("add", 10)
	ev.ParentID = "100_210"
	out := applyFeed(t, imp, ev)[0]
	require.Equal(t, "applied", out.Class, "%s", out.ErrorClass)
	assert.Equal(t, message(t, st, src.ID, "100_210").ID, message(t, st, src.ID, "100_201").ReplyToMessageID.Int64)
	assert.Equal(t, message(t, st, src.ID, "100_200").ID, message(t, st, src.ID, "100_210").ReplyToMessageID.Int64)
}
