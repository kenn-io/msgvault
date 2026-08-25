package store

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

func TestPersonFactOrganizationFingerprintPreservesLegacyLookupKeyEncoding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	keys := []personFactOrganizationLookupKey{
		{Kind: "domain", Value: "example.com"},
		{Kind: "name", Value: "example incorporated"},
	}
	encodedKey, err := json.Marshal(keys[0])
	requirements.NoError(err)
	assertions.True(bytes.Equal([]byte(`{"Kind":"domain","Value":"example.com"}`), encodedKey),
		"organization lookup-key encoding changed")

	ref := personfacts.OrganizationReference{Name: "Example Incorporated", Domain: "example.com"}
	match := OrganizationMatch{Status: OrganizationAmbiguous, CandidateIDs: []int64{9, 3}}
	fingerprint, err := personFactOrganizationFingerprint(ref, keys, match)
	requirements.NoError(err)
	assertions.Equal("1bf99b4a089f65e5830da6f735673810a38118d47561d11c325c92b85d05cf75", fingerprint)
}

func TestPersonFactEmploymentResolvesOrganizationReferences(t *testing.T) {
	tests := []struct {
		name      string
		seed      func(*testing.T, *Store) *Organization
		submitted func(*Organization) string
	}{
		{
			name: "existing id",
			seed: func(t *testing.T, st *Store) *Organization {
				t.Helper()
				return createPersonFactOrganization(t, st, "Existing Company", "existing.example")
			},
			submitted: func(organization *Organization) string {
				return fmt.Sprintf(`{"organization":{"id":%d,"name":"Existing Company","domain":"existing.example"},"title":"Engineer"}`, organization.ID)
			},
		},
		{
			name: "exact domain",
			seed: func(t *testing.T, st *Store) *Organization {
				t.Helper()
				return createPersonFactOrganization(t, st, "Domain Company", "domain.example")
			},
			submitted: func(*Organization) string {
				return `{"organization":{"name":"Domain Company","domain":"domain.example"},"title":"Engineer"}`
			},
		},
		{
			name: "normalized name without domain",
			seed: func(t *testing.T, st *Store) *Organization {
				t.Helper()
				return createPersonFactOrganization(t, st, "Normalized Company", "")
			},
			submitted: func(*Organization) string {
				return `{"organization":{"name":"  NORMALIZED   Company  "},"title":"Engineer"}`
			},
		},
		{
			name: "zero match creates company",
			seed: func(*testing.T, *Store) *Organization { return nil },
			submitted: func(*Organization) string {
				return `{"organization":{"name":"Created Company","domain":"created.example"},"title":"Engineer"}`
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			seeded := test.seed(t, st)
			claim := personFactProjectionClaim(personID, target, test.submitted(seeded), test.name)

			result, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, test.name, []personfacts.ProposedClaim{claim}, nil), nil)
			require.NoError(err)
			require.Len(result.Projections, 1)
			assert.Equal("employment", result.Projections[0].Kind)

			employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
				PersonID: personID, CurrentOnly: true,
			})
			require.NoError(err)
			require.Len(employments, 1)
			if seeded != nil {
				assert.Equal(seeded.ID, employments[0].OrganizationID)
			} else {
				created, loadErr := st.GetOrganizationContext(t.Context(), employments[0].OrganizationID)
				require.NoError(loadErr)
				assert.Equal("Created Company", created.Name)
				assert.Equal(OrganizationKindCompany, created.Kind)
				require.NotNil(created.PrimaryDomain)
				assert.Equal("created.example", *created.PrimaryDomain)
			}
		})
	}
}

func TestPersonFactEmploymentRequiresEveryOrganizationIdentifierToMatch(t *testing.T) {
	type organizationFixture struct {
		name   string
		domain string
	}
	tests := []struct {
		name          string
		organizations []organizationFixture
		reference     organizationFixture
		wantReuse     int
	}{
		{
			name: "same company matches name and domain",
			organizations: []organizationFixture{
				{name: "Exact Company", domain: "exact.complete.example"},
			},
			reference: organizationFixture{name: "Exact Company", domain: "exact.complete.example"},
			wantReuse: 0,
		},
		{
			name: "matching name does not override missing domain",
			organizations: []organizationFixture{
				{name: "Name Match Company", domain: "old-name-match.example"},
			},
			reference: organizationFixture{name: "Name Match Company", domain: "new-name-match.example"},
			wantReuse: -1,
		},
		{
			name: "matching domain does not override missing name",
			organizations: []organizationFixture{
				{name: "Original Domain Company", domain: "domain-match.example"},
			},
			reference: organizationFixture{name: "Different Domain Company", domain: "domain-match.example"},
			wantReuse: -1,
		},
		{
			name: "name and domain resolving to different companies do not reuse either",
			organizations: []organizationFixture{
				{name: "Split Name Company", domain: "split-name.example"},
				{name: "Split Domain Company", domain: "split-domain.example"},
			},
			reference: organizationFixture{name: "Split Name Company", domain: "split-domain.example"},
			wantReuse: -1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			seededIDs := make([]int64, 0, len(test.organizations))
			for _, fixture := range test.organizations {
				organization := createPersonFactOrganization(t, st, fixture.name, fixture.domain)
				seededIDs = append(seededIDs, organization.ID)
			}
			submitted := fmt.Sprintf(
				`{"organization":{"name":%q,"domain":%q},"title":"Engineer"}`,
				test.reference.name, test.reference.domain)

			result, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "complete-reference-"+test.name,
					[]personfacts.ProposedClaim{
						personFactProjectionClaim(personID, target, submitted, "complete-reference"),
					}, nil), nil)
			require.NoError(err)
			require.Len(result.Projections, 1)
			employment, err := st.GetEmploymentContext(t.Context(), result.Projections[0].RowID)
			require.NoError(err)
			if test.wantReuse >= 0 {
				assert.Equal(seededIDs[test.wantReuse], employment.OrganizationID)
				return
			}
			assert.NotContains(seededIDs, employment.OrganizationID)
			created, err := st.GetOrganizationContext(t.Context(), employment.OrganizationID)
			require.NoError(err)
			assert.Equal(test.reference.name, created.Name)
			require.NotNil(created.PrimaryDomain)
			assert.Equal(test.reference.domain, *created.PrimaryDomain)
		})
	}
}

func TestPersonFactOrganizationExistingIDRejectsDisagreeingIdentity(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	organization := createPersonFactOrganization(t, st, "Exact Company", "exact.example")
	ref := personfacts.OrganizationReference{
		ID: &organization.ID, Name: "Different Company", Domain: "exact.example",
	}

	err := st.withTxContext(t.Context(), func(tx *loggedTx) error {
		_, prepareErr := st.preparePersonFactOrganizationTx(t.Context(), tx, ref)
		return prepareErr
	})
	require.ErrorIs(t, err, ErrOrganizationInvalid)
}

func TestPersonFactOrganizationReferenceFailureClassifier(t *testing.T) {
	assert := assert.New(t)
	for _, err := range []error{
		ErrOrganizationNotFound,
		fmt.Errorf("wrapped missing: %w", ErrOrganizationNotFound),
		ErrOrganizationInvalid,
		fmt.Errorf("wrapped invalid: %w", ErrOrganizationInvalid),
	} {
		failure := personFactOrganizationReferenceFailure(err)
		if assert.NotNil(failure) {
			assert.Equal(personfacts.DecisionInvalid, failure.Action)
			assert.Equal(personfacts.ReasonMalformedValue, failure.Reason)
			assert.Contains(failure.Detail, err.Error())
		}
	}
	assert.Nil(personFactOrganizationReferenceFailure(sql.ErrConnDone))
}

func TestPersonFactEmploymentInvalidValuesDoNotRollbackValidSibling(t *testing.T) {
	tests := []struct {
		name       string
		employment func(*testing.T, *Store) string
	}{
		{
			name: "missing organization id",
			employment: func(t *testing.T, st *Store) string {
				t.Helper()
				organization := createPersonFactOrganization(t, st, "Existing", "existing.invalid-ref")
				return fmt.Sprintf(`{"organization":{"id":%d,"name":"Missing"},"title":"Engineer"}`, organization.ID+9999)
			},
		},
		{
			name: "mismatched organization name",
			employment: func(t *testing.T, st *Store) string {
				t.Helper()
				organization := createPersonFactOrganization(t, st, "Exact", "exact.invalid-ref")
				return fmt.Sprintf(`{"organization":{"id":%d,"name":"Different"},"title":"Engineer"}`, organization.ID)
			},
		},
		{
			name: "retired organization",
			employment: func(t *testing.T, st *Store) string {
				t.Helper()
				require := require.New(t)
				organization := createPersonFactOrganization(t, st, "Retired", "retired.invalid-ref")
				_, err := st.db.Exec(`UPDATE organizations SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, organization.ID)
				require.NoError(err)
				return fmt.Sprintf(`{"organization":{"id":%d,"name":"Retired"},"title":"Engineer"}`, organization.ID)
			},
		},
		{
			name: "non-company organization",
			employment: func(t *testing.T, st *Store) string {
				t.Helper()
				require := require.New(t)
				organization := createPersonFactOrganization(t, st, "School", "school.invalid-ref")
				_, err := st.db.Exec(`UPDATE organizations SET kind = ? WHERE id = ?`, OrganizationKindSchool, organization.ID)
				require.NoError(err)
				return fmt.Sprintf(`{"organization":{"id":%d,"name":"School"},"title":"Engineer"}`, organization.ID)
			},
		},
		{
			name: "reversed partial dates",
			employment: func(*testing.T, *Store) string {
				return `{"organization":{"name":"Dates"},"title":"Engineer","start_date":{"year":2025,"month":7},"end_date":{"year":2025,"month":6}}`
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			employmentTarget := projectionTargetBySlug(t, st, "employment")
			input := personFactProjectionInput(personID, "invalid-employment-"+test.name,
				[]personfacts.ProposedClaim{
					personFactProjectionClaim(personID, employmentTarget, test.employment(t, st), "invalid-employment"),
					personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "valid-sibling"),
				}, nil)

			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Projections, 1)
			assert.Equal("person_attribute", result.Projections[0].Kind)
			invalid := 0
			for _, decision := range result.Decisions {
				if decision.Action == personfacts.DecisionInvalid {
					invalid++
					assert.Equal(personfacts.ReasonMalformedValue, decision.Reason)
				}
			}
			assert.Equal(1, invalid)
			claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
			require.NoError(err)
			require.Len(claims, 2)
			for _, claim := range claims {
				if claim.Target.Kind == personfacts.TargetEmployment {
					assert.Nil(claim.Normalized)
					require.NotNil(claim.Failure)
					assert.Equal(personfacts.ReasonMalformedValue, claim.Failure.Reason)
				}
			}
		})
	}
}

func TestPersonFactOrganizationAmbiguityRetainsWithoutMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	first := createPersonFactOrganization(t, st, "Shared Company", "first.example")
	second := createPersonFactOrganization(t, st, "Shared Company", "second.example")
	var prepared OrganizationMatch
	require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
		var prepareErr error
		prepared, prepareErr = st.preparePersonFactOrganizationTx(t.Context(), tx,
			personfacts.OrganizationReference{Name: "Shared Company"})
		return prepareErr
	}))
	assert.Equal(OrganizationAmbiguous, prepared.Status)
	assert.Equal([]int64{first.ID, second.ID}, prepared.CandidateIDs)
	assert.NotEmpty(prepared.Fingerprint)
	claim := personFactProjectionClaim(personID, target,
		`{"organization":{"name":"Shared Company"},"title":"Engineer"}`, "ambiguous")

	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "ambiguous", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.DecisionAmbiguousRetained, result.Decisions[0].Action)
	assert.Equal(personfacts.ReasonOrganizationAmbiguous, result.Decisions[0].Reason)
	assert.Empty(result.Projections)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	require.NoError(err)
	assert.Empty(employments)
	for _, organizationID := range []int64{first.ID, second.ID} {
		organization, loadErr := st.GetOrganizationContext(t.Context(), organizationID)
		require.NoError(loadErr)
		assert.Nil(organization.MergedIntoID)
		assert.Nil(organization.RetiredAt)
	}
}

func TestPersonFactOrganizationBelowThresholdDoesNotCreate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	claim := personFactProjectionClaim(personID, target,
		`{"organization":{"name":"Weak Company","domain":"weak.example"},"title":"Engineer"}`, "weak")
	claim.Confidence.ReportedScore = 0
	claim.Evidence[0].Directness = personfacts.Indirect
	claim.Evidence[0].Authority = personfacts.AuthorityAggregator

	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "weak", []personfacts.ProposedClaim{claim}, nil), nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
	assert.Equal(personfacts.ReasonBelowThreshold, result.Decisions[0].Reason)
	assert.Empty(result.Projections)
	assert.Equal(int64(0), personFactProjectionRowCount(t, st, "organizations"))
}

func TestPersonFactOrganizationConcurrentZeroMatchRequeriesUnderLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, _ := newPersonFactProjectionStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ref := personfacts.OrganizationReference{Name: "Concurrent Company", Domain: "concurrent.example"}
	firstPrepared := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	type organizationResult struct {
		organization *Organization
		status       OrganizationResolutionStatus
		err          error
	}
	results := make(chan organizationResult, 2)

	go func() {
		var result organizationResult
		result.err = st.withTxContext(ctx, func(tx *loggedTx) error {
			prepared, err := st.preparePersonFactOrganizationTx(ctx, tx, ref)
			close(firstPrepared)
			if err != nil {
				return err
			}
			<-releaseFirst
			result.organization, result.status, err = st.materializePersonFactOrganizationTx(
				ctx, tx, ref, prepared)
			return err
		})
		results <- result
	}()
	<-firstPrepared

	go func() {
		var result organizationResult
		result.err = st.withTxContext(ctx, func(tx *loggedTx) error {
			close(secondStarted)
			prepared, err := st.preparePersonFactOrganizationTx(ctx, tx, ref)
			if err != nil {
				return err
			}
			result.organization, result.status, err = st.materializePersonFactOrganizationTx(
				ctx, tx, ref, prepared)
			return err
		})
		results <- result
	}()
	<-secondStarted
	close(releaseFirst)

	first := <-results
	second := <-results
	require.NoError(first.err)
	require.NoError(second.err)
	require.NotNil(first.organization)
	require.NotNil(second.organization)
	assert.Equal(first.organization.ID, second.organization.ID)
	assert.ElementsMatch(
		[]OrganizationResolutionStatus{OrganizationCreated, OrganizationReused},
		[]OrganizationResolutionStatus{first.status, second.status})
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "organizations"))
}

func TestPersonFactEmploymentCorrectionRevisesUniqueRowAndPreservesHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Correction Company", "correction.example")
	firstJSON := fmt.Sprintf(`{"organization":{"id":%d,"name":"Correction Company","domain":"correction.example"},"title":"Engineer","role":"Platform"}`, organization.ID)
	secondJSON := fmt.Sprintf(`{"organization":{"id":%d,"name":"Correction Company","domain":"correction.example"},"title":" engineer ","role":"Infrastructure","department":"Systems"}`, organization.ID)

	firstResult, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "correction-first", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, firstJSON, "correction-first"),
		}, nil), nil)
	require.NoError(err)
	require.Len(firstResult.Projections, 1)
	first, err := st.GetEmploymentContext(t.Context(), firstResult.Projections[0].RowID)
	require.NoError(err)
	require.True(first.IsPrimary)

	secondResult, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "correction-second", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, secondJSON, "correction-second"),
		}, nil), nil)
	require.NoError(err)
	require.Len(secondResult.Projections, 1)
	assert.Equal(first.ID, secondResult.Projections[0].RowID)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	require.NoError(err)
	require.Len(employments, 1)
	corrected := employments[0]
	assert.Equal(first.ID, corrected.ID)
	assert.Equal(first.Revision+1, corrected.Revision)
	assert.True(corrected.IsPrimary)
	assert.Equal(ProvenanceExtraction, corrected.Source)
	require.NotNil(corrected.SourceRef)
	assert.Contains(*corrected.SourceRef, "person-fact-decision:")
	require.NotNil(corrected.Role)
	assert.Equal("Infrastructure", *corrected.Role)
	require.NotNil(corrected.Department)
	assert.Equal("Systems", *corrected.Department)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	assert.Len(claims, 2)
}

func TestPersonFactEmploymentSameGenerationCurrentClaimsCompeteByStableIdentity(t *testing.T) {
	type decisionSummary struct {
		ClaimKey          string
		Action            personfacts.DecisionAction
		Reason            personfacts.DecisionReason
		Score             int
		CompetingClaimKey string
		Projected         bool
	}
	type resultSummary struct {
		GenerationKey string
		Decisions     []decisionSummary
		Role          string
		Revision      int64
	}
	var summaries []resultSummary
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(
				t, st, "Same Generation Company", "same-generation.example")
			platformJSON := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Same Generation Company","domain":"same-generation.example"},"title":"Engineer","role":"Platform"}`,
				organization.ID)
			infrastructureJSON := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Same Generation Company","domain":"same-generation.example"},"title":" engineer ","role":"Infrastructure","department":"Systems"}`,
				organization.ID)
			platform := personFactProjectionClaim(
				personID, target, platformJSON, "same-generation-platform")
			platform.Confidence.ReportedScore = 1000
			infrastructure := personFactProjectionClaim(
				personID, target, infrastructureJSON, "same-generation-infrastructure")
			infrastructure.Confidence.ReportedScore = 0
			claims := []personfacts.ProposedClaim{platform, infrastructure}
			if reverse {
				slices.Reverse(claims)
			}

			result, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "same-generation-current", claims, nil), nil)
			require.NoError(err)
			require.Len(result.Decisions, 2)
			require.Len(result.Projections, 1)
			applied, superseded := 0, 0
			summary := resultSummary{GenerationKey: result.GenerationKey}
			for _, decision := range result.Decisions {
				summary.Decisions = append(summary.Decisions, decisionSummary{
					ClaimKey: decision.ClaimKey, Action: decision.Action, Reason: decision.Reason,
					Score: decision.Score.Total, CompetingClaimKey: decision.CompetingClaimKey,
					Projected: decision.Projection != nil,
				})
				switch decision.Action {
				case personfacts.DecisionApplied:
					applied++
					assert.NotNil(decision.Projection)
				case personfacts.DecisionSuperseded:
					superseded++
					assert.Equal(personfacts.ReasonAppliedProjection, decision.Reason)
					assert.NotEmpty(decision.CompetingClaimKey)
					assert.Nil(decision.Projection)
				default:
					assert.Fail("unexpected decision action", "action: %s", decision.Action)
				}
			}
			assert.Equal(1, applied)
			assert.Equal(1, superseded)

			employment, err := st.GetEmploymentContext(t.Context(), result.Projections[0].RowID)
			require.NoError(err)
			require.NotNil(employment.Role)
			assert.Equal("Platform", *employment.Role)
			assert.Equal(int64(1), employment.Revision)
			summary.Role = *employment.Role
			summary.Revision = employment.Revision
			summaries = append(summaries, summary)
		})
	}
	require.Len(t, summaries, 2)
	assert.Equal(t, summaries[0], summaries[1])
}

func TestPersonFactEmploymentSameGenerationCompetitionHonorsTieAndReplacementMargin(t *testing.T) {
	type decisionSummary struct {
		ClaimKey          string
		Action            personfacts.DecisionAction
		Reason            personfacts.DecisionReason
		Score             int
		CompetingClaimKey string
	}
	tests := []struct {
		name                     string
		platformConfidence       int
		infrastructureConfidence int
		wantActions              []personfacts.DecisionAction
	}{
		{
			name: "equal scores", platformConfidence: 900, infrastructureConfidence: 900,
			wantActions: []personfacts.DecisionAction{
				personfacts.DecisionAmbiguousRetained,
				personfacts.DecisionAmbiguousRetained,
			},
		},
		{
			name: "below replacement margin", platformConfidence: 1000, infrastructureConfidence: 700,
			wantActions: []personfacts.DecisionAction{
				personfacts.DecisionRetained,
				personfacts.DecisionConflictRejected,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var orderSummaries [][]decisionSummary
			for _, reverse := range []bool{false, true} {
				orderName := "forward"
				if reverse {
					orderName = "reverse"
				}
				t.Run(orderName, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					st, personID, _ := newPersonFactProjectionStore(t)
					target := projectionTargetBySlug(t, st, "employment")
					organization := createPersonFactOrganization(
						t, st, "Policy Company", "policy-company.example")
					platformJSON := fmt.Sprintf(
						`{"organization":{"id":%d,"name":"Policy Company","domain":"policy-company.example"},"title":"Engineer","role":"Platform"}`,
						organization.ID)
					infrastructureJSON := fmt.Sprintf(
						`{"organization":{"id":%d,"name":"Policy Company","domain":"policy-company.example"},"title":" engineer ","role":"Infrastructure"}`,
						organization.ID)
					platform := personFactProjectionClaim(
						personID, target, platformJSON, "policy-platform")
					platform.Confidence.ReportedScore = test.platformConfidence
					infrastructure := personFactProjectionClaim(
						personID, target, infrastructureJSON, "policy-infrastructure")
					infrastructure.Confidence.ReportedScore = test.infrastructureConfidence
					claims := []personfacts.ProposedClaim{platform, infrastructure}
					if reverse {
						slices.Reverse(claims)
					}
					input := personFactProjectionInput(
						personID, "same-generation-policy-"+test.name, claims, nil)

					result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
					require.NoError(err)
					require.Len(result.Decisions, 2)
					assert.Empty(result.Projections)
					assert.Equal(int64(0), personFactProjectionRowCount(t, st, "employments"))
					actions := make([]personfacts.DecisionAction, 0, len(result.Decisions))
					summary := make([]decisionSummary, 0, len(result.Decisions))
					for _, decision := range result.Decisions {
						actions = append(actions, decision.Action)
						assert.NotEmpty(decision.CompetingClaimKey)
						assert.Nil(decision.Projection)
						summary = append(summary, decisionSummary{
							ClaimKey: decision.ClaimKey, Action: decision.Action,
							Reason: decision.Reason, Score: decision.Score.Total,
							CompetingClaimKey: decision.CompetingClaimKey,
						})
					}
					assert.ElementsMatch(test.wantActions, actions)
					if test.name == "equal scores" {
						for _, decision := range result.Decisions {
							assert.Equal(personfacts.ReasonCompetingTie, decision.Reason)
						}
					} else {
						for _, decision := range result.Decisions {
							assert.Equal(personfacts.ReasonInsufficientMargin, decision.Reason)
						}
					}

					input.ResolvedAt = input.ResolvedAt.Add(time.Hour)
					replay, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
					require.NoError(err)
					assert.Equal(result.GenerationID, replay.GenerationID)
					assert.Equal(result.Decisions, replay.Decisions)
					assert.Empty(replay.Projections)
					assert.Equal(int64(0), personFactProjectionRowCount(t, st, "employments"))
					orderSummaries = append(orderSummaries, summary)
				})
			}
			require.Len(t, orderSummaries, 2)
			assert.Equal(t, orderSummaries[0], orderSummaries[1])
		})
	}
}

func TestPersonFactEmploymentSameGenerationCompetitionRewritesWholeDirections(t *testing.T) {
	type decisionSummary struct {
		ClaimKey          string
		Action            personfacts.DecisionAction
		Reason            personfacts.DecisionReason
		Score             int
		CompetingClaimKey string
		Projected         bool
	}
	tests := []struct {
		name                     string
		platformConfidence       int
		infrastructureConfidence int
		wantActions              []personfacts.DecisionAction
		wantReason               personfacts.DecisionReason
		wantProjection           bool
	}{
		{
			name: "tie", platformConfidence: 900, infrastructureConfidence: 900,
			wantActions: []personfacts.DecisionAction{
				personfacts.DecisionAmbiguousRetained,
				personfacts.DecisionAmbiguousRetained,
				personfacts.DecisionAmbiguousRetained,
			},
			wantReason: personfacts.ReasonCompetingTie,
		},
		{
			name: "insufficient margin", platformConfidence: 1000, infrastructureConfidence: 700,
			wantActions: []personfacts.DecisionAction{
				personfacts.DecisionRetained,
				personfacts.DecisionRetained,
				personfacts.DecisionConflictRejected,
			},
			wantReason: personfacts.ReasonInsufficientMargin,
		},
		{
			name: "duplicate direction clearly loses", platformConfidence: 0, infrastructureConfidence: 1000,
			wantActions: []personfacts.DecisionAction{
				personfacts.DecisionSuperseded,
				personfacts.DecisionSuperseded,
				personfacts.DecisionApplied,
			},
			wantReason:     personfacts.ReasonAppliedProjection,
			wantProjection: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var orderSummaries [][]decisionSummary
			for _, reverse := range []bool{false, true} {
				orderName := "forward"
				if reverse {
					orderName = "reverse"
				}
				t.Run(orderName, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					st, personID, _ := newPersonFactProjectionStore(t)
					target := projectionTargetBySlug(t, st, "employment")
					organization := createPersonFactOrganization(
						t, st, "Direction Company", "direction-company.example")
					platformJSON := fmt.Sprintf(
						`{"organization":{"id":%d,"name":"Direction Company","domain":"direction-company.example"},"title":"Engineer","role":"Platform"}`,
						organization.ID)
					infrastructureJSON := fmt.Sprintf(
						`{"organization":{"id":%d,"name":"Direction Company","domain":"direction-company.example"},"title":" engineer ","role":"Infrastructure"}`,
						organization.ID)
					platformA := personFactProjectionClaim(
						personID, target, platformJSON, "direction-platform-a")
					platformA.Confidence.ReportedScore = test.platformConfidence
					platformB := personFactProjectionClaim(
						personID, target, platformJSON, "direction-platform-b")
					platformB.Confidence.ReportedScore = test.platformConfidence
					// Keep the evidence independently addressable while ensuring the
					// duplicate direction does not gain a corroboration bonus.
					platformB.Evidence[0].SourceURL = platformA.Evidence[0].SourceURL
					infrastructure := personFactProjectionClaim(
						personID, target, infrastructureJSON, "direction-infrastructure")
					infrastructure.Confidence.ReportedScore = test.infrastructureConfidence
					claims := []personfacts.ProposedClaim{platformA, platformB, infrastructure}
					if reverse {
						slices.Reverse(claims)
					}
					input := personFactProjectionInput(
						personID, "same-generation-directions-"+test.name, claims, nil)

					result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
					require.NoError(err)
					require.Len(result.Decisions, 3)
					actions := make([]personfacts.DecisionAction, 0, len(result.Decisions))
					summary := make([]decisionSummary, 0, len(result.Decisions))
					winner := ""
					for _, decision := range result.Decisions {
						actions = append(actions, decision.Action)
						assert.Equal(test.wantReason, decision.Reason)
						summary = append(summary, decisionSummary{
							ClaimKey: decision.ClaimKey, Action: decision.Action,
							Reason: decision.Reason, Score: decision.Score.Total,
							CompetingClaimKey: decision.CompetingClaimKey,
							Projected:         decision.Projection != nil,
						})
						if decision.Action == personfacts.DecisionApplied {
							winner = decision.ClaimKey
							assert.NotNil(decision.Projection)
						}
					}
					assert.ElementsMatch(test.wantActions, actions)

					if test.wantProjection {
						require.Len(result.Projections, 1)
						assert.NotEmpty(winner)
						for _, decision := range result.Decisions {
							if decision.Action == personfacts.DecisionSuperseded {
								assert.Equal(winner, decision.CompetingClaimKey)
								assert.Nil(decision.Projection)
							}
						}
						employment, err := st.GetEmploymentContext(
							t.Context(), result.Projections[0].RowID)
						require.NoError(err)
						require.NotNil(employment.Role)
						assert.Equal("Infrastructure", *employment.Role)
						assert.Equal(int64(1), employment.Revision)
					} else {
						assert.Empty(result.Projections)
						assert.Equal(int64(0), personFactProjectionRowCount(t, st, "employments"))
						for _, decision := range result.Decisions {
							assert.NotEmpty(decision.CompetingClaimKey)
							assert.Nil(decision.Projection)
						}
					}

					input.ResolvedAt = input.ResolvedAt.Add(time.Hour)
					replay, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
					require.NoError(err)
					assert.Equal(result.GenerationID, replay.GenerationID)
					assert.Equal(result.Decisions, replay.Decisions)
					assert.Equal(result.Projections, replay.Projections)
					orderSummaries = append(orderSummaries, summary)
				})
			}
			require.Len(t, orderSummaries, 2)
			assert.Equal(t, orderSummaries[0], orderSummaries[1])
		})
	}
}

func TestPersonFactEmploymentStrongDirectionSupersedesBelowThresholdDirection(t *testing.T) {
	type decisionSummary struct {
		ClaimKey          string
		Action            personfacts.DecisionAction
		Reason            personfacts.DecisionReason
		CompetingClaimKey string
		Projected         bool
	}
	var orderSummaries [][]decisionSummary
	for _, reverse := range []bool{false, true} {
		orderName := "forward"
		if reverse {
			orderName = "reverse"
		}
		t.Run(orderName, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(
				t, st, "Threshold Company", "threshold-company.example")
			platformJSON := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Threshold Company","domain":"threshold-company.example"},"title":"Engineer","role":"Platform"}`,
				organization.ID)
			infrastructureJSON := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Threshold Company","domain":"threshold-company.example"},"title":" engineer ","role":"Infrastructure"}`,
				organization.ID)
			strong := personFactProjectionClaim(
				personID, target, platformJSON, "threshold-strong")
			strong.Confidence.ReportedScore = 1000
			weakA := personFactProjectionClaim(
				personID, target, infrastructureJSON, "threshold-weak-a")
			weakB := personFactProjectionClaim(
				personID, target, infrastructureJSON, "threshold-weak-b")
			for _, weak := range []*personfacts.ProposedClaim{&weakA, &weakB} {
				weak.Confidence.ReportedScore = 0
				weak.Evidence[0].SourceClass = personfacts.EvidenceProviderAssertion
				weak.Evidence[0].Directness = personfacts.Indirect
				weak.Evidence[0].Authority = personfacts.AuthorityAggregator
				weak.Evidence[0].SourceRef = "threshold-provider"
				weak.Evidence[0].EventTime = personFactLedgerNow.Add(-3 * 365 * 24 * time.Hour)
			}
			weakB.Evidence[0].SourceURL = weakA.Evidence[0].SourceURL
			weakB.Evidence[0].EventTime = weakA.Evidence[0].EventTime.Add(-time.Hour)
			claims := []personfacts.ProposedClaim{strong, weakA, weakB}
			if reverse {
				slices.Reverse(claims)
			}
			input := personFactProjectionInput(
				personID, "same-generation-threshold-directions", claims, nil)

			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Decisions, 3)
			require.Len(result.Projections, 1)
			winner := ""
			for _, decision := range result.Decisions {
				if decision.Action == personfacts.DecisionApplied {
					winner = decision.ClaimKey
				}
			}
			assert.NotEmpty(winner)
			actions := make([]personfacts.DecisionAction, 0, len(result.Decisions))
			summary := make([]decisionSummary, 0, len(result.Decisions))
			for _, decision := range result.Decisions {
				actions = append(actions, decision.Action)
				assert.Equal(personfacts.ReasonAppliedProjection, decision.Reason)
				if decision.Action == personfacts.DecisionApplied {
					assert.Empty(decision.CompetingClaimKey)
					assert.NotNil(decision.Projection)
				} else {
					assert.Equal(winner, decision.CompetingClaimKey)
					assert.Nil(decision.Projection)
				}
				summary = append(summary, decisionSummary{
					ClaimKey: decision.ClaimKey, Action: decision.Action, Reason: decision.Reason,
					CompetingClaimKey: decision.CompetingClaimKey,
					Projected:         decision.Projection != nil,
				})
			}
			assert.ElementsMatch([]personfacts.DecisionAction{
				personfacts.DecisionApplied,
				personfacts.DecisionSuperseded,
				personfacts.DecisionSuperseded,
			}, actions)
			employment, err := st.GetEmploymentContext(t.Context(), result.Projections[0].RowID)
			require.NoError(err)
			require.NotNil(employment.Role)
			assert.Equal("Platform", *employment.Role)
			assert.Equal(int64(1), employment.Revision)

			input.ResolvedAt = input.ResolvedAt.Add(time.Hour)
			replay, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			assert.Equal(result.GenerationID, replay.GenerationID)
			assert.Equal(result.Decisions, replay.Decisions)
			assert.Equal(result.Projections, replay.Projections)
			orderSummaries = append(orderSummaries, summary)
		})
	}
	require.Len(t, orderSummaries, 2)
	assert.Equal(t, orderSummaries[0], orderSummaries[1])
}

func TestPersonFactEmploymentEndedSupportIsNotReinsertedByUnrelatedResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	historicalOrganization := createPersonFactOrganization(
		t, st, "Historical Company", "historical.example")
	unrelatedOrganization := createPersonFactOrganization(
		t, st, "Unrelated Company", "unrelated.example")
	historicalJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Historical Company","domain":"historical.example"},"title":"Engineer","end_date":{"year":2024,"month":6}}`,
		historicalOrganization.ID)
	unrelatedJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Unrelated Company","domain":"unrelated.example"},"title":"Advisor"}`,
		unrelatedOrganization.ID)

	firstInput := personFactProjectionInput(personID, "historical", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, historicalJSON, "historical"),
	}, nil)
	firstInput.ResolvedAt = personFactLedgerNow
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), firstInput, nil)
	require.NoError(err)
	require.Len(first.Projections, 1)
	historicalID := first.Projections[0].RowID

	secondInput := personFactProjectionInput(personID, "unrelated", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, unrelatedJSON, "unrelated"),
	}, nil)
	secondInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), secondInput, nil)
	require.NoError(err)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	require.NoError(err)
	assert.Len(employments, 2)
	historicalCount := 0
	for _, employment := range employments {
		if employment.OrganizationID == historicalOrganization.ID {
			historicalCount++
			assert.Equal(historicalID, employment.ID)
			assert.False(employment.IsCurrent)
		}
	}
	assert.Equal(1, historicalCount)
}

func TestPersonFactEmploymentRepeatedEndedSupportUsesStableEpisodeIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Ended Company", "ended.example")
	submitted := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Ended Company","domain":"ended.example"},"title":"Engineer","role":"Platform","end_date":{"year":2024,"month":6}}`,
		organization.ID)

	firstInput := personFactProjectionInput(personID, "ended-first", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, submitted, "ended-first"),
	}, nil)
	firstInput.ResolvedAt = personFactLedgerNow
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), firstInput, nil)
	require.NoError(err)
	require.Len(first.Projections, 1)
	firstEmployment, err := st.GetEmploymentContext(t.Context(), first.Projections[0].RowID)
	require.NoError(err)

	secondInput := personFactProjectionInput(personID, "ended-second", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, submitted, "ended-second"),
	}, nil)
	secondInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), secondInput, nil)
	require.NoError(err)
	assert.Empty(second.Projections)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Equal(firstEmployment.ID, employments[0].ID)
	assert.Equal(firstEmployment.Revision, employments[0].Revision)
}

func TestPersonFactEmploymentIncrementalNonOverlappingStintCreatesNewEpisode(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Repeat Company", "repeat.example")
	value := func(startYear, endYear int) string {
		return fmt.Sprintf(
			`{"organization":{"id":%d,"name":"Repeat Company","domain":"repeat.example"},"title":"Engineer","start_date":{"year":%d},"end_date":{"year":%d}}`,
			organization.ID, startYear, endYear)
	}

	first, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "repeat-first", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, value(2018, 2020), "repeat-first"),
		}, nil), nil)
	requirements.NoError(err)
	requirements.Len(first.Projections, 1)

	secondInput := personFactProjectionInput(personID, "repeat-second", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, value(2022, 2024), "repeat-second"),
	}, nil)
	secondInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), secondInput, nil)
	requirements.NoError(err)
	requirements.Len(second.Projections, 1)
	assertions.NotEqual(first.Projections[0].RowID, second.Projections[0].RowID)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	requirements.NoError(err)
	requirements.Len(employments, 2)
	assertions.ElementsMatch([]int{2018, 2022}, []int{
		*employments[0].StartDate.Year, *employments[1].StartDate.Year,
	})
}

func TestPersonFactEmploymentExplicitUnpinRepairsMissingOrEditedHistoricalProjection(t *testing.T) {
	for _, mode := range []string{"deleted", "edited"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(t, st, "Repair "+mode, "repair-"+mode+".example")
			submitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Repair %s","domain":"repair-%s.example"},"title":"Engineer","role":"Platform","end_date":{"year":2024,"month":6}}`,
				organization.ID, mode, mode)
			input := personFactProjectionInput(personID, "repair-"+mode, []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target, submitted, "repair-"+mode),
			}, nil)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Projections, 1)
			original, err := st.GetEmploymentContext(t.Context(), result.Projections[0].RowID)
			require.NoError(err)
			renamedDomain := "renamed-repair-" + mode + ".example"
			organization, err = st.ReplaceOrganizationContext(
				t.Context(), organization.ID, organization.Revision, OrganizationInput{
					Name: "Renamed Repair " + mode, Kind: OrganizationKindCompany,
					PrimaryDomain: &renamedDomain,
				}, false)
			require.NoError(err)

			if mode == "deleted" {
				require.NoError(st.DeleteEmploymentContext(t.Context(), original.ID, original.Revision))
			} else {
				_, err = st.UpdateEmploymentContext(t.Context(), original.ID, original.Revision, EmploymentInput{
					PersonID: personID, OrganizationID: organization.ID,
					Title: original.Title, Role: new("Manually edited"), EndDate: original.EndDate,
					Source: ProvenanceUser,
				})
				require.NoError(err)
			}

			unpinned, err := st.SetPersonFactPinContext(t.Context(), personID,
				personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
				false, "repair-test")
			require.NoError(err)
			require.NotEmpty(unpinned.Projections)
			employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
			require.NoError(err)
			require.Len(employments, 1)
			if mode == "deleted" {
				assert.NotEqual(original.ID, employments[0].ID)
			} else {
				assert.Equal(original.ID, employments[0].ID)
			}
			require.NotNil(employments[0].Role)
			assert.Equal("Platform", *employments[0].Role)
			assert.Equal(personID, employments[0].PersonID)
			assert.Equal(organization.ID, employments[0].OrganizationID)
			assert.Equal(ProvenanceExtraction, employments[0].Source)
			require.NotNil(employments[0].Confidence)
			assert.InDelta(0.9, *employments[0].Confidence, 0.0001)
			assert.False(employments[0].IsCurrent)
		})
	}
}

func TestPersonFactEmploymentLaterEndedCorrectionRevisesStableEpisode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Ended Correction", "ended-correction.example")
	firstJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Ended Correction","domain":"ended-correction.example"},"title":"Engineer","role":"Platform","start_date":{"year":2020},"end_date":{"year":2024,"month":6}}`,
		organization.ID)
	correctedJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Ended Correction","domain":"ended-correction.example"},"title":" engineer ","role":"Infrastructure","department":"Systems","start_date":{"year":2020},"end_date":{"year":2024,"month":7}}`,
		organization.ID)

	firstInput := personFactProjectionInput(personID, "ended-correction-first", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, firstJSON, "ended-correction-first"),
	}, nil)
	firstInput.ResolvedAt = personFactLedgerNow
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), firstInput, nil)
	require.NoError(err)
	require.Len(first.Projections, 1)
	before, err := st.GetEmploymentContext(t.Context(), first.Projections[0].RowID)
	require.NoError(err)

	secondInput := personFactProjectionInput(personID, "ended-correction-second", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, correctedJSON, "ended-correction-second"),
	}, nil)
	secondInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), secondInput, nil)
	require.NoError(err)
	require.Len(second.Projections, 1)
	assert.Equal(before.ID, second.Projections[0].RowID)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	require.NoError(err)
	require.Len(employments, 1)
	corrected := employments[0]
	assert.Equal(before.ID, corrected.ID)
	assert.Equal(before.Revision+1, corrected.Revision)
	require.NotNil(corrected.Role)
	assert.Equal("Infrastructure", *corrected.Role)
	require.NotNil(corrected.Department)
	assert.Equal("Systems", *corrected.Department)
	require.NotNil(corrected.EndDate)
	require.NotNil(corrected.EndDate.Month)
	assert.Equal(7, *corrected.EndDate.Month)
}

func TestPersonFactEmploymentHistoricalCorrectionsPreserveDistinctEpisodesAndCurrentStint(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Episode Company", "episodes.example")
	employmentJSON := func(startYear, endYear int, role string) string {
		return fmt.Sprintf(
			`{"organization":{"id":%d,"name":"Episode Company","domain":"episodes.example"},"title":"Engineer","role":%q,"start_date":{"year":%d},"end_date":{"year":%d}}`,
			organization.ID, role, startYear, endYear)
	}

	seedInput := personFactProjectionInput(personID, "episode-seed", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, employmentJSON(2018, 2020, "First"), "episode-first"),
		personFactProjectionClaim(personID, target, employmentJSON(2021, 2023, "Second"), "episode-second"),
	}, nil)
	seedInput.ResolvedAt = personFactLedgerNow
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(), seedInput, nil)
	requirements.NoError(err)
	requirements.Len(seed.Projections, 2)
	historicalIDs := []int64{seed.Projections[0].RowID, seed.Projections[1].RowID}

	currentJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Episode Company","domain":"episodes.example"},"title":"Engineer","role":"Current","start_date":{"year":2025}}`,
		organization.ID)
	currentInput := personFactProjectionInput(personID, "episode-current", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, currentJSON, "episode-current"),
	}, nil)
	currentInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	currentResult, err := st.ApplyPersonFactGenerationContext(t.Context(), currentInput, nil)
	requirements.NoError(err)
	requirements.Len(currentResult.Projections, 1)
	currentBefore, err := st.GetEmploymentContext(t.Context(), currentResult.Projections[0].RowID)
	requirements.NoError(err)

	correctionInput := personFactProjectionInput(personID, "episode-corrections", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, employmentJSON(2018, 2019, "First corrected"), "episode-first-correction"),
		personFactProjectionClaim(personID, target, employmentJSON(2021, 2024, "Second corrected"), "episode-second-correction"),
	}, nil)
	correctionInput.ResolvedAt = personFactLedgerNow.Add(2 * time.Hour)
	corrected, err := st.ApplyPersonFactGenerationContext(t.Context(), correctionInput, nil)
	requirements.NoError(err)
	requirements.Len(corrected.Projections, 2)
	assertions.ElementsMatch(historicalIDs,
		[]int64{corrected.Projections[0].RowID, corrected.Projections[1].RowID})

	currentAfter, err := st.GetEmploymentContext(t.Context(), currentBefore.ID)
	requirements.NoError(err)
	assertions.Equal(currentBefore.Revision, currentAfter.Revision)
	assertions.True(currentAfter.IsCurrent)
	requirements.NotNil(currentAfter.Role)
	assertions.Equal("Current", *currentAfter.Role)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	requirements.NoError(err)
	requirements.Len(employments, 3)
	rolesByStart := make(map[int]string)
	for _, employment := range employments {
		if employment.StartDate == nil || employment.StartDate.Year == nil || employment.Role == nil {
			continue
		}
		rolesByStart[*employment.StartDate.Year] = *employment.Role
	}
	assertions.Equal("First corrected", rolesByStart[2018])
	assertions.Equal("Second corrected", rolesByStart[2021])
	assertions.Equal("Current", rolesByStart[2025])
}

func TestPersonFactEmploymentLaterCurrentSupportCreatesNewEpisodeAfterEndedSupport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Return Company", "return.example")
	historicalJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Return Company","domain":"return.example"},"title":"Engineer","role":"Platform","end_date":{"year":2024,"month":6}}`,
		organization.ID)
	currentJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Return Company","domain":"return.example"},"title":" engineer ","role":"Platform"}`,
		organization.ID)

	firstInput := personFactProjectionInput(personID, "return-historical", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, historicalJSON, "return-historical"),
	}, nil)
	firstInput.ResolvedAt = personFactLedgerNow
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), firstInput, nil)
	require.NoError(err)
	require.Len(first.Projections, 1)
	historical, err := st.GetEmploymentContext(t.Context(), first.Projections[0].RowID)
	require.NoError(err)
	assert.False(historical.IsCurrent)

	secondInput := personFactProjectionInput(personID, "return-current", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, currentJSON, "return-current"),
	}, nil)
	secondInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), secondInput, nil)
	require.NoError(err)
	require.Len(second.Projections, 1)
	assert.NotEqual(historical.ID, second.Projections[0].RowID)

	unchanged, err := st.GetEmploymentContext(t.Context(), historical.ID)
	require.NoError(err)
	assert.Equal(historical.Revision, unchanged.Revision)
	assert.False(unchanged.IsCurrent)
	require.NotNil(unchanged.EndDate)

	current, err := st.GetEmploymentContext(t.Context(), second.Projections[0].RowID)
	require.NoError(err)
	assert.True(current.IsCurrent)
	assert.Nil(current.EndDate)
	assert.Equal(organization.ID, current.OrganizationID)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	require.NoError(err)
	assert.Len(employments, 2)
}

func TestPersonFactEmploymentOlderCorrectionCannotOverwriteNewerOnLaterResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Chronology Company", "chronology.example")
	unrelated := createPersonFactOrganization(t, st, "Later Company", "later.example")
	olderJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Chronology Company","domain":"chronology.example"},"title":"Engineer","role":"Platform"}`,
		organization.ID)
	newerJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Chronology Company","domain":"chronology.example"},"title":" engineer ","role":"Infrastructure","department":"Systems"}`,
		organization.ID)
	unrelatedJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Later Company","domain":"later.example"},"title":"Advisor"}`,
		unrelated.ID)

	olderInput := personFactProjectionInput(personID, "chronology-older", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, olderJSON, "chronology-older"),
	}, nil)
	olderInput.ResolvedAt = personFactLedgerNow
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), olderInput, nil)
	require.NoError(err)

	newerInput := personFactProjectionInput(personID, "chronology-newer", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, newerJSON, "chronology-newer"),
	}, nil)
	newerInput.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	newerResult, err := st.ApplyPersonFactGenerationContext(t.Context(), newerInput, nil)
	require.NoError(err)
	require.Len(newerResult.Projections, 1)
	corrected, err := st.GetEmploymentContext(t.Context(), newerResult.Projections[0].RowID)
	require.NoError(err)
	correctedRevision := corrected.Revision

	unrelatedInput := personFactProjectionInput(personID, "chronology-unrelated", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, unrelatedJSON, "chronology-unrelated"),
	}, nil)
	unrelatedInput.ResolvedAt = personFactLedgerNow.Add(2 * time.Hour)
	unrelatedResult, err := st.ApplyPersonFactGenerationContext(t.Context(), unrelatedInput, nil)
	require.NoError(err)
	require.Len(unrelatedResult.Projections, 1)

	after, err := st.GetEmploymentContext(t.Context(), corrected.ID)
	require.NoError(err)
	assert.Equal(correctedRevision, after.Revision)
	require.NotNil(after.Role)
	assert.Equal("Infrastructure", *after.Role)
	require.NotNil(after.Department)
	assert.Equal("Systems", *after.Department)
}

func TestPersonFactEmploymentLaterSupportRehiresAfterRetirement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Rehire Company", "rehire.example")
	submitted := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Rehire Company","domain":"rehire.example"},"title":"Engineer"}`,
		organization.ID)

	seedInput := personFactProjectionInput(personID, "rehire-seed", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, submitted, "rehire-seed"),
	}, nil)
	seedInput.ResolvedAt = personFactLedgerNow
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(), seedInput, nil)
	require.NoError(err)
	require.Len(seed.Projections, 1)
	originalID := seed.Projections[0].RowID

	retiredAt := personFactLedgerNow.Add(time.Hour)
	retirement := personFactProjectionClaim(personID, target, submitted, "rehire-retire")
	retirement.Relation = personfacts.RelationSupersede
	retirement.ValidFrom = &retiredAt
	retireInput := personFactProjectionInput(
		personID, "rehire-retire", []personfacts.ProposedClaim{retirement}, nil)
	retireInput.ResolvedAt = retiredAt
	retired, err := st.ApplyPersonFactGenerationContext(t.Context(), retireInput, nil)
	require.NoError(err)
	require.Len(retired.Projections, 1)
	assert.Equal(originalID, retired.Projections[0].RowID)

	rehireAt := personFactLedgerNow.Add(2 * time.Hour)
	rehire := personFactProjectionClaim(personID, target, submitted, "rehire-support")
	rehire.ValidFrom = &rehireAt
	rehireInput := personFactProjectionInput(
		personID, "rehire-support", []personfacts.ProposedClaim{rehire}, nil)
	rehireInput.ResolvedAt = rehireAt
	rehired, err := st.ApplyPersonFactGenerationContext(t.Context(), rehireInput, nil)
	require.NoError(err)
	require.Len(rehired.Projections, 1)
	assert.NotEqual(originalID, rehired.Projections[0].RowID)

	current, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	require.NoError(err)
	require.Len(current, 1)
	assert.Equal(rehired.Projections[0].RowID, current[0].ID)
	assert.Equal(organization.ID, current[0].OrganizationID)
}

func TestPersonFactEmploymentNewJobRetainsUnrelatedCurrentJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	firstOrganization := createPersonFactOrganization(t, st, "First Company", "first-job.example")
	secondOrganization := createPersonFactOrganization(t, st, "Second Company", "second-job.example")
	firstJSON := fmt.Sprintf(`{"organization":{"id":%d,"name":"First Company","domain":"first-job.example"},"title":"Engineer"}`, firstOrganization.ID)
	secondJSON := fmt.Sprintf(`{"organization":{"id":%d,"name":"Second Company","domain":"second-job.example"},"title":"Advisor"}`, secondOrganization.ID)

	_, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "first-job", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, firstJSON, "first-job"),
		}, nil), nil)
	require.NoError(err)
	_, err = st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "second-job", []personfacts.ProposedClaim{
			func() personfacts.ProposedClaim {
				claim := personFactProjectionClaim(personID, target, secondJSON, "second-job")
				claim.Origin = personfacts.OriginEnrichment
				return claim
			}(),
		}, nil), nil)
	require.NoError(err)

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	require.NoError(err)
	require.Len(employments, 2)
	organizationIDs := []int64{employments[0].OrganizationID, employments[1].OrganizationID}
	slices.Sort(organizationIDs)
	assert.Equal([]int64{firstOrganization.ID, secondOrganization.ID}, organizationIDs)
	primaryCount := 0
	for _, employment := range employments {
		if employment.IsPrimary {
			primaryCount++
		}
		if employment.OrganizationID == secondOrganization.ID {
			assert.Equal(ProvenanceEnrichment, employment.Source)
			require.NotNil(employment.SourceRef)
			assert.Contains(*employment.SourceRef, "person-fact-decision:")
		}
	}
	assert.Equal(1, primaryCount)
}

func TestPersonFactEmploymentSupersessionEndsOnlyExactRowAtEffectiveDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	firstOrganization := createPersonFactOrganization(t, st, "Ending Company", "ending.example")
	secondOrganization := createPersonFactOrganization(t, st, "Retained Company", "retained.example")
	endingJSON := fmt.Sprintf(`{"organization":{"id":%d,"name":"Ending Company","domain":"ending.example"},"title":"Engineer","start_date":{"year":2020,"month":2}}`, firstOrganization.ID)
	retainedJSON := fmt.Sprintf(`{"organization":{"id":%d,"name":"Retained Company","domain":"retained.example"},"title":"Advisor"}`, secondOrganization.ID)
	seed := []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, endingJSON, "ending-seed"),
		personFactProjectionClaim(personID, target, retainedJSON, "retained-seed"),
	}
	_, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "employment-seed", seed, nil), nil)
	require.NoError(err)
	current, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	require.NoError(err)
	require.Len(current, 2)
	var endingID, retainedID int64
	for _, employment := range current {
		switch employment.OrganizationID {
		case firstOrganization.ID:
			endingID = employment.ID
		case secondOrganization.ID:
			retainedID = employment.ID
		}
	}
	require.NotZero(endingID)
	require.NotZero(retainedID)

	effectiveAt := time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC)
	supersession := personFactProjectionClaim(personID, target, endingJSON, "ending-supersession")
	supersession.Relation = personfacts.RelationSupersede
	supersession.ValidFrom = &effectiveAt
	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "employment-supersession", []personfacts.ProposedClaim{supersession}, nil), nil)
	require.NoError(err)
	require.Len(result.Projections, 1)
	assert.Equal(personfacts.ProjectionRef{Kind: "employment", RowID: endingID}, result.Projections[0])

	ended, err := st.GetEmploymentContext(t.Context(), endingID)
	require.NoError(err)
	assert.False(ended.IsCurrent)
	assert.Equal(ProvenanceExtraction, ended.Source)
	require.NotNil(ended.SourceRef)
	assert.Contains(*ended.SourceRef, "person-fact-decision:")
	require.NotNil(ended.EndDate)
	assert.Equal(PartialDate{Year: new(2026), Month: new(1), Day: new(15)}, *ended.EndDate)
	retained, err := st.GetEmploymentContext(t.Context(), retainedID)
	require.NoError(err)
	assert.True(retained.IsCurrent)
}

func TestPersonFactEmploymentSupersessionClampsEndDateToEmploymentStart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Backdated Company", "backdated-employment.example")
	submitted := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Backdated Company","domain":"backdated-employment.example"},"title":"Engineer","start_date":{"year":2020,"month":2}}`,
		organization.ID)

	seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "backdated-employment-seed", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, submitted, "backdated-employment-seed"),
		}, nil), nil)
	require.NoError(err)
	require.Len(seed.Projections, 1)

	backdated := time.Date(2019, time.January, 15, 0, 0, 0, 0, time.UTC)
	supersession := personFactProjectionClaim(
		personID, target, submitted, "backdated-employment-supersession")
	supersession.Relation = personfacts.RelationSupersede
	supersession.ValidFrom = &backdated
	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "backdated-employment-supersession",
			[]personfacts.ProposedClaim{supersession}, nil), nil)
	require.NoError(err)
	require.Len(result.Projections, 1)
	assert.Equal(seed.Projections[0], result.Projections[0])

	ended, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
	require.NoError(err)
	assert.False(ended.IsCurrent)
	require.NotNil(ended.EndDate)
	assert.Equal(PartialDate{Year: new(2020), Month: new(2)}, *ended.EndDate)
}

func TestPersonFactEmploymentNegativeClaimRetractsOwnedHistoricalProjection(t *testing.T) {
	for _, relation := range []personfacts.ClaimRelation{
		personfacts.RelationContradict,
		personfacts.RelationSupersede,
	} {
		t.Run(string(relation), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(t, st, "Retracted Company", "retracted.example")
			submitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Retracted Company","domain":"retracted.example"},"title":"Engineer","start_date":{"year":2020},"end_date":{"year":2022}}`,
				organization.ID)

			seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "historical-negative-seed", []personfacts.ProposedClaim{
					personFactProjectionClaim(personID, target, submitted, "historical-negative-seed"),
				}, nil), nil)
			requirements.NoError(err)
			requirements.Len(seed.Projections, 1)

			negative := personFactProjectionClaim(personID, target, submitted, "historical-negative")
			negative.Relation = relation
			input := personFactProjectionInput(
				personID, "historical-negative", []personfacts.ProposedClaim{negative}, nil)
			input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			requirements.NoError(err)
			requirements.Len(result.Projections, 1)
			assertions.Equal(seed.Projections[0], result.Projections[0])

			_, err = st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
			requirements.ErrorIs(err, ErrEmploymentNotFound)
			employments, err := st.ListEmploymentsContext(
				t.Context(), EmploymentFilter{PersonID: personID})
			requirements.NoError(err)
			assertions.Empty(employments)
		})
	}
}

func TestPersonFactEmploymentMultipleProjectionsBumpVCardOnce(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	first := createPersonFactOrganization(t, st, "Bump One", "bump-one.example")
	second := createPersonFactOrganization(t, st, "Bump Two", "bump-two.example")
	before := personFactProjectionRevision(t, st, personID)
	claims := []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target,
			fmt.Sprintf(`{"organization":{"id":%d,"name":"Bump One","domain":"bump-one.example"},"title":"Engineer"}`, first.ID), "bump-one"),
		personFactProjectionClaim(personID, target,
			fmt.Sprintf(`{"organization":{"id":%d,"name":"Bump Two","domain":"bump-two.example"},"title":"Advisor"}`, second.ID), "bump-two"),
	}

	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "employment-bump", claims, nil), nil)
	require.NoError(t, err)
	assert.Len(t, result.Projections, 2)
	assert.Equal(t, before+1, personFactProjectionRevision(t, st, personID))
}

func TestPersonFactEmploymentExpiresOwnedProjectionAtExclusiveValidityEnd(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	expiringOrganization := createPersonFactOrganization(
		t, st, "Expiring Company", "expiring-employment.example")
	retainedOrganization := createPersonFactOrganization(
		t, st, "Retained Company", "retained-employment.example")
	expiringJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Expiring Company","domain":"expiring-employment.example"},"title":"Engineer","start_date":{"year":2020,"month":2}}`,
		expiringOrganization.ID)
	expiredCorrectionJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Expiring Company","domain":"expiring-employment.example"},"title":"Engineer","role":"Platform","start_date":{"year":2020,"month":2}}`,
		expiringOrganization.ID)
	retainedJSON := fmt.Sprintf(
		`{"organization":{"id":%d,"name":"Retained Company","domain":"retained-employment.example"},"title":"Advisor"}`,
		retainedOrganization.ID)
	validUntil := personFactLedgerNow.Add(48 * time.Hour)
	expiring := personFactProjectionClaim(
		personID, target, expiringJSON, "employment-expiry-seed")
	expiring.ValidUntil = &validUntil
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(
		personID, "employment-expiry-seed", []personfacts.ProposedClaim{
			expiring,
			personFactProjectionClaim(personID, target, retainedJSON, "employment-expiry-retained"),
		}, nil), nil)
	requirements.NoError(err)
	requirements.Len(seed.Projections, 2)

	boundary := personFactProjectionClaim(
		personID, target, expiredCorrectionJSON, "employment-expiry-boundary")
	boundary.ValidUntil = &validUntil
	boundaryInput := personFactProjectionInput(
		personID, "employment-expiry-boundary", []personfacts.ProposedClaim{boundary}, nil)
	boundaryInput.ResolvedAt = validUntil
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), boundaryInput, nil)
	requirements.NoError(err)
	foundExpired := false
	for _, decision := range result.Decisions {
		if decision.Reason == personfacts.ReasonOutsideValidity {
			foundExpired = true
		}
	}
	assertions.True(foundExpired)
	replayed, err := st.ApplyPersonFactGenerationContext(t.Context(), boundaryInput, nil)
	requirements.NoError(err)
	assertions.Equal(result, replayed)

	employments, err := st.ListEmploymentsContext(
		t.Context(), EmploymentFilter{PersonID: personID})
	requirements.NoError(err)
	requirements.Len(employments, 2)
	for _, employment := range employments {
		switch employment.OrganizationID {
		case expiringOrganization.ID:
			assertions.False(employment.IsCurrent)
			requirements.NotNil(employment.EndDate)
			assertions.Equal(
				PartialDate{Year: new(2026), Month: new(8), Day: new(24)}, *employment.EndDate)
		case retainedOrganization.ID:
			assertions.True(employment.IsCurrent)
			assertions.Nil(employment.EndDate)
		default:
			assertions.Fail("unexpected employment", "organization ID %d", employment.OrganizationID)
		}
	}
}

func TestPersonFactEmploymentUnsupportedEvidenceDoesNotRetire(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	organization := createPersonFactOrganization(t, st, "Unsupported Company", "unsupported.example")
	submitted := fmt.Sprintf(`{"organization":{"id":%d,"name":"Unsupported Company","domain":"unsupported.example"},"title":"Engineer"}`, organization.ID)
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "unsupported-seed", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, submitted, "unsupported-seed"),
		}, nil), nil)
	require.NoError(err)
	require.Len(seed.Projections, 1)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	require.Len(evidence, 1)
	beforeRevision := personFactProjectionRevision(t, st, personID)

	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "unsupported-status", nil, []personfacts.EvidenceStatusChange{{
			EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion,
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}}), nil)
	require.NoError(err)
	assert.Empty(result.Projections)
	assert.Equal(beforeRevision, personFactProjectionRevision(t, st, personID))
	employment, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
	require.NoError(err)
	assert.True(employment.IsCurrent)
	assert.Nil(employment.EndDate)
}

func TestPersonFactEmploymentReplayIsByteIdentical(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, "employment")
	submitted := `{"organization":{"name":"Replay Company","domain":"replay.example"},"title":"Engineer"}`
	input := personFactProjectionInput(personID, "employment-replay", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, submitted, "employment-replay"),
	}, nil)

	fresh, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	replayed, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	assert.Equal(fresh, replayed)
	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: personID})
	require.NoError(err)
	assert.Len(employments, 1)
}

func TestPersonFactEmploymentAutomaticLockPrecedesCurrentStateSQLite(t *testing.T) {
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("SQLite production-path lock-order observation")
	}
	assert := assert.New(t)
	require := require.New(t)
	gate := newPersonFactEmploymentLockOrderGate(personFactEmploymentStagePerson)
	defer gate.release()
	st := newPersonFactEmploymentSQLiteGateStore(t, gate)
	personID, target, organization := newPersonFactEmploymentLockOrderFixture(t, st, "sqlite")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	declaredCtx := context.WithValue(ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorDeclared)
	automaticCtx := context.WithValue(ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorAutomatic)
	type declaredResult struct {
		employment *Employment
		err        error
	}
	declaredDone := make(chan declaredResult, 1)
	go func() {
		employment, err := st.AddEmploymentContext(declaredCtx, EmploymentInput{
			PersonID: personID, OrganizationID: organization.ID,
			Title: new("Declared Advisor"), Source: ProvenanceUser, IsPrimary: new(false),
		})
		declaredDone <- declaredResult{employment: employment, err: err}
	}()
	waitPersonFactEmploymentLockSignal(t, gate.declaredPaused, "declared mutation did not reach its person lock")

	automaticDone := make(chan error, 1)
	go func() {
		submitted := fmt.Sprintf(`{"organization":{"id":%d,"name":"Lock Order Company","domain":"lock-order.example"},"title":"Automatic Engineer"}`, organization.ID)
		_, err := st.ApplyPersonFactGenerationContext(automaticCtx,
			personFactProjectionInput(personID, "lock-order-sqlite", []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target, submitted, "lock-order-sqlite"),
			}, nil), nil)
		automaticDone <- err
	}()
	firstAutomatic := waitPersonFactEmploymentLockStage(t, gate.automaticFirst)
	require.NoError(waitPersonFactEmploymentLockResult(t, automaticDone, "automatic projection"))
	gate.release()
	declared := waitPersonFactEmploymentLockResult(t, declaredDone, "declared mutation")
	require.NoError(declared.err)
	require.NotNil(declared.employment)
	assert.Equal(personFactEmploymentStagePerson, firstAutomatic,
		"automatic projection must lock the person before reading current employment or organizations")

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	require.NoError(err)
	assert.Len(employments, 2)
}

func TestPersonFactEmploymentAutomaticAndDeclaredMutationSerializePostgres(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("PostgreSQL person/organization lock interleaving requires MSGVAULT_TEST_DB")
	}
	assert := assert.New(t)
	require := require.New(t)
	gate := newPersonFactEmploymentLockOrderGate(personFactEmploymentStageOrganization)
	defer gate.release()
	st := newPersonFactEmploymentPostgresGateStore(t, dbURL, gate)
	personID, target, organization := newPersonFactEmploymentLockOrderFixture(t, st, "postgres")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	declaredCtx := context.WithValue(ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorDeclared)
	automaticCtx := context.WithValue(ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorAutomatic)
	type declaredResult struct {
		employment *Employment
		err        error
	}
	declaredDone := make(chan declaredResult, 1)
	go func() {
		employment, err := st.AddEmploymentContext(declaredCtx, EmploymentInput{
			PersonID: personID, OrganizationID: organization.ID,
			Title: new("Declared Advisor"), Source: ProvenanceUser, IsPrimary: new(false),
		})
		declaredDone <- declaredResult{employment: employment, err: err}
	}()
	waitPersonFactEmploymentLockSignal(t, gate.declaredPaused,
		"declared mutation did not hold the person lock before organization validation")

	automaticDone := make(chan struct {
		result *personfacts.GenerationResult
		err    error
	}, 1)
	go func() {
		submitted := fmt.Sprintf(`{"organization":{"id":%d,"name":"Lock Order Company","domain":"lock-order.example"},"title":"Automatic Engineer"}`, organization.ID)
		result, err := st.ApplyPersonFactGenerationContext(automaticCtx,
			personFactProjectionInput(personID, "lock-order-postgres", []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target, submitted, "lock-order-postgres"),
			}, nil), nil)
		automaticDone <- struct {
			result *personfacts.GenerationResult
			err    error
		}{result: result, err: err}
	}()
	firstAutomatic := waitPersonFactEmploymentLockStage(t, gate.automaticFirst)
	gate.release()
	declared := waitPersonFactEmploymentLockResult(t, declaredDone, "declared mutation")
	automatic := waitPersonFactEmploymentLockResult(t, automaticDone, "automatic projection")
	require.NoError(declared.err)
	require.NotNil(declared.employment)
	require.NoError(automatic.err)
	require.NotNil(automatic.result)
	assert.Equal(personFactEmploymentStagePerson, firstAutomatic,
		"automatic projection must wait on the person before touching current employment or organizations")
	assert.Empty(automatic.result.Projections,
		"the automatic projection must observe the committed declared pin after waiting")

	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Equal("Declared Advisor", *employments[0].Title)
	assert.Equal(ProvenanceUser, employments[0].Source)
}

func TestPersonFactEmploymentNonIDTableLockDeadlockRetriesPostgres(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("PostgreSQL table and row locks are required for the retry regression")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	gate := newPersonFactEmploymentLockOrderGate(personFactEmploymentStageOrganization)
	t.Cleanup(gate.release)
	st := newPersonFactEmploymentPostgresGateStore(t, dbURL, gate)
	personID, target, organization := newPersonFactEmploymentLockOrderFixture(
		t, st, "non-id-deadlock")
	var deadlocksBefore int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `
		SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&deadlocksBefore))

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	declaredCtx := context.WithValue(
		ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorDeclared)
	automaticCtx := context.WithValue(
		ctx, personFactEmploymentLockActorKey{}, personFactEmploymentActorAutomatic)
	type declaredResult struct {
		employment *Employment
		err        error
	}
	declaredDone := make(chan declaredResult, 1)
	go func() {
		employment, err := st.AddEmploymentContext(declaredCtx, EmploymentInput{
			PersonID: personID, OrganizationID: organization.ID,
			Title: new("Declared Advisor"), Source: ProvenanceUser, IsPrimary: new(false),
		})
		declaredDone <- declaredResult{employment: employment, err: err}
	}()
	waitPersonFactEmploymentLockSignal(t, gate.declaredPaused,
		"declared mutation did not hold the person before organization validation")

	automaticDone := make(chan struct {
		result *personfacts.GenerationResult
		err    error
	}, 1)
	go func() {
		result, err := st.ApplyPersonFactGenerationContext(automaticCtx,
			personFactProjectionInput(personID, "non-id-deadlock", []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target,
					`{"organization":{"name":"Lock Order Company","domain":"lock-order.example"},"title":"Automatic Engineer"}`,
					"non-id-deadlock"),
			}, nil), nil)
		automaticDone <- struct {
			result *personfacts.GenerationResult
			err    error
		}{result: result, err: err}
	}()
	firstAutomatic := waitPersonFactEmploymentLockStage(t, gate.automaticFirst)
	gate.release()
	declared := waitPersonFactEmploymentLockResult(t, declaredDone, "declared mutation")
	automatic := waitPersonFactEmploymentLockResult(t, automaticDone, "automatic projection")
	requirements.NoError(declared.err)
	requirements.NotNil(declared.employment)
	requirements.NoError(automatic.err)
	requirements.NotNil(automatic.result)
	assertions.Equal(personFactEmploymentStagePerson, firstAutomatic,
		"non-ID projection must take its table lock, then preserve person-before-row ordering")
	requirements.Eventually(func() bool {
		var deadlocksAfter int64
		return st.DB().QueryRowContext(t.Context(), `
			SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()
		`).Scan(&deadlocksAfter) == nil && deadlocksAfter > deadlocksBefore
	}, 5*time.Second, 10*time.Millisecond,
		"the non-ID table/person lock cycle did not exercise PostgreSQL deadlock retry")
	employments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	requirements.NoError(err)
	requirements.NotEmpty(employments)
	assertions.Contains([]string{"Declared Advisor", "Automatic Engineer"},
		*employments[0].Title)
}

type personFactEmploymentLockActorKey struct{}

const (
	personFactEmploymentActorAutomatic    = "automatic"
	personFactEmploymentActorDeclared     = "declared"
	personFactEmploymentStagePerson       = "person"
	personFactEmploymentStageCurrent      = "current-employment"
	personFactEmploymentStageOrganization = "organization"
)

type personFactEmploymentLockOrderGate struct {
	mu              sync.Mutex
	pauseStage      string
	armed           bool
	declaredPaused  chan struct{}
	releaseDeclared chan struct{}
	automaticFirst  chan string
	declaredSeen    bool
	automaticSeen   bool
	releaseOnce     sync.Once
}

func newPersonFactEmploymentLockOrderGate(pauseStage string) *personFactEmploymentLockOrderGate {
	return &personFactEmploymentLockOrderGate{
		pauseStage: pauseStage, armed: true,
		declaredPaused: make(chan struct{}), releaseDeclared: make(chan struct{}),
		automaticFirst: make(chan string, 1),
	}
}

func (g *personFactEmploymentLockOrderGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseDeclared) })
}

func (g *personFactEmploymentLockOrderGate) beforeStatement(
	ctx context.Context, query string,
) error {
	actor, _ := ctx.Value(personFactEmploymentLockActorKey{}).(string)
	if actor == "" {
		return nil
	}
	stage := personFactEmploymentStatementStage(query)
	if stage == "" {
		return nil
	}
	g.mu.Lock()
	if !g.armed {
		g.mu.Unlock()
		return nil
	}
	if actor == personFactEmploymentActorDeclared && stage == g.pauseStage && !g.declaredSeen {
		g.declaredSeen = true
		close(g.declaredPaused)
		release := g.releaseDeclared
		g.mu.Unlock()
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if actor == personFactEmploymentActorAutomatic && !g.automaticSeen {
		g.automaticSeen = true
		g.automaticFirst <- stage
	}
	g.mu.Unlock()
	return nil
}

func personFactEmploymentStatementStage(query string) string {
	query = strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(query, "SELECT id FROM persons WHERE id ="):
		return personFactEmploymentStagePerson
	case strings.Contains(query, "FROM employments WHERE person_id ="):
		return personFactEmploymentStageCurrent
	case strings.Contains(query, "FROM organizations WHERE id ="):
		return personFactEmploymentStageOrganization
	default:
		return ""
	}
}

type personFactEmploymentGateConnector struct {
	driver.Connector

	gate *personFactEmploymentLockOrderGate
}

func (c *personFactEmploymentGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &personFactEmploymentGateConn{Conn: conn, gate: c.gate}, nil
}

type personFactEmploymentSQLiteConnector struct {
	driver driver.Driver
	dsn    string
}

func (c *personFactEmploymentSQLiteConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.Open(c.dsn)
}

func (c *personFactEmploymentSQLiteConnector) Driver() driver.Driver { return c.driver }

type personFactEmploymentGateConn struct {
	driver.Conn

	gate *personFactEmploymentLockOrderGate
}

func (c *personFactEmploymentGateConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	if err := c.gate.beforeStatement(ctx, query); err != nil {
		return nil, err
	}
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}

func (c *personFactEmploymentGateConn) ExecContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	if err := c.gate.beforeStatement(ctx, query); err != nil {
		return nil, err
	}
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *personFactEmploymentGateConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *personFactEmploymentGateConn) BeginTx(
	ctx context.Context, opts driver.TxOptions,
) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, errors.New("wrapped employment lock-order connection does not implement ConnBeginTx")
}

func (c *personFactEmploymentGateConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *personFactEmploymentGateConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *personFactEmploymentGateConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *personFactEmploymentGateConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func newPersonFactEmploymentSQLiteGateStore(
	t *testing.T, gate *personFactEmploymentLockOrderGate,
) *Store {
	t.Helper()
	require := require.New(t)
	sqliteDriver := &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		return conn.RegisterFunc(sqliteutil.UnicodeLowerFunction, strings.ToLower, true)
	}}
	base := &personFactEmploymentSQLiteConnector{
		driver: sqliteDriver,
		dsn:    filepath.Join(t.TempDir(), "employment-lock-order.db") + testSQLiteParams,
	}
	db := sql.OpenDB(&personFactEmploymentGateConnector{Connector: base, gate: gate})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	dialect := &SQLiteDialect{}
	st := &Store{
		db: newLoggedDB(db, dialect.Rebind), dbPath: base.dsn, dialect: dialect,
	}
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchemaContext(t.Context()))
	return st
}

func newPersonFactEmploymentPostgresGateStore(
	t *testing.T, dbURL string, gate *personFactEmploymentLockOrderGate,
) *Store {
	t.Helper()
	require := require.New(t)
	base := newPGStoreInternal(t, dbURL)
	config, err := postgresConnConfig(base.dbPath, false)
	require.NoError(err)
	db := sql.OpenDB(&personFactEmploymentGateConnector{
		Connector: stdlib.GetConnector(*config), gate: gate,
	})
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	dialect := &PostgreSQLDialect{}
	require.NoError(dialect.InitConn(db))
	st := &Store{
		db: newLoggedDB(db, dialect.Rebind), dbPath: base.dbPath, dialect: dialect,
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newPersonFactEmploymentLockOrderFixture(
	t *testing.T, st *Store, suffix string,
) (int64, personfacts.TargetDescriptor, *Organization) {
	t.Helper()
	require := require.New(t)
	participantID, err := st.EnsureParticipant(
		"employment-lock-order-"+suffix+"@example.invalid", "Lock Order Person", "example.invalid")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	organization := createPersonFactOrganization(
		t, st, "Lock Order Company", "lock-order.example")
	return person.ID, projectionTargetBySlug(t, st, "employment"), organization
}

func waitPersonFactEmploymentLockSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	require := require.New(t)
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		require.FailNow(message)
	}
}

func waitPersonFactEmploymentLockStage(t *testing.T, stages <-chan string) string {
	t.Helper()
	require := require.New(t)
	select {
	case stage := <-stages:
		return stage
	case <-time.After(5 * time.Second):
		require.FailNow("automatic projection did not reach employment lock-sensitive state")
		return ""
	}
}

func waitPersonFactEmploymentLockResult[T any](
	t *testing.T, results <-chan T, operation string,
) T {
	t.Helper()
	require := require.New(t)
	select {
	case result := <-results:
		return result
	case <-time.After(10 * time.Second):
		require.FailNow(operation + " did not finish")
		var zero T
		return zero
	}
}

func createPersonFactOrganization(t *testing.T, st *Store, name, domain string) *Organization {
	t.Helper()
	input := OrganizationInput{Name: name, Kind: OrganizationKindCompany}
	if domain != "" {
		input.PrimaryDomain = &domain
	}
	organization, err := st.CreateOrganizationContext(t.Context(), input)
	require.NoError(t, err)
	return organization
}
