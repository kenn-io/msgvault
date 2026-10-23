package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func stalenessTestApply(t *testing.T, st *Store, personID int64, slug, value, suffix string, age time.Duration) *personfacts.GenerationResult {
	t.Helper()
	claim := personFactProjectionClaim(personID, projectionTargetBySlug(t, st, slug), `"`+value+`"`, suffix)
	claim.Evidence[0].EventTime = personFactLedgerNow.Add(-age)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, suffix, []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(t, err)
	return result
}

func stalenessTestItem(t *testing.T, st *Store, personID int64, slug string, asOf time.Time) PersonFactStaleness {
	t.Helper()
	items, err := st.PersonFactStalenessContext(t.Context(), personID, asOf, true)
	require.NoError(t, err)
	for _, item := range items {
		if item.Slug == slug {
			return item
		}
	}
	require.FailNow(t, "staleness item not found", slug)
	return PersonFactStaleness{}
}

func TestPersonFactStalenessVolatileAfterHorizon(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "stale-horizon", 100*24*time.Hour)
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow.Add(90*24*time.Hour))
	assert.True(t, item.Stale)
	assert.Equal(t, personfacts.StabilityVolatile, item.Stability)
	require.NotNil(t, item.VerifyBy)
	assert.Equal(t, personFactLedgerNow.Add(80*24*time.Hour), *item.VerifyBy)
	item = stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.False(t, item.Stale)
}

func stalenessTestTwoSupports(t *testing.T) (*Store, int64, string) {
	t.Helper()
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "support-old", 100*24*time.Hour)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "support-new", 10*24*time.Hour)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{Limit: 10})
	require.NoError(t, err)
	for _, item := range evidence {
		if item.Input.SourceURL == "https://example.com/support-new" {
			return st, personID, item.Key
		}
	}
	require.FailNow(t, "new evidence not found")
	return nil, 0, ""
}

func TestPersonFactStalenessUsesNewestSupportedEvidence(t *testing.T) {
	st, personID, key := stalenessTestTwoSupports(t)
	initial := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.Equal(t, personFactLedgerNow.Add(-10*24*time.Hour), initial.NewestSupport)
	input := personFactProjectionInput(personID, "delete-new-support", nil, []personfacts.EvidenceStatusChange{{EvidenceKey: key, SourceVersion: "source-v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}})
	input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(t, err)
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.Equal(t, personFactLedgerNow.Add(-100*24*time.Hour), item.NewestSupport)
}

func TestPersonFactStalenessFollowsGenerationChronology(t *testing.T) {
	st, personID, key := stalenessTestTwoSupports(t)
	for _, tc := range []struct {
		suffix    string
		offset    time.Duration
		supported bool
		reason    personfacts.EvidenceStatusReason
	}{
		{"delete-newer-first", 2 * time.Hour, false, personfacts.EvidenceStatusSourceDeleted},
		{"reimport-older-late", time.Hour, true, personfacts.EvidenceStatusSourceReimported},
	} {
		input := personFactProjectionInput(personID, tc.suffix, nil, []personfacts.EvidenceStatusChange{{EvidenceKey: key, SourceVersion: "source-v1", Supported: tc.supported, Reason: tc.reason}})
		input.ResolvedAt = personFactLedgerNow.Add(tc.offset)
		if tc.supported {
			// The late re-import also carries an older claim, so this generation
			// has a distinct resolver input while testing status chronology.
			claim := personFactProjectionClaim(personID, projectionTargetBySlug(t, st, AttributeSlugInterestsFunNow), `"climbing"`, "reimport-older-claim")
			claim.Evidence[0].EventTime = personFactLedgerNow.Add(-120 * 24 * time.Hour)
			input.Claims = []personfacts.ProposedClaim{claim}
		}
		_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
		require.NoError(t, err)
	}
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.Equal(t, personFactLedgerNow.Add(-100*24*time.Hour), item.NewestSupport)
}

func stalenessTestDeclared(t *testing.T, st *Store, personID int64, slug, value string, activeFrom time.Time) {
	t.Helper()
	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: slug, Value: AttributeValue{Type: AttributeValueText, Text: &value},
		ActiveFrom: &activeFrom, Source: ProvenanceUser,
	})
	require.NoError(t, err)
}

func TestPersonFactStalenessDeclaredValueUsesActiveFrom(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	activeFrom := personFactLedgerNow.AddDate(-3, 0, 0)
	stalenessTestDeclared(t, st, personID, AttributeSlugLocation, "Porto", activeFrom)
	item := stalenessTestItem(t, st, personID, AttributeSlugLocation, personFactLedgerNow)
	assert.True(t, item.Declared)
	assert.True(t, item.Stale)
	assert.Equal(t, activeFrom, item.NewestSupport)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugLocation})
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.Equal(t, "Porto", *values[0].Value.Text)
}

func TestPersonFactStalenessFixedNeverStale(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugBirthplace, "Porto", "fixed-ancient", 10*365*24*time.Hour)
	item := stalenessTestItem(t, st, personID, AttributeSlugBirthplace, personFactLedgerNow)
	assert.False(t, item.Stale)
	assert.Nil(t, item.VerifyBy)
}

func TestPersonFactStalenessAgedVolatileValueStaysCurrent(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "aging-initial", 10*24*time.Hour)
	asOf := personFactLedgerNow.Add(200 * 24 * time.Hour)
	claim := personFactProjectionClaim(personID, projectionTargetBySlug(t, st, AttributeSlugInterestsFunNow), `"sailing"`, "aging-later")
	claim.Evidence[0].EventTime = asOf.Add(-190 * 24 * time.Hour)
	claim.Evidence[0].RecordedTime = asOf
	input := personFactProjectionInput(personID, "aging-later", []personfacts.ProposedClaim{claim}, nil)
	input.ResolvedAt = asOf
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(t, err)
	for _, decision := range result.Decisions {
		assert.NotEqual(t, personfacts.DecisionApplied, decision.Action)
	}
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugInterestsFunNow})
	require.NoError(t, err)
	require.Len(t, values, 1)
	assert.Equal(t, "climbing", *values[0].Value.Text)
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, asOf)
	assert.True(t, item.Stale)
	assert.Equal(t, `"climbing"`, string(item.Value))
}

func TestPersonFactStalenessHidesSensitiveByDefault(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestDeclared(t, st, personID, AttributeSlugReligion, "fixture faith", personFactLedgerNow.AddDate(-3, 0, 0))
	items, err := st.PersonFactStalenessContext(t.Context(), personID, personFactLedgerNow, false)
	require.NoError(t, err)
	assert.Empty(t, items)
	items, err = st.PersonFactStalenessContext(t.Context(), personID, personFactLedgerNow, true)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, AttributeSlugReligion, items[0].Slug)
}

func TestPersonFactStalenessNeverWrites(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "read-only", 100*24*time.Hour)
	before := personFactProjectionCounts(t, st)
	_, err := st.PersonFactStalenessContext(t.Context(), personID, personFactLedgerNow.AddDate(1, 0, 0), true)
	require.NoError(t, err)
	assert.Equal(t, before, personFactProjectionCounts(t, st))
	_, err = st.PersonFactStalenessContext(t.Context(), personID+100000, personFactLedgerNow, true)
	assert.ErrorIs(t, err, ErrPersonNotFound)
}
