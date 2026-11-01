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
	rejectCursor         bool
	freshCommentReads    int
}

func newGraphFixture(t *testing.T) (*graphFixture, *Client) {
	t.Helper()
	f := &graphFixture{posts: []Object{{ID: "100_200", Message: "Example post", From: &Actor{ID: "100", Name: "Example Page"}}}, comments: map[string][]Object{}, denied: map[string]bool{}}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if f.rejectCursor && r.URL.Query().Get("after") != "" {
			http.Error(w, "synthetic expired cursor", 400)
			return
		}
		if r.URL.Path == "/v26.0/100_200/comments" && r.URL.Query().Get("after") == "" {
			f.freshCommentReads++
		}
		path := strings.TrimPrefix(r.URL.Path, "/v26.0/")
		if f.denied[path] {
			http.Error(w, "synthetic denial", 403)
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
		start, _ := strconv.Atoi(r.URL.Query().Get("after"))
		end := min(start+2, len(data))
		require.LessOrEqual(t, start, end)
		if data == nil {
			data = []Object{}
		}
		response := map[string]any{"data": data[start:end]}
		if end < len(data) {
			response["paging"] = map[string]any{"cursors": map[string]string{"after": strconv.Itoa(end)}, "next": "https://graph.facebook.com/unused"}
		}
		blob, err := json.Marshal(response)
		require.NoError(t, err)
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
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "searchable example")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.False(t, sum.Partial)
	got := message(t, st, src.ID, "100_201")
	body, err := st.GetMessageBodyText(got.ID)
	require.NoError(t, err)
	assert.Equal(t, "searchable example", body)
	assert.Equal(t, SourceType, got.MessageType)
	root := message(t, st, src.ID, "100_200")
	assert.Equal(t, root.ConversationID, got.ConversationID)
	assert.Equal(t, root.ID, got.ReplyToMessageID.Int64)
	assert.True(t, root.IsFromMe)
	var hits int
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'searchable'`).Scan(&hits))
	assert.Equal(t, 1, hits)
	f.comments["100_200"][0].Message = "edited example"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, got.ID, message(t, st, src.ID, "100_201").ID)
	body, err = st.GetMessageBodyText(got.ID)
	require.NoError(t, err)
	assert.Equal(t, "edited example", body)
}

func TestImportNestedReplies(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "parent")}
	f.comments["100_201"] = []Object{*comment("100_202", "100_201", "child"), *comment("100_203", "100_201", "second"), *comment("100_204", "100_201", "third")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	parent := message(t, st, src.ID, "100_201")
	for _, id := range []string{"100_202", "100_203", "100_204"} {
		child := message(t, st, src.ID, id)
		assert.Equal(t, parent.ID, child.ReplyToMessageID.Int64)
		assert.Equal(t, parent.ConversationID, child.ConversationID)
	}
	f.comments["100_201"][0].Parent.ID = "other-post"
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.Error(t, err)
	assert.Equal(t, parent.ID, message(t, st, src.ID, "100_202").ReplyToMessageID.Int64)
}

func TestImportResumeAfterBudget(t *testing.T) {
	st, f, imp, src := setupImport(t)
	for i := 1; i <= 5; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(fmt.Sprint(400+i), "100_200", "budget"))
	}
	completed := false
	for range 15 {
		before := f.calls
		sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
		require.NoError(t, err)
		assert.LessOrEqual(t, f.calls-before, 4)
		assert.Equal(t, f.calls-before, sum.Requests)
		if !sum.Partial {
			completed = true
			break
		}
	}
	assert.True(t, completed)
	for i := 1; i <= 5; i++ {
		message(t, st, src.ID, fmt.Sprint(400+i))
	}
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(t, 6, count)
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
	st, f, imp, src := setupImport(t)
	f.ads = f.posts
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "shared organic and ad")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(t, 2, count)
	got := message(t, st, src.ID, "100_201")
	meta, err := st.GetMessageMetadata(got.ID)
	require.NoError(t, err)
	assert.Contains(t, meta.String, `"organic":true`)
	assert.Contains(t, meta.String, `"ad":true`)
}

func TestImportScopedIdentityAndPersonLink(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "example identity")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	first := message(t, st, src.ID, "100_201").SenderID.Int64
	secondSource, err := imp.Register(t.Context(), "99", "100")
	require.NoError(t, err)
	_, err = imp.Import(t.Context(), "99", "100", ImportOptions{})
	require.NoError(t, err)
	second := message(t, st, secondSource.ID, "100_201").SenderID.Int64
	assert.NotEqual(t, first, second)
	email, err := st.EnsureParticipant("example@example.com", "Example Customer", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(email)
	require.NoError(t, err)
	_, err = st.LinkParticipants(email, first)
	require.NoError(t, err)
	linked, err := st.PersonForParticipants([]int64{first})
	require.NoError(t, err)
	assert.Equal(t, person.ID, linked.ID)
	assert.Equal(t, "facebook_comments:1:9:3:100:3:300", ActorIdentifier("9", "100", "300"))
	observations, err := st.FindObservationsByProviderUserIDContext(t.Context(), "facebook_comments:1:9:3:100:3:300", 10)
	require.NoError(t, err)
	require.NotEmpty(t, observations)
	assert.Equal(t, first, observations[0].ParticipantID)
}

func TestImportMissingAuthor(t *testing.T) {
	st, f, imp, src := setupImport(t)
	c := comment("100_201", "100_200", "anonymous")
	c.From = nil
	f.comments["100_200"] = []Object{*c}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.False(t, message(t, st, src.ID, c.ID).SenderID.Valid)
	c.From = &Actor{ID: "300", Name: "Example Customer"}
	f.comments["100_200"] = []Object{*c}
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	known := message(t, st, src.ID, c.ID).SenderID
	require.True(t, known.Valid)
	c.From = nil
	f.comments["100_200"] = []Object{*c}
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, known, message(t, st, src.ID, c.ID).SenderID)
}

func TestImportFailureKeepsCheckpoint(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "retained on failure")}
	f.denied["100_201/comments"] = true
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.Error(t, err)
	assert.True(t, sum.Partial)
	got := message(t, st, src.ID, "100_201")
	checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
	require.NoError(t, err)
	assert.Contains(t, checkpoint.CursorBefore.String, "100_201")
	assert.NotContains(t, checkpoint.CursorBefore.String, "synthetic-page-secret")
	assert.Equal(t, store.SyncStatusFailed, checkpoint.Status)
	delete(f.denied, "100_201/comments")
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.False(t, sum.Partial)
	assert.Equal(t, got.ID, message(t, st, src.ID, "100_201").ID)
}

func TestAdPermissionFailureDoesNotStarveOrganic(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.denied["100/ads_posts"] = true
	f.denied["601/comments"] = true
	f.posts = append(f.posts, Object{ID: "100_900"}, Object{ID: "100_901"})
	for i := 1; i <= 3; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(fmt.Sprint(600+i), "100_200", "new organic"))
	}
	// Repeated provider errors must retain the unfinished multi-page organic
	// response and nested traversal, while completed independent phases rescan.
	for range 2 {
		sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
		require.ErrorContains(t, err, "Facebook ads_posts")
		assert.True(t, sum.Partial)
		checkpoint, err := st.GetLatestCheckpointedSync(src.ID)
		require.NoError(t, err)
		var state syncState
		require.NoError(t, json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
		organic := state.Phases[Posts]
		assert.False(t, organic.Done)
		assert.False(t, organic.Rescan)
		assert.Equal(t, "2", organic.After)
		require.Len(t, organic.Posts, 1)
		assert.Equal(t, "100_900", organic.Posts[0].ID)
		require.Len(t, organic.Stack, 2)
		assert.Equal(t, "601", organic.Stack[1].Parent)
		assert.Equal(t, "2", organic.Stack[0].Next)
		require.Len(t, organic.Stack[0].Pending, 1)
		assert.Equal(t, "602", organic.Stack[0].Pending[0].ID)
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
			assert.LessOrEqual(t, f.calls-before, 4)
			assert.True(t, sum.Partial)
			if err != nil {
				require.ErrorContains(t, err, "Facebook ads_posts")
			}
			ids, readErr := st.MessageExistsBatch(src.ID, []string{target})
			require.NoError(t, readErr)
			if ids[target] != 0 && err != nil {
				foundDuringDenial = true
				break
			}
		}
		require.True(t, foundDuringDenial, "bounded organic progress must reach %s despite ongoing ad denial", target)
	}
	assert.Equal(t, message(t, st, src.ID, "100_200").ID, message(t, st, src.ID, "604").ReplyToMessageID.Int64)
}

type yieldingAPI struct {
	API
	cancel context.CancelCauseFunc
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
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{*comment("100_201", "100_200", "archived before handoff")}
			providerFailure := outcome != "pure yield"
			f.denied["100/visitor_posts"] = providerFailure
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			base := imp.api
			imp.api = yieldingAPI{API: base, cancel: cancel}
			sum, err := imp.Import(ctx, "9", "100", ImportOptions{})
			require.ErrorIs(t, err, context.Canceled)
			assert.True(t, sum.Partial)
			run, readErr := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(t, readErr)
			var state syncState
			require.NoError(t, json.Unmarshal([]byte(run.CursorBefore.String), &state))
			assert.True(t, state.Phases[Posts].Done)
			if providerFailure {
				var providerErr *APIError
				require.ErrorAs(t, jobctx.ErrorAfterYield(ctx, err), &providerErr)
				assert.Equal(t, http.StatusForbidden, providerErr.Status)
				assert.Equal(t, store.SyncStatusFailed, run.Status)
				assert.Positive(t, run.ErrorsCount)
				assert.Contains(t, run.ErrorMessage.String, "HTTP 403")
				return
			}
			assert.NoError(t, jobctx.ErrorAfterYield(ctx, err))
			assert.Equal(t, store.SyncStatusCancelled, run.Status)
			assert.Zero(t, run.ErrorsCount)
			assert.Empty(t, run.ErrorMessage.String)
			assert.False(t, state.Phases[Posts].Rescan)
			archived := message(t, st, src.ID, "100_201")
			reads := f.freshCommentReads
			imp.api = base
			sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(t, err)
			assert.False(t, sum.Partial)
			assert.Equal(t, reads, f.freshCommentReads, "handoff must resume without rescanning completed organic history")
			assert.Equal(t, archived.ID, message(t, st, src.ID, "100_201").ID)
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
	st, f, imp, src := setupImport(t)
	run, err := st.StartSync(src.ID, SourceType)
	require.NoError(t, err)
	require.NoError(t, st.FailSyncWithCheckpoint(run, "synthetic interruption", &store.Checkpoint{PageToken: `{"version":1,"source":"app:99:page:100","phases":{"posts":{},"visitor_posts":{},"ads_posts":{}}}`}))
	before := f.calls
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.Error(t, err)
	assert.Equal(t, before, f.calls, "validate checkpoint binding before provider reads")
}

func TestImportRetiredSourceNeverCallsProvider(t *testing.T) {
	st, f, imp, src := setupImport(t)
	other, err := st.GetOrCreateSource("facebook_comments", "app:99:page:100")
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	require.NoError(t, err)
	before := f.calls
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.ErrorIs(t, err, store.ErrSourceRetired)
	assert.Equal(t, before, f.calls)
}

func TestImportVisitorPostComments(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.visitors = []Object{{ID: "100_900", Message: "Visitor post", From: &Actor{ID: "301", Name: "Example Page"}}}
	f.comments["100_900"] = []Object{*comment("100_901", "100_900", "visitor conversation")}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	assert.False(t, sum.Partial)
	root := message(t, st, src.ID, "100_900")
	assert.False(t, root.IsFromMe, "matching Page display name is not ownership")
	child := message(t, st, src.ID, "100_901")
	assert.Equal(t, root.ConversationID, child.ConversationID)
	assert.Equal(t, root.ID, child.ReplyToMessageID.Int64)
}

func TestFullRescanRecoversExpiredCursor(t *testing.T) {
	st, f, imp, src := setupImport(t)
	for i := 1; i <= 4; i++ {
		f.comments["100_200"] = append(f.comments["100_200"], *comment(fmt.Sprint(700+i), "100_200", "recoverable history"))
	}
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 4})
	require.NoError(t, err)
	require.True(t, sum.Partial)
	original := message(t, st, src.ID, "701").ID
	f.rejectCursor = true
	_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.ErrorContains(t, err, "--full")
	assert.Equal(t, original, message(t, st, src.ID, "701").ID)
	f.rejectCursor = false
	reads := f.freshCommentReads
	sum, err = imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
	require.NoError(t, err)
	assert.False(t, sum.Partial)
	assert.Greater(t, f.freshCommentReads, reads, "explicit full restarts enumeration without saved cursors")
	assert.Equal(t, original, message(t, st, src.ID, "701").ID)
	for i := 1; i <= 4; i++ {
		message(t, st, src.ID, fmt.Sprint(700+i))
	}
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assert.Equal(t, 5, count)
}

func TestImportWritesMetadataFormatVersion(t *testing.T) {
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_201", "100_200", "versioned comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	for _, nativeID := range []string{"100_200", "100_201"} {
		row := message(t, st, src.ID, nativeID)
		saved, err := st.GetMessageMetadata(row.ID)
		require.NoError(t, err)
		var metadata struct {
			Version int `json:"version"`
		}
		require.NoError(t, json.Unmarshal([]byte(saved.String), &metadata))
		assert.Equal(t, 1, metadata.Version, "persisted object %s must declare its metadata format", nativeID)
	}
}

func TestImportRefusesUnknownMetadataVersionBeforeReplay(t *testing.T) {
	for _, target := range []string{"100_200", "100_201"} {
		for _, version := range []string{"missing", "future"} {
			t.Run(target+"/"+version, func(t *testing.T) {
				st, f, imp, src := setupImport(t)
				f.comments["100_200"] = []Object{*comment("100_201", "100_200", "original comment")}
				_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
				require.NoError(t, err)
				row := message(t, st, src.ID, target)
				originalBody, err := st.GetMessageBodyText(row.ID)
				require.NoError(t, err)
				saved, err := st.GetMessageMetadata(row.ID)
				require.NoError(t, err)
				var metadata map[string]any
				require.NoError(t, json.Unmarshal([]byte(saved.String), &metadata))
				if version == "future" {
					metadata["version"] = 99
				} else {
					delete(metadata, "version")
				}
				encoded, err := json.Marshal(metadata)
				require.NoError(t, err)
				require.NoError(t, st.SetMessageMetadata(row.ID, sql.NullString{String: string(encoded), Valid: true}))
				f.posts[0].Message = "replacement post must not overwrite an unknown format"
				f.comments["100_200"][0].Message = "replacement comment must not overwrite an unknown format"
				sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
				assert.Error(t, err)
				if sum != nil {
					assert.True(t, sum.Partial)
				}
				body, readErr := st.GetMessageBodyText(row.ID)
				require.NoError(t, readErr)
				assert.Equal(t, originalBody, body, "unknown metadata must be refused before changing that object")
				after, readErr := st.GetMessageMetadata(row.ID)
				require.NoError(t, readErr)
				assert.JSONEq(t, string(encoded), after.String, "refusal must preserve the stored metadata")
			})
		}
	}
}

// These are synthetic capability fixtures, not observed Meta response shapes.
// The provider-declared parent may differ from the edge being traversed only
// when its archived source, root, and actual conversation are safe to use.
func TestImportDeclaredParentSelection(t *testing.T) {
	for _, declared := range []bool{true, false} {
		name := "provider absent uses traversal frame"
		if declared {
			name = "provider selects already archived parent"
		}
		t.Run(name, func(t *testing.T) {
			st, f, imp, src := setupImport(t)
			parent := *comment("100_201", "100_200", "synthetic parent")
			child := *comment("100_202", "100_201", "synthetic child")
			f.comments["100_200"] = []Object{parent}
			if declared {
				// The first object is archived before the next object is handled.
				// Both arrive on the root edge, but the child declares 100_201.
				f.comments["100_200"] = append(f.comments["100_200"], child)
			} else {
				child.Parent = nil
				f.comments["100_201"] = []Object{child}
			}
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(t, err)
			rootRow := message(t, st, src.ID, "100_200")
			parentRow := message(t, st, src.ID, "100_201")
			childRow := message(t, st, src.ID, "100_202")
			assert.Equal(t, parentRow.ID, childRow.ReplyToMessageID.Int64)
			assert.Equal(t, rootRow.ConversationID, childRow.ConversationID)
			metadata, err := st.GetMessageMetadata(childRow.ID)
			require.NoError(t, err)
			var context struct {
				Root   string `json:"post_id"`
				Parent string `json:"parent_id"`
			}
			require.NoError(t, json.Unmarshal([]byte(metadata.String), &context))
			assert.Equal(t, "100_200", context.Root)
			assert.Equal(t, "100_201", context.Parent)
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(t, err)
			assert.Equal(t, childRow.ID, message(t, st, src.ID, "100_202").ID, "same declared parent replays without a duplicate")
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
			st, f, imp, src := setupImport(t)
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(t, err)
			root := message(t, st, src.ID, "100_200")
			if tt.seed {
				// Seed real Store rows to distinguish source scoping, metadata
				// claims, and actual conversation ownership for a NEW child.
				sourceID, conversationID, app := src.ID, root.ConversationID, "9"
				if tt.foreignSource {
					other, err := st.GetOrCreateSource(SourceType, "app:99:page:100")
					require.NoError(t, err)
					sourceID, app = other.ID, "99"
				}
				if tt.foreignSource || tt.otherConversation {
					conversationID, err = st.EnsureConversationWithTypeContext(t.Context(), sourceID, "100_900", "public_comments", "synthetic other conversation")
					require.NoError(t, err)
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
				require.NoError(t, err)
			}
			f.comments["100_200"] = []Object{*comment("100_202", tt.parentID, "must not link outside the selected root")}
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			assert.Error(t, err)
			ids, readErr := st.MessageExistsBatch(src.ID, []string{"100_202"})
			require.NoError(t, readErr)
			assert.Zero(t, ids["100_202"], "unsafe declared parent must be rejected before persisting the new child")
		})
	}
}

func TestImportDeclaredParentPreservesArchivedRelationships(t *testing.T) {
	for _, target := range []string{"existing comment", "root post"} {
		t.Run(target, func(t *testing.T) {
			st, f, imp, src := setupImport(t)
			f.comments["100_200"] = []Object{
				*comment("100_201", "100_200", "first parent"),
				*comment("100_203", "100_200", "another archived parent"),
			}
			f.comments["100_201"] = []Object{*comment("100_202", "100_201", "original child")}
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(t, err)
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
			require.NoError(t, err)
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			assert.Error(t, err)
			after := message(t, st, src.ID, id)
			assert.Equal(t, before.ReplyToMessageID, after.ReplyToMessageID)
			assert.Equal(t, before.ConversationID, after.ConversationID)
			afterBody, err := st.GetMessageBodyText(before.ID)
			require.NoError(t, err)
			assert.Equal(t, body, afterBody)
		})
	}
}
