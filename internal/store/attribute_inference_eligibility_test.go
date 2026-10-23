package store

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

const inferenceEligibilityPublicGolden = "sha256:93e4933aff610e0977c18f7bde53f6496ecddf5de1cf9e3617ee32bdbb635ea1"
const inferenceEligibilitySensitiveGolden = "sha256:a91f6acbf745202f1674c1b21f7864cbc22ab3b97a03b1fd4010d2dfb16188e6"

func inferenceEligibilityDefinition(t *testing.T, st *Store, slug string) *AttributeDefinition {
	t.Helper()
	definition, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, slug)
	require.NoError(t, err)
	return definition
}

func inferenceEligibilitySet(t *testing.T, st *Store, slug string, eligible bool) {
	t.Helper()
	definition := inferenceEligibilityDefinition(t, st, slug)
	_, err := st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, AttributeDefinitionUpdate{InferenceEligible: &eligible})
	require.NoError(t, err)
}

func inferenceEligibilityAssertSeeds(t *testing.T, st *Store) {
	t.Helper()
	assert.False(t, inferenceEligibilityDefinition(t, st, AttributeSlugPersonality).InferenceEligible)
	assert.False(t, inferenceEligibilityDefinition(t, st, AttributeSlugContactFrequency).InferenceEligible)
	assert.True(t, inferenceEligibilityDefinition(t, st, AttributeSlugLocation).InferenceEligible)
}

func TestInferenceEligibleSeedsFreshArchive(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	inferenceEligibilityAssertSeeds(t, st)
	input := SeededAttributeDefinitions()[0]
	input.UniversalID = "custom-inference-eligibility"
	input.Slug = "custom_inference_eligibility"
	input.Label = "Custom inference eligibility"
	input.Ownership = AttributeOwnershipUser
	input.InferenceEligible = nil
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), input)
	require.NoError(t, err)
	assert.True(t, definition.InferenceEligible)
}

func TestInferenceEligibleUpgradeBackfillsSeeds(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	_, err := st.db.ExecContext(t.Context(), `ALTER TABLE attribute_definitions DROP COLUMN inference_eligible`)
	require.NoError(t, err)
	_, err = st.db.ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'attribute_definition_inference_eligible_v1'`)
	require.NoError(t, err)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	inferenceEligibilityAssertSeeds(t, st)
	version, err := st.SchemaVersionContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SchemaVersion, version)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	inferenceEligibilityAssertSeeds(t, st)
}

func TestInferenceEligibleEditSurvivesReopen(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	before := inferenceEligibilityDefinition(t, st, AttributeSlugContactFrequency)
	inferenceEligibilitySet(t, st, AttributeSlugContactFrequency, true)
	require.NoError(t, st.InitSchemaContext(t.Context()))
	require.NoError(t, st.EnsureSeededAttributeDefinitionsContext(t.Context()))
	after := inferenceEligibilityDefinition(t, st, AttributeSlugContactFrequency)
	assert.True(t, after.InferenceEligible)
	assert.Equal(t, before.Revision+1, after.Revision)
}

func TestInferenceEligibilityKeepsCatalogFingerprint(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	for _, sensitive := range []bool{false, true} {
		before, err := st.BuildPersonFactCatalogContext(t.Context(), sensitive)
		require.NoError(t, err)
		// Insert the pre-change fresh-archive goldens captured in Task12 Step1.
		golden := inferenceEligibilityPublicGolden
		if sensitive {
			golden = inferenceEligibilitySensitiveGolden
		}
		assert.Equal(t, golden, before.Fingerprint)
		inferenceEligibilitySet(t, st, AttributeSlugContactFrequency, true)
		after, err := st.BuildPersonFactCatalogContext(t.Context(), sensitive)
		require.NoError(t, err)
		assert.Equal(t, before.Fingerprint, after.Fingerprint)
		assert.Equal(t, before.Targets, after.Targets)
		encoded, err := json.Marshal(after)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "inference_eligible")
		inferenceEligibilitySet(t, st, AttributeSlugContactFrequency, false)
	}
}

func TestApplyRejectsClaimForIneligibleTarget(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugContactFrequency)
	claim := personFactProjectionClaim(personID, target, `21`, "ineligible-claim")
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "ineligible-claim", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(t, err)
	require.Len(t, result.Decisions, 1)
	assert.Equal(t, personfacts.DecisionInvalid, result.Decisions[0].Action)
	assert.Equal(t, personfacts.ReasonUnsupportedTarget, result.Decisions[0].Reason)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.NotNil(t, claims[0].Failure)
	assert.Equal(t, "target is not eligible for automatic inference", claims[0].Failure.Detail)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{DefinitionSlug: AttributeSlugContactFrequency})
	require.NoError(t, err)
	assert.Empty(t, values)
}

func TestIneligibleTargetRetiresMachineValueKeepsDeclared(t *testing.T) {
	st, aliceID, _ := newPersonFactProjectionStore(t)
	participantID, err := st.EnsureParticipant("bob@example.com", "bob", "example.com")
	require.NoError(t, err)
	bob, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	bobID := bob.ID
	_, err = st.SetPersonTrackingContext(t.Context(), bobID, true)
	require.NoError(t, err)
	inferenceEligibilitySet(t, st, AttributeSlugContactFrequency, true)
	target := projectionTargetBySlug(t, st, AttributeSlugContactFrequency)
	claim := personFactProjectionClaim(aliceID, target, `21`, "eligible-machine")
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(aliceID, "eligible-machine", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(t, err)
	require.Len(t, result.Decisions, 1)
	require.Equal(t, personfacts.DecisionApplied, result.Decisions[0].Action)
	days := int64(21)
	_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: bobID, DefinitionSlug: AttributeSlugContactFrequency, Value: AttributeValue{Type: AttributeValueInteger, Integer: &days}, Source: ProvenanceUser})
	require.NoError(t, err)
	inferenceEligibilitySet(t, st, AttributeSlugContactFrequency, false)
	for _, personID := range []int64{aliceID, bobID} {
		claim = personFactProjectionClaim(personID, target, `21`, "ineligible-touch")
		result, err = st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "ineligible-touch", []personfacts.ProposedClaim{claim}, nil), nil)
		require.NoError(t, err)
		for _, decision := range result.Decisions {
			assert.Equal(t, personfacts.DecisionInvalid, decision.Action)
			assert.Equal(t, personfacts.ReasonUnsupportedTarget, decision.Reason)
		}
	}
	machine, err := st.ListPersonAttributeValuesContext(t.Context(), aliceID, PersonAttributeQuery{DefinitionSlug: AttributeSlugContactFrequency})
	require.NoError(t, err)
	assert.Empty(t, machine)
	declared, err := st.ListPersonAttributeValuesContext(t.Context(), bobID, PersonAttributeQuery{DefinitionSlug: AttributeSlugContactFrequency})
	require.NoError(t, err)
	require.Len(t, declared, 1)
	assert.Equal(t, days, *declared[0].Value.Integer)
}

func TestOwnerCanSetAndPinIneligibleAttribute(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	days := int64(21)
	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: personID, DefinitionSlug: AttributeSlugContactFrequency, Value: AttributeValue{Type: AttributeValueInteger, Integer: &days}, Source: ProvenanceUser})
	require.NoError(t, err)
	target := projectionTargetBySlug(t, st, AttributeSlugContactFrequency)
	pin, err := st.SetPersonFactPinContext(t.Context(), personID, personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, true, "owner")
	require.NoError(t, err)
	assert.True(t, pin.State.Pinned)
}

func TestPersonFactStalenessIncludesIneligibleTargets(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	days := int64(21)
	activeFrom := personFactLedgerNow.AddDate(-3, 0, 0)
	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: personID, DefinitionSlug: AttributeSlugContactFrequency, Value: AttributeValue{Type: AttributeValueInteger, Integer: &days}, Source: ProvenanceUser, ActiveFrom: &activeFrom})
	require.NoError(t, err)
	item := stalenessTestItem(t, st, personID, AttributeSlugContactFrequency, personFactLedgerNow)
	assert.True(t, item.Stale)
	assert.True(t, item.Declared)
	assert.Equal(t, activeFrom, item.NewestSupport)
	assert.Equal(t, time.Duration(730)*24*time.Hour, item.VerifyBy.Sub(item.NewestSupport))
}
