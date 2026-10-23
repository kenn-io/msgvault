package store

import (
	"encoding/json"
	"strings"
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
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "stale-horizon", 100*24*time.Hour)
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow.Add(90*24*time.Hour))
	assert.True(item.Stale)
	assert.Equal(personfacts.StabilityVolatile, item.Stability)
	require.NotNil(item.VerifyBy)
	assert.Equal(personFactLedgerNow.Add(80*24*time.Hour), *item.VerifyBy)
	item = stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.False(item.Stale)
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
	assert := assert.New(t)
	require := require.New(t)
	st, personID, key := stalenessTestTwoSupports(t)
	initial := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.Equal(personFactLedgerNow.Add(-10*24*time.Hour), initial.NewestSupport)
	input := personFactProjectionInput(personID, "delete-new-support", nil, []personfacts.EvidenceStatusChange{{EvidenceKey: key, SourceVersion: "source-v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}})
	input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.Equal(personFactLedgerNow.Add(-100*24*time.Hour), item.NewestSupport)
}

func TestPersonFactStalenessIgnoresClaimsOutsideValidity(t *testing.T) {
	targets := []struct {
		name       string
		slug       string
		employment bool
	}{
		{name: "attribute", slug: AttributeSlugInterestsFunNow},
		{name: "employment", slug: "employment", employment: true},
	}
	futureFrom := personFactLedgerNow.Add(2 * 24 * time.Hour)
	expiredUntil := personFactLedgerNow.Add(-2 * 24 * time.Hour)
	validityCases := []struct {
		name         string
		inactiveAsOf time.Time
		activeAsOf   time.Time
		resolvedAt   time.Time
		eventTime    time.Time
		validFrom    *time.Time
		validUntil   *time.Time
	}{
		{
			name:         "not yet valid",
			inactiveAsOf: futureFrom.Add(-time.Nanosecond),
			activeAsOf:   futureFrom,
			resolvedAt:   futureFrom,
			eventTime:    personFactLedgerNow.Add(-24 * time.Hour),
			validFrom:    &futureFrom,
		},
		{
			name:         "expired",
			inactiveAsOf: expiredUntil,
			activeAsOf:   expiredUntil.Add(-time.Nanosecond),
			resolvedAt:   expiredUntil.Add(-24 * time.Hour),
			eventTime:    expiredUntil.Add(-48 * time.Hour),
			validUntil:   &expiredUntil,
		},
	}
	for _, targetCase := range targets {
		for _, validityCase := range validityCases {
			t.Run(targetCase.name+"/"+validityCase.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, personID, _ := newPersonFactProjectionStore(t)
				if targetCase.employment {
					createPersonFactOrganization(t, st, "Example Incorporated", "example.com")
				}
				target := projectionTargetBySlug(t, st, targetCase.slug)
				makeClaim := func(suffix string, eventTime, recordedTime time.Time) personfacts.ProposedClaim {
					submitted := `"climbing"`
					if targetCase.employment {
						value, err := json.Marshal(personfacts.EmploymentValue{
							Organization: personfacts.OrganizationReference{Name: "Example Incorporated"},
							Title:        "Engineer",
							StartDate:    &personfacts.PartialDateValue{Year: 2010},
						})
						require.NoError(err)
						submitted = string(value)
					}
					claim := personFactProjectionClaim(personID, target, submitted, suffix)
					claim.Evidence[0].EventTime = eventTime
					claim.Evidence[0].RecordedTime = recordedTime
					return claim
				}

				oldEventTime := personFactLedgerNow.Add(-1000 * 24 * time.Hour)
				oldResolvedAt := oldEventTime.Add(24 * time.Hour)
				baseline := makeClaim("validity-old", oldEventTime, oldResolvedAt)
				baselineInput := personFactProjectionInput(personID, "validity-old", []personfacts.ProposedClaim{baseline}, nil)
				baselineInput.ResolvedAt = oldResolvedAt
				result, err := st.ApplyPersonFactGenerationContext(t.Context(), baselineInput, nil)
				require.NoError(err)
				require.Len(result.Projections, 1)

				claim := makeClaim("validity-recent-"+strings.ReplaceAll(validityCase.name, " ", "-"), validityCase.eventTime, validityCase.resolvedAt)
				claim.ValidFrom = validityCase.validFrom
				claim.ValidUntil = validityCase.validUntil
				input := personFactProjectionInput(personID,
					"validity-recent-"+strings.ReplaceAll(validityCase.name, " ", "-"),
					[]personfacts.ProposedClaim{claim}, nil)
				input.ResolvedAt = validityCase.resolvedAt
				_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
				require.NoError(err)

				inactive := stalenessTestItem(t, st, personID, targetCase.slug, validityCase.inactiveAsOf)
				assert.True(inactive.Stale)
				assert.Equal(oldEventTime, inactive.NewestSupport)

				active := stalenessTestItem(t, st, personID, targetCase.slug, validityCase.activeAsOf)
				assert.False(active.Stale)
				assert.Equal(validityCase.eventTime, active.NewestSupport)
			})
		}
	}
}

func TestPersonFactStalenessFollowsGenerationChronology(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
		require.NoError(err)
	}
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow)
	assert.Equal(personFactLedgerNow.Add(-100*24*time.Hour), item.NewestSupport)
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
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	activeFrom := personFactLedgerNow.AddDate(-3, 0, 0)
	stalenessTestDeclared(t, st, personID, AttributeSlugLocation, "Porto", activeFrom)
	item := stalenessTestItem(t, st, personID, AttributeSlugLocation, personFactLedgerNow)
	assert.True(item.Declared)
	assert.True(item.Stale)
	assert.Equal(activeFrom, item.NewestSupport)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugLocation})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("Porto", *values[0].Value.Text)
}

func TestPersonFactStalenessFixedNeverStale(t *testing.T) {
	assert := assert.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugBirthplace, "Porto", "fixed-ancient", 10*365*24*time.Hour)
	item := stalenessTestItem(t, st, personID, AttributeSlugBirthplace, personFactLedgerNow)
	assert.False(item.Stale)
	assert.Nil(item.VerifyBy)
}

func TestPersonFactStalenessAgedVolatileValueStaysCurrent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "aging-initial", 10*24*time.Hour)
	asOf := personFactLedgerNow.Add(200 * 24 * time.Hour)
	claim := personFactProjectionClaim(personID, projectionTargetBySlug(t, st, AttributeSlugInterestsFunNow), `"sailing"`, "aging-later")
	claim.Evidence[0].EventTime = asOf.Add(-190 * 24 * time.Hour)
	claim.Evidence[0].RecordedTime = asOf
	input := personFactProjectionInput(personID, "aging-later", []personfacts.ProposedClaim{claim}, nil)
	input.ResolvedAt = asOf
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	for _, decision := range result.Decisions {
		assert.NotEqual(personfacts.DecisionApplied, decision.Action)
	}
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugInterestsFunNow})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("climbing", *values[0].Value.Text)
	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, asOf)
	assert.True(item.Stale)
	assert.Equal(`"climbing"`, string(item.Value))
}

func TestPersonFactStalenessHidesSensitiveByDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestDeclared(t, st, personID, AttributeSlugReligion, "fixture faith", personFactLedgerNow.AddDate(-3, 0, 0))
	items, err := st.PersonFactStalenessContext(t.Context(), personID, personFactLedgerNow, false)
	require.NoError(err)
	assert.Empty(items)
	items, err = st.PersonFactStalenessContext(t.Context(), personID, personFactLedgerNow, true)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal(AttributeSlugReligion, items[0].Slug)
}

func TestPersonFactStalenessNeverWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "read-only", 100*24*time.Hour)
	before := personFactProjectionCounts(t, st)
	_, err := st.PersonFactStalenessContext(t.Context(), personID, personFactLedgerNow.AddDate(1, 0, 0), true)
	require.NoError(err)
	assert.Equal(before, personFactProjectionCounts(t, st))
	_, err = st.PersonFactStalenessContext(t.Context(), personID+100000, personFactLedgerNow, true)
	assert.ErrorIs(err, ErrPersonNotFound)
}

func TestPersonFactStalenessIgnoresIdentityRejectedSupport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	stalenessTestApply(t, st, personID, AttributeSlugInterestsFunNow, "climbing", "identity-old-support", 100*24*time.Hour)

	claim := personFactProjectionClaim(personID,
		projectionTargetBySlug(t, st, AttributeSlugInterestsFunNow), `"climbing"`, "identity-rejected-support")
	claim.Origin = personfacts.OriginEnrichment
	claim.Evidence[0].Directness = personfacts.DirectOther
	claim.Evidence[0].SubjectPersonID = &personID
	claim.Evidence[0].EventTime = personFactLedgerNow.Add(-5 * 24 * time.Hour)
	claim.Evidence[0].IdentityScore = 0
	validEvidence := claim.Evidence[0]
	validEvidence.Directness = personfacts.DirectSelf
	validEvidence.SourceURL = "https://example.com/identity-valid"
	validEvidence.IdentityScore = 950
	claim.Evidence = append(claim.Evidence, validEvidence)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "identity-rejected-support", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	identityRejected := false
	for _, decision := range result.Decisions {
		identityRejected = identityRejected || decision.Action == personfacts.DecisionIdentityRejected
	}
	assert.True(identityRejected)

	item := stalenessTestItem(t, st, personID, AttributeSlugInterestsFunNow, personFactLedgerNow.Add(100*24*time.Hour))
	assert.True(item.Stale)
	assert.Equal(personFactLedgerNow.Add(-100*24*time.Hour), item.NewestSupport)
}

func TestPersonFactStalenessUsesResolvedEmploymentSupport(t *testing.T) {
	for _, tc := range []struct {
		name         string
		organization personfacts.OrganizationReference
	}{
		{name: "organization name", organization: personfacts.OrganizationReference{Name: "Example Incorporated"}},
		{name: "organization domain", organization: personfacts.OrganizationReference{Name: "Domain Alias", Domain: "example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			createPersonFactOrganization(t, st, "Example Incorporated", "example.com")
			target := projectionTargetBySlug(t, st, "employment")
			startDate := &personfacts.PartialDateValue{Year: 2010}
			value, err := json.Marshal(personfacts.EmploymentValue{
				Organization: tc.organization, Title: "Engineer", StartDate: startDate,
			})
			require.NoError(err)
			claim := personFactProjectionClaim(personID, target, string(value), "employment-"+strings.ReplaceAll(tc.name, " ", "-"))
			claim.Evidence[0].EventTime = personFactLedgerNow.Add(-5 * 24 * time.Hour)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "employment-"+strings.ReplaceAll(tc.name, " ", "-"),
					[]personfacts.ProposedClaim{claim}, nil), nil)
			require.NoError(err)
			require.Len(result.Projections, 1)

			item := stalenessTestItem(t, st, personID, "employment", personFactLedgerNow)
			assert.False(item.Stale)
			assert.Equal(personFactLedgerNow.Add(-5*24*time.Hour), item.NewestSupport)
		})
	}
}

func TestPersonFactStalenessIncludesAttributesWithoutInferenceDescriptions(t *testing.T) {
	longDescription := strings.Repeat("x", 281)
	for _, tc := range []struct {
		name        string
		description *string
	}{
		{name: "missing description"},
		{name: "long description", description: &longDescription},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			slug := "stale_" + strings.ReplaceAll(tc.name, " ", "_")
			definition, err := st.CreateAttributeDefinitionContext(t.Context(), AttributeDefinitionInput{
				UniversalID: "test-" + slug, ObjectType: AttributeObjectPerson, Slug: slug,
				Label: slug, Description: tc.description, ValueType: AttributeValueText,
				FieldType: AttributeFieldText, Cardinality: AttributeCardinalitySingle,
				Ownership: AttributeOwnershipUser, UICreatable: true, UIEditable: true,
				APIMutable: true, IsAudited: true, IsDeletable: true,
			})
			require.NoError(err)
			value := "still current"
			activeFrom := personFactLedgerNow.Add(-800 * 24 * time.Hour)
			_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
				PersonID: personID, DefinitionSlug: definition.Slug,
				Value:      AttributeValue{Type: AttributeValueText, Text: &value},
				ActiveFrom: &activeFrom, Source: ProvenanceUser,
			})
			require.NoError(err)

			item := stalenessTestItem(t, st, personID, slug, personFactLedgerNow)
			assert.True(item.Stale)
			assert.JSONEq(`"still current"`, string(item.Value))
		})
	}
}
