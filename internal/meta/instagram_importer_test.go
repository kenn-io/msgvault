package meta

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type instagramImportFixture struct {
	linkedID                                string
	media                                   []IGMedia
	comments, replies                       []IGComment
	calls, pageReads, linkReads, mediaReads atomic.Int32
}

func newInstagramImportFixture(t *testing.T) (*instagramImportFixture, *Client) {
	t.Helper()
	f := &instagramImportFixture{
		linkedID: "200",
		media:    []IGMedia{{ID: "300", Caption: "Synthetic caption", Timestamp: "2000-01-02T03:04:05+0000", Permalink: "https://www.instagram.com/p/synthetic/", Owner: &IGUser{ID: "200"}}},
		comments: []IGComment{{ID: "400", Text: "searchableorchard", Timestamp: "2000-01-02T04:00:00+0000", From: &IGCommentFromUser{ID: "200", Username: "example_business", SelfIGScopedID: "200"}}, {ID: "401", Text: "Unknown author", From: &IGCommentFromUser{Username: "example_business"}}},
		replies:  []IGComment{{ID: "500", Text: "Synthetic reply", ParentID: "400", From: &IGCommentFromUser{ID: "200", Username: "example_business"}}},
	}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		items := []any{}
		switch r.URL.Path {
		case "/v26.0/me":
			f.pageReads.Add(1)
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
			return
		case "/v26.0/100":
			f.linkReads.Add(1)
			_, _ = fmt.Fprintf(w, `{"id":"100","instagram_business_account":{"id":%q,"username":"example_business"}}`, f.linkedID)
			return
		case "/v26.0/200/media":
			f.mediaReads.Add(1)
			for _, media := range f.media {
				items = append(items, media)
			}
		case "/v26.0/300/comments":
			for _, comment := range f.comments {
				items = append(items, comment)
			}
		case "/v26.0/400/replies":
			for _, reply := range f.replies {
				items = append(items, reply)
			}
		case "/v26.0/401/replies", "/v26.0/402/replies", "/v26.0/500/replies", "/v26.0/501/replies", "/v26.0/502/replies":
		default:
			assert.Fail(t, "unexpected Instagram fixture request", r.URL.Path)
			http.Error(w, "unexpected fixture route", http.StatusNotFound)
			return
		}
		after := r.URL.Query().Get("after")
		start, err := strconv.Atoi(after)
		if !assert.True(t, (after == "" || err == nil) && start >= 0 && start <= len(items), "invalid fixture cursor") {
			http.Error(w, "invalid fixture cursor", http.StatusBadRequest)
			return
		}
		end := min(start+2, len(items))
		wire := map[string]any{"data": items[start:end]}
		if end < len(items) {
			wire["paging"] = map[string]any{"cursors": map[string]string{"after": strconv.Itoa(end)}, "next": "https://example.test/unused"}
		}
		body, err := json.Marshal(wire)
		if !assert.NoError(t, err) {
			http.Error(w, "cannot encode fixture", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	return f, c
}

func TestInstagramImportVerificationBudgetAndFences(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, c := newInstagramImportFixture(t)
	imp := NewImporter(st, c)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	assert.Equal("instagram_comments", src.SourceType)
	assert.Equal("app:9:page:100:instagram:200", src.Identifier)
	for _, limit := range []int{1, 2} {
		before, pages, links := f.calls.Load(), f.pageReads.Load(), f.linkReads.Load()
		sum, err := imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: limit})
		require.NoError(err)
		require.NotNil(sum)
		assert.True(sum.Partial)
		assert.Equal(limit, sum.Requests)
		assert.Equal(int32(limit), f.calls.Load()-before)
		assert.Equal(int32(1), f.pageReads.Load()-pages)
		assert.Equal(int32(limit-1), f.linkReads.Load()-links)
		assert.Zero(f.mediaReads.Load(), "both verification reads consume budget before collection")
		assert.Zero(sum.MessagesAdded)
	}
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Zero(count)
	f.linkedID = "201"
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 3})
	require.Error(err)
	assert.Zero(f.mediaReads.Load(), "changed Page linkage cannot retarget the registered source")
	f.linkedID = "200"
	execution, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(execution.Release()) })
	before := f.calls.Load()
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.ErrorIs(err, store.ErrSyncAlreadyActive)
	assert.Equal(before, f.calls.Load(), "execution fence precedes provider reads")
	require.NoError(execution.Release())
	wrongPlatform, err := json.Marshal(freshState(src.Identifier))
	require.NoError(err)
	run, err := st.StartSync(src.ID, "instagram_comments")
	require.NoError(err)
	require.NoError(st.FailSyncWithCheckpoint(run, "synthetic incompatible checkpoint", &store.Checkpoint{PageToken: string(wrongPlatform)}))
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.ErrorContains(err, "--full")
	assert.Equal(before, f.calls.Load(), "Facebook phases cannot be resumed as an Instagram checkpoint")
	other, err := st.GetOrCreateSource("instagram_comments", "app:99:page:100:instagram:200")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	require.NoError(err)
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.ErrorIs(err, store.ErrSourceRetired)
	assert.Equal(before, f.calls.Load(), "retired source refuses before provider reads")
}

func TestInstagramImportMediaRepliesAndLateComments(t *testing.T) {
	for _, owner := range []string{"owned", "missing", "foreign"} {
		t.Run(owner, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, c := newInstagramImportFixture(t)
			switch owner {
			case "missing":
				f.media[0].Owner, f.media[0].Caption = nil, ""
			case "foreign":
				f.media[0].Owner.ID = "201"
			}
			imp := NewImporter(st, c)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			completed, paused := false, false
			for range 10 {
				before := f.calls.Load()
				sum, err := imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{MaxRequests: 5})
				require.NotNil(sum)
				assert.Equal(int(f.calls.Load()-before), sum.Requests)
				assert.LessOrEqual(sum.Requests, 5)
				if owner == "foreign" {
					require.Error(err)
					assert.True(sum.Partial)
					var count int
					require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
					assert.Zero(count)
					return
				}
				require.NoError(err)
				if !sum.Partial {
					completed = true
					break
				}
				paused = true
				checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
				require.NoError(err)
				assert.Contains(checkpoint.CursorBefore.String, src.Identifier)
				assert.NotContains(checkpoint.CursorBefore.String, "synthetic-page-secret")
			}
			require.True(completed)
			assert.True(paused, "bounded traversal must resume its durable checkpoint")
			var cacheBoundary int64
			require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&cacheBoundary))
			beforeReplay := f.calls.Load()
			replayed, err := imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			require.NoError(err)
			assert.Zero(replayed.MessagesAdded)
			assert.Equal(4, replayed.MessagesProcessed)
			assert.Greater(f.calls.Load(), beforeReplay, "the quiet replay must actually read every provider surface")
			var cacheChanges int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM cache_related_change_journal WHERE seq > ? AND dataset = 'message_facts'`, cacheBoundary).Scan(&cacheChanges))
			assert.Zero(cacheChanges, "unchanged Instagram media/comments/replies must not invalidate analytics")
			root, child, reply := message(t, st, src.ID, "300"), message(t, st, src.ID, "400"), message(t, st, src.ID, "500")
			assert.Equal("instagram_comments", child.MessageType)
			assert.Equal(root.ID, child.ReplyToMessageID.Int64)
			assert.Equal(child.ID, reply.ReplyToMessageID.Int64)
			assert.Equal(root.ConversationID, reply.ConversationID)
			assert.Equal(owner == "owned", root.IsFromMe)
			assert.False(child.IsFromMe, "equal IGUser/from/self_ig_scoped_id digits do not establish self")
			assert.False(reply.IsFromMe)
			require.True(child.SenderID.Valid)
			assert.Equal(child.SenderID, reply.SenderID)
			if owner == "owned" {
				assert.NotEqual(root.SenderID.Int64, child.SenderID.Int64, "IGUser and IGCommentFromUser remain distinct families")
			} else {
				assert.False(root.SenderID.Valid)
			}
			assert.False(message(t, st, src.ID, "401").SenderID.Valid)
			stored, err := st.StoredMessagesContext(t.Context(), src.ID, "instagram-comment-json", []string{"300", "400", "401", "500"})
			require.NoError(err)
			require.Len(stored, 4)
			for _, row := range stored {
				assert.NotEmpty(row.Raw, "all rows retain the Instagram raw format")
			}
			assert.True(stored["300"].SentAt.Valid)
			assert.Equal(2000, stored["300"].SentAt.Time.Year())
			body, err := st.GetMessageBodyText(child.ID)
			require.NoError(err)
			assert.Equal("searchableorchard", body)
			var title string
			require.NoError(st.DB().QueryRow(`SELECT title FROM conversations WHERE id=?`, root.ConversationID).Scan(&title))
			wantTitle := "Synthetic caption"
			if owner == "missing" {
				wantTitle = "Instagram media"
			}
			assert.Equal(wantTitle, title)
			var media IGMedia
			require.NoError(json.Unmarshal(stored["300"].Raw, &media))
			assert.Equal(f.media[0].Permalink, media.Permalink)
			saved, err := st.GetMessageMetadata(reply.ID)
			require.NoError(err)
			var metadata map[string]jsontext.Value
			require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
			for key, want := range map[string]string{"version": "1", "app_id": `"9"`, "page_id": `"100"`, "instagram_account_id": `"200"`, "media_id": `"300"`, "parent_id": `"400"`, "organic": "true"} {
				assert.JSONEq(want, string(metadata[key]), key)
			}
			var hits int
			require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'searchableorchard'`).Scan(&hits))
			assert.Equal(1, hits)
			reads := f.mediaReads.Load()
			knownUsername := f.comments[0].From.Username
			f.media[0].Owner, f.comments[0].From = nil, nil
			f.comments = append(f.comments, IGComment{ID: "402", Text: "Late comment on old media"})
			sum, err := imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(1, sum.MessagesAdded)
			assert.Greater(f.mediaReads.Load(), reads)
			assert.Equal(child.ID, message(t, st, src.ID, "400").ID)
			assert.Equal(child.SenderID, message(t, st, src.ID, "400").SenderID)
			var displayName, indexedName string
			require.NoError(st.DB().QueryRow(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`, child.ID).Scan(&displayName))
			require.NoError(st.DB().QueryRow(`SELECT from_addr FROM messages_fts WHERE rowid=?`, child.ID).Scan(&indexedName))
			assert.Equal("example_business", displayName)
			assert.Equal("example_business", indexedName)
			f.comments[0].From = &IGCommentFromUser{ID: "200"}
			_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
			require.NoError(err)
			require.NoError(st.DB().QueryRow(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`, child.ID).Scan(&displayName))
			require.NoError(st.DB().QueryRow(`SELECT from_addr FROM messages_fts WHERE rowid=?`, child.ID).Scan(&indexedName))
			assert.Equal("example_business", displayName, "same author without username retains saved evidence")
			assert.Equal("example_business", indexedName)
			assert.Equal(root.IsFromMe, message(t, st, src.ID, "300").IsFromMe)
			assert.Equal(root.ID, message(t, st, src.ID, "402").ReplyToMessageID.Int64)
			var retainedName string
			require.NoError(st.DB().QueryRow(`SELECT display_name FROM message_recipients WHERE message_id = ? AND recipient_type = 'from'`, child.ID).Scan(&retainedName))
			assert.Equal(knownUsername, retainedName, "missing author projection must preserve its identifier-only snapshot")
		})
	}
}
