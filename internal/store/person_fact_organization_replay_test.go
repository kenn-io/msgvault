package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactEmploymentHistoricalNameDomainClaimKeepsResolvedOrganization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "name-domain-binding-seed", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target,
				`{"organization":{"name":"Original Company","domain":"original.example"},"title":"Engineer"}`,
				"name-domain-binding-seed"),
		}, nil), nil)
	requirements.NoError(err)
	requirements.Len(seed.Projections, 1)
	employment, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
	requirements.NoError(err)
	organization, err := st.GetOrganizationContext(t.Context(), employment.OrganizationID)
	requirements.NoError(err)

	renamedDomain := "renamed.example"
	renamed, err := st.ReplaceOrganizationContext(t.Context(), organization.ID, organization.Revision,
		OrganizationInput{
			Name: "Renamed Company", Kind: OrganizationKindCompany,
			PrimaryDomain: &renamedDomain,
		}, false)
	requirements.NoError(err)

	weak := personFactProjectionClaim(personID, target,
		`{"organization":{"name":"Weak Company","domain":"weak.example"},"title":"Advisor"}`,
		"name-domain-binding-later")
	weak.Confidence.ReportedScore = 0
	weak.Evidence[0].Directness = personfacts.Indirect
	weak.Evidence[0].Authority = personfacts.AuthorityAggregator
	later, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "name-domain-binding-later",
			[]personfacts.ProposedClaim{weak}, nil), nil)
	requirements.NoError(err)
	assertions.Positive(later.GenerationID)
	assertions.Empty(later.Projections)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	requirements.NoError(err)
	requirements.Len(employments, 1)
	assertions.Equal(renamed.ID, employments[0].OrganizationID)
	organizations, err := st.ListOrganizationsContext(t.Context(), OrganizationFilter{})
	requirements.NoError(err)
	requirements.Len(organizations, 1)
	assertions.Equal("Renamed Company", organizations[0].Name)
}

func TestPersonFactEmploymentHistoricalMergedOrganizationReplaysAsSurvivor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	losing := createPersonFactOrganization(t, st, "Merged Company", "merged-old.example")
	survivor := createPersonFactOrganization(t, st, "Surviving Company", "survivor.example")
	submitted := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Merged Company","domain":"merged-old.example"},"title":"Engineer"}`,
		losing.ID)
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "merged-replay-seed", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, submitted, "merged-replay-seed"),
		}, nil), nil)
	requirements.NoError(err)
	requirements.Len(seed.Projections, 1)

	merged, err := st.MergeOrganizationsContext(
		t.Context(), survivor.ID, survivor.Revision, losing.ID, losing.Revision)
	requirements.NoError(err)
	assertions.Equal(survivor.ID, merged.ID)
	terminal := createPersonFactOrganization(t, st, "Canonical Company", "canonical.example")
	canonical, err := st.MergeOrganizationsContext(
		t.Context(), terminal.ID, terminal.Revision, merged.ID, merged.Revision)
	requirements.NoError(err)
	assertions.Equal(terminal.ID, canonical.ID)

	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	later, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "merged-replay-later", nil,
			[]personfacts.EvidenceStatusChange{{
				EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion,
				Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
			}}), nil)
	requirements.NoError(err)
	assertions.Positive(later.GenerationID)
	requirements.Len(later.Decisions, 1)
	assertions.Equal(personfacts.DecisionRetained, later.Decisions[0].Action)
	assertions.Equal(personfacts.ReasonEvidenceUnsupported, later.Decisions[0].Reason)
	assertions.Empty(later.Projections)

	employment, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
	requirements.NoError(err)
	assertions.Equal(terminal.ID, employment.OrganizationID)
	assertions.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestPersonFactEmploymentHistoricalOrganizationRenameAllowsPin(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Original Company", "original.example")
	submitted := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Original Company","domain":"original.example"},"title":"Engineer"}`,
		organization.ID)
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "rename-pin-seed", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, submitted, "rename-pin-seed"),
		}, nil), nil)
	requirements.NoError(err)
	requirements.Len(seed.Projections, 1)

	renamedDomain := "renamed.example"
	renamed, err := st.ReplaceOrganizationContext(t.Context(), organization.ID, organization.Revision,
		OrganizationInput{
			Name: "Renamed Company", Kind: OrganizationKindCompany,
			PrimaryDomain: &renamedDomain,
		}, false)
	requirements.NoError(err)
	assertions.Equal("Renamed Company", renamed.Name)

	pinned, err := st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
		true, "organization-rename-test")
	requirements.NoError(err)
	assertions.True(pinned.State.Pinned)
	requirements.Len(pinned.Resolutions, 1)
	requirements.Len(pinned.Resolutions[0].Decisions, 1)
	assertions.Equal(personfacts.DecisionRetained, pinned.Resolutions[0].Decisions[0].Action)
	assertions.Equal(personfacts.ReasonPinRetained, pinned.Resolutions[0].Decisions[0].Reason)
	assertions.Empty(pinned.Projections)

	employment, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
	requirements.NoError(err)
	assertions.Equal(renamed.ID, employment.OrganizationID)
	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	requirements.NoError(err)
	requirements.Len(pins, 1)
	assertions.True(pins[0].Pinned)
	assertions.Equal(target.Key, pins[0].Target.Key)
	assertions.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestPersonFactEmploymentUnavailableHistoricalOrganizationRejectsOnlyThatClaim(t *testing.T) {
	for _, unavailable := range []string{"deleted", "retired"} {
		t.Run(unavailable, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			stale := createPersonFactOrganization(t, st, "Stale Company", "stale.example")
			staleSubmitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Stale Company","domain":"stale.example"},"title":"Advisor"}`,
				stale.ID)
			staleClaim := personFactProjectionClaim(
				personID, target, staleSubmitted, "unavailable-replay-seed")
			staleClaim.Confidence.ReportedScore = 0
			staleClaim.Evidence[0].Directness = personfacts.Indirect
			staleClaim.Evidence[0].Authority = personfacts.AuthorityAggregator
			seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "unavailable-replay-seed",
					[]personfacts.ProposedClaim{staleClaim}, nil), nil)
			requirements.NoError(err)
			requirements.Len(seed.Decisions, 1)
			assertions.Equal(personfacts.DecisionRetained, seed.Decisions[0].Action)
			assertions.Empty(seed.Projections)
			if unavailable == "deleted" {
				requirements.NoError(st.DeleteOrganizationContext(
					t.Context(), stale.ID, stale.Revision))
			} else {
				_, err = st.ReplaceOrganizationContext(t.Context(), stale.ID, stale.Revision,
					OrganizationInput{Name: stale.Name, Kind: stale.Kind}, true)
				requirements.NoError(err)
			}

			current := createPersonFactOrganization(t, st, "Current Company", "current.example")
			currentSubmitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Current Company","domain":"current.example"},"title":"Engineer"}`,
				current.ID)
			later, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "unavailable-replay-later",
					[]personfacts.ProposedClaim{
						personFactProjectionClaim(
							personID, target, currentSubmitted, "unavailable-replay-later"),
					}, nil), nil)
			requirements.NoError(err)
			assertions.Positive(later.GenerationID)
			requirements.Len(later.Decisions, 2)
			invalid, applied := 0, 0
			for _, decision := range later.Decisions {
				if decision.Action == personfacts.DecisionInvalid {
					invalid++
					assertions.Equal(personfacts.ReasonMalformedValue, decision.Reason)
				}
				if decision.Action == personfacts.DecisionApplied {
					applied++
				}
			}
			assertions.Equal(1, invalid)
			assertions.Equal(1, applied)
			requirements.Len(later.Projections, 1)
			employment, err := st.GetEmploymentContext(t.Context(), later.Projections[0].RowID)
			requirements.NoError(err)
			assertions.Equal(current.ID, employment.OrganizationID)
			assertions.Equal(int64(2), personFactProjectionRowCount(
				t, st, "person_fact_generations"))
			claims, err := st.ListPersonFactClaimsContext(
				t.Context(), personID, personfacts.ClaimFilter{})
			requirements.NoError(err)
			assertions.Len(claims, 2)
		})
	}
}
