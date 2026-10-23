package store

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestApplyPersonFactV2FixedBirthplaceKeepsOlderValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugBirthplace)
	first := personFactProjectionClaim(personID, target, `"Porto"`, "birthplace-old")
	first.Evidence[0].EventTime = personFactLedgerNow.AddDate(-8, 0, 0)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "birthplace-old", []personfacts.ProposedClaim{first}, nil), nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	require.Equal(personfacts.DecisionApplied, result.Decisions[0].Action)
	second := personFactProjectionClaim(personID, target, `"Lisbon"`, "birthplace-new")
	second.Evidence[0].EventTime = personFactLedgerNow.Add(-24 * time.Hour)
	result, err = st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "birthplace-new", []personfacts.ProposedClaim{second}, nil), nil)
	require.NoError(err)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugBirthplace})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("Porto", *values[0].Value.Text)
	require.Len(result.Decisions, 2)
	for _, decision := range result.Decisions {
		assert.Equal(personfacts.DecisionAmbiguousRetained, decision.Action)
		assert.Equal(personfacts.ReasonCompetingTie, decision.Reason)
	}
}

func TestApplyPersonFactV2VolatileOldEvidenceRetained(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugInterestsFunNow)
	claim := personFactProjectionClaim(personID, target, `"climbing"`, "volatile-old")
	claim.Evidence[0].EventTime = personFactLedgerNow.Add(-200 * 24 * time.Hour)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "volatile-old", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	decision := result.Decisions[0]
	assert.Equal(personfacts.DecisionRetained, decision.Action)
	assert.Equal(personfacts.ReasonBelowThreshold, decision.Reason)
	var score string
	require.NoError(st.db.QueryRowContext(t.Context(), `SELECT score_json FROM person_fact_decisions WHERE id = ?`, decision.ID).Scan(&score))
	var breakdown personfacts.ScoreBreakdown
	require.NoError(json.Unmarshal([]byte(score), &breakdown))
	assert.True(breakdown.StabilityWindow)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugInterestsFunNow})
	require.NoError(err)
	assert.Empty(values)
}

func TestApplyPersonFactStabilityChangeReresolvesOnNextTouch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugLocation)
	claim := personFactProjectionClaim(personID, target, `"Porto"`, "stability-first")
	claim.Evidence[0].EventTime = personFactLedgerNow.Add(-60 * 24 * time.Hour)
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "stability-first", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	require.Len(first.Resolutions, 1)
	require.Len(first.Decisions, 1)
	assert.Equal(60, first.Decisions[0].Score.Freshness)
	definition, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugLocation)
	require.NoError(err)
	stability := AttributeStabilityVolatile
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, AttributeDefinitionUpdate{Stability: &stability})
	require.NoError(err)
	claim = personFactProjectionClaim(personID, target, `"Porto"`, "stability-second")
	claim.Evidence[0].EventTime = personFactLedgerNow.Add(-60 * 24 * time.Hour)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "stability-second", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	require.Len(second.Resolutions, 1)
	assert.Equal("person-fact-resolver-v2", second.Resolutions[0].ResolverVersion)
	require.Len(second.Decisions, 2)
	for _, decision := range second.Decisions {
		assert.Equal(40, decision.Score.Freshness)
	}
}

func TestApplyPersonFactV2LeavesStoredV1ResolutionsIntact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugLocation)
	claim := personFactProjectionClaim(personID, target, `"Porto"`, "legacy-resolution")
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), personFactProjectionInput(personID, "legacy-resolution", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	legacy := personfacts.Resolution{
		Target:          personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
		ResolverVersion: personfacts.ResolverVersionV1, InputFingerprint: strings.Repeat("d", 64), ResolvedAt: personFactLedgerNow,
	}
	stored := persistPersonFactLedgerGeneration(t, st, prepared, []personfacts.Resolution{legacy})
	require.Len(stored.Resolutions, 1)
	next := personFactProjectionInput(personID, "after-legacy-resolution", []personfacts.ProposedClaim{personFactProjectionClaim(personID, target, `"Porto"`, "new-resolution")}, nil)
	applied, err := st.ApplyPersonFactGenerationContext(t.Context(), next, nil)
	require.NoError(err)
	require.Len(applied.Resolutions, 1)
	assert.Equal("person-fact-resolver-v2", applied.Resolutions[0].ResolverVersion)
	var after personFactLedgerGeneration
	require.NoError(st.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
		var loadErr error
		after, loadErr = st.loadPersonFactGenerationTx(t.Context(), tx, personID, prepared.GenerationKey())
		return loadErr
	}))
	assert.Equal(stored, after)
}
