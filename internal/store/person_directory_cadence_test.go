package store_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A stale projection must never produce a trustworthy due-list entry.
func TestDirectoryCadenceFiltersAndCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newActivityQueryFixture(t)
	last := time.Now().UTC().AddDate(0, 0, -30).Truncate(time.Second)
	seedProjectedActivity(t, f, personID, []time.Time{last})
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
		Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	page, err := f.Store.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "overdue", Sort: "most_overdue"})
	require.NoError(err)
	require.Len(page.People, 1)
	person := page.People[0]
	assert.Equal(personID, person.ID)
	assert.Equal("A", person.Cadence.List)
	assert.Equal(&days, person.Cadence.FrequencyDays)
	require.NotNil(person.Cadence.DueAt)
	assert.Equal(last.AddDate(0, 0, 21), *person.Cadence.DueAt)
	assert.Equal("email", person.LastContactChannel)
	assert.Equal(store.CadenceOverdue, person.Cadence.Status)
	assert.False(person.Cadence.Stale)
	require.NotNil(person.Cadence.DaysOverdue)
	assert.GreaterOrEqual(*person.Cadence.DaysOverdue, 9)
	require.NotNil(page.CadenceCoverage)
	assert.Equal(1, page.CadenceCoverage.Scheduled)
	assert.Len(page.CadencePresets, 4)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE person_contact_state SET dirty_at = CURRENT_TIMESTAMP WHERE person_id = ?`), personID)
	require.NoError(err)
	page, err = f.Store.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "overdue"})
	require.NoError(err)
	assert.Empty(page.People)
	assert.Equal(1, page.CadenceCoverage.Unknown)
	page, err = f.Store.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "unknown"})
	require.NoError(err)
	require.Len(page.People, 1)
	assert.True(page.People[0].Cadence.Stale)
	assert.Nil(page.People[0].Cadence.DueAt)
}

func TestDirectoryCadenceNoListAndUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	p := createDirectoryPerson(t, st, "Example", "cadence@example.test", "friend", "inactive", "Example Co")
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "no_list"})
	require.NoError(err)
	assert.Equal([]int64{p.ID}, directoryPersonIDs(page.People))
	for _, days := range []int64{0, 30} {
		_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: p.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
		require.NoError(err)
		page, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Sort: "next_due"})
		require.NoError(err)
		require.Len(page.People, 1)
		assert.Empty(page.People[0].Cadence.List)
		assert.Nil(page.People[0].Cadence.DueAt)
		if days == 0 {
			assert.Equal(1, page.CadenceCoverage.NoList)
		} else {
			assert.Equal(1, page.CadenceCoverage.Unknown)
		}
	}
}

func TestDirectoryCadenceFilterKeepsNonMatchingCandidates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	listed := createDirectoryPerson(t, st, "Listed", "listed-cadence@example.test", "friend", "inactive", "Example Co")
	unlisted := createDirectoryPerson(t, st, "Unlisted", "unlisted-cadence@example.test", "friend", "inactive", "Example Co")
	days := int64(21)
	_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: listed.ID, DefinitionSlug: store.AttributeSlugContactFrequency,
		Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "no_list"})
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal(unlisted.ID, page.People[0].ID)
}

func TestDirectoryCadenceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	createDirectoryPerson(t, st, "Alpha", "cadence-alpha@example.test", "friend", "inactive", "Example Co")
	createDirectoryPerson(t, st, "Beta", "cadence-beta@example.test", "friend", "inactive", "Example Co")
	q := store.DirectoryPeopleQuery{Sort: "next_due", Limit: 1}
	first, err := st.DirectoryPeoplePageContext(t.Context(), q)
	require.NoError(err)
	require.NotEmpty(first.NextCursor)
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	require.NoError(err)
	var cursor map[string]any
	require.NoError(json.Unmarshal(raw, &cursor))
	assert.NotEmpty(cursor["evaluated_at"])
	q.Cursor = first.NextCursor
	second, err := st.DirectoryPeoplePageContext(t.Context(), q)
	require.NoError(err)
	require.Len(second.People, 1)
	assert.NotEqual(first.People[0].ID, second.People[0].ID)
	q.Cadence = "no_list"
	_, err = st.DirectoryPeoplePageContext(t.Context(), q)
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE persons SET display_name = ? WHERE id = ?`), "Zed", first.People[0].ID)
	require.NoError(err)
	q.Cadence = ""
	_, err = st.DirectoryPeoplePageContext(t.Context(), q)
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
}

func TestDirectoryCadenceValidation(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, tc := range []struct {
		cadence string
		days    int
		valid   bool
	}{{"due", 0, true}, {"due", 3660, true}, {"due", -1, false}, {"due", 3661, false}, {"overdue", 1, false}, {"", 0, false}, {"bad", 0, false}} {
		_, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: tc.cadence, DueWithinDays: &tc.days})
		if tc.valid {
			assert.NoError(t, err)
		} else {
			assert.ErrorIs(t, err, store.ErrInvalidDirectoryQuery)
		}
	}
}

func TestDirectoryCadenceRejectsConflictingAndMalformedFrequency(t *testing.T) {
	st := testutil.NewTestStore(t)
	p := createDirectoryPerson(t, st, "Example", "invalid-cadence@example.test", "friend", "inactive", "Example Co")
	var definitionID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM attribute_definitions WHERE universal_id = ?`), store.AttributeUniversalIDContactFrequency).Scan(&definitionID))
	for _, tc := range []struct {
		name    string
		column  string
		value   any
		ordinal int
	}{{"custom", "value_integer", 30, 0}, {"conflict", "value_integer", 21, 1}, {"malformed", "value_text", "bad", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			if tc.name == "malformed" {
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM person_attribute_values WHERE person_id = ?`), p.ID)
				require.NoError(err)
			}
			column := tc.column // Fixed test-owned column names; values remain bound.
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_attribute_values (person_id,definition_id,ordinal,`+column+`,source) VALUES (?,?,?,?,'user')`), p.ID, definitionID, tc.ordinal, tc.value)
			require.NoError(err)
			page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "no_list"})
			require.NoError(err)
			if tc.name == "custom" {
				assert.Empty(page.People)
			} else {
				assert.Equal([]int64{p.ID}, directoryPersonIDs(page.People))
			}
		})
	}
}

// A cursor's evaluation instant, rather than the wall clock of page two,
// determines status. An older valid cursor exercises a crossed due boundary
// without sleeping or adding a test-only clock to Store.
func TestDirectoryCadenceCursorRetainsEvaluationInstant(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, firstID := newActivityQueryFixture(t)
	participant := f.EnsureParticipant("second-cadence@example.test", "Second Example", "example.test")
	second, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	last := time.Now().UTC().AddDate(0, 0, -40).Truncate(time.Second)
	for _, id := range []int64{firstID, second.ID} {
		seedProjectedActivity(t, f, id, []time.Time{last})
		days := int64(21)
		_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: id, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
		require.NoError(err)
	}
	revisions, err := f.Store.ContactRevisionsContext(t.Context())
	require.NoError(err)
	seedActivityReconciledEpoch(t, f.Store, revisions)
	q := store.DirectoryPeopleQuery{Sort: "next_due", Limit: 1}
	first, err := f.Store.DirectoryPeoplePageContext(t.Context(), q)
	require.NoError(err)
	require.NotEmpty(first.NextCursor)
	raw, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	require.NoError(err)
	var cursor map[string]any
	require.NoError(json.Unmarshal(raw, &cursor))
	cursor["evaluated_at"] = last.AddDate(0, 0, 20).Format(time.RFC3339Nano)
	raw, err = json.Marshal(cursor)
	require.NoError(err)
	q.Cursor = base64.RawURLEncoding.EncodeToString(raw)
	page, err := f.Store.DirectoryPeoplePageContext(t.Context(), q)
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal(store.CadenceOK, page.People[0].Cadence.Status)
	assert.Nil(page.People[0].Cadence.DaysOverdue)
}

func TestContactListFirstCreationIsCompareAndSwap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newActivityQueryFixture(t)
	zero, days := int64(0), int64(21)
	input := store.PersonAttributeValueInput{PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser, ExpectedValueID: &zero}
	first, err := f.Store.SetPersonAttributeValueContext(t.Context(), input)
	require.NoError(err)
	require.NotNil(first.Value)
	days = 60
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), input)
	var conflict *store.AttributeValueConflictError
	require.ErrorAs(err, &conflict)
	require.NotNil(conflict.CurrentValue)
	assert.Equal(first.Value.ID, conflict.CurrentValue.ID)
	assert.Equal(int64(21), *conflict.CurrentValue.Value.Integer)
}

func TestDirectoryCadenceUnrepresentableIntervalIsAssigned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newActivityQueryFixture(t)
	for _, days := range []int64{366*10000 + 1, 1<<63 - 1} {
		_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
		require.NoError(err)
		page, err := f.Store.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cadence: "unknown"})
		require.NoError(err)
		require.Len(page.People, 1)
		assert.Equal(&days, page.People[0].Cadence.FrequencyDays)
		assert.Equal(0, page.CadenceCoverage.NoList)
	}
}
