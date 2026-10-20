package store_test

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonCardFactFiltersReadAcrossCatalogRevisions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	participant, err := st.EnsureParticipantByIdentifier("email", "person@example.test", "Example Person")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	requirements.NoError(err)
	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	requirements.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(ctx, store.AttributeDefinitionInput{
		UniversalID: "example-card-topic", ObjectType: store.AttributeObjectPerson,
		Slug: "card_topic", Label: "Topic", Description: new("Current discussion topic"),
		ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText,
		Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser,
		UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true, IsDeletable: true,
	})
	requirements.NoError(err)
	oldTarget := cardFactFilterTarget(t, st, definition.Slug)
	unrelated := cardFactFilterTarget(t, st, store.AttributeSlugAskMeAbout)
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	input := personfacts.GenerationInput{
		PersonID:      person.ID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "first", End: "first-end"}},
		ProgramID:     "card-filter-fixture", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "catalog-fixture",
		Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: at, Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"},
		Claims: []personfacts.ProposedClaim{
			cardFactFilterClaim(person.ID, oldTarget, "old-topic", at),
			cardFactFilterClaim(person.ID, unrelated, "other-topic", at),
		},
	}
	result, err := st.ApplyPersonFactGenerationContext(ctx, input, nil)
	requirements.NoError(err)
	requirements.Len(result.Projections, 2, "the regression starts from applied claims")

	// Editing a description changes its catalog revision without changing the
	// target's identity or making its earlier applied fact cease to exist.
	_, err = st.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision,
		store.AttributeDefinitionUpdate{Description: new(new("Topic to discuss at the next meeting"))})
	requirements.NoError(err)
	newTarget := cardFactFilterTarget(t, st, definition.Slug)
	requirements.NotEqual(oldTarget.Revision, newTarget.Revision)
	target := personfacts.TargetRef{Kind: newTarget.Kind, Key: newTarget.Key, Revision: newTarget.Revision}
	claims, err := st.ListPersonFactClaimsContext(ctx, person.ID, personfacts.ClaimFilter{Target: &target})
	requirements.NoError(err)
	assertions.Empty(claims, "default filtering remains revision-specific")
	evidence, err := st.ListPersonFactEvidenceContext(ctx, person.ID, personfacts.EvidenceFilter{Target: &target})
	requirements.NoError(err)
	assertions.Empty(evidence)

	claims, err = st.ListPersonFactClaimsContext(ctx, person.ID,
		personfacts.ClaimFilter{Target: &target, AnyRevision: true})
	requirements.NoError(err)
	requirements.Len(claims, 1)
	assertions.Equal(oldTarget.Revision, claims[0].Target.Revision)
	assertions.Equal(oldTarget.Key, claims[0].Target.Key)
	oldClaimID := claims[0].ID
	evidence, err = st.ListPersonFactEvidenceContext(ctx, person.ID,
		personfacts.EvidenceFilter{Target: &target, AnyRevision: true})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	assertions.Equal("https://example.test/old-topic", evidence[0].Input.SourceURL)
	assertions.True(evidence[0].Supported)
	oldEvidenceID := evidence[0].ID

	input.SourceCursors[0].Start = "second"
	input.SourceCursors[0].End = "second-end"
	input.Claims = []personfacts.ProposedClaim{cardFactFilterClaim(person.ID, newTarget, "new-topic", at.Add(time.Hour))}
	input.ResolvedAt = at.Add(time.Hour)
	_, err = st.ApplyPersonFactGenerationContext(ctx, input, nil)
	requirements.NoError(err)
	claims, err = st.ListPersonFactClaimsContext(ctx, person.ID,
		personfacts.ClaimFilter{Target: &target, AnyRevision: true, Limit: 1, Offset: 1})
	requirements.NoError(err)
	requirements.Len(claims, 1)
	assertions.Equal(oldClaimID, claims[0].ID, "cross-revision reads retain bounded newest-first paging")
	evidence, err = st.ListPersonFactEvidenceContext(ctx, person.ID,
		personfacts.EvidenceFilter{Target: &target, AnyRevision: true, Limit: 1, Offset: 1})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	assertions.Equal(oldEvidenceID, evidence[0].ID)

	wrongKind := target
	wrongKind.Kind = personfacts.TargetEmployment
	claims, err = st.ListPersonFactClaimsContext(ctx, person.ID,
		personfacts.ClaimFilter{Target: &wrongKind, AnyRevision: true})
	requirements.NoError(err)
	assertions.Empty(claims)
	evidence, err = st.ListPersonFactEvidenceContext(ctx, person.ID,
		personfacts.EvidenceFilter{Target: &wrongKind, AnyRevision: true})
	requirements.NoError(err)
	assertions.Empty(evidence)
	claims, err = st.ListPersonFactClaimsContext(ctx, person.ID+1,
		personfacts.ClaimFilter{Target: &target, AnyRevision: true})
	requirements.NoError(err)
	assertions.Empty(claims)
	evidence, err = st.ListPersonFactEvidenceContext(ctx, person.ID+1,
		personfacts.EvidenceFilter{Target: &target, AnyRevision: true})
	requirements.NoError(err)
	assertions.Empty(evidence)
}

func cardFactFilterTarget(t *testing.T, st *store.Store, slug string) personfacts.TargetDescriptor {
	t.Helper()
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	for _, target := range catalog.Targets {
		if target.Slug == slug {
			return target
		}
	}
	require.FailNow(t, "missing fixture target", slug)
	return personfacts.TargetDescriptor{}
}

func cardFactFilterClaim(personID int64, target personfacts.TargetDescriptor, topic string, at time.Time) personfacts.ProposedClaim {
	return personfacts.ProposedClaim{
		Target: target, Relation: personfacts.RelationSupport, SubmittedValue: jsontext.Value(`"` + topic + `"`),
		Evidence: []personfacts.EvidenceInput{{
			PersonID: personID, SourceClass: personfacts.EvidencePublic,
			Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
			SourceURL: "https://example.test/" + topic, SubjectPersonID: &personID,
			SubjectRef: "synthetic-person", Excerpt: "Synthetic evidence " + topic,
			SourceVersion: "source-v1", EventTime: at.Add(-time.Hour), RecordedTime: at, IdentityScore: 990,
		}},
		Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
	}
}
