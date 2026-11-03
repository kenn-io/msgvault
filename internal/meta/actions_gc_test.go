package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestActionsUnknownResolutionSurvivesGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	graph.status, graph.response = 500, `{}`
	unknown, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "outcome_unknown")
	require.NotNil(unknown.Receipt)
	graph.status, graph.response = 200, `{"success":true}`
	_, err = actionRun(t, actions, id, ActionDelete)
	require.NoError(err)
	require.NoError(st.MarkMessageDeleted(src.ID, "100_200"))

	plan, err := st.PlanGCContext(t.Context())
	require.NoError(err)
	_, err = st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	resolved, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide, ResolveUnknown: "not-sent"})
	require.NoError(err, "cleanup must retain the target and parent needed for local resolution")
	require.NotNil(resolved.Receipt)
	assert.Equal(unknown.Receipt.ID, resolved.Receipt.ID)
	assert.Equal("failed", resolved.Receipt.State)
	assert.Equal(2, graph.count(), "resolution must not contact the provider")

	plan, err = st.PlanGCContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(2), plan.SourceDeleted, "resolved receipts no longer retain deleted rows")
	deleted, err := st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Equal(int64(2), deleted)
}

func TestActionsDeleteRetainsExistingRepliesThroughGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	fixture, client := newGraphFixture(t)
	fixture.comments["100_200"] = []Object{*comment("100_200_300", "100_200", "Parent comment")}
	fixture.comments["100_200_300"] = []Object{*comment("100_200_301", "100_200_300", "Earlier reply")}
	fixture.comments["100_200_301"] = []Object{*comment("100_200_302", "100_200_301", "Surviving reply")}
	_, err := NewImporter(st, client).Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	middle := message(t, st, src.ID, "100_200_301").ID
	child := message(t, st, src.ID, "100_200_302").ID
	for _, target := range []int64{id, middle} {
		_, err = actionRun(t, actions, target, ActionDelete)
		require.NoError(err)
	}
	plan, err := st.PlanGCContext(t.Context())
	require.NoError(err)
	assert.Zero(plan.SourceDeleted, "surviving replies still need their archived ancestor chain")
	_, err = st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)

	queue, err := st.ListCommentQueueContext(t.Context(), store.CommentQueueFilter{SourceID: src.ID})
	require.NoError(err)
	require.Len(queue, 1)
	assert.Equal(child, queue[0].MessageID)
	assert.Equal(middle, queue[0].ParentMessageID)
	_, err = st.SetCommentTriageContext(t.Context(), child, store.CommentTriageHandled, nil)
	require.NoError(err)
	_, err = actions.Run(t.Context(), ActionRequest{MessageID: child, Action: ActionHide})
	require.NoError(err, "GC must preserve the reply's moderation identity")
	assert.Equal(3, graph.count())

	_, err = actionRun(t, actions, child, ActionDelete)
	require.NoError(err)
	plan, err = st.PlanGCContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(3), plan.SourceDeleted, "deleted chains become purgeable once no replies survive")
	deleted, err := st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Equal(int64(3), deleted)
}
