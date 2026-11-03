package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionsDisabledProfileAllowsReceiptResolution(t *testing.T) {
	for _, kind := range []ActionKind{ActionHide, ActionUnhide, ActionDelete} {
		for _, resolution := range []string{"sent", "not-sent"} {
			t.Run(string(kind)+"/"+resolution, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				_, _, id, actions, graph := actionFixture(t)
				graph.status = 500
				unknown, err := actionRun(t, actions, id, kind)
				_ = actionCode(t, err, "outcome_unknown")
				require.NotNil(unknown.Receipt)
				allowedChecks := 0
				actions.opts.Allowed = func(int64, ActionKind) (bool, error) {
					allowedChecks++
					return false, nil
				}
				actions.opts.OpenAPI = nil
				resolved, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Confirm: unknown.Confirm, ResolveUnknown: resolution})
				require.NoError(err)
				require.NotNil(resolved.Receipt)
				want := "succeeded"
				if resolution == "not-sent" {
					want = "failed"
				}
				assert.Equal(want, resolved.Receipt.State)
				assert.Equal(unknown.Receipt.ID, resolved.Receipt.ID)
				assert.Zero(allowedChecks, "local recovery must not require provider-write opt-ins")
				assert.Equal(1, graph.count(), "local recovery must not dispatch again")
			})
		}
	}
}

func TestActionsDisabledProfileRecoveryRetainsConfirmationAndSourceLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	graph.status = 500
	unknown, err := actionRun(t, actions, id, ActionDelete)
	_ = actionCode(t, err, "outcome_unknown")
	actions.opts.Allowed = nil
	actions.opts.OpenAPI = nil
	request := ActionRequest{MessageID: id, Action: ActionDelete, ResolveUnknown: "sent"}
	_, err = actions.Run(t.Context(), request)
	_ = actionCode(t, err, "confirmation_mismatch")
	request.Confirm = unknown.Confirm
	lease, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	t.Cleanup(func() { _ = lease.Release() })
	_, err = actions.Run(t.Context(), request)
	_ = actionCode(t, err, "sync_active")
	require.NoError(lease.Release())
	resolved, err := actions.Run(t.Context(), request)
	require.NoError(err)
	assert.Equal("succeeded", resolved.Receipt.State)
	assert.Equal(1, graph.count())
}
