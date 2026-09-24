package deletion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceReferenceForTargets(t *testing.T) {
	tests := []struct {
		name    string
		targets []query.DeletionTarget
		want    SourceReference
		wantErr error
	}{
		{name: "empty", wantErr: ErrNoDeletionTargets},
		{
			name: "incomplete",
			targets: []query.DeletionTarget{
				{MessageID: 1, SourceID: 1, SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-1"},
			},
			wantErr: ErrIncompleteDeletionSource,
		},
		{
			name: "multiple sources",
			targets: []query.DeletionTarget{
				{MessageID: 1, SourceID: 1, SourceType: "gmail", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-1"},
				{MessageID: 2, SourceID: 2, SourceType: "imap", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-2"},
			},
			wantErr: ErrMultipleDeletionSources,
		},
		{
			name: "one source",
			targets: []query.DeletionTarget{
				{MessageID: 1, SourceID: 7, SourceType: "gmail", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-1"},
				{MessageID: 2, SourceID: 7, SourceType: "gmail", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-2"},
			},
			want: SourceReference{ID: 7, Type: "gmail", Identifier: "user@example.invalid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SourceReferenceForTargets(tt.targets)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSourceMessageIDs(t *testing.T) {
	targets := []query.DeletionTarget{
		{SourceMessageID: "remote-2"},
		{SourceMessageID: "remote-1"},
	}

	assert.Equal(t, []string{"remote-2", "remote-1"}, SourceMessageIDs(targets))
}

func TestResolveSourceReferenceUsesEffectiveTypeAndExactIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	legacy, err := st.GetOrCreateSource("", "legacy@example.invalid")
	require.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "legacy@example.invalid")
	require.NoError(err)

	got, err := ResolveSourceReference(st, SourceReference{
		ID: legacy.ID, Type: "gmail", Identifier: legacy.Identifier,
	})
	require.NoError(err)
	assert.Equal(legacy.ID, got.ID)
	assert.Empty(got.SourceType)
}

func TestResolveSourceReferenceFallsBackToUniqueEffectiveType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	legacy, err := st.GetOrCreateSource("", "legacy@example.invalid")
	require.NoError(err)

	got, err := ResolveSourceReference(st, SourceReference{
		ID: legacy.ID + 1000, Type: "gmail", Identifier: legacy.Identifier,
	})
	require.NoError(err)
	assert.Equal(legacy.ID, got.ID)
	assert.Empty(got.SourceType)
}

func TestResolveSourceReferenceRejectsAmbiguousEffectiveTypeFallback(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	legacy, err := st.GetOrCreateSource("", "shared@example.invalid")
	require.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "shared@example.invalid")
	require.NoError(err)

	_, err = ResolveSourceReference(st, SourceReference{
		ID: legacy.ID + 1000, Type: "gmail", Identifier: legacy.Identifier,
	})
	require.Error(err)
	require.ErrorContains(err, "ambiguous")
}

func TestResolveSourceReferenceReturnsNotFoundForOtherProvider(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("imap", "imap@example.invalid")
	require.NoError(err)

	_, err = ResolveSourceReference(st, SourceReference{
		ID: 9999, Type: "gmail", Identifier: "imap@example.invalid",
	})
	require.Error(err)
	assert.ErrorIs(err, store.ErrSourceNotFound)
}
