package meta

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type actionGraph struct {
	mu                 sync.Mutex
	calls              int
	method, path, form string
	status             int
	response           string
	barrier, release   chan struct{}
}

func (f *actionGraph) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func actionFixture(t *testing.T) (*store.Store, *store.Source, int64, *Actions, *actionGraph) {
	t.Helper()
	st, f, imp, src := setupImport(t)
	f.comments["100_200"] = []Object{*comment("100_200_300", "100_200", "Example comment")}
	_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(t, err)
	id := message(t, st, src.ID, "100_200_300").ID
	graph := &actionGraph{status: 200, response: `{"success":true}`}
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.URL.RawQuery)
		assert.NoError(t, r.ParseForm())
		graph.mu.Lock()
		graph.calls++
		graph.method = r.Method
		graph.path = r.URL.Path
		graph.form = r.Form.Get("is_hidden")
		graph.mu.Unlock()
		if graph.barrier != nil {
			close(graph.barrier)
			<-graph.release
		}
		w.WriteHeader(graph.status)
		_, _ = fmt.Fprint(w, graph.response)
	})
	a := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
	return st, src, id, a, graph
}
func actionRun(t *testing.T, a *Actions, id int64, kind ActionKind) (*ActionResult, error) {
	t.Helper()
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, DryRun: true})
	require.NoError(t, err)
	return a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Confirm: preview.Confirm})
}
func actionCode(t *testing.T, err error, want string) *ActionError {
	t.Helper()
	var failure *ActionError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, want, failure.Code)
	return failure
}
func TestActionsDryRunNoNetwork(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, src, id, a, g := actionFixture(t)
	a.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	a.opts.OpenAPI = func(context.Context, *store.Source) (ActionAPI, error) {
		assertions.Fail("preview read credentials")
		return nil, errors.New("unavailable")
	}
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	requirements.NoError(err)
	assertions.Regexp(`^[0-9a-f]{32}$`, got.Confirm)
	assertions.Equal("100_200_300", got.Disclosure.CommentID)
	assertions.Zero(g.count())
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	requirements.NoError(err)
	assertions.Empty(rows)
}
func TestActionsHideFacebookExactRequest(t *testing.T) {
	assert := assert.New(t)
	_, _, id, a, g := actionFixture(t)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	require.NoError(t, err)
	assert.Equal("succeeded", got.Receipt.State)
	assert.Equal("POST", g.method)
	assert.Equal("/v26.0/100_200_300", g.path)
	assert.Equal("true", g.form)
}
func TestActionsUnhide(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, _, id, a, g := actionFixture(t)
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	require.NoError(err)
	assert.Equal("false", g.form)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	require.NoError(err)
	assert.Equal(2, g.count())
}
func TestActionsDeleteRequiresConfirm(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete})
	_ = actionCode(t, err, "confirmation_mismatch")
	assert.Zero(g.count())
	_, err = actionRun(t, a, id, ActionDelete)
	require.NoError(err)
	assert.Equal("DELETE", g.method)
	var deleted bool
	require.NoError(st.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", id).Scan(&deleted))
	assert.True(deleted)
	_, err = actionRun(t, a, id, ActionDelete)
	require.NoError(err)
	assert.Equal(1, g.count(), "repeat delete only repairs local evidence")
}
func TestActionsNotEnabled(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	a.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	a.opts.OpenAPI = func(context.Context, *store.Source) (ActionAPI, error) {
		assert.Fail(t, "denied action read credentials")
		return nil, errors.New("unexpected credential lookup")
	}
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "action_not_enabled")
	assert.Zero(t, g.count())
}
func TestActionsCredentialUnavailableNoReceipt(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	a.opts.OpenAPI = func(context.Context, *store.Source) (ActionAPI, error) { return nil, errors.New("missing env") }
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "credential_unavailable")
	assert.Zero(t, g.count())
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	require.NoError(t, err)
	assert.Empty(t, rows)
}
func TestActionsStaleBindingRefused(t *testing.T) {
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	require.NoError(err)
	raw, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(raw.String), &metadata))
	metadata.PageID = "101"
	data, err := json.Marshal(metadata)
	require.NoError(err)
	require.NoError(st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: string(data), Valid: true}))
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, Confirm: preview.Confirm})
	_ = actionCode(t, err, "stale_identity")
	assert.Zero(t, g.count())
}
func TestActionsRootPostRefused(t *testing.T) {
	st, src, _, a, g := actionFixture(t)
	root := message(t, st, src.ID, "100_200").ID
	_, err := a.Run(t.Context(), ActionRequest{MessageID: root, Action: ActionHide})
	_ = actionCode(t, err, "not_a_comment")
	assert.Zero(t, g.count())
}
func TestActionsPermissionMissingListsRequired(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 403
	g.response = `{"error":{"code":200,"message":"must not escape"}}`
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	failure := actionCode(t, err, "permission_missing")
	assert.Equal(t, []string{"pages_manage_engagement", "pages_read_user_content"}, failure.RequiredPermissions)
	assert.NotContains(t, err.Error(), "must not escape")
}
func TestActionsRateLimitedNoRetry(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 400
	g.response = `{"error":{"code":4}}`
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "rate_limited")
	require.NotNil(t, got.Receipt)
	assert.Equal(t, "failed", got.Receipt.State)
	assert.Equal(t, 1, g.count())
}
func TestActionsServerErrorUnknown(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	g.response = `{}`
	got, err := actionRun(t, a, id, ActionDelete)
	_ = actionCode(t, err, "outcome_unknown")
	assert.Equal(t, "unknown", got.Receipt.State)
	_, err = actionRun(t, a, id, ActionDelete)
	_ = actionCode(t, err, "outcome_unknown")
	assert.Equal(t, 1, g.count())
}
func TestActionsCancelBeforeDispatchNoCall(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Run(ctx, ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "cancelled_before_dispatch")
	assert.Zero(t, g.count())
}

// Cancel after the real HTTP client decoded acceptance, not while its response
// body is in flight (which would correctly have an unknown outcome).
type cancelAcceptedAPI struct {
	ActionAPI

	cancel context.CancelFunc
}

func (c cancelAcceptedAPI) DeleteComment(ctx context.Context, id string) error {
	err := c.ActionAPI.DeleteComment(ctx, id)
	if err == nil {
		c.cancel()
	}
	return err
}
func TestActionsCancelAfterAcceptFinishesReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, _, id, a, g := actionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	open := a.opts.OpenAPI
	a.opts.OpenAPI = func(ctx context.Context, s *store.Source) (ActionAPI, error) {
		api, err := open(ctx, s)
		return cancelAcceptedAPI{api, cancel}, err
	}
	preview, err := a.Run(ctx, ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	requirements.NoError(err)
	got, err := a.Run(ctx, ActionRequest{MessageID: id, Action: ActionDelete, Confirm: preview.Confirm})
	requirements.NoError(err)
	assertions.Equal("succeeded", got.Receipt.State)
	assertions.Equal(1, g.count())
	var deleted bool
	requirements.NoError(st.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", id).Scan(&deleted))
	assertions.True(deleted)
}
func TestActionsDeleteRetryRepairsMark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	_, err := st.DB().Exec("CREATE TRIGGER fail_delete_mark BEFORE UPDATE OF deleted_from_source_at ON messages BEGIN SELECT RAISE(FAIL,'synthetic local mark failure'); END")
	require.NoError(err)
	got, err := actionRun(t, a, id, ActionDelete)
	_ = actionCode(t, err, "remote_accepted_local_failed")
	assert.Equal("succeeded", got.Receipt.State)
	_, err = st.DB().Exec("DROP TRIGGER fail_delete_mark")
	require.NoError(err)
	a.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	a.opts.OpenAPI = nil
	_, err = actionRun(t, a, id, ActionDelete)
	require.NoError(err)
	assert.Equal(1, g.count())
}
func TestActionsDeleteUnknownResolveSentMarksDeleted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	g.status = 500
	_, err := actionRun(t, a, id, ActionDelete)
	_ = actionCode(t, err, "outcome_unknown")
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	require.NoError(err)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, Confirm: preview.Confirm, ResolveUnknown: "sent"})
	require.NoError(err)
	assert.Equal("succeeded", got.Receipt.State)
	assert.Equal(1, g.count())
	var deleted bool
	require.NoError(st.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", id).Scan(&deleted))
	assert.True(deleted)
}
func TestActionsConcurrentSourceLockSyncActive(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.barrier = make(chan struct{})
	g.release = make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(g.release) })
	done := make(chan error, 1)
	go func() { _, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide}); done <- err }()
	<-g.barrier
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	_ = actionCode(t, err, "sync_active")
	assert.Equal(t, 1, g.count())
	once.Do(func() { close(g.release) })
	require.NoError(t, <-done)
}

// The source lock proves these rows belong to an abandoned call. A dispatched
// row must block replay even if the process died before it saved an outcome.
func TestActionsRecoverAbandonedReceipts(t *testing.T) {
	for _, kind := range []ActionKind{ActionHide, ActionDelete} {
		for _, state := range []string{"claimed", "dispatched"} {
			t.Run(string(kind)+"/"+state, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, src, id, a, g := actionFixture(t)
				target, err := ResolveTarget(t.Context(), st, id)
				require.NoError(err)
				key := fmt.Sprintf("%x", sha256.Sum256([]byte(actionBinding(target, kind, ""))))
				if kind == ActionHide {
					key += "-abandoned-attempt"
				}
				row, _, err := st.ClaimMetaCommentActionContext(t.Context(), store.MetaCommentActionClaim{ActionKey: key, SourceID: src.ID, SourceIdentifier: src.Identifier, MessageID: id, TargetCommentID: "100_200_300", Action: string(kind)})
				require.NoError(err)
				if state == "dispatched" {
					_, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
					require.NoError(err)
				}
				got, err := actionRun(t, a, id, kind)
				wantCode, wantState := "cancelled_before_dispatch", "failed"
				if state == "dispatched" {
					wantCode, wantState = "outcome_unknown", "unknown"
				}
				_ = actionCode(t, err, wantCode)
				require.NotNil(got.Receipt)
				assert.Equal(wantState, got.Receipt.State)
				assert.Zero(g.count())
				if state == "dispatched" {
					preview, previewErr := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, DryRun: true})
					require.NoError(previewErr)
					resolved, resolveErr := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Confirm: preview.Confirm, ResolveUnknown: "sent"})
					require.NoError(resolveErr)
					assert.Equal("succeeded", resolved.Receipt.State)
					assert.Zero(g.count())
				}
			})
		}
	}
}

func TestActionsRejectionReceiptWriteFailureIsLocalFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	g.status, g.response = 400, `{"error":{"code":4}}`
	_, err := st.DB().Exec("CREATE TRIGGER fail_receipt_finish BEFORE UPDATE ON meta_comment_actions WHEN NEW.state IN ('failed','succeeded','unknown') BEGIN SELECT RAISE(FAIL,'synthetic receipt failure'); END")
	require.NoError(err)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "local_failed")
	require.NotNil(got.Receipt)
	assert.Equal("dispatched", got.Receipt.State)
	assert.Equal(1, g.count())
}

func TestActionsHideUnhideUpdatesSharedMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, a, g := actionFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	original, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(original.String), &metadata))
	metadata.BodyTime = now.Add(-time.Hour).Unix()
	data, err := json.Marshal(metadata)
	require.NoError(err)
	require.NoError(st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: string(data), Valid: true}))
	for _, kind := range []ActionKind{ActionHide, ActionUnhide} {
		_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind})
		require.NoError(err)
		saved, err := st.GetMessageMetadataContext(t.Context(), id)
		require.NoError(err)
		var got objectMetadata
		require.NoError(json.Unmarshal([]byte(saved.String), &got))
		require.NotNil(got.Hidden)
		assert.Equal(kind == ActionHide, *got.Hidden)
		rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
		require.NoError(err)
		require.NotEmpty(rows)
		require.NotNil(rows[len(rows)-1].DispatchedAt)
		assert.Equal(rows[len(rows)-1].DispatchedAt.Unix(), got.StateTime)
		assert.Equal(metadata.BodyTime, got.BodyTime)
		assert.True(got.matches("9", "100", "", "100_200"))
	}
	assert.Equal(2, g.count())
}
func TestActionsHideResolveSentRepairsMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	g.status = 500
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "outcome_unknown")
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: "sent"})
	require.NoError(err)
	saved, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var got objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &got))
	require.NotNil(got.Hidden)
	assert.True(*got.Hidden)
	assert.Positive(got.StateTime)
	assert.Equal(1, g.count())
}

func TestActionsReceiptRecoveryEvidence(t *testing.T) {
	assert := assert.New(t)
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	_ = actionCode(t, err, "outcome_unknown")
	require.NotNil(t, got.Receipt)
	assert.Equal(ActionUnhide, got.Receipt.Action)
	assert.Equal("outcome_unknown", got.Receipt.ErrorCode)
	assert.False(got.Receipt.CreatedAt.IsZero())
	assert.False(got.Receipt.UpdatedAt.Before(got.Receipt.CreatedAt))
}

func TestActionsRecoveredHidePreservesNewerState(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(strconv.FormatBool(newer), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, _, id, a, g := actionFixture(t)
			g.status = 500
			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
			_ = actionCode(t, err, "outcome_unknown")
			requirements.NotNil(got.Receipt)
			rows, err := st.ListMetaCommentActionsContext(t.Context(), got.SourceID, got.Disclosure.CommentID)
			requirements.NoError(err)
			requirements.Len(rows, 1)
			requirements.NotNil(rows[0].DispatchedAt)
			// Persist an earlier dispatch so newer metadata is still older than
			// resolution, regardless of how quickly the test runs.
			dispatchedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			_, err = st.DB().Exec(st.Rebind(`UPDATE meta_comment_actions SET dispatched_at=? WHERE id=?`), dispatchedAt, rows[0].ID)
			requirements.NoError(err)
			dispatched := dispatchedAt.Unix()
			saved, err := st.GetMessageMetadataContext(t.Context(), id)
			requirements.NoError(err)
			var metadata objectMetadata
			requirements.NoError(json.Unmarshal([]byte(saved.String), &metadata))
			hidden := false
			metadata.Hidden = &hidden
			metadata.StateTime = dispatched - 60
			if newer {
				metadata.StateTime = dispatched + 60
			}
			data, err := json.Marshal(metadata)
			requirements.NoError(err)
			requirements.NoError(st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: string(data), Valid: true}))
			_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: "sent"})
			requirements.NoError(err)
			saved, err = st.GetMessageMetadataContext(t.Context(), id)
			requirements.NoError(err)
			var after objectMetadata
			requirements.NoError(json.Unmarshal([]byte(saved.String), &after))
			requirements.NotNil(after.Hidden)
			assertions.Equal(!newer, *after.Hidden)
			wantTime := dispatched
			if newer {
				wantTime = metadata.StateTime
			}
			assertions.Equal(wantTime, after.StateTime)
			assertions.Equal(1, g.count())
		})
	}
}

func TestActionsResolveUnknownAfterTargetDeleted(t *testing.T) {
	for _, resolution := range []string{"sent", "not-sent"} {
		t.Run(resolution, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, src, id, a, g := actionFixture(t)
			g.status = 500
			unknown, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
			_ = actionCode(t, err, "outcome_unknown")
			require.NoError(st.MarkMessageDeletedContext(t.Context(), src.ID, "100_200_300"))
			preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, DryRun: true})
			require.NoError(err)
			assert.Equal(unknown.Confirm, preview.Confirm)
			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: resolution})
			require.NoError(err)
			require.NotNil(got.Receipt)
			assert.Equal(unknown.Receipt.ID, got.Receipt.ID)
			assert.Equal(1, g.count())
			_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
			_ = actionCode(t, err, "target_deleted")
			assert.Equal(1, g.count())
		})
	}
}

func TestActionsRecoveredHidePreservesLaterSameSecondUnhide(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, a, g := actionFixture(t)
	g.status = 500
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "outcome_unknown")
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	_ = actionCode(t, err, "outcome_unknown")
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	require.NoError(err)
	require.Len(rows, 2)
	second := time.Now().UTC().Truncate(time.Second)
	for i, row := range rows {
		_, err = st.DB().Exec(st.Rebind(`UPDATE meta_comment_actions SET dispatched_at=? WHERE id=?`), second.Add(time.Duration(i+1)*100*time.Millisecond), row.ID)
		require.NoError(err)
	}
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide, ResolveUnknown: "sent"})
	require.NoError(err)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: "sent"})
	require.NoError(err)
	saved, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var got objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &got))
	require.NotNil(got.Hidden)
	assert.False(*got.Hidden)
	assert.Equal(2, g.count())
}
