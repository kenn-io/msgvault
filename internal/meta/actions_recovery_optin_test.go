package meta

import (
	"encoding/json"
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

func TestActionsSentResolutionCanRetryFailedLocalMetadataWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, actions, graph := actionFixture(t)
	graph.status = 500
	unknown, err := actionRun(t, actions, id, ActionHide)
	_ = actionCode(t, err, "outcome_unknown")
	require.NotNil(unknown.Receipt)

	_, err = st.DB().Exec("CREATE TRIGGER fail_action_metadata BEFORE UPDATE OF metadata ON messages BEGIN SELECT RAISE(FAIL,'synthetic metadata failure'); END")
	require.NoError(err)
	request := ActionRequest{MessageID: id, Action: ActionHide, Confirm: unknown.Confirm, ResolveUnknown: "sent"}
	_, err = actions.Run(t.Context(), request)
	require.Error(err)
	_, err = st.DB().Exec("DROP TRIGGER fail_action_metadata")
	require.NoError(err)

	// The receipt can now be repaired locally, even after provider writes are disabled.
	actions.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	actions.opts.OpenAPI = nil
	recovered, err := actions.Run(t.Context(), request)
	require.NoError(err)
	require.NotNil(recovered.Receipt)
	assert.Equal("succeeded", recovered.Receipt.State)
	assert.Equal(unknown.Receipt.ID, recovered.Receipt.ID)
	assert.Equal(1, graph.count(), "local recovery must not dispatch the provider action again")

	saved, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
	require.NotNil(metadata.Hidden)
	assert.True(*metadata.Hidden)
}

func TestActionsAcceptedHideCanRetryFailedMetadataWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, actions, graph := actionFixture(t)
	_, err := st.DB().Exec("CREATE TRIGGER fail_action_metadata BEFORE UPDATE OF metadata ON messages BEGIN SELECT RAISE(FAIL,'synthetic metadata failure'); END")
	require.NoError(err)

	first, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionHide})
	_ = actionCode(t, err, "remote_accepted_local_failed")
	require.NotNil(first.Receipt)
	assert.Equal("dispatched", first.Receipt.State)
	assert.Equal(1, graph.count())

	_, err = st.DB().Exec("DROP TRIGGER fail_action_metadata")
	require.NoError(err)
	actions.opts.Allowed = func(int64, ActionKind) (bool, error) { return false, nil }
	actions.opts.OpenAPI = nil

	recovered, err := actions.Run(t.Context(), ActionRequest{
		MessageID:      id,
		Action:         ActionHide,
		ResolveUnknown: "sent",
	})
	require.NoError(err)
	require.NotNil(recovered.Receipt)
	assert.Equal("succeeded", recovered.Receipt.State)
	assert.Equal(1, graph.count(), "local recovery must not dispatch the provider action again")

	saved, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var metadata objectMetadata
	require.NoError(json.Unmarshal([]byte(saved.String), &metadata))
	require.NotNil(metadata.Hidden)
	assert.True(*metadata.Hidden)
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
