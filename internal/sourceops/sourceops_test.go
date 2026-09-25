package sourceops_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestResolveExactOneRequiresExactlyOneSelector(t *testing.T) {
	f := storetest.New(t)

	tests := []struct {
		name     string
		selector sourceops.Selector
		want     string
	}{
		{name: "missing", selector: sourceops.Selector{}, want: "account or source ID is required"},
		{name: "negative ID", selector: sourceops.Selector{SourceID: -1}, want: "source ID must be positive"},
		{name: "explicit zero ID", selector: sourceops.Selector{SourceIDSet: true}, want: "source ID must be positive"},
		{
			name: "account and ID",
			selector: sourceops.Selector{
				Account: "test@example.com", SourceID: f.Source.ID,
			},
			want: "mutually exclusive",
		},
		{
			name: "ID and type",
			selector: sourceops.Selector{
				SourceID: f.Source.ID, SourceType: "gmail",
			},
			want: "mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sourceops.ResolveExactOne(f.Store, tt.selector)
			require.Error(t, err)
			assert.Equal(t, opserr.KindInvalid, opserr.KindOf(err))
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestResolveExactOneTreatsNumericAccountAsToken(t *testing.T) {
	f := storetest.New(t)
	numeric, err := f.Store.GetOrCreateSource("synctech-sms", "15551234567")
	require.NoError(t, err)

	got, err := sourceops.ResolveExactOne(f.Store, sourceops.Selector{Account: "15551234567"})
	require.NoError(t, err)
	assert.Equal(t, numeric.ID, got.ID)
}

func TestResolveExactOneRejectsAmbiguityWithoutFirstMatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	gmail, err := f.Store.GetOrCreateSource("gmail", "shared@example.com")
	require.NoError(err)
	_, err = f.Store.GetOrCreateSource("imap", "shared@example.com")
	require.NoError(err)

	_, err = sourceops.ResolveExactOne(f.Store, sourceops.Selector{Account: "shared@example.com"})
	require.ErrorContains(err, "ambiguous")
	assert.Equal(opserr.KindInvalid, opserr.KindOf(err))

	got, err := sourceops.ResolveExactOne(f.Store, sourceops.Selector{SourceID: gmail.ID})
	require.NoError(err)
	assert.Equal(gmail.ID, got.ID)
}

func TestResolveExactOneTypeFilterStillRejectsSameTypeDisplayCollision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	first, err := f.Store.GetOrCreateSource("gmail", "first@example.com")
	require.NoError(err)
	second, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
	require.NoError(err)
	require.NoError(f.Store.UpdateSourceDisplayName(first.ID, "Work"))
	require.NoError(f.Store.UpdateSourceDisplayName(second.ID, "Work"))

	_, err = sourceops.ResolveExactOne(f.Store, sourceops.Selector{
		Account: "Work", SourceType: "gmail",
	})
	require.Error(err)
	assert.Equal(opserr.KindInvalid, opserr.KindOf(err))
	assert.ErrorContains(err, "ambiguous")
}

func TestResolveAccountFamilyExpandsRelatedCalendars(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	primary, err := f.Store.GetOrCreateSource("gmail", "family@example.com")
	require.NoError(err)
	require.NoError(f.Store.UpdateSourceDisplayName(primary.ID, "Family"))
	related := createCalendarSource(t, f, "family@example.com/primary", "family@example.com")
	other := createCalendarSource(t, f, "other@example.com/primary", "other@example.com")

	selection, err := sourceops.ResolveAccountFamily(f.Store, sourceops.Selector{Account: "Family"})
	require.NoError(err)
	require.NotNil(selection.Primary)
	assert.Equal(primary.ID, selection.Primary.ID)
	assert.ElementsMatch([]int64{primary.ID, related.ID}, sourceIDs(selection.Sources))
	assert.NotContains(sourceIDs(selection.Sources), other.ID)
}

func TestResolveAccountFamilySupportsCalendarOnlyAccount(t *testing.T) {
	f := storetest.New(t)
	calendar := createCalendarSource(t, f, "calendar-only/primary", "calendar.only@example.com")

	selection, err := sourceops.ResolveAccountFamily(f.Store, sourceops.Selector{
		Account: "Calendar.Only@Example.com",
	})
	require.NoError(t, err)
	assert.Nil(t, selection.Primary)
	assert.Equal(t, []int64{calendar.ID}, sourceIDs(selection.Sources))
}

func TestResolveAllMatchesPreservesIntentionalFanout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	gmail, err := f.Store.GetOrCreateSource("gmail", "fanout@example.com")
	require.NoError(err)
	imap, err := f.Store.GetOrCreateSource("imap", "fanout@example.com")
	require.NoError(err)

	selection, err := sourceops.ResolveAllMatches(f.Store, sourceops.Selector{Account: "fanout@example.com"})
	require.NoError(err)
	assert.ElementsMatch([]int64{gmail.ID, imap.ID}, sourceIDs(selection.Sources))
}

func TestExplicitSourceIDIsExactForEveryCardinality(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	primary, err := f.Store.GetOrCreateSource("gmail", "exact@example.com")
	require.NoError(err)
	_ = createCalendarSource(t, f, "exact@example.com/primary", "exact@example.com")

	exact, err := sourceops.ResolveExactOne(f.Store, sourceops.Selector{SourceID: primary.ID})
	require.NoError(err)
	assert.Equal(primary.ID, exact.ID)

	family, err := sourceops.ResolveAccountFamily(f.Store, sourceops.Selector{SourceID: primary.ID})
	require.NoError(err)
	assert.Equal([]int64{primary.ID}, sourceIDs(family.Sources))

	all, err := sourceops.ResolveAllMatches(f.Store, sourceops.Selector{SourceID: primary.ID})
	require.NoError(err)
	assert.Equal([]int64{primary.ID}, sourceIDs(all.Sources))
}

func createCalendarSource(
	t *testing.T,
	f *storetest.Fixture,
	identifier string,
	account string,
) *store.Source {
	t.Helper()
	source, err := f.Store.GetOrCreateSource("gcal", identifier)
	require.NoError(t, err)
	config, err := json.Marshal(map[string]string{
		"account_email": account,
		"calendar_id":   identifier,
	})
	require.NoError(t, err)
	require.NoError(t, f.Store.UpdateSourceSyncConfig(source.ID, string(config)))
	return source
}

func sourceIDs(sources []*store.Source) []int64 {
	ids := make([]int64, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.ID)
	}
	return ids
}
