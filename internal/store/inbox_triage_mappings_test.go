package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type triageMappingStore interface {
	InboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity) (map[string]string, int64, error)
	ReplaceInboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expectedRevision int64, principal inboxcontrol.Principal) (int64, error)
}

func TestInboxTriageMappingsRevisionAndScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, receipt := inboxReceiptFixture(t)
	source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
	bindings, ok := any(f.Store).(triageMappingStore)
	requirements.True(ok, "native owner mappings require durable source-bound revision checks")
	owner := inboxcontrol.Principal{ID: "owner-fixture", Owner: true}
	got, revision, err := bindings.InboxTriageMappings(t.Context(), source)
	requirements.NoError(err)
	assertions.Empty(got)
	assertions.Zero(revision)
	want := map[string]string{"todo": "Label_1", "watch": "Label_2"}
	revision, err = bindings.ReplaceInboxTriageMappings(t.Context(), source, want, 0, owner)
	requirements.NoError(err)
	assertions.Equal(int64(1), revision)
	got, revision, err = bindings.InboxTriageMappings(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(want, got)
	assertions.Equal(int64(1), revision)
	got["todo"] = "caller-edit"
	got, _, err = bindings.InboxTriageMappings(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(want, got, "caller-owned maps cannot change stored configuration")
	_, err = bindings.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_3"}, 0, owner)
	require.ErrorIs(t, err, inboxcontrol.ErrConflict)
	foreign := source
	foreign.AccountID = "other@example.com"
	_, _, err = bindings.InboxTriageMappings(t.Context(), foreign)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	_, err = bindings.ReplaceInboxTriageMappings(t.Context(), foreign, want, 1, owner)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	revision, err = bindings.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{}, 1, owner)
	requirements.NoError(err)
	assertions.Equal(int64(2), revision)
	got, revision, err = bindings.InboxTriageMappings(t.Context(), source)
	requirements.NoError(err)
	assertions.Empty(got)
	assertions.Equal(int64(2), revision, "clear must retain the revision against stale creates")
	state, err := f.Store.GetInboxProviderState(t.Context(), receipt.Before.Target)
	requirements.NoError(err)
	assertions.Nil(state, "mapping updates cannot invent provider observations")
}

func TestInboxTriageMappingsRejectDelegateAndInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal inboxcontrol.Principal
		entries   map[string]string
		revision  int64
		want      error
	}{
		{name: "delegate", principal: inboxcontrol.Principal{ID: "delegate-fixture"}, entries: map[string]string{"todo": "Label_1"}, want: inboxcontrol.ErrDenied},
		{name: "missing owner identity", principal: inboxcontrol.Principal{Owner: true}, entries: map[string]string{"todo": "Label_1"}, want: inboxcontrol.ErrDenied},
		{name: "unknown category", principal: inboxcontrol.Principal{ID: "owner-fixture", Owner: true}, entries: map[string]string{"autoarchive": "Label_1"}, want: inboxcontrol.ErrInvalid},
		{name: "empty tag", principal: inboxcontrol.Principal{ID: "owner-fixture", Owner: true}, entries: map[string]string{"todo": ""}, want: inboxcontrol.ErrInvalid},
		{name: "negative revision", principal: inboxcontrol.Principal{ID: "owner-fixture", Owner: true}, entries: map[string]string{"todo": "Label_1"}, revision: -1, want: inboxcontrol.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f, _ := inboxReceiptFixture(t)
			bindings, ok := any(f.Store).(triageMappingStore)
			requirements.True(ok, "native mapping persistence is missing")
			source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
			_, err := bindings.ReplaceInboxTriageMappings(t.Context(), source, tc.entries, tc.revision, tc.principal)
			require.ErrorIs(t, err, tc.want)
			got, revision, err := bindings.InboxTriageMappings(t.Context(), source)
			requirements.NoError(err)
			assertions.Empty(got)
			assertions.Zero(revision)
		})
	}
}

func TestInboxTriageMappingsConcurrentCAS(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, _ := inboxReceiptFixture(t)
	bindings, ok := any(f.Store).(triageMappingStore)
	requirements.True(ok, "native mapping revision CAS is missing")
	source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
	owner := inboxcontrol.Principal{ID: "owner-fixture", Owner: true}
	_, err := bindings.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_1"}, 0, owner)
	requirements.NoError(err)
	outcomes := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := bindings.ReplaceInboxTriageMappings(t.Context(), source, map[string]string{"todo": "Label_2"}, 1, owner)
			outcomes <- err
		})
	}
	wg.Wait()
	close(outcomes)
	succeeded := 0
	for err := range outcomes {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, inboxcontrol.ErrConflict)
		}
	}
	assertions.Equal(1, succeeded)
	got, revision, err := bindings.InboxTriageMappings(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(map[string]string{"todo": "Label_2"}, got)
	assertions.Equal(int64(2), revision)
}
