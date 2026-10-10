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
	assertions := assert.New(t)
	requirements := require.New(t)

	f, source, states := inboxCandidateFixture(t)
	owner := inboxcontrol.Principal{ID: "owner-fixture", Owner: true}
	_, err := f.Store.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_1"}, 0, owner)
	requirements.NoError(err)
	targets := []inboxcontrol.Target{states[0].Target, states[3].Target}
	before, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	requirements.NoError(err)
	requirements.Len(before.Candidates, 2)
	assertions.Equal(targets[0], before.Candidates[0].State.Target)
	assertions.Equal(targets[1], before.Candidates[1].State.Target)
	assertions.Equal(map[string]string{"todo": "Label_1"}, before.Mappings)
	assertions.Equal(int64(1), before.MappingRevision)
	requirements.Len(before.ArchiveRevision, 64)
	requirements.Len(before.IncomingWatermark, 64)
	changed := states[0]
	changed.Tags = []string{"INBOX", "Label_1"}
	changed.ObservedAt = changed.ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), changed)
	requirements.NoError(err)
	after, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	requirements.NoError(err)
	assertions.NotEqual(before.ArchiveRevision, after.ArchiveRevision)
	assertions.Equal(before.IncomingWatermark, after.IncomingWatermark)
	assertions.Equal([]string{"INBOX", "Label_1"}, after.Candidates[0].State.Tags)
	f.CreateMessage("new-incoming")
	incoming, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	requirements.NoError(err)
	assertions.NotEqual(after.IncomingWatermark, incoming.IncomingWatermark)
	assertions.NotEqual(after.ArchiveRevision, incoming.ArchiveRevision)
	_, err = f.Store.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_2"}, 1, owner)
	requirements.NoError(err)
	mapped, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	requirements.NoError(err)
	assertions.Equal(int64(2), mapped.MappingRevision)
	assertions.Equal(map[string]string{"todo": "Label_2"}, mapped.Mappings)
}

func TestInboxTriageSnapshotRejectsUnavailableAndForeignTargets(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

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
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	wrong := states[0].Target
	wrong.ProviderID = "wrong-provider-id"
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{wrong})
	require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
	unknown := states[0]
	unknown.Read = nil
	unknown.ObservedAt = unknown.ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), unknown)
	requirements.NoError(err)
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{unknown.Target})
	require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
	archived := states[1]
	archived.Inbox = new(false)
	archived.ObservedAt = archived.ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), archived)
	requirements.NoError(err)
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{archived.Target})
	assertions.ErrorIs(err, inboxcontrol.ErrPlanChanged)
}

func TestInboxTriageSnapshotSelectsHundredExplicitItemsFromLargerInbox(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, source, states := inboxCandidateFixture(t)
	targets := make([]inboxcontrol.Target, 100)
	for i := range targets {
		state := states[0]
		state.Target.ProviderID = fmt.Sprintf("selected-%d", i)
		state.Target.ItemID = f.CreateMessage(state.Target.ProviderID)
		_, err := f.Store.ObserveInboxState(t.Context(), state)
		requirements.NoError(err)
		targets[i] = state.Target
	}
	got, err := f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	requirements.NoError(err)
	requirements.Len(got.Candidates, 100)
	for i, candidate := range got.Candidates {
		assertions.Equal(targets[i], candidate.State.Target)
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), time.Now(), targets[50].ItemID)
	requirements.NoError(err)
	_, err = f.Store.InboxTriageSnapshot(t.Context(), source, targets)
	assertions.ErrorIs(err, inboxcontrol.ErrPlanChanged)
}

func TestInboxTriageSnapshotDoesNotReadBodiesAndHonorsCancellation(t *testing.T) {
	requirements := require.New(t)

	f, source, states := inboxCandidateFixture(t)
	_, err := f.Store.DB().Exec(`DROP TABLE message_bodies`)
	requirements.NoError(err)
	got, err := f.Store.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{states[0].Target})
	requirements.NoError(err)
	requirements.Len(got.Candidates, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.InboxTriageSnapshot(ctx, source, []inboxcontrol.Target{states[0].Target})
	assert.ErrorIs(t, err, context.Canceled)
}
