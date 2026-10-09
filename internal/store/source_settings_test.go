package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceSettingsAliasLifecycleAtomic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "recovered-example")
	require.NoError(err)
	other, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	alias := "History"
	history := true
	settings, err := st.UpdateSourceSettingsContext(ctx, source.ID, store.SourceSettingsUpdate{Alias: &alias, HistoryOnly: &history})
	require.NoError(err)
	assert.Equal(alias, settings.Alias)
	assert.True(settings.HistoryOnly)
	matches, err := st.GetSourcesByIdentifierOrDisplayName("history")
	require.NoError(err)
	require.Len(matches, 1)
	assert.Equal(source.ID, matches[0].ID)
	assert.Equal("recovered-example", matches[0].Identifier)
	_, err = st.UpdateSourceSettingsContext(ctx, other.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.Error(err)
	_, err = st.GetOrCreateSource("mbox", "HISTORY")
	require.ErrorIs(err, store.ErrSourceSettingsInvalid, "new provider identifiers cannot shadow archive aliases")
	bad := " bad "
	history = false
	_, err = st.UpdateSourceSettingsContext(ctx, source.ID, store.SourceSettingsUpdate{Alias: &bad, HistoryOnly: &history})
	require.Error(err)
	settings, err = st.GetSourceSettingsContext(ctx, source.ID)
	require.NoError(err)
	assert.True(settings.HistoryOnly, "invalid combined requests are atomic")
	require.NoError(st.SetArchiveMarker(ctx, store.BeeperReanchorMarkerKey(source.ID), "synthetic mismatch"))
	settings, err = st.UpdateSourceSettingsContext(ctx, source.ID, store.SourceSettingsUpdate{AcceptReanchor: true, HistoryOnly: &history})
	require.NoError(err)
	assert.False(settings.HistoryOnly)
	assert.False(settings.ReanchorRequired)
	assert.Equal(alias, settings.Alias)
	_, marked, err := st.GetArchiveMarker(ctx, store.BeeperReanchorMarkerKey(source.ID))
	require.NoError(err)
	assert.False(marked)
}

func TestUpdateSourceSettingsAliasRejectsOtherDisplayName(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("imap", "source-one.example.test")
	require.NoError(err)
	second, err := st.GetOrCreateSource("imap", "source-two.example.test")
	require.NoError(err)

	displayName := "Legacy Mail"
	_, err = st.UpdateSourceSettingsContext(t.Context(), second.ID, store.SourceSettingsUpdate{DisplayName: &displayName})
	require.NoError(err)

	alias := "lEgAcY mAiL"
	_, err = st.UpdateSourceSettingsContext(t.Context(), first.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.ErrorIs(err, store.ErrSourceSettingsInvalid)
}

func TestSourceSettingsAliasAllowsOtherSourceWithoutDisplayName(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "source-one.example.test")
	require.NoError(err)
	_, err = st.GetOrCreateSource("imap", "source-two.example.test")
	require.NoError(err)

	alias := "History"
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.NoError(err)

	matches, err := st.GetSourcesByIdentifierOrDisplayName("history")
	require.NoError(err)
	require.Len(matches, 1)
	require.Equal(source.ID, matches[0].ID)
}

func TestSourceDisplayNameUpdatesRejectOtherSelectors(t *testing.T) {
	tests := []struct {
		name   string
		update func(context.Context, *store.Store, int64, string) error
	}{
		{
			name: "display name API",
			update: func(ctx context.Context, st *store.Store, sourceID int64, displayName string) error {
				return st.UpdateSourceDisplayNameContext(ctx, sourceID, displayName)
			},
		},
		{
			name: "source settings API",
			update: func(ctx context.Context, st *store.Store, sourceID int64, displayName string) error {
				_, err := st.UpdateSourceSettingsContext(ctx, sourceID, store.SourceSettingsUpdate{DisplayName: &displayName})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			first, err := st.GetOrCreateSource("imap", "source-one.example.test")
			require.NoError(err)
			second, err := st.GetOrCreateSource("imap", "source-two.example.test")
			require.NoError(err)

			sharedDisplayName := "Shared Source"
			require.NoError(st.UpdateSourceDisplayNameContext(ctx, first.ID, sharedDisplayName))
			require.NoError(st.UpdateSourceDisplayNameContext(ctx, second.ID, "Original Second Source"))
			alias := "History"
			_, err = st.UpdateSourceSettingsContext(ctx, first.ID, store.SourceSettingsUpdate{Alias: &alias})
			require.NoError(err)

			// Duplicate display names remain valid when neither name shadows another selector.
			require.NoError(tt.update(ctx, st, second.ID, sharedDisplayName))
			err = tt.update(ctx, st, second.ID, "hIsToRy")
			require.ErrorIs(err, store.ErrSourceSettingsInvalid)

			err = tt.update(ctx, st, second.ID, strings.ToUpper(first.Identifier))
			require.ErrorIs(err, store.ErrSourceSettingsInvalid)

			updated, err := st.GetSourceByID(second.ID)
			require.NoError(err)
			require.Equal(sharedDisplayName, updated.DisplayName.String)
		})
	}
}

func TestSourceDisplayNamesAllowSharedMailAndTeamsIdentifier(t *testing.T) {
	tests := []struct {
		name       string
		firstType  string
		secondType string
	}{
		{name: "mail then Teams", firstType: "imap", secondType: "teams"},
		{name: "Teams then mail", firstType: "teams", secondType: "imap"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			const email = "shared@example.test"

			first, err := st.GetOrCreateSource(test.firstType, email)
			require.NoError(err)
			require.NoError(st.UpdateSourceDisplayNameContext(t.Context(), first.ID, email))

			second, err := st.GetOrCreateSource(test.secondType, email)
			require.NoError(err)
			require.NoError(st.UpdateSourceDisplayNameContext(t.Context(), second.ID, email))

			matches, err := st.GetSourcesByIdentifierOrDisplayName(email)
			require.NoError(err)
			require.Len(matches, 2)
			assert.ElementsMatch([]int64{first.ID, second.ID}, []int64{matches[0].ID, matches[1].ID})
		})
	}
}

// FuzzSourceAliasResolution exercises allocation and resolution together. The
// case variants must resolve to the same durable source and cannot be allocated
// to a second source, regardless of the alias spelling.
func FuzzSourceAliasResolution(f *testing.F) {
	for _, seed := range []string{"Archive", "history-2026", "Recovered chat", "a'b", "Source:42"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, alias string) {
		require := require.New(t)
		assert := assert.New(t)
		if len(alias) > 80 {
			t.Skip()
		}
		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("beeper", "provider-one.example.test")
		require.NoError(err)
		other, err := st.GetOrCreateSource("beeper", "provider-two.example.test")
		require.NoError(err)
		_, err = st.UpdateSourceSettingsContext(context.Background(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
		if err != nil {
			return
		}
		matches, err := st.GetSourcesByIdentifierOrDisplayName(strings.ToLower(alias))
		require.NoError(err)
		require.Len(matches, 1)
		assert.Equal(source.ID, matches[0].ID)
		_, err = st.UpdateSourceSettingsContext(context.Background(), other.ID, store.SourceSettingsUpdate{Alias: &alias})
		require.Error(err)
	})
}

func TestSourceSettingsRejectInvalidUpdates(t *testing.T) {
	checks := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("mbox", "archive@example.test")
	checks.NoError(err)
	for _, value := range []string{"", " ", "a\nb", "a\x00b", "archive@example.test "} {
		t.Run(value, func(t *testing.T) {
			_, err := st.UpdateSourceSettingsContext(context.Background(), source.ID, store.SourceSettingsUpdate{Alias: &value})
			require.New(t).Error(err)
		})
	}
	_, err = st.UpdateSourceSettingsContext(context.Background(), source.ID, store.SourceSettingsUpdate{AcceptReanchor: true})
	checks.Error(err)
	_, err = st.UpdateSourceSettingsContext(context.Background(), source.ID, store.SourceSettingsUpdate{})
	checks.Error(err)
	_, err = st.GetSourceSettingsContext(context.Background(), 99999)
	checks.ErrorIs(err, store.ErrSourceNotFound)
}

func TestUpdateSourceIdentifierCannotShadowArchiveAlias(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	owner, err := st.GetOrCreateSource("imap", "alias-owner@example.test")
	require.NoError(err)
	renamed, err := st.GetOrCreateSource("imap", "renamed@example.test")
	require.NoError(err)
	alias := "Archive-Example"
	_, err = st.UpdateSourceSettingsContext(t.Context(), owner.ID, store.SourceSettingsUpdate{Alias: &alias})
	require.NoError(err)
	require.ErrorIs(st.UpdateSourceIdentifier(renamed.ID, "archive-example"), store.ErrSourceSettingsInvalid)
	source, err := st.GetSourceByID(renamed.ID)
	require.NoError(err)
	assert.Equal("renamed@example.test", source.Identifier)
	require.NoError(st.UpdateSourceIdentifier(owner.ID, "archive-example"), "a source can retain its own alias")
}
