package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestApplyPersonFactGenerationTransitionsEmptySourceVersionEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	claim := personFactProjectionClaim(personID, target, `"chat"`, "empty-source-version")
	claim.Evidence[0].SourceVersion = ""

	seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "empty-source-version-seed",
			[]personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	require.Len(seed.Projections, 1)
	evidence, err := st.ListPersonFactEvidenceContext(
		t.Context(), personID, personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(evidence, 1)
	assert.Empty(evidence[0].Input.SourceVersion)
	evidenceKey := evidence[0].Key

	unsupported, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "empty-source-version-off", nil,
			[]personfacts.EvidenceStatusChange{{
				EvidenceKey: evidenceKey, SourceVersion: "", Supported: false,
				Reason: personfacts.EvidenceStatusSourceDeleted,
			}}), nil)
	require.NoError(err)
	require.Len(unsupported.Decisions, 1)
	assert.Equal(personfacts.ReasonEvidenceUnsupported, unsupported.Decisions[0].Reason)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(values)

	reactivated, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "empty-source-version-on", nil,
			[]personfacts.EvidenceStatusChange{{
				EvidenceKey: evidenceKey, SourceVersion: "", Supported: true,
				Reason: personfacts.EvidenceStatusSourceReimported,
			}}), nil)
	require.NoError(err)
	require.Len(reactivated.Projections, 1)
	evidence, err = st.ListPersonFactEvidenceContext(
		t.Context(), personID, personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(evidence, 1)
	assert.True(evidence[0].Supported)
	require.NotNil(evidence[0].LatestStatus)
	assert.Empty(evidence[0].LatestStatus.SourceVersion)
	assert.Equal(personfacts.EvidenceStatusSourceReimported, evidence[0].LatestStatus.Reason)
}
