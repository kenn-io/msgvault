package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type igActionGraph struct {
	calls              int
	method, path, body string
	status             int
	response           string
}

type instagramActionTestFixture struct {
	store    *store.Store
	source   *store.Source
	id       int64
	actions  *Actions
	graph    *igActionGraph
	input    *instagramImportFixture
	importer *Importer
}

func instagramActionFixture(t *testing.T) instagramActionTestFixture {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	f, readClient := newInstagramImportFixture(t)
	f.comments[0].From = &IGCommentFromUser{ID: "600", Username: "example_author"}
	f.replies[0].From = &IGCommentFromUser{ID: "600", Username: "example_author"}
	imp := NewImporter(st, readClient)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(t, err)
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(t, err)
	id := message(t, st, src.ID, "400").ID
	g := &igActionGraph{status: 200, response: `{"id":"501"}`}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		g.calls++
		g.method, g.path = r.Method, r.URL.Path
		assert.Empty(t, r.URL.RawQuery)
		data, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		g.body = string(data)
		w.WriteHeader(g.status)
		_, _ = fmt.Fprint(w, g.response)
	})
	a := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return c.InstagramActions(), nil }, Now: func() time.Time { return time.Date(2000, 1, 2, 5, 0, 0, 0, time.UTC) }})
	return instagramActionTestFixture{store: st, source: src, id: id, actions: a, graph: g, input: f, importer: imp}
}
func TestInstagramReplyExactRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := instagramActionFixture(t)
	got, err := replyRun(t, fx.actions, fx.id, ActionPublicReply, "Example reply")
	require.NoError(err)
	assert.Equal("instagram", got.Disclosure.Provider)
	assert.Equal("example_author", got.Disclosure.AuthorName)
	assert.Equal("200", got.Disclosure.AssetID)
	assert.Equal("POST", fx.graph.method)
	assert.Equal("/v26.0/400/replies", fx.graph.path)
	form, err := url.ParseQuery(fx.graph.body)
	require.NoError(err)
	assert.Equal(url.Values{"message": {"Example reply"}}, form)
	assert.Equal("501", got.Receipt.RemoteID)
	assert.False(got.Receipt.ArchiveComplete)
}
func TestInstagramReplyNonTopLevelRefused(t *testing.T) {
	fx := instagramActionFixture(t)
	nested := message(t, fx.store, fx.source.ID, "500").ID
	_, err := fx.actions.Run(t.Context(), ActionRequest{MessageID: nested, Action: ActionPublicReply, Body: "reply", DryRun: true})
	failure := actionCode(t, err, "ig_target_not_top_level")
	assert.Contains(t, failure.Message, strconv.FormatInt(fx.id, 10))
	assert.Zero(t, fx.graph.calls)
}
func TestInstagramReplyHiddenTargetRefused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := instagramActionFixture(t)
	fx.graph.response = `{"success":true}`
	_, err := fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionHide})
	require.NoError(err)
	form, err := url.ParseQuery(fx.graph.body)
	require.NoError(err)
	assert.Equal("true", form.Get("hide"))
	assert.Empty(form.Get("is_hidden"))
	_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	_ = actionCode(t, err, "target_hidden")
	assert.Equal(1, fx.graph.calls)
}

func TestInstagramExistingReplyReceiptSurvivesHiddenTarget(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(strconv.FormatBool(unknown), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fx := instagramActionFixture(t)
			if unknown {
				fx.graph.status, fx.graph.response = 500, `{}`
			}
			first, err := replyRun(t, fx.actions, fx.id, ActionPublicReply, "Example reply")
			if unknown {
				_ = actionCode(t, err, "outcome_unknown")
			} else {
				require.NoError(err)
			}
			require.NotNil(first.Receipt)
			fx.graph.status, fx.graph.response = 200, `{"success":true}`
			_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionHide})
			require.NoError(err)

			preview, err := fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
			require.NoError(err, "an existing receipt remains inspectable after a later hide")
			require.NotNil(preview.Receipt)
			assert.Equal(first.Receipt.ID, preview.Receipt.ID)
			req := ActionRequest{MessageID: fx.id, Action: ActionPublicReply, Body: "Example reply", Confirm: preview.Confirm}
			again, err := fx.actions.Run(t.Context(), req)
			if unknown {
				_ = actionCode(t, err, "outcome_unknown")
				req.ResolveUnknown = "sent"
				again, err = fx.actions.Run(t.Context(), req)
			}
			require.NoError(err)
			assert.Equal(first.Receipt.ID, again.Receipt.ID)
			assert.Equal("succeeded", again.Receipt.State)

			_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionPublicReply, Body: "Different reply", DryRun: true})
			_ = actionCode(t, err, "target_hidden")
			target, err := ResolveTarget(t.Context(), fx.store, fx.id)
			require.NoError(err)
			_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionPublicReply, Body: "Different reply", Confirm: ConfirmToken(target, ActionPublicReply, "Different reply")})
			_ = actionCode(t, err, "target_hidden")
			assert.Equal(2, fx.graph.calls, "known receipt reuse and resolution must not dispatch again")
		})
	}
}
func TestInstagramHideSelfRefused(t *testing.T) {
	fx := instagramActionFixture(t)
	_, err := fx.store.DB().Exec("UPDATE messages SET is_from_me=1 WHERE id=?", fx.id)
	require.NoError(t, err)
	_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionHide, DryRun: true})
	_ = actionCode(t, err, "not_hideable")
	assert.Zero(t, fx.graph.calls)
}

func TestInstagramActionTargetRestoresOmittedAuthor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := instagramActionFixture(t)
	original, err := ResolveTarget(t.Context(), fx.store, fx.id)
	require.NoError(err)
	fx.input.comments[0].From = nil
	_, err = fx.importer.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	archived, err := fx.store.MetaCommentTargetContext(t.Context(), fx.id)
	require.NoError(err)
	// Account IDs and comment-author IDs belong to separate identity families.
	require.NoError(fx.store.SetParticipantIdentifierContext(t.Context(), archived.SenderID, InstagramSourceType, instagramActorIdentifier("9", "100", "200", "ig_user", "777")))
	restored, err := ResolveTarget(t.Context(), fx.store, fx.id)
	require.NoError(err)
	assert.Equal(original.AuthorID, restored.AuthorID)
	assert.Equal(original.AuthorName, restored.AuthorName)
	preview, err := fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionPrivateReply, Body: "Private reply", DryRun: true})
	require.NoError(err)
	assert.Equal(original.AuthorName, preview.Disclosure.AuthorName)
	assert.Zero(fx.graph.calls)
}

func TestInstagramReplyArchivedByNextSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := instagramActionFixture(t)
	got, err := replyRun(t, fx.actions, fx.id, ActionPublicReply, "Example reply")
	require.NoError(err)
	assert.False(got.Receipt.ArchiveComplete)
	fx.input.replies = append(fx.input.replies, IGComment{ID: "501", Text: "Example reply", Timestamp: "2000-01-02T05:00:00+0000", ParentID: "400", From: &IGCommentFromUser{ID: "200", Username: "example_business"}})
	_, err = fx.importer.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	rows, err := fx.store.ListMetaCommentActionsContext(t.Context(), fx.source.ID, "400")
	require.NoError(err)
	require.Len(rows, 1)
	assert.True(rows[0].ArchiveComplete)
	replyID := message(t, fx.store, fx.source.ID, "501").ID
	assert.Equal(replyID, rows[0].ReplyMessageID.Int64)
	// Repair a row archived by the older importer, even if this replay omits
	// the author and must preserve its previously archived sender.
	_, err = fx.store.DB().Exec(`UPDATE messages SET is_from_me=FALSE,source_is_from_me=FALSE WHERE id=?`, replyID)
	require.NoError(err)
	fx.input.replies[len(fx.input.replies)-1].From = nil
	_, err = fx.importer.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	archived, err := fx.store.GetMessageContext(t.Context(), replyID)
	require.NoError(err)
	assert.True(archived.IsFromMe, "the succeeded receipt proves this exact reply is ours")
	candidates, err := fx.store.LoadActivityCandidatesByIDContext(t.Context(), []int64{replyID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(store.DirectionOutbound, store.ClassifyActivityCandidate(candidates[0], store.DefaultMaxDirectActivityCounterparts).Direction)
	_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: replyID, Action: ActionHide, DryRun: true})
	_ = actionCode(t, err, "not_hideable")
	assert.Equal(1, fx.graph.calls)
	queue, err := fx.store.ListCommentQueueContext(t.Context(), store.CommentQueueFilter{MessageTypes: []string{InstagramSourceType}, SourceID: fx.source.ID})
	require.NoError(err)
	var found bool
	for _, item := range queue {
		assert.NotEqual("501", item.CommentID, "a sent reply is not incoming work")
		if item.MessageID == fx.id {
			found = true
			assert.True(item.PageReplied)
		}
	}
	assert.True(found, "original comment remains available for triage")
}

func TestInstagramReplyEvidenceSurvivesSourceMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := instagramActionFixture(t)
	got, err := replyRun(t, fx.actions, fx.id, ActionPublicReply, "Example reply")
	require.NoError(err)
	fx.input.replies = append(fx.input.replies, IGComment{ID: "501", Text: "Example reply", Timestamp: "2000-01-02T05:00:00+0000", ParentID: "400", From: &IGCommentFromUser{ID: "200", Username: "example_business"}})
	_, err = fx.importer.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(err)
	replyID := message(t, fx.store, fx.source.ID, "501").ID
	into, err := fx.store.GetOrCreateSource(InstagramSourceType, InstagramSourceIdentifier("9", "101", "201"))
	require.NoError(err)
	_, err = fx.store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: fx.source.ID, IntoSourceID: into.ID})
	require.NoError(err)
	rows, err := fx.store.ListMessageMetaCommentActionsContext(t.Context(), fx.id)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(got.Receipt.ID, rows[0].ID)
	assert.True(rows[0].ArchiveComplete)
	assert.Equal(replyID, rows[0].ReplyMessageID.Int64)
	queue, err := fx.store.ListCommentQueueContext(t.Context(), store.CommentQueueFilter{MessageTypes: []string{InstagramSourceType}, SourceID: into.ID})
	require.NoError(err)
	var found bool
	for _, item := range queue {
		assert.NotEqual(replyID, item.MessageID, "the archived sent reply stays out of incoming work")
		if item.MessageID == fx.id {
			found = true
			assert.True(item.PageReplied)
		}
	}
	assert.True(found)
	assert.Equal(1, fx.graph.calls, "a source merge does not repeat provider writes")
}
func TestInstagramPrivateReplyUsesIGUser(t *testing.T) {
	assert := assert.New(t)
	fx := instagramActionFixture(t)
	fx.graph.response = `{"recipient_id":"opaque-600","message_id":"private-501"}`
	got, err := replyRun(t, fx.actions, fx.id, ActionPrivateReply, "Private reply")
	require.NoError(t, err)
	assert.Equal("/v26.0/200/messages", fx.graph.path)
	assert.JSONEq(`{"recipient":{"comment_id":"400"},"message":{"text":"Private reply"}}`, fx.graph.body)
	assert.Equal("opaque-600", got.Receipt.RecipientID)
}
func TestInstagramPermissionMissingList(t *testing.T) {
	fx := instagramActionFixture(t)
	fx.graph.status, fx.graph.response = 403, `{"error":{"code":200}}`
	_, err := replyRun(t, fx.actions, fx.id, ActionPrivateReply, "Private reply")
	failure := actionCode(t, err, "permission_missing")
	assert.Contains(t, failure.RequiredPermissions, "instagram_manage_comments")
	assert.Contains(t, failure.RequiredPermissions, "instagram_manage_messages")
}
func TestInstagramActionRefusesNonOwnedProvenance(t *testing.T) {
	require := require.New(t)
	fx := instagramActionFixture(t)
	saved, err := fx.store.GetMessageMetadataContext(t.Context(), fx.id)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
	metadata.Organic, metadata.Ad = false, true
	data, err := json.Marshal(metadata)
	require.NoError(err)
	require.NoError(fx.store.SetMessageMetadataContext(t.Context(), fx.id, sql.NullString{String: string(data), Valid: true}))
	_, err = fx.actions.Run(t.Context(), ActionRequest{MessageID: fx.id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	_ = actionCode(t, err, "unsupported_source")
	assert.Zero(t, fx.graph.calls)
}
