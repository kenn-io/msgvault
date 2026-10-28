package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactContestsIncludesChallengerToUnpinnedCurrentValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		relation   personfacts.ClaimRelation
		confidence int
		authority  personfacts.EvidenceAuthority
		score      int
		reason     personfacts.DecisionReason
		contested  bool
	}{
		{"replacement", `"email"`, personfacts.RelationSupport, 740, personfacts.AuthorityAuthoritative, 924, personfacts.ReasonInsufficientMargin, true},
		{"contradiction", `"chat"`, personfacts.RelationContradict, 0, personfacts.AuthorityAuthoritative, 850, personfacts.ReasonExplicitContradiction, true},
		{"below threshold", `"email"`, personfacts.RelationSupport, 740, personfacts.AuthorityOrdinary, 724, personfacts.ReasonBelowThreshold, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			target := targets[AttributeSlugPrimaryChannel]
			_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
				PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
				Value: AttributeValue{Type: AttributeValueText, Text: new("chat")}, Source: ProvenanceUser,
			})
			requirements.NoError(err)
			_, err = st.SetPersonFactPinContext(t.Context(), personID,
				personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, false, "test")
			requirements.NoError(err)

			claim := personFactProjectionClaim(personID, target, tc.value, "unpinned-challenger")
			claim.Relation = tc.relation
			claim.Confidence.ReportedScore = tc.confidence
			claim.Evidence[0].Directness = personfacts.DirectOther
			claim.Evidence[0].Authority = tc.authority
			input := personFactProjectionInput(personID, "unpinned-challenger", []personfacts.ProposedClaim{claim}, nil)
			input.ResolvedAt = time.Now().UTC()
			input.Claims[0].Evidence[0].EventTime = input.ResolvedAt.Add(-time.Hour)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			requirements.NoError(err)
			requirements.Len(result.Decisions, 1)
			assertions.Equal(tc.score, result.Decisions[0].Score.Total)
			assertions.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
			assertions.Equal(tc.reason, result.Decisions[0].Reason)
			assertions.Empty(result.Decisions[0].CompetingClaimKey)

			values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
				PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
			requirements.NoError(err)
			requirements.Len(values, 1)
			assertions.Equal("chat", *values[0].Value.Text)
			contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
			requirements.NoError(err)
			if !tc.contested {
				assertions.Empty(contests)
				return
			}
			requirements.Len(contests, 1)
			requirements.Len(contests[0].Current, 1)
			assertions.JSONEq(`"chat"`, string(contests[0].Current[0].Normalized.JSON))
			requirements.Len(contests[0].Claims, 1)
			assertions.JSONEq(tc.value, string(contests[0].Claims[0].Claim.Claim.SubmittedValue))
			assertions.Equal(result.Decisions[0], contests[0].Claims[0].Decision)
			requirements.Len(contests[0].Claims[0].Claim.Evidence, 1)
			assertions.True(contests[0].Claims[0].Claim.Evidence[0].Supported)
			assertions.Equal(claim.Evidence[0].SourceURL, contests[0].Claims[0].Claim.Evidence[0].Input.SourceURL)
		})
	}
}

func TestPersonFactContestsManualAttributeChoiceSettlesContest(t *testing.T) {
	for _, name := range []string{"explicit pin", "user attribute value"} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			target := targets[AttributeSlugPrimaryChannel]
			_, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "manual-choice-current", []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target, `"chat"`, "manual-choice-chat"),
			}, nil), nil)
			requirements.NoError(err)
			_, err = st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "manual-choice-challenger", []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target, `"email"`, "manual-choice-email"),
			}, nil), nil)
			requirements.NoError(err)
			contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
			requirements.NoError(err)
			requirements.Len(contests, 1)
			requirements.Len(contests[0].Current, 1)
			requirements.JSONEq(`"chat"`, string(contests[0].Current[0].Normalized.JSON))

			if name == "explicit pin" {
				_, err = st.SetPersonFactPinContext(t.Context(), personID,
					personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, true, "test")
			} else {
				_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
					PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
					Value: AttributeValue{Type: AttributeValueText, Text: new("email")}, Source: ProvenanceUser,
				})
			}
			requirements.NoError(err)
			contests, err = st.ListPersonFactContestsContext(t.Context(), personID, false)
			requirements.NoError(err)
			requirements.Empty(contests)
		})
	}
}

func TestPersonFactContestsReportsSupportedTieWithoutWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	input := personFactProjectionInput(personID, "contested-tie", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "contested-chat"),
		personFactProjectionClaim(personID, target, `"email"`, "contested-email"),
	}, nil)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	requirements.Len(result.Decisions, 2)
	before := personFactProjectionCounts(t, st)
	contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	requirements.Len(contests, 1)
	assertions.Equal(target.Key, contests[0].Target.Key)
	requirements.Len(contests[0].Claims, 2)
	for _, claim := range contests[0].Claims {
		assertions.Equal(personfacts.DecisionAmbiguousRetained, claim.Decision.Action)
		assertions.Len(claim.Claim.Evidence, 1)
	}
	assertions.Equal(before, personFactProjectionCounts(t, st))
}

func TestPersonFactContestsSensitiveTargetRequiresOptIn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "religion")
	input := personFactProjectionInput(personID, "sensitive-contested-tie", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"Buddhism"`, "sensitive-first"),
		personFactProjectionClaim(personID, target, `"Hinduism"`, "sensitive-second"),
	}, nil)
	input.Policy.AllowSensitive = true
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)

	contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	assertions.Empty(contests)

	contests, err = st.ListPersonFactContestsContext(t.Context(), personID, true)
	requirements.NoError(err)
	requirements.Len(contests, 1)
	assertions.Equal(target.Key, contests[0].Target.Key)
}

func TestPersonFactContestsIncludesEveryNearTiedNegativeClaim(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	seed := personFactProjectionInput(personID, "contested-negative-current", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "contested-negative-current"),
	}, nil)
	seedResult, err := st.ApplyPersonFactGenerationContext(t.Context(), seed, nil)
	requirements.NoError(err)
	requirements.Len(seedResult.Projections, 1)

	contradiction := personFactProjectionClaim(personID, target, `"chat"`, "contested-negative-contradiction")
	contradiction.Relation = personfacts.RelationContradict
	contradiction.Confidence.ReportedScore = 850
	contradiction.Evidence[0].SourceURL = "https://example.com/contested-negative"
	supersession := personFactProjectionClaim(personID, target, `"chat"`, "contested-negative-supersession")
	supersession.Relation = personfacts.RelationSupersede
	supersession.Confidence.ReportedScore = 850
	supersession.Evidence[0].SourceURL = "https://example.com/contested-negative"
	input := personFactProjectionInput(personID, "contested-negative-claims", []personfacts.ProposedClaim{
		contradiction, supersession,
	}, nil)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	requirements.Len(result.Decisions, 3)

	reasons := make(map[personfacts.DecisionReason]bool, len(result.Decisions))
	for _, decision := range result.Decisions {
		reasons[decision.Reason] = true
	}
	assertions.True(reasons[personfacts.ReasonInsufficientMargin])
	assertions.True(reasons[personfacts.ReasonExplicitContradiction])
	assertions.True(reasons[personfacts.ReasonExplicitSupersession])

	contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	requirements.Len(contests, 1)
	requirements.Len(contests[0].Claims, 3)
	requirements.Len(contests[0].Current, 1)
	for _, claim := range contests[0].Claims {
		assertions.Len(claim.Claim.Evidence, 1)
	}
}

func TestPersonFactContestsFollowsEvidenceStatus(t *testing.T) {
	requirements := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	input := personFactProjectionInput(personID, "contested-status", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "status-chat"),
		personFactProjectionClaim(personID, target, `"email"`, "status-email"),
	}, nil)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{})
	requirements.NoError(err)
	requirements.Len(evidence, 2)
	withdrawal := personFactProjectionInput(personID, "contest-withdraw", nil,
		[]personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion,
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}})
	withdrawal.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), withdrawal, nil)
	requirements.NoError(err)
	contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	requirements.Empty(contests)

	statusInput := personFactProjectionInput(personID, "contest-newer-reactivate", nil,
		[]personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion,
			Supported: true, Reason: personfacts.EvidenceStatusSourceReimported}})
	statusInput.ResolvedAt = personFactLedgerNow.Add(2 * time.Hour)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), statusInput, nil)
	requirements.NoError(err)
	contests, err = st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	requirements.Len(contests, 1)
	requirements.Len(contests[0].Claims, 2)
}

func TestPersonFactContestsCurrentEmploymentExcludesHistoricalProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	historicalOrg := createPersonFactOrganization(t, st, "Old Example", "old.example.test")
	currentOrg := createPersonFactOrganization(t, st, "Current Example", "current.example.test")
	contestedOrg := createPersonFactOrganization(t, st, "Contested Example", "contested.example.test")
	oldValue := fmt.Sprintf(`{"organization":{"id":%d,"name":"Old Example","domain":"old.example.test"},"title":"Engineer"}`, historicalOrg.ID)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "historical-employment", []personfacts.ProposedClaim{personFactProjectionClaim(personID, target, oldValue, "historical-employment")}, nil), nil)
	requirements.NoError(err)
	requirements.Len(result.Projections, 1)
	_, err = st.db.ExecContext(t.Context(), st.Rebind(`UPDATE employments SET is_current = FALSE WHERE id = ?`), result.Projections[0].RowID)
	requirements.NoError(err)
	values := []personfacts.ProposedClaim{}
	for _, role := range []string{"Platform", "Infrastructure"} {
		value := fmt.Sprintf(`{"organization":{"id":%d,"name":"Contested Example","domain":"contested.example.test"},"title":"Engineer","role":"%s"}`, contestedOrg.ID, role)
		values = append(values, personFactProjectionClaim(personID, target, value, "contest-"+role))
	}
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(personID, "employment-contest", values, nil), nil)
	requirements.NoError(err)
	declared, err := st.AddEmploymentContext(t.Context(), EmploymentInput{PersonID: personID, OrganizationID: currentOrg.ID, Title: new("Manager"), IsCurrent: new(true), Source: ProvenanceUser})
	requirements.NoError(err)
	contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	assertions.Empty(contests)
	_, err = st.SetPersonFactPinContext(t.Context(), personID, personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, false, "test")
	requirements.NoError(err)
	contests, err = st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	requirements.Len(contests, 1)
	requirements.Len(contests[0].Current, 1)
	assertions.Equal(declared.ID, contests[0].Current[0].Ref.RowID)
	assertions.True(contests[0].Current[0].Declared)
}

func TestPersonFactContestsSurvivesDefinitionDescriptionChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	input := personFactProjectionInput(personID, "contest-definition", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "definition-chat"),
		personFactProjectionClaim(personID, target, `"email"`, "definition-email"),
	}, nil)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	definition, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugPrimaryChannel)
	requirements.NoError(err)
	description := "Preferred channel for reaching this person"
	descriptionUpdate := &description
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision, AttributeDefinitionUpdate{Description: &descriptionUpdate})
	requirements.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), false)
	requirements.NoError(err)
	var current personfacts.TargetDescriptor
	for _, item := range catalog.Targets {
		if item.Key == target.Key {
			current = item
		}
	}
	requirements.NotEqual(target.Revision, current.Revision)
	contests, err := st.ListPersonFactContestsContext(t.Context(), personID, false)
	requirements.NoError(err)
	requirements.Len(contests, 1)
	assertions.Equal(current.Revision, contests[0].Target.Revision)
	assertions.Equal(target.Revision, contests[0].ResolutionRevision)
}
