package meta

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
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
	st, src, id, a, g := actionFixture(t)
	a.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	a.opts.OpenAPI = func(context.Context, *store.Source) (ActionAPI, error) {
		assert.Fail(t, "preview read credentials")
		return nil, errors.New("unavailable")
	}
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{32}$`, got.Confirm)
	assert.Equal(t, "100_200_300", got.Disclosure.CommentID)
	assert.Zero(t, g.count())
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	require.NoError(t, err)
	assert.Empty(t, rows)
}
func TestActionsHideFacebookExactRequest(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.Equal(t, "POST", g.method)
	assert.Equal(t, "/v26.0/100_200_300", g.path)
	assert.Equal(t, "true", g.form)
}
func TestActionsUnhide(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	require.NoError(t, err)
	assert.Equal(t, "false", g.form)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	require.NoError(t, err)
	assert.Equal(t, 2, g.count())
}
func TestActionsDeleteRequiresConfirm(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete})
	actionCode(t, err, "confirmation_mismatch")
	assert.Zero(t, g.count())
	_, err = actionRun(t, a, id, ActionDelete)
	require.NoError(t, err)
	assert.Equal(t, "DELETE", g.method)
	var deleted bool
	require.NoError(t, st.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", id).Scan(&deleted))
	assert.True(t, deleted)
	_, err = actionRun(t, a, id, ActionDelete)
	require.NoError(t, err)
	assert.Equal(t, 1, g.count(), "repeat delete only repairs local evidence")
}
func TestActionsNotEnabled(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	a.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	a.opts.OpenAPI = func(context.Context, *store.Source) (ActionAPI, error) {
		assert.Fail(t, "denied action read credentials")
		return nil, nil
	}
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "action_not_enabled")
	assert.Zero(t, g.count())
}
func TestActionsCredentialUnavailableNoReceipt(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	a.opts.OpenAPI = func(context.Context, *store.Source) (ActionAPI, error) { return nil, errors.New("missing env") }
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "credential_unavailable")
	assert.Zero(t, g.count())
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	require.NoError(t, err)
	assert.Empty(t, rows)
}
func TestActionsSyncActive(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	lock, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(t, err)
	defer lock.Release()
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "sync_active")
	assert.Zero(t, g.count())
}
func TestActionsStaleBindingRefused(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	require.NoError(t, err)
	raw, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(t, err)
	var metadata objectMetadata
	require.NoError(t, json.Unmarshal([]byte(raw.String), &metadata))
	metadata.PageID = "101"
	data, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: string(data), Valid: true}))
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, Confirm: preview.Confirm})
	actionCode(t, err, "stale_identity")
	assert.Zero(t, g.count())
}
func TestActionsRootPostRefused(t *testing.T) {
	st, src, _, a, g := actionFixture(t)
	root := message(t, st, src.ID, "100_200").ID
	_, err := a.Run(t.Context(), ActionRequest{MessageID: root, Action: ActionHide})
	actionCode(t, err, "not_a_comment")
	assert.Zero(t, g.count())
}
func TestActionsRetiredSourceNoProviderCall(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	other, err := st.GetOrCreateSource(SourceType, SourceIdentifier("9", "101"))
	require.NoError(t, err)
	_, err = st.DB().Exec("INSERT INTO source_settings(source_id,merged_into_source_id) VALUES (?,?)", src.ID, other.ID)
	require.NoError(t, err)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "stale_identity")
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
	actionCode(t, err, "rate_limited")
	require.NotNil(t, got.Receipt)
	assert.Equal(t, "failed", got.Receipt.State)
	assert.Equal(t, 1, g.count())
}
func TestActionsServerErrorUnknown(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	g.response = `{}`
	got, err := actionRun(t, a, id, ActionDelete)
	actionCode(t, err, "outcome_unknown")
	assert.Equal(t, "unknown", got.Receipt.State)
	_, err = actionRun(t, a, id, ActionDelete)
	actionCode(t, err, "outcome_unknown")
	assert.Equal(t, 1, g.count())
}
func TestActionsCancelBeforeDispatchNoCall(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Run(ctx, ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "cancelled_before_dispatch")
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
	st, _, id, a, g := actionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	open := a.opts.OpenAPI
	a.opts.OpenAPI = func(ctx context.Context, s *store.Source) (ActionAPI, error) {
		api, err := open(ctx, s)
		return cancelAcceptedAPI{api, cancel}, err
	}
	preview, err := a.Run(ctx, ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	require.NoError(t, err)
	got, err := a.Run(ctx, ActionRequest{MessageID: id, Action: ActionDelete, Confirm: preview.Confirm})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.Equal(t, 1, g.count())
	var deleted bool
	require.NoError(t, st.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", id).Scan(&deleted))
	assert.True(t, deleted)
}
func TestActionsDeleteRetryRepairsMark(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	_, err := st.DB().Exec("CREATE TRIGGER fail_delete_mark BEFORE UPDATE OF deleted_from_source_at ON messages BEGIN SELECT RAISE(FAIL,'synthetic local mark failure'); END")
	require.NoError(t, err)
	got, err := actionRun(t, a, id, ActionDelete)
	actionCode(t, err, "remote_accepted_local_failed")
	assert.Equal(t, "succeeded", got.Receipt.State)
	_, err = st.DB().Exec("DROP TRIGGER fail_delete_mark")
	require.NoError(t, err)
	_, err = actionRun(t, a, id, ActionDelete)
	require.NoError(t, err)
	assert.Equal(t, 1, g.count())
}
func TestActionsDeleteUnknownResolveSentMarksDeleted(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	g.status = 500
	_, err := actionRun(t, a, id, ActionDelete)
	actionCode(t, err, "outcome_unknown")
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, DryRun: true})
	require.NoError(t, err)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionDelete, Confirm: preview.Confirm, ResolveUnknown: "sent"})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.Equal(t, 1, g.count())
	var deleted bool
	require.NoError(t, st.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", id).Scan(&deleted))
	assert.True(t, deleted)
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
	actionCode(t, err, "sync_active")
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
				st, src, id, a, g := actionFixture(t)
				target, err := ResolveTarget(t.Context(), st, id)
				require.NoError(t, err)
				key := fmt.Sprintf("%x", sha256.Sum256([]byte(actionBinding(target, kind, ""))))
				if kind == ActionHide {
					key += "-abandoned-attempt"
				}
				row, _, err := st.ClaimMetaCommentActionContext(t.Context(), store.MetaCommentActionClaim{ActionKey: key, SourceID: src.ID, SourceIdentifier: src.Identifier, MessageID: id, TargetCommentID: "100_200_300", Action: string(kind)})
				require.NoError(t, err)
				if state == "dispatched" {
					row, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
					require.NoError(t, err)
				}
				got, err := actionRun(t, a, id, kind)
				wantCode, wantState := "cancelled_before_dispatch", "failed"
				if state == "dispatched" {
					wantCode, wantState = "outcome_unknown", "unknown"
				}
				actionCode(t, err, wantCode)
				require.NotNil(t, got.Receipt)
				assert.Equal(t, wantState, got.Receipt.State)
				assert.Zero(t, g.count())
				if state == "dispatched" {
					preview, previewErr := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, DryRun: true})
					require.NoError(t, previewErr)
					resolved, resolveErr := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Confirm: preview.Confirm, ResolveUnknown: "sent"})
					require.NoError(t, resolveErr)
					assert.Equal(t, "succeeded", resolved.Receipt.State)
					assert.Zero(t, g.count())
				}
			})
		}
	}
}

func TestActionsRejectionReceiptWriteFailureIsLocalFailure(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	g.status, g.response = 400, `{"error":{"code":4}}`
	_, err := st.DB().Exec("CREATE TRIGGER fail_receipt_finish BEFORE UPDATE ON meta_comment_actions WHEN NEW.state IN ('failed','succeeded','unknown') BEGIN SELECT RAISE(FAIL,'synthetic receipt failure'); END")
	require.NoError(t, err)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "local_failed")
	require.NotNil(t, got.Receipt)
	assert.Equal(t, "dispatched", got.Receipt.State)
	assert.Equal(t, 1, g.count())
}

func TestActionsHideUnhideUpdatesSharedMetadata(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	a.opts.Now = func() time.Time { return now }
	original, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(t, err)
	var metadata objectMetadata
	require.NoError(t, json.Unmarshal([]byte(original.String), &metadata))
	metadata.BodyTime = now.Add(-time.Hour).Unix()
	data, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: string(data), Valid: true}))
	for _, kind := range []ActionKind{ActionHide, ActionUnhide} {
		_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind})
		require.NoError(t, err)
		saved, err := st.GetMessageMetadataContext(t.Context(), id)
		require.NoError(t, err)
		var got objectMetadata
		require.NoError(t, json.Unmarshal([]byte(saved.String), &got))
		assert.Equal(t, kind == ActionHide, got.Hidden)
		assert.Equal(t, now.Unix(), got.StateTime)
		assert.Equal(t, metadata.BodyTime, got.BodyTime)
		assert.True(t, got.matches("9", "100", "", "100_200"))
		now = now.Add(time.Second)
	}
	assert.Equal(t, 2, g.count())
}
func TestActionsHideResolveSentRepairsMetadata(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	g.status = 500
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	actionCode(t, err, "outcome_unknown")
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: "sent"})
	require.NoError(t, err)
	saved, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(t, err)
	var got objectMetadata
	require.NoError(t, json.Unmarshal([]byte(saved.String), &got))
	assert.True(t, got.Hidden)
	assert.Positive(t, got.StateTime)
	assert.Equal(t, 1, g.count())
}

func TestActionsReceiptRecoveryEvidence(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnhide})
	actionCode(t, err, "outcome_unknown")
	require.NotNil(t, got.Receipt)
	assert.Equal(t, ActionUnhide, got.Receipt.Action)
	assert.Equal(t, "outcome_unknown", got.Receipt.ErrorCode)
	assert.False(t, got.Receipt.CreatedAt.IsZero())
	assert.True(t, !got.Receipt.UpdatedAt.Before(got.Receipt.CreatedAt))
}
