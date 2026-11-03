package meta

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestActionsHistoryOnlySourceRefusesFreshDispatch(t *testing.T) {
	for _, kind := range []ActionKind{ActionHide, ActionUnhide, ActionDelete} {
		t.Run(string(kind), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, src, id, actions, graph := actionFixture(t)
			_, err := st.DB().Exec("INSERT INTO source_settings(source_id,history_only) VALUES (?,TRUE)", src.ID)
			require.NoError(err)
			preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, DryRun: true})
			require.NoError(err)
			opened := false
			openAPI := actions.opts.OpenAPI
			actions.opts.OpenAPI = func(ctx context.Context, source *store.Source) (ActionAPI, error) {
				opened = true
				return openAPI(ctx, source)
			}
			_, err = actions.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Confirm: preview.Confirm})
			_ = actionCode(t, err, "stale_identity")
			assert.False(opened, "history-only sources must be refused before credentials are opened")
			assert.Zero(graph.count())
			rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
			require.NoError(err)
			assert.Empty(rows)
		})
	}
}

func TestActionsHistoryOnlySourceAllowsReceiptResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	graph.status = 500
	_, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "outcome_unknown")
	_, err = st.DB().Exec("INSERT INTO source_settings(source_id,history_only) VALUES (?,TRUE)", src.ID)
	require.NoError(err)
	actions.opts.OpenAPI = nil
	preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, DryRun: true})
	require.NoError(err)
	resolved, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, Confirm: preview.Confirm, ResolveUnknown: "sent"})
	require.NoError(err)
	require.NotNil(resolved.Receipt)
	assert.Equal("succeeded", resolved.Receipt.State)
	assert.Equal(1, graph.count())
}
