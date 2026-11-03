package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// HTTP fixture exercises the actual wire decoder, importer and archive. All
// identities and messages are synthetic; no native provider configuration.
type graphFixture struct {
	posts, ads, visitors []Object
	comments             map[string][]Object
	denied               map[string]bool
	calls                int
	rejectCursor         string
	freshCommentReads    int
	paths                []string
	beforeRequest        func(string)
}

func newGraphFixture(t *testing.T) (*graphFixture, *Client) {
	t.Helper()
	f := &graphFixture{posts: []Object{{ID: "100_200", Message: "Example post", From: &Actor{ID: "100", Name: "Example Page"}}}, comments: map[string][]Object{}, denied: map[string]bool{}}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.paths = append(f.paths, r.URL.Path)
		if f.rejectCursor != "" && r.URL.Query().Get("after") != "" {
			http.Error(w, f.rejectCursor, http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v26.0/100_200/comments" && r.URL.Query().Get("after") == "" {
			f.freshCommentReads++
		}
		path := strings.TrimPrefix(r.URL.Path, "/v26.0/")
		if f.beforeRequest != nil {
			f.beforeRequest(path)
		}
		if f.denied[path] {
			http.Error(w, "synthetic denial", http.StatusForbidden)
			return
		}
		if path == "me" {
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
			return
		}
		var data []Object
		switch path {
		case "100/posts":
			data = f.posts
		case "100/ads_posts":
			data = f.ads
		case "100/visitor_posts":
			data = f.visitors
		default:
			data = f.comments[strings.TrimSuffix(path, "/comments")]
		}
		after := r.URL.Query().Get("after")
		start, parseErr := strconv.Atoi(after)
		if !assert.True(t, (after == "" || parseErr == nil) && start >= 0, "invalid fixture cursor") {
			http.Error(w, "invalid fixture cursor", http.StatusBadRequest)
			return
		}
		end := min(start+2, len(data))
		if !assert.LessOrEqual(t, start, end) {
			http.Error(w, "fixture cursor exceeds response", http.StatusBadRequest)
			return
		}
		if data == nil {
			data = []Object{}
		}
		response := map[string]any{"data": data[start:end]}
		if end < len(data) {
			response["paging"] = map[string]any{"cursors": map[string]string{"after": strconv.Itoa(end)}, "next": "https://graph.facebook.com/unused"}
		}
		blob, err := json.Marshal(response)
		if !assert.NoError(t, err) {
			http.Error(w, "cannot encode fixture response", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(blob)
	})
	return f, c
}
func comment(id, parent, text string) *Object {
	return &Object{ID: id, Message: text, CreatedTime: "2026-10-01T12:00:00+0000", From: &Actor{ID: "300", Name: "Example Customer"}, Parent: &Actor{ID: parent}}
}
func setupImport(t *testing.T) (*store.Store, *graphFixture, *Importer, *store.Source) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	f, c := newGraphFixture(t)
	imp := NewImporter(st, c)
	src, err := imp.Register(t.Context(), "9", "100")
	require.NoError(t, err)
	return st, f, imp, src
}
func message(t *testing.T, st *store.Store, source int64, id string) *store.Message {
	t.Helper()
	ids, err := st.MessageExistsBatch(source, []string{id})
	require.NoError(t, err)
	require.NotZero(t, ids[id])
	var got store.Message
	require.NoError(t, st.DB().QueryRow(`SELECT id,source_id,source_message_id,conversation_id,message_type,sender_id,reply_to_message_id,is_from_me FROM messages WHERE id=?`, ids[id]).Scan(&got.ID, &got.SourceID, &got.SourceMessageID, &got.ConversationID, &got.MessageType, &got.SenderID, &got.ReplyToMessageID, &got.IsFromMe))
	return &got
}

func TestImportPageComments(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "searchable example")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	got := message(t, st, src.ID, "100_201")
	body, err := st.GetMessageBodyText(got.ID)
	require.NoError(err)
	assert.Equal("searchable example", body)
	assert.Equal(SourceType, got.MessageType)
	root := message(t, st, src.ID, "100_200")
	assert.Equal(root.ConversationID, got.ConversationID)
	assert.Equal(root.ID, got.ReplyToMessageID.Int64)
	assert.True(root.IsFromMe)
	var hits int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'`).Scan(&hits))
	assert.Equal(1, hits)
	f.comments["100_200"][0].Message = "edited example"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.Equal(got.ID, message(t, st, src.ID, "100_201").ID)
	body, err = st.GetMessageBodyText(got.ID)
	require.NoError(err)
	assert.Equal("edited example", body)
}

func TestImportCachedOrganicCommentKeepsNewerSameRunAdSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, fixture, imp, src := setupImport(t)
	fixture.ads = fixture.posts
	fixture.comments["100_200"] = []Object{
		*comment("100_201", "100_200", "first example"),
		*comment("100_202", "100_200", "old organic example"),
	}
	failed := false
	fixture.beforeRequest = func(path string) {
		if path != "100_201/comments" {
			return
		}
		if !failed {
			// Pause the cached organic traversal before its second comment.
			// The later ad response in this same run observes the edit.
			failed = true
			fixture.denied[path] = true
			fixture.comments["100_200"][1].Message = "new ad example"
			fixture.comments["100_200"][1].From = nil
			fixture.comments["100_200"][1].CreatedTime = ""
		} else {
			delete(fixture.denied, path)
		}
	}
	summary, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.ErrorContains(err, "comments for \"100_201\"")
	require.True(summary.Partial)
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	var state syncState
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
	require.True(state.Phases[AdsPosts].Done)
	require.Len(state.Phases[Posts].Stack[0].Pending, 1)
	row := message(t, st, src.ID, "100_202")
	body, err := st.GetMessageBodyText(row.ID)
	require.NoError(err)
	require.Equal("new ad example", body)
	stored, err := st.StoredMessagesContext(t.Context(), src.ID, rawFormat, []string{row.SourceMessageID})
	require.NoError(err)
	require.False(stored[row.SourceMessageID].SenderID.Valid)
	require.False(stored[row.SourceMessageID].SentAt.Valid)
	summary, err = imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 2})
	require.NoError(err)
	require.True(summary.Partial)
	body, err = st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("new ad example", body, "a cached response must not overwrite a later response from the same run")
	stored, err = st.StoredMessagesContext(t.Context(), src.ID, rawFormat, []string{row.SourceMessageID})
	require.NoError(err)
	assert.True(stored[row.SourceMessageID].SenderID.Valid, "cached responses still supply missing author evidence")
	assert.True(stored[row.SourceMessageID].SentAt.Valid, "cached responses still supply missing creation evidence")
	var matches int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '"new ad example"'`).Scan(&matches))
	assert.Equal(1, matches, "bounded resume must retain the newer searchable text")
	fixture.comments["100_200"][1].Message = "fresh reconciled example"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	body, err = st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("fresh reconciled example", body)
}

func TestImportCachedAdCommentKeepsNewerOrganicSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, fixture, imp, src := setupImport(t)
	fixture.ads = fixture.posts
	fixture.denied["100/visitor_posts"] = true
	fixture.comments["100_200"] = []Object{
		*comment("100_201", "100_200", "first example"),
		*comment("100_202", "100_200", "old cached example"),
	}
	// Organic completes, visitor access fails, and the ad response is cached
	// before the budget pauses its first child's reply traversal.
	summary, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 8})
	require.ErrorContains(err, "visitor_posts")
	require.True(summary.Partial)
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	var state syncState
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
	require.Len(state.Phases[AdsPosts].Stack, 2)
	require.Len(state.Phases[AdsPosts].Stack[0].Pending, 1)
	fixture.comments["100_200"][1].Message = "new organic example"
	summary, err = imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 6})
	require.ErrorContains(err, "visitor_posts")
	require.True(summary.Partial)
	row := message(t, st, src.ID, "100_202")
	body, err := st.GetMessageBodyText(row.ID)
	require.NoError(err)
	require.Equal("new organic example", body)
	delete(fixture.denied, "100/visitor_posts")
	summary, err = imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 2})
	require.NoError(err)
	require.True(summary.Partial)
	body, err = st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("new organic example", body, "older cached ad reads must preserve a later organic snapshot")
	fixture.comments["100_200"][1].Message = "fresh reconciled example"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	body, err = st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("fresh reconciled example", body, "fresh reads still reconcile the archived body")
}

func TestImportPollingCacheProgress(t *testing.T) {
	for _, anonymous := range []bool{false, true} {
		t.Run(fmt.Sprintf("anonymous=%t", anonymous), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, fixture, imp, src := setupImport(t)
			obj := comment("100_201", "100_200", "original example")
			if anonymous {
				obj.From, fixture.posts[0].From = nil, nil
			}
			fixture.comments["100_200"] = []Object{*obj}
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			var before, after int64
			require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&before))
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			require.NoError(st.DB().QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM cache_related_change_journal`).Scan(&after))
			assert.Equal(before, after, "unchanged polling must not invalidate analytics through recipient replacement")
			fixture.comments["100_200"][0].Message = "updated example"
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			latest, err := st.GetLatestSync(src.ID)
			require.NoError(err)
			assert.Equal(int64(1), latest.MessagesUpdated, "anonymous edits also need a durable cache rebuild signal")
			body, err := st.GetMessageBodyText(message(t, st, src.ID, "100_201").ID)
			require.NoError(err)
			assert.Equal("updated example", body)
		})
	}
}

func TestImportUnchangedPollingDoesNotEmitContentEdits(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, fixture, imp, src := setupImport(t)
	fixture.comments["100_200"] = []Object{*comment("100_201", "100_200", "original example")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	row := message(t, st, src.ID, "100_201")
	person, _, err := st.CreatePersonFromParticipant(row.SenderID.Int64)
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET content_changed_at='2000-01-01 00:00:00.000' WHERE id=?`, row.ID)
	require.NoError(err)
	var before, after string
	require.NoError(st.DB().QueryRow(`SELECT content_changed_at FROM messages WHERE id=?`, row.ID).Scan(&before))
	sequence, err := st.LatestPersonSweepChangeSequence(t.Context())
	require.NoError(err)
	for range 2 {
		_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
		require.NoError(err)
	}
	require.NoError(st.DB().QueryRow(`SELECT content_changed_at FROM messages WHERE id=?`, row.ID).Scan(&after))
	assert.Equal(before, after, "polling freshness is operational state, not a content edit")
	changes, err := st.ScanPersonSweepChanges(t.Context(), person.ID, sequence, 1000)
	require.NoError(err)
	assert.Empty(changes, "unchanged polling must not requeue a tracked person's evidence")
	sequence, err = st.LatestPersonSweepChangeSequence(t.Context())
	require.NoError(err)
	fixture.comments["100_200"][0].Message = "edited example"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	require.NoError(st.DB().QueryRow(`SELECT content_changed_at FROM messages WHERE id=?`, row.ID).Scan(&after))
	assert.NotEqual(before, after)
	changes, err = st.ScanPersonSweepChanges(t.Context(), person.ID, sequence, 1000)
	require.NoError(err)
	assert.NotEmpty(changes, "actual provider edits must still notify tracked people")
}

func TestImportResponseMarkerFailureKeepsPriorBody(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, fixture, imp, src := setupImport(t)
	fixture.comments["100_200"] = []Object{*comment("100_201", "100_200", "original example")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	row := message(t, st, src.ID, "100_201")
	_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER fail_poll_marker BEFORE UPDATE ON archive_metadata
		WHEN NEW.key='meta.poll_read:%d:100_201' BEGIN SELECT RAISE(ABORT, 'synthetic marker unavailable'); END`, src.ID))
	require.NoError(err)
	fixture.comments["100_200"][0].Message = "edited example"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.ErrorContains(err, "synthetic marker unavailable")
	body, err := st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("original example", body, "content must wait for its durable read ordering")
	_, err = st.DB().Exec(`DROP TRIGGER fail_poll_marker`)
	require.NoError(err)
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	body, err = st.GetMessageBodyText(row.ID)
	require.NoError(err)
	assert.Equal("edited example", body, "the cached response resumes after storage recovers")
}

func TestImportResumeAfterBudget(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	for i := 1; i <= 5; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(strconv.Itoa(400+i), "100_200", "budget"))
	}
	completed := false
	for range 15 {
		before := f.calls
		sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
		require.NoError(err)
		assert.LessOrEqual(f.calls-before, 4)
		assert.Equal(f.calls-before, sum.Requests)
		if !sum.Partial {
			completed = true
			break
		}
	}
	assert.True(completed)
	for i := 1; i <= 5; i++ {
		message(t, st, src.ID, strconv.Itoa(400+i))
	}
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(6, count)
}

func TestImportLateCommentOnOldPost(t *testing.T) {
	st, f, imp, src := setupImport(t)
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "late arrival")}
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	message(t, st, src.ID, "100_201")
}

func TestImportOrganicAdDedup(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.ads = f.posts
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "shared organic and ad")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(2, count)
	got := message(t, st, src.ID, "100_201")
	meta, err := st.GetMessageMetadata(got.ID)
	require.NoError(err)
	assert.Contains(meta.String, `"organic":true`)
	assert.Contains(meta.String, `"ad":true`)
}

func TestImportScopedIdentityAndPersonLink(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "example identity")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	first := message(t, st, src.ID, "100_201").SenderID.Int64
	secondSource, err := imp.Register(t.Context(), "99", "100")
	require.NoError(err)
	_, err = imp.Import(t.Context(), "99", "100", ImportOptions{})
	require.NoError(err)
	second := message(t, st, secondSource.ID, "100_201").SenderID.Int64
	assert.NotEqual(first, second)
	email, err := st.EnsureParticipant("example@example.com", "Example Customer", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(email)
	require.NoError(err)
	_, err = st.LinkParticipants(email, first)
	require.NoError(err)
	linked, err := st.PersonForParticipants([]int64{first})
	require.NoError(err)
	assert.Equal(person.ID, linked.ID)
	assert.Equal("facebook_comments:1:9:3:100:3:300", ActorIdentifier("9", "100", "300"))
	observations, err := st.FindObservationsByProviderUserIDContext(t.Context(), "facebook_comments:1:9:3:100:3:300", 10)
	require.NoError(err)
	require.NotEmpty(observations)
	assert.Equal(first, observations[0].ParticipantID)
}

func TestImportMissingAuthor(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	c := comment("100_201", "100_200", "anonymous")
	c.From = nil
	f.comments["100_200"] = []Object{*c}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(message(t, st, src.ID, c.ID).SenderID.Valid)
	c.From = &Actor{ID: "300", Name: "Example Customer"}
	f.comments["100_200"] = []Object{*c}
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	known := message(t, st, src.ID, c.ID).SenderID
	require.True(known.Valid)
	for _, author := range []*Actor{nil, {ID: "300"}} {
		c.From = author
		f.comments["100_200"] = []Object{*c}
		_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
		require.NoError(err)
		assert.Equal(known, message(t, st, src.ID, c.ID).SenderID)
		var displayName, indexedName string
		require.NoError(st.DB().QueryRow(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`, message(t, st, src.ID, c.ID).ID).Scan(&displayName))
		require.NoError(st.DB().QueryRow(`SELECT from_addr FROM messages_fts WHERE rowid=?`, message(t, st, src.ID, c.ID).ID).Scan(&indexedName))
		assert.Equal("Example Customer", displayName)
		assert.Equal("Example Customer", indexedName)
	}
}

type recordingCommentErrorAPI struct {
	API

	err error
}

func (a *recordingCommentErrorAPI) Comments(ctx context.Context, parent, after string) (ObjectPage, error) {
	page, err := a.API.Comments(ctx, parent, after)
	a.err = err
	return page, err
}

func TestImportFailureKeepsCheckpoint(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	provider := &recordingCommentErrorAPI{API: imp.api}
	imp.api = provider
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "retained on failure")}
	f.denied["100_201/comments"] = true
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.Error(err)
	assert.Contains(err.Error(), strconv.Quote("100_201"))
	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	require.ErrorIs(err, provider.err)
	assert.Equal(http.StatusForbidden, apiErr.Status)
	assert.NotContains(err.Error(), "synthetic-page-secret")
	assert.NotContains(err.Error(), "retained on failure")
	assert.True(sum.Partial)
	got := message(t, st, src.ID, "100_201")
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	assert.Contains(checkpoint.CursorBefore.String, "100_201")
	assert.NotContains(checkpoint.CursorBefore.String, "synthetic-page-secret")
	assert.Contains(checkpoint.ErrorMessage.String, strconv.Quote("100_201"))
	assert.NotContains(checkpoint.ErrorMessage.String, "synthetic-page-secret")
	assert.Equal(store.SyncStatusFailed, checkpoint.Status)
	delete(f.denied, "100_201/comments")
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(got.ID, message(t, st, src.ID, "100_201").ID)
}

func TestImportTraversalRefusalKeepsPending(t *testing.T) {
	for _, cause := range []string{"cyclic Facebook reply traversal", "bounded depth"} {
		t.Run(cause, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			blocked := "100_200"
			if cause == "bounded depth" {
				parent := blocked
				for i := 1; i <= 128; i++ {
					blocked = "100_" + strconv.Itoa(200+i)
					f.comments[parent] = []Object{*comment(blocked, parent, "synthetic blocked object")}
					parent = blocked
				}
			} else {
				f.comments[blocked] = []Object{*comment(blocked, blocked, "synthetic blocked object")}
			}
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.ErrorContains(err, cause)
			assert.Contains(err.Error(), strconv.Quote(blocked))
			assert.NotContains(err.Error(), "synthetic-page-secret")
			assert.NotContains(err.Error(), "synthetic blocked object")
			assert.True(sum.Partial)
			checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			var before syncState
			require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &before))
			frames := before.Phases[Posts].Stack
			require.NotEmpty(frames)
			pending := frames[len(frames)-1].Pending
			require.NotEmpty(pending)
			assert.Equal(blocked, pending[0].ID)
			sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.ErrorContains(err, cause)
			assert.Contains(err.Error(), strconv.Quote(blocked))
			assert.Zero(sum.MessagesAdded)
			checkpoint, err = st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			assert.Equal(store.SyncStatusFailed, checkpoint.Status)
			assert.Contains(checkpoint.ErrorMessage.String, strconv.Quote(blocked))
			var after syncState
			require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &after))
			assert.Equal(before.Phases[Posts], after.Phases[Posts], "retry must retain the same blocked object without skipping work")
		})
	}
}

func TestAdPermissionFailureDoesNotStarveOrganic(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.denied["100/ads_posts"] = true
	f.denied["601/comments"] = true
	f.posts = append(f.posts, Object{ID: "100_900"}, Object{ID: "100_901"})
	for i := 1; i <= 3; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(strconv.Itoa(600+i), "100_200", "new organic"))
	}
	// Repeated provider errors must retain the unfinished multi-page organic
	// response and nested traversal, while completed independent phases rescan.
	for range 2 {
		sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
		require.ErrorContains(err, "facebook ads_posts")
		assert.True(sum.Partial)
		checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
		require.NoError(err)
		var state syncState
		require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
		organic := state.Phases[Posts]
		assert.False(organic.Done)
		assert.False(organic.Rescan)
		assert.Equal("2", organic.After)
		require.Len(organic.Posts, 1)
		assert.Equal("100_900", organic.Posts[0].ID)
		require.Len(organic.Stack, 2)
		assert.Equal("601", organic.Stack[1].Parent)
		assert.Equal("2", organic.Stack[0].Next)
		require.Len(organic.Stack[0].Pending, 1)
		assert.Equal("602", organic.Stack[0].Pending[0].ID)
	}
	delete(f.denied, "601/comments")
	for _, target := range []string{"100_901", "604"} {
		if target == "604" {
			f.comments["100_200"] = append(f.comments["100_200"], *comment(target, "100_200", "late comment on old organic root"))
		}
		foundDuringDenial := false
		for range 12 {
			before := f.calls
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
			assert.LessOrEqual(f.calls-before, 4)
			assert.True(sum.Partial)
			if err != nil {
				require.ErrorContains(err, "facebook ads_posts")
			}
			ids, readErr := st.MessageExistsBatch(src.ID, []string{target})
			require.NoError(readErr)
			if ids[target] != 0 && err != nil {
				foundDuringDenial = true
				break
			}
		}
		require.True(foundDuringDenial, "bounded organic progress must reach %s despite ongoing ad denial", target)
	}
	assert.Equal(message(t, st, src.ID, "100_200").ID, message(t, st, src.ID, "604").ReplyToMessageID.Int64)
}

type yieldingAPI struct {
	API

	cancel                context.CancelCauseFunc
	yieldBeforeCollection bool
}

func (a yieldingAPI) VerifyPage(ctx context.Context, page string) (Page, error) {
	result, err := a.API.VerifyPage(ctx, page)
	if a.yieldBeforeCollection {
		a.cancel(jobctx.ErrYieldedToWaiter)
	}
	return result, err
}

func (a yieldingAPI) Posts(ctx context.Context, page string, edge PostEdge, after string) (ObjectPage, error) {
	result, err := a.API.Posts(ctx, page, edge, after)
	if edge == VisitorPosts {
		a.cancel(jobctx.ErrYieldedToWaiter)
		err = errors.Join(err, ctx.Err())
	}
	return result, err
}

func TestImportSchedulerYieldHandoff(t *testing.T) {
	for _, outcome := range []string{"pure yield", "yield joined with provider failure"} {
		t.Run(outcome, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "archived before handoff")}
			providerFailure := outcome != "pure yield"
			f.denied["100/visitor_posts"] = providerFailure
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			base := imp.api
			imp.api = yieldingAPI{API: base, cancel: cancel}
			sum, err := imp.Import(ctx, "9", "100", ImportOptions{})
			require.ErrorIs(err, context.Canceled)
			assert.True(sum.Partial)
			run, readErr := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(readErr)
			var state syncState
			require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
			assert.True(state.Phases[Posts].Done)
			if providerFailure {
				var providerErr *APIError
				require.ErrorAs(jobctx.ErrorAfterYield(ctx, err), &providerErr)
				assert.Equal(http.StatusForbidden, providerErr.Status)
				assert.Equal(store.SyncStatusFailed, run.Status)
				assert.Positive(run.ErrorsCount)
				assert.Contains(run.ErrorMessage.String, "HTTP 403")
				return
			}
			require.NoError(jobctx.ErrorAfterYield(ctx, err))
			assert.Equal(store.SyncStatusCancelled, run.Status)
			assert.Zero(run.ErrorsCount)
			assert.Empty(run.ErrorMessage.String)
			assert.False(state.Phases[Posts].Rescan)
			archived := message(t, st, src.ID, "100_201")
			reads := f.freshCommentReads
			imp.api = base
			sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(reads, f.freshCommentReads, "handoff must resume without rescanning completed organic history")
			assert.Equal(archived.ID, message(t, st, src.ID, "100_201").ID)
		})
	}
}

func TestImportStreamedBodyYieldKeepsProgress(t *testing.T) {
	for _, yielding := range []bool{true, false} {
		name := "truncated body without cancellation"
		if yielding {
			name = "scheduler yield during body read"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var streaming atomic.Bool
			streaming.Store(true)
			var organicReads, bodyReads atomic.Int32
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v26.0/me":
					_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
				case "/v26.0/100/posts":
					organicReads.Add(1)
					_, _ = fmt.Fprint(w, `{"data":[{"id":"100_200"}]}`)
				case "/v26.0/100_200/comments":
					_, _ = fmt.Fprint(w, `{"data":[{"id":"100_201","parent":{"id":"100_200"}}]}`)
				case "/v26.0/100/visitor_posts":
					_, _ = fmt.Fprint(w, `{"data":[{"id":"100_300"}]}`)
				case "/v26.0/100_300/comments":
					if r.URL.Query().Get("after") == "after-two" {
						_, _ = fmt.Fprint(w, `{"data":[{"id":"100_303","parent":{"id":"100_300"}}]}`)
					} else {
						_, _ = fmt.Fprint(w, `{"data":[{"id":"100_301","parent":{"id":"100_300"}},{"id":"100_302","parent":{"id":"100_300"}}],"paging":{"cursors":{"after":"after-two"},"next":"https://example.test/unused"}}`)
					}
				case "/v26.0/100_301/comments":
					if streaming.Load() {
						w.Header().Set("Content-Length", "1024")
						_, _ = fmt.Fprint(w, `{"data":[{"message":"synthetic-page-secret private-provider-body`)
						if !assert.NoError(http.NewResponseController(w).Flush()) {
							return
						}
						if yielding {
							<-r.Context().Done()
						}
						return
					}
					_, _ = fmt.Fprint(w, `{"data":[]}`)
				case "/v26.0/100_201/comments", "/v26.0/100_302/comments", "/v26.0/100_303/comments", "/v26.0/100/ads_posts":
					_, _ = fmt.Fprint(w, `{"data":[]}`)
				default:
					assert.Fail("unexpected Graph request", r.URL.Path)
					http.Error(w, "unexpected fixture route", http.StatusNotFound)
				}
			})
			base := c.http.Transport
			transport := streamedGraphTransport{RoundTripper: base, reads: &bodyReads}
			if yielding {
				transport.cancel = cancel
			}
			c.http.Transport = transport
			imp := NewImporter(st, c)
			src, err := imp.Register(t.Context(), "9", "100")
			require.NoError(err)
			sum, err := imp.Import(ctx, "9", "100", ImportOptions{})
			require.Error(err)
			assert.Positive(bodyReads.Load(), "cancellation must happen after Do returns and actual body bytes are read")
			assert.True(sum.Partial)
			assert.NotContains(err.Error(), "synthetic-page-secret")
			assert.NotContains(err.Error(), "private-provider-body")
			run, readErr := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(readErr)
			assert.NotContains(run.ErrorMessage.String, "synthetic-page-secret")
			assert.NotContains(run.ErrorMessage.String, "private-provider-body")
			var state syncState
			require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
			assert.True(state.Phases[Posts].Done)
			frames := state.Phases[VisitorPosts].Stack
			require.Len(frames, 2)
			require.Len(frames[0].Pending, 1)
			assert.Equal("100_302", frames[0].Pending[0].ID)
			assert.Equal("after-two", frames[0].Next)
			assert.Equal("100_301", frames[1].Parent)
			assert.False(frames[1].Loaded)
			if !yielding {
				require.NoError(ctx.Err())
				require.NotErrorIs(err, context.Canceled)
				require.ErrorContains(jobctx.ErrorAfterYield(ctx, err), "graph response unreadable or too large")
				assert.Equal(store.SyncStatusFailed, run.Status)
				assert.Positive(run.ErrorsCount)
				assert.True(state.Phases[Posts].Rescan)
				return
			}
			require.ErrorIs(context.Cause(ctx), jobctx.ErrYieldedToWaiter)
			require.ErrorIs(err, context.Canceled)
			require.NoError(jobctx.ErrorAfterYield(ctx, err))
			assert.Equal(store.SyncStatusCancelled, run.Status)
			assert.Zero(run.ErrorsCount)
			assert.Empty(run.ErrorMessage.String)
			assert.False(state.Phases[Posts].Rescan)
			archived := message(t, st, src.ID, "100_201")
			reads := organicReads.Load()
			streaming.Store(false)
			c.http.Transport = base
			sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(reads, organicReads.Load(), "resume must not rescan completed organic history")
			assert.Equal(archived.ID, message(t, st, src.ID, "100_201").ID)
			for _, id := range []string{"100_302", "100_303"} {
				assert.Equal(message(t, st, src.ID, "100_300").ID, message(t, st, src.ID, id).ReplyToMessageID.Int64)
			}
		})
	}
}

func TestImportRejectsCrossConversationReuse(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.posts = append(f.posts, Object{ID: "100_900", Message: "other root"})
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "first root")}
	f.comments["100_900"] = []Object{*comment("100_201", "100_900", "wrong root")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.Error(t, err)
	got := message(t, st, src.ID, "100_201")
	root := message(t, st, src.ID, "100_200")
	assert.Equal(t, root.ConversationID, got.ConversationID)
	assert.Equal(t, sql.NullInt64{Int64: root.ID, Valid: true}, got.ReplyToMessageID)
}

func TestImportCheckpointRejectsDifferentSource(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.FailSyncWithCheckpoint(run, "synthetic interruption", &store.Checkpoint{PageToken: `{"version":2,"source":"app:99:page:100","next_surface":"posts","phases":{"posts":{},"visitor_posts":{},"ads_posts":{}}}`}))
	before := f.calls
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.Error(err)
	assert.Equal(t, before, f.calls, "validate checkpoint binding before provider reads")
}

func TestImportRetiredSourceNeverCallsProvider(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	other, err := st.GetOrCreateSource("facebook_comments", "app:99:page:100")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	require.NoError(err)
	before := f.calls
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.ErrorIs(err, store.ErrSourceRetired)
	assert.Equal(t, before, f.calls)
}

func TestImportVisitorPostComments(t *testing.T) {
	assert := assert.New(t)
	st, f, imp, src := setupImport(t)
	f.visitors = []Object{{ID: "100_900", Message: "Visitor post", From: &Actor{ID: "301", Name: "Example Page"}}}
	f.comments["100_900"] = []Object{*comment("100_901", "100_900", "visitor conversation")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.False(sum.Partial)
	root := message(t, st, src.ID, "100_900")
	assert.False(root.IsFromMe, "matching Page display name is not ownership")
	child := message(t, st, src.ID, "100_901")
	assert.Equal(root.ConversationID, child.ConversationID)
	assert.Equal(root.ID, child.ReplyToMessageID.Int64)
}

func TestFullRescanRecoversExpiredCursor(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	for i := 1; i <= 4; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(strconv.Itoa(700+i), "100_200", "recoverable history"))
	}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 5})
	require.NoError(err)
	require.True(sum.Partial)
	original := message(t, st, src.ID, "701").ID
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(err)
	var before syncState
	require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &before))
	frames := before.Phases[Posts].Stack
	require.NotEmpty(frames)
	require.NotEmpty(frames[len(frames)-1].After, "fixture must resume an actual saved cursor")
	provider := &recordingCommentErrorAPI{API: imp.api}
	imp.api = provider
	for _, response := range []string{"synthetic unsupported field", "synthetic expired cursor"} {
		f.rejectCursor = response
		sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
		require.Error(err)
		require.ErrorIs(err, provider.err)
		var apiErr *APIError
		require.ErrorAs(err, &apiErr)
		assert.Equal(http.StatusBadRequest, apiErr.Status)
		assert.Contains(err.Error(), strconv.Quote("100_200"))
		assert.NotContains(err.Error(), "--full", response)
		assert.NotContains(err.Error(), "cursor was rejected", response)
		assert.NotContains(err.Error(), response)
		assert.True(sum.Partial)
		assert.Zero(sum.MessagesAdded)
		checkpoint, err = st.GetLatestCheckpointedSync(src.ID)
		require.NoError(err)
		assert.Equal(store.SyncStatusFailed, checkpoint.Status)
		assert.Positive(checkpoint.ErrorsCount)
		assert.NotContains(checkpoint.ErrorMessage.String, "--full")
		assert.NotContains(checkpoint.ErrorMessage.String, "cursor was rejected")
		var after syncState
		require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &after))
		assert.Equal(before.Phases[Posts], after.Phases[Posts], "failed read must preserve its saved cursor and pending traversal")
		assert.Equal(original, message(t, st, src.ID, "701").ID)
	}
	f.rejectCursor = ""
	reads := f.freshCommentReads
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Greater(f.freshCommentReads, reads, "explicit full restarts enumeration without saved cursors")
	assert.Equal(original, message(t, st, src.ID, "701").ID)
	for i := 1; i <= 4; i++ {
		message(t, st, src.ID, strconv.Itoa(700+i))
	}
	var count int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(5, count)
}

func TestImportWritesMetadataFormatVersion(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "versioned comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	for _, nativeID := range []string{"100_200", "100_201"} {
		row := message(t, st, src.ID, nativeID)
		saved, err := st.GetMessageMetadata(row.ID)
		require.NoError(err)
		var metadata struct {
			Version int `json:"version"`
		}
		require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
		assert.Equal(t, 1, metadata.Version, "persisted object %s must declare its metadata format", nativeID)
	}
}

func TestImportRefusesUnknownMetadataVersionBeforeReplay(t *testing.T) {
	for _, target := range []string{"100_200", "100_201"} {
		for _, version := range []string{"missing", "future"} {
			t.Run(target+"/"+version, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				st, f, imp, src := setupImport(t)
				f.comments["100_200"] = []Object{*comment("100_201", "100_200", "original comment")}
				_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
				require.NoError(err)
				row := message(t, st, src.ID, target)
				originalBody, err := st.GetMessageBodyText(row.ID)
				require.NoError(err)
				saved, err := st.GetMessageMetadata(row.ID)
				require.NoError(err)
				var metadata map[string]any
				require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
				if version == "future" {
					metadata["version"] = 99
				} else {
					delete(metadata, "version")
				}
				encoded, err := json.Marshal(metadata)
				require.NoError(err)
				require.NoError(st.SetMessageMetadata(row.ID, sql.NullString{String: string(encoded), Valid: true}))
				f.posts[0].Message = "replacement post must not overwrite an unknown format"
				f.comments["100_200"][0].Message = "replacement comment must not overwrite an unknown format"
				sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
				require.Error(err)
				assert.Contains(err.Error(), strconv.Quote(target))
				if sum != nil {
					assert.True(sum.Partial)
				}
				body, readErr := st.GetMessageBodyText(row.ID)
				require.NoError(readErr)
				assert.Equal(originalBody, body, "unknown metadata must be refused before changing that object")
				after, readErr := st.GetMessageMetadata(row.ID)
				require.NoError(readErr)
				assert.JSONEq(string(encoded), after.String, "refusal must preserve the stored metadata")
			})
		}
	}
}

// These are synthetic capability fixtures, not observed Meta response shapes.
// The provider-declared parent may differ from the edge being traversed only
// when its archived source, root, and actual conversation are safe to use.
func TestImportDeclaredParentSelection(t *testing.T) {
	for _, mode := range []string{"provider selects already archived parent", "provider absent uses traversal frame", "three siblings on nested edge"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			parent := *comment("100_201", "100_200", "synthetic parent")
			child := *comment("100_202", "100_201", "synthetic child")
			f.comments["100_200"] = []Object{parent}
			ids := []string{"100_202"}
			switch mode {
			case "provider selects already archived parent":
				// The first object is archived before the next object is handled.
				// Both arrive on the root edge, but the child declares 100_201.
				f.comments["100_200"] = append(f.comments["100_200"], child)
			case "provider absent uses traversal frame":
				child.Parent = nil
				f.comments["100_201"] = []Object{child}
			case "three siblings on nested edge":
				// The fixture returns two objects per page, so the third sibling
				// requires returning from nested frames and reading the next page.
				f.comments["100_201"] = []Object{child, *comment("100_203", "100_201", "second"), *comment("100_204", "100_201", "third")}
				ids = append(ids, "100_203", "100_204")
			}
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			rootRow := message(t, st, src.ID, "100_200")
			parentRow := message(t, st, src.ID, "100_201")
			originals := make(map[string]int64, len(ids))
			for _, id := range ids {
				childRow := message(t, st, src.ID, id)
				originals[id] = childRow.ID
				assert.Equal(parentRow.ID, childRow.ReplyToMessageID.Int64)
				assert.Equal(rootRow.ConversationID, childRow.ConversationID)
				assert.Equal(parentRow.ConversationID, childRow.ConversationID)
				metadata, err := st.GetMessageMetadata(childRow.ID)
				require.NoError(err)
				var context struct {
					Root   string `json:"post_id"`
					Parent string `json:"parent_id"`
				}
				require.NoError(json.Unmarshal([]byte(metadata.String), &context))
				assert.Equal("100_200", context.Root)
				assert.Equal("100_201", context.Parent)
			}
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			for _, id := range ids {
				assert.Equal(originals[id], message(t, st, src.ID, id).ID, "same declared parent replays without a duplicate")
			}
		})
	}
}

func TestImportDeclaredParentRefusesUnsafeTargets(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		parentID               string
		seed                   bool
		foreignSource          bool
		otherConversation      bool
		metadataNamesOtherRoot bool
	}{
		{name: "missing", parentID: "100_999"},
		{name: "foreign source", parentID: "100_999", seed: true, foreignSource: true},
		{name: "other root despite same conversation", parentID: "100_999", seed: true, metadataNamesOtherRoot: true},
		{name: "other conversation despite matching metadata root", parentID: "100_999", seed: true, otherConversation: true},
		{name: "self parent", parentID: "100_202"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			root := message(t, st, src.ID, "100_200")
			if tt.seed {
				// Seed real Store rows to distinguish source scoping, metadata
				// claims, and actual conversation ownership for a NEW child.
				sourceID, conversationID, app := src.ID, root.ConversationID, "9"
				if tt.foreignSource {
					other, err := st.GetOrCreateSource(SourceType, "app:99:page:100")
					require.NoError(err)
					sourceID, app = other.ID, "99"
				}
				if tt.foreignSource || tt.otherConversation {
					conversationID, err = st.EnsureConversationWithTypeContext(t.Context(), sourceID, "100_900", "public_comments", "synthetic other conversation")
					require.NoError(err)
				}
				metadataRoot := "100_200"
				if tt.metadataNamesOtherRoot {
					metadataRoot = "100_900"
				}
				metadata := sql.NullString{Valid: true, String: fmt.Sprintf(`{"version":1,"app_id":%q,"page_id":"100","post_id":%q,"parent_id":"100_200"}`, app, metadataRoot)}
				_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
					Message:  &store.Message{SourceID: sourceID, ConversationID: conversationID, SourceMessageID: tt.parentID, MessageType: SourceType},
					Metadata: &metadata, BodyText: sql.NullString{String: "synthetic parent boundary", Valid: true},
				})
				require.NoError(err)
			}
			f.comments["100_200"] = []Object{*comment("100_202", tt.parentID, "must not link outside the selected root")}
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.Error(err)
			assert.Contains(err.Error(), strconv.Quote("100_202"))
			ids, readErr := st.MessageExistsBatch(src.ID, []string{"100_202"})
			require.NoError(readErr)
			assert.Zero(ids["100_202"], "unsafe declared parent must be rejected before persisting the new child")
		})
	}
}

func TestImportDeclaredParentPreservesArchivedRelationships(t *testing.T) {
	for _, target := range []string{"existing comment", "root post"} {
		t.Run(target, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{
				*comment("100_201", "100_200", "first parent"),
				*comment("100_203", "100_200", "another archived parent"),
			}
			f.comments["100_201"] = []Object{*comment("100_202", "100_201", "original child")}
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			id := "100_202"
			if target == "root post" {
				id = "100_200"
				f.posts[0].Parent = &Actor{ID: "100_203"}
				f.posts[0].Message = "must not reparent root"
			} else {
				f.comments["100_201"][0].Parent.ID = "100_203"
				f.comments["100_201"][0].Message = "must not reparent existing child"
			}
			before := message(t, st, src.ID, id)
			body, err := st.GetMessageBodyText(before.ID)
			require.NoError(err)
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.Error(err)
			after := message(t, st, src.ID, id)
			assert.Equal(before.ReplyToMessageID, after.ReplyToMessageID)
			assert.Equal(before.ConversationID, after.ConversationID)
			afterBody, err := st.GetMessageBodyText(before.ID)
			require.NoError(err)
			assert.Equal(body, afterBody)
		})
	}
}

func TestTriageSurvivesImportReplay(t *testing.T) {
	require := require.New(t)
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_200_300", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	id := message(t, st, src.ID, "100_200_300").ID
	_, err = st.SetCommentTriageContext(t.Context(), id, store.CommentTriageHandled, nil)
	require.NoError(err)
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
	require.NoError(err)
	got, err := st.CommentTriageContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(t, store.CommentTriageHandled, got)
}
