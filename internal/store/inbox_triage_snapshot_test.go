package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// These checks catch stale incoming evidence, binding bypasses, and confusing
// the daemon's own tag reconciliation with arrival of a new message.
func TestInboxTriageSnapshotExactTargetsAndRevisions(t *testing.T) {
	f, source, states := inboxCandidateFixture(t)
	owner := inboxcontrol.Principal{ID: "owner-fixture", Owner: true}
	_, err := f.Store.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_1"}, 0, owner)
	require.NoError(t, err)
	targets := []inboxcontrol.Target{states[0].Target, states[3].Target}
	before, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	require.NoError(t, err)
	require.Len(t, before.Candidates, 2)
	assert.Equal(t, targets[0], before.Candidates[0].State.Target)
	assert.Equal(t, targets[1], before.Candidates[1].State.Target)
	assert.Equal(t, map[string]string{"todo": "Label_1"}, before.Mappings)
	assert.Equal(t, int64(1), before.MappingRevision)
	require.Len(t, before.ArchiveRevision, 64)
	require.Len(t, before.IncomingWatermark, 64)
	changed := states[0]
	changed.Tags = []string{"INBOX", "Label_1"}
	changed.ObservedAt = changed.ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), changed)
	require.NoError(t, err)
	after, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	require.NoError(t, err)
	assert.NotEqual(t, before.ArchiveRevision, after.ArchiveRevision)
	assert.Equal(t, before.IncomingWatermark, after.IncomingWatermark)
	assert.Equal(t, []string{"INBOX", "Label_1"}, after.Candidates[0].State.Tags)
	f.CreateMessage("new-incoming")
	incoming, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	require.NoError(t, err)
	assert.NotEqual(t, after.IncomingWatermark, incoming.IncomingWatermark)
	assert.NotEqual(t, after.ArchiveRevision, incoming.ArchiveRevision)
	_, err = f.Store.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_2"}, 1, owner)
	require.NoError(t, err)
	mapped, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	require.NoError(t, err)
	assert.Equal(t, int64(2), mapped.MappingRevision)
	assert.Equal(t, map[string]string{"todo": "Label_2"}, mapped.Mappings)
}

func TestInboxTriageSnapshotRejectsUnavailableAndForeignTargets(t *testing.T) {
	f, source, states := inboxCandidateFixture(t)
	for _, tc := range []struct {
		name    string
		targets []inboxcontrol.Target
		want    error
	}{
		{"empty", nil, inboxcontrol.ErrInvalid},
		{"too many", make([]inboxcontrol.Target, 101), inboxcontrol.ErrInvalid},
		{"duplicate", []inboxcontrol.Target{states[0].Target, states[0].Target}, inboxcontrol.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Store.InboxTriageSnapshot(t.Context(), source, tc.targets)
			assert.ErrorIs(t, err, tc.want)
		})
	}
	foreign := states[0].Target
	foreign.AccountID = "foreign@example.com"
	_, err := f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{foreign})
	assert.ErrorIs(t, err, inboxcontrol.ErrDenied)
	wrong := states[0].Target
	wrong.ProviderID = "wrong-provider-id"
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{wrong})
	assert.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
	unknown := states[0]
	unknown.Read = nil
	unknown.ObservedAt = unknown.ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), unknown)
	require.NoError(t, err)
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{unknown.Target})
	assert.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	archived := states[1]
	archived.Inbox = new(false)
	archived.ObservedAt = archived.ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), archived)
	require.NoError(t, err)
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{archived.Target})
	assert.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
}

func TestInboxTriageSnapshotSelectsHundredExplicitItemsFromLargerInbox(t *testing.T) {
	f, source, states := inboxCandidateFixture(t)
	targets := make([]inboxcontrol.Target, 100)
	for i := range targets {
		state := states[0]
		state.Target.ProviderID = fmt.Sprintf("selected-%d", i)
		state.Target.ItemID = f.CreateMessage(state.Target.ProviderID)
		_, err := f.Store.ObserveInboxState(t.Context(), state)
		require.NoError(t, err)
		targets[i] = state.Target
	}
	got, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	require.NoError(t, err)
	require.Len(t, got.Candidates, 100)
	for i, candidate := range got.Candidates {
		assert.Equal(t, targets[i], candidate.State.Target)
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), time.Now(), targets[50].ItemID)
	require.NoError(t, err)
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	assert.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
}

func TestInboxTriageSnapshotDoesNotReadBodiesAndHonorsCancellation(t *testing.T) {
	f, source, states := inboxCandidateFixture(t)
	_, err := f.Store.DB().Exec(`DROP TABLE message_bodies`)
	require.NoError(t, err)
	got, err := f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{states[0].Target})
	require.NoError(t, err)
	require.Len(t, got.Candidates, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.InboxTriageSnapshot(ctx, source, []inboxcontrol.Target{states[0].Target})
	assert.ErrorIs(t, err, context.Canceled)
}
