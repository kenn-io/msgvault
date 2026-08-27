package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestApplyPersonFactGenerationProjectsValidClaimsAndRetainsInvalidClaims(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "mixed", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "valid"),
		personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout], `42`, "invalid"),
	}, nil)

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Resolutions, 2)
	require.Len(result.Decisions, 2)
	assert.Equal([]personfacts.DecisionAction{
		personfacts.DecisionApplied, personfacts.DecisionInvalid,
	}, []personfacts.DecisionAction{result.Decisions[0].Action, result.Decisions[1].Action})
	assert.NotEmpty(result.Projections)

	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("chat", *values[0].Value.Text)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	require.Len(claims, 2)
	for _, claim := range claims {
		if claim.Target.Key == targets[AttributeSlugAskMeAbout].Key {
			assert.Nil(claim.Normalized)
		}
	}
}

func TestLoadPersonFactResolvedClaimsQueriesStayBoundedAcrossUnrelatedHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	primaryTarget := targets[AttributeSlugPrimaryChannel]
	unrelatedTarget := targets[AttributeSlugAskMeAbout]

	for index := range 8 {
		suffix := fmt.Sprintf("bounded-history-%d", index)
		_, err := st.ApplyPersonFactGenerationContext(t.Context(),
			personFactProjectionInput(personID, suffix, []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, primaryTarget, `"chat"`, suffix+"-primary"),
				personFactProjectionClaim(personID, unrelatedTarget, `"topic"`, suffix+"-unrelated"),
			}, nil), nil)
		require.NoError(err)
	}

	storeLogOptions := SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000}
	ConfigureSQLLogging(storeLogOptions)
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })
	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	var resolved []personfacts.ResolvedClaim
	require.NoError(st.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
		var loadErr error
		resolved, _, loadErr = st.loadPersonFactResolvedClaimsTx(
			t.Context(), tx, personID, primaryTarget, nil)
		return loadErr
	}))
	require.Len(resolved, 8)
	for _, claim := range resolved {
		assert.Equal(primaryTarget.Key, claim.Claim.Target.Key)
	}
	queryCount := strings.Count(logBuffer.String(), `"msg":"sql"`)
	assert.Positive(queryCount, "SQL trace must observe the production query path")
	assert.LessOrEqual(queryCount, 4,
		"target claim hydration must use a bounded number of SQL statements")
}

func TestApplyPersonFactGenerationPersistsDistinctMalformedEvidenceSubmissions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)

	input := personFactProjectionInput(personID, "malformed-evidence-identity", nil, nil)
	claim := personFactProjectionClaim(
		personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "malformed-evidence-first")
	claim.Evidence[0].PersonID = personID + 1
	input.Claims = []personfacts.ProposedClaim{claim}
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	requirements.Len(first.Decisions, 1)
	assertions.Equal(personfacts.DecisionIdentityRejected, first.Decisions[0].Action)

	input.Claims[0].Evidence[0].SourceURL = "https://example.com/malformed-evidence-second"
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	requirements.Len(second.Decisions, 2)
	for _, decision := range second.Decisions {
		assertions.Equal(personfacts.DecisionIdentityRejected, decision.Action)
	}
	assertions.NotEqual(first.GenerationID, second.GenerationID)
	assertions.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assertions.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_claims"))
}

func TestApplyPersonFactGenerationPersistsUnsupportedTargetBesideValidSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	unsupported := targets[AttributeSlugAskMeAbout]
	unsupported.Kind = personfacts.TargetKind("candidate")
	unsupported.Key = "system:candidate"
	unsupported.UniversalID = unsupported.Key
	unsupported.Revision = strings.Repeat("a", 64)
	input := personFactProjectionInput(personID, "unsupported-target", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, unsupported, `"candidate"`, "unsupported-target"),
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "supported-target"),
	}, nil)

	first, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(first.Decisions, 2)
	require.Len(first.Projections, 1)
	invalid := 0
	for _, decision := range first.Decisions {
		if decision.Action == personfacts.DecisionInvalid {
			invalid++
			assert.Equal(personfacts.ReasonUnsupportedTarget, decision.Reason)
			assert.Nil(decision.Projection)
		}
	}
	assert.Equal(1, invalid)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	require.Len(claims, 2)
	foundUnsupported := false
	for _, claim := range claims {
		if claim.Target.Kind != unsupported.Kind {
			continue
		}
		foundUnsupported = true
		require.NotNil(claim.Failure)
		assert.Equal(personfacts.ReasonUnsupportedTarget, claim.Failure.Reason)
		assert.Nil(claim.Normalized)
	}
	assert.True(foundUnsupported)

	replayed, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	assert.Equal(first.GenerationID, replayed.GenerationID)
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_attribute_values"))

	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	var evidenceKey string
	for _, item := range evidence {
		if strings.HasSuffix(item.Input.SourceURL, "/unsupported-target") {
			evidenceKey = item.Key
		}
	}
	require.NotEmpty(evidenceKey)
	status := personFactProjectionInput(personID, "unsupported-target-status", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	statusResult, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	require.Len(statusResult.Resolutions, 1)
	require.Len(statusResult.Decisions, 1)
	assert.Equal(unsupported.Kind, statusResult.Resolutions[0].Target.Kind)
	assert.Equal(personfacts.ReasonUnsupportedTarget, statusResult.Decisions[0].Reason)
	statusReplay, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	assert.Equal(statusResult.GenerationID, statusReplay.GenerationID)
}

func TestApplyPersonFactGenerationPreservesSensitivePolicyFailureForNonProjectableTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugReligion)
	input := personFactProjectionInput(personID, "sensitive-policy", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"Buddhist"`, "sensitive-policy"),
	}, nil)
	input.Policy.AllowSensitive = false

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.DecisionPolicyRejected, result.Decisions[0].Action)
	assert.Equal(personfacts.ReasonSensitivePolicy, result.Decisions[0].Reason)
	assert.Empty(result.Projections)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	require.Len(claims, 1)
	require.NotNil(claims[0].Failure)
	assert.Equal(personfacts.DecisionPolicyRejected, claims[0].Failure.Action)
	assert.Equal(personfacts.ReasonSensitivePolicy, claims[0].Failure.Reason)
}

func TestApplyPersonFactStatusOnlyGenerationAppliesCurrentSensitivePolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugReligion)
	seedInput := personFactProjectionInput(personID, "sensitive-policy-status-seed",
		[]personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, `"Buddhist"`, "sensitive-policy-status-seed"),
		}, nil)
	seedInput.Policy.AllowSensitive = true
	seed, err := st.ApplyPersonFactGenerationContext(t.Context(), seedInput, nil)
	require.NoError(err)
	require.Len(seed.Decisions, 1)
	assert.Equal(personfacts.DecisionApplied, seed.Decisions[0].Action)
	require.NotEmpty(seed.Projections)

	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	require.Len(evidence, 1)
	statusInput := personFactProjectionInput(personID, "sensitive-policy-status", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion,
			Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	statusInput.Policy.AllowSensitive = false

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), statusInput, nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.DecisionPolicyRejected, result.Decisions[0].Action)
	assert.Equal(personfacts.ReasonSensitivePolicy, result.Decisions[0].Reason)
	assert.Empty(result.Projections)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	require.Len(claims, 1)
	assert.Nil(claims[0].Failure)
}

func TestApplyPersonFactStatusOnlyGenerationKeepsUnavailableSensitiveTargetUnsupported(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store, AttributeDefinition)
	}{
		{
			name: "removed",
			mutate: func(t *testing.T, st *Store, definition AttributeDefinition) {
				t.Helper()
				_, err := st.db.Exec(`DELETE FROM person_attribute_values WHERE definition_id = ?`,
					definition.ID)
				require.NoError(t, err)
				_, err = st.db.Exec(`DELETE FROM attribute_definitions WHERE id = ?`, definition.ID)
				require.NoError(t, err)
			},
		},
		{
			name: "inactive",
			mutate: func(t *testing.T, st *Store, definition AttributeDefinition) {
				t.Helper()
				_, err := st.db.Exec(`UPDATE attribute_definitions SET is_active = FALSE WHERE id = ?`,
					definition.ID)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, AttributeSlugReligion)
			seedInput := personFactProjectionInput(personID, "sensitive-unavailable-seed-"+test.name,
				[]personfacts.ProposedClaim{
					personFactProjectionClaim(personID, target, `"Buddhist"`,
						"sensitive-unavailable-seed-"+test.name),
				}, nil)
			seedInput.Policy.AllowSensitive = true
			seed, err := st.ApplyPersonFactGenerationContext(t.Context(), seedInput, nil)
			require.NoError(err)
			require.Len(seed.Decisions, 1)
			assert.Equal(personfacts.DecisionApplied, seed.Decisions[0].Action)

			definition, err := st.GetAttributeDefinitionBySlugContext(
				t.Context(), AttributeObjectPerson, AttributeSlugReligion)
			require.NoError(err)
			test.mutate(t, st, *definition)
			evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
				personfacts.EvidenceFilter{Limit: 10})
			require.NoError(err)
			require.Len(evidence, 1)
			statusInput := personFactProjectionInput(personID, "sensitive-unavailable-status-"+test.name,
				nil, []personfacts.EvidenceStatusChange{{
					EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion,
					Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted,
				}})
			statusInput.Policy.AllowSensitive = false

			result, err := st.ApplyPersonFactGenerationContext(t.Context(), statusInput, nil)
			require.NoError(err)
			require.Len(result.Decisions, 1)
			assert.Equal(personfacts.DecisionInvalid, result.Decisions[0].Action)
			assert.Equal(personfacts.ReasonUnsupportedTarget, result.Decisions[0].Reason)
			assert.Empty(result.Projections)
		})
	}
}

func TestApplyPersonFactGenerationPersistsRemovedOrIneligibleTargetBesideValidSibling(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store, personfacts.TargetDescriptor)
	}{
		{
			name: "removed",
			mutate: func(t *testing.T, st *Store, target personfacts.TargetDescriptor) {
				t.Helper()
				require.NoError(t, func() error {
					_, err := st.db.Exec(`DELETE FROM attribute_definitions WHERE universal_id = ?`, target.Key)
					return err
				}())
			},
		},
		{
			name: "ineligible",
			mutate: func(t *testing.T, st *Store, target personfacts.TargetDescriptor) {
				t.Helper()
				require.NoError(t, func() error {
					_, err := st.db.Exec(`UPDATE attribute_definitions SET api_mutable = FALSE WHERE universal_id = ?`, target.Key)
					return err
				}())
			},
		},
		{
			name: "unsupported value type",
			mutate: func(t *testing.T, st *Store, target personfacts.TargetDescriptor) {
				t.Helper()
				require.NoError(t, func() error {
					_, err := st.db.Exec(`UPDATE attribute_definitions SET value_type = 'json' WHERE universal_id = ?`, target.Key)
					return err
				}())
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			unsupported := targets[AttributeSlugAskMeAbout]
			valid := targets[AttributeSlugPrimaryChannel]
			test.mutate(t, st, unsupported)
			input := personFactProjectionInput(personID, "catalog-target-"+test.name,
				[]personfacts.ProposedClaim{
					personFactProjectionClaim(personID, unsupported, `"sailing"`, "unsupported-catalog-target"),
					personFactProjectionClaim(personID, valid, `"chat"`, "valid-catalog-sibling"),
				}, nil)

			first, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(first.Decisions, 2)
			require.Len(first.Projections, 1)
			assert.Equal("person_attribute", first.Projections[0].Kind)
			unsupportedDecisions := 0
			for _, decision := range first.Decisions {
				if decision.Reason != personfacts.ReasonUnsupportedTarget {
					continue
				}
				unsupportedDecisions++
				assert.Equal(personfacts.DecisionInvalid, decision.Action)
				assert.Nil(decision.Projection)
			}
			assert.Equal(1, unsupportedDecisions)

			claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
			require.NoError(err)
			require.Len(claims, 2)
			for _, claim := range claims {
				if claim.Target.Key == unsupported.Key {
					require.NotNil(claim.Failure)
					assert.Equal(personfacts.ReasonUnsupportedTarget, claim.Failure.Reason)
					assert.Nil(claim.Normalized)
				}
			}

			replayed, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			assert.Equal(first.GenerationID, replayed.GenerationID)
			assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_attribute_values"))
		})
	}
}

func TestApplyPersonFactGenerationPersistsInvalidEnvelopeClaimsAlongsideValidSibling(t *testing.T) {
	tests := []struct {
		name     string
		rawToken string
		mutate   func(*personfacts.ProposedClaim)
		check    func(*testing.T, personfacts.Claim)
	}{
		{
			name: "invalid relation", rawToken: "agrees",
			mutate: func(claim *personfacts.ProposedClaim) {
				claim.Relation = personfacts.ClaimRelation("agrees")
			},
			check: func(t *testing.T, claim personfacts.Claim) {
				t.Helper()
				assert.Equal(t, personfacts.RelationInvalid, claim.Relation)
				assert.Equal(t, personfacts.OriginExtraction, claim.Origin)
			},
		},
		{
			name: "invalid origin", rawToken: "crawler",
			mutate: func(claim *personfacts.ProposedClaim) {
				claim.Origin = personfacts.ClaimOrigin("crawler")
			},
			check: func(t *testing.T, claim personfacts.Claim) {
				t.Helper()
				assert.Equal(t, personfacts.RelationSupport, claim.Relation)
				assert.Equal(t, personfacts.OriginInvalid, claim.Origin)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			valid := personFactProjectionClaim(
				personID, targets[AttributeSlugPrimaryChannel], `"chat"`, test.name+"-valid")
			invalid := personFactProjectionClaim(
				personID, targets[AttributeSlugAskMeAbout], `"sailing"`, test.name+"-invalid")
			test.mutate(&invalid)
			input := personFactProjectionInput(
				personID, test.name, []personfacts.ProposedClaim{invalid, valid}, nil)

			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Decisions, 2)
			require.Len(result.Projections, 1)
			assert.Equal("person_attribute", result.Projections[0].Kind)
			invalidDecisions := 0
			for _, decision := range result.Decisions {
				if decision.Action == personfacts.DecisionInvalid {
					invalidDecisions++
					assert.Equal(personfacts.ReasonMalformedValue, decision.Reason)
					assert.Nil(decision.Projection)
				}
			}
			assert.Equal(1, invalidDecisions)

			claims, err := st.ListPersonFactClaimsContext(
				t.Context(), personID, personfacts.ClaimFilter{})
			require.NoError(err)
			require.Len(claims, 2)
			foundInvalid := false
			for _, claim := range claims {
				if claim.Target.Key != targets[AttributeSlugAskMeAbout].Key {
					continue
				}
				foundInvalid = true
				test.check(t, claim)
				assert.Nil(claim.Normalized)
				require.NotNil(claim.Failure)
				assert.Equal(personfacts.DecisionInvalid, claim.Failure.Action)
				assert.Equal(personfacts.ReasonMalformedValue, claim.Failure.Reason)
				assert.Contains(claim.Failure.Detail, test.rawToken)
			}
			assert.True(foundInvalid)

			var reloaded []personfacts.ResolvedClaim
			require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
				var loadErr error
				reloaded, _, loadErr = st.loadPersonFactResolvedClaimsTx(
					t.Context(), tx, personID, targets[AttributeSlugAskMeAbout], nil)
				return loadErr
			}))
			require.Len(reloaded, 1)
			require.NotNil(reloaded[0].Failure)
			assert.Contains(reloaded[0].Failure.Detail, test.rawToken)

			values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
				PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
			require.NoError(err)
			require.Len(values, 1)
			assert.Equal("chat", *values[0].Value.Text)
		})
	}
}

func TestApplyPersonFactGenerationDoesNotRetireUnownedDerivedAttribute(t *testing.T) {
	for _, source := range []Provenance{
		ProvenanceExtraction,
		ProvenanceEnrichment,
		ProvenanceSystem,
	} {
		t.Run(string(source), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			activeFrom := personFactLedgerNow.Add(-24 * time.Hour)
			confidence := 0.8
			unowned, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
				PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
				Value:      AttributeValue{Type: AttributeValueText, Text: new("chat")},
				ActiveFrom: &activeFrom, Source: source, Confidence: &confidence,
			})
			require.NoError(err)

			input := personFactProjectionInput(personID, "unowned-"+string(source),
				[]personfacts.ProposedClaim{
					personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel],
						`42`, "unowned-"+string(source)),
				}, nil)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Decisions, 1)
			assert.Equal(personfacts.DecisionInvalid, result.Decisions[0].Action)
			assert.Equal(personfacts.ReasonMalformedValue, result.Decisions[0].Reason)

			current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
				PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
			require.NoError(err)
			require.Len(current, 1)
			assert.Equal(unowned.Value.ID, current[0].ID)
			assert.Equal("chat", *current[0].Value.Text)
		})
	}
}

func TestApplyPersonFactGenerationProtectsUnownedDerivedAttributeFromProjectionPlans(t *testing.T) {
	for _, source := range []Provenance{
		ProvenanceExtraction,
		ProvenanceEnrichment,
		ProvenanceSystem,
	} {
		for _, operation := range []struct {
			name     string
			value    string
			relation personfacts.ClaimRelation
		}{
			{name: "replacement", value: `"email"`, relation: personfacts.RelationSupport},
			{name: "explicit retirement", value: `"chat"`, relation: personfacts.RelationSupersede},
		} {
			t.Run(string(source)+"/"+operation.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, personID, targets := newPersonFactProjectionStore(t)
				activeFrom := personFactLedgerNow.Add(-24 * time.Hour)
				confidence := 0.8
				unowned, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
					PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
					Value:      AttributeValue{Type: AttributeValueText, Text: new("chat")},
					ActiveFrom: &activeFrom, Source: source, Confidence: &confidence,
				})
				require.NoError(err)

				claim := personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel],
					operation.value, "unowned-plan-"+string(source)+"-"+operation.name)
				claim.Relation = operation.relation
				result, err := st.ApplyPersonFactGenerationContext(t.Context(),
					personFactProjectionInput(personID, "unowned-plan-"+string(source)+"-"+operation.name,
						[]personfacts.ProposedClaim{claim}, nil), nil)
				require.NoError(err)
				require.Len(result.Decisions, 1)
				assert.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
				assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)
				assert.Empty(result.Projections)

				current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
					PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
				require.NoError(err)
				require.Len(current, 1)
				assert.Equal(unowned.Value.ID, current[0].ID)
				assert.Equal("chat", *current[0].Value.Text)
				assert.Equal(int64(0), personFactProjectionRowCount(t, st, "person_fact_pin_events"))
			})
		}
	}
}

func TestPersonFactProjectionOwnershipUsesScopedCompositeIndex(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite query-plan coverage; PostgreSQL schema uses the same composite key order")
	}
	plan := explainPlan(t, st, personFactAttributeProjectionOwnershipSQL,
		personID, "person_attribute", int64(1))
	t.Logf("query plan:\n%s", plan)
	assert.Contains(t, plan,
		"USING COVERING INDEX idx_person_fact_decisions_projection",
		"ownership lookup must be bounded by person and projection identity:\n%s", plan)
}

func TestApplyPersonFactGenerationPersistsOverLimitTextClaimAlongsideValidSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	overLimit := strings.Repeat("🙂", 121)
	submitted, err := json.Marshal(overLimit)
	require.NoError(err)
	input := personFactProjectionInput(personID, "over-limit-text",
		[]personfacts.ProposedClaim{
			personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout],
				string(submitted), "over-limit-text"),
			personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel],
				`"chat"`, "over-limit-valid-sibling"),
		}, nil)

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Decisions, 2)
	require.Len(result.Projections, 1)
	assert.Equal("person_attribute", result.Projections[0].Kind)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	require.Len(claims, 2)
	for _, claim := range claims {
		if claim.Target.Key != targets[AttributeSlugAskMeAbout].Key {
			continue
		}
		assert.Nil(claim.Normalized)
		require.NotNil(claim.Failure)
		assert.Equal(personfacts.DecisionInvalid, claim.Failure.Action)
		assert.Equal(personfacts.ReasonMalformedValue, claim.Failure.Reason)
		assert.Contains(claim.Failure.Detail, "max_length 120")
	}
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("chat", *values[0].Value.Text)
}

func TestApplyPersonFactGenerationPersistsTamperedTargetDescriptorAlongsideValidSibling(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	tampered := targets[AttributeSlugAskMeAbout]
	tampered.MaxLength = 0
	overLimit := strings.Repeat("🙂", 121)
	submitted, err := json.Marshal(overLimit)
	require.NoError(err)
	input := personFactProjectionInput(personID, "tampered-target-descriptor",
		[]personfacts.ProposedClaim{
			personFactProjectionClaim(personID, tampered,
				string(submitted), "tampered-target-descriptor"),
			personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel],
				`"chat"`, "tampered-target-valid-sibling"),
		}, nil)

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Decisions, 2)
	require.Len(result.Projections, 1)
	assert.Equal("person_attribute", result.Projections[0].Kind)

	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	require.Len(claims, 2)
	foundTampered := false
	for _, claim := range claims {
		if claim.Target.Key != tampered.Key {
			continue
		}
		foundTampered = true
		assert.Nil(claim.Normalized)
		require.NotNil(claim.Failure)
		assert.Equal(personfacts.DecisionInvalid, claim.Failure.Action)
		assert.Equal(personfacts.ReasonStaleTargetRevision, claim.Failure.Reason)
	}
	assert.True(foundTampered)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("chat", *values[0].Value.Text)
}

func TestApplyPersonFactGenerationPersistsOutsideValidityDecision(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*personfacts.ProposedClaim)
	}{
		{
			name: "future",
			mutate: func(claim *personfacts.ProposedClaim) {
				validFrom := personFactLedgerNow.Add(time.Hour)
				claim.ValidFrom = &validFrom
			},
		},
		{
			name: "expired",
			mutate: func(claim *personfacts.ProposedClaim) {
				validUntil := personFactLedgerNow.Add(-time.Hour)
				claim.ValidUntil = &validUntil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, targets := newPersonFactProjectionStore(t)
			claim := personFactProjectionClaim(
				personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "validity-"+test.name)
			test.mutate(&claim)
			input := personFactProjectionInput(
				personID, "validity-"+test.name, []personfacts.ProposedClaim{claim}, nil)

			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Decisions, 1)
			assert.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
			assert.Equal(personfacts.ReasonOutsideValidity, result.Decisions[0].Reason)
			assert.Empty(result.Projections)

			decisions, err := st.ListPersonFactDecisionsContext(
				t.Context(), personID, personfacts.DecisionFilter{})
			require.NoError(err)
			require.Len(decisions, 1)
			assert.Equal(personfacts.ReasonOutsideValidity, decisions[0].Reason)
		})
	}
}

func TestApplyPersonFactGenerationExpiresProjectionAtExclusiveValidityEndWhenProcessedLate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	validUntil := personFactLedgerNow.Add(time.Hour)
	firstClaim := personFactProjectionClaim(
		personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "expiry-first")
	firstClaim.ValidUntil = &validUntil
	firstInput := personFactProjectionInput(
		personID, "expiry-first", []personfacts.ProposedClaim{firstClaim}, nil)
	firstInput.ResolvedAt = personFactLedgerNow

	first, err := st.ApplyPersonFactGenerationContext(t.Context(), firstInput, nil)
	require.NoError(err)
	require.Len(first.Projections, 1)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	projectedID := values[0].ID

	boundaryClaim := personFactProjectionClaim(
		personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "expiry-boundary")
	boundaryClaim.ValidUntil = &validUntil
	boundaryInput := personFactProjectionInput(
		personID, "expiry-boundary", []personfacts.ProposedClaim{boundaryClaim}, nil)
	boundaryInput.ResolvedAt = validUntil.Add(24 * time.Hour)
	boundary, err := st.ApplyPersonFactGenerationContext(t.Context(), boundaryInput, nil)
	require.NoError(err)
	require.Len(boundary.Projections, 1)
	assert.Equal(first.Projections[0], boundary.Projections[0])
	linkedDecisions := 0
	for _, decision := range boundary.Decisions {
		assert.Equal(personfacts.ReasonOutsideValidity, decision.Reason)
		if decision.Projection != nil {
			linkedDecisions++
			assert.Equal(first.Projections[0], *decision.Projection)
		}
	}
	assert.Equal(1, linkedDecisions)

	current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(current)
	history, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel, IncludeHistory: true})
	require.NoError(err)
	require.Len(history, 1)
	assert.Equal(projectedID, history[0].ID)
	require.NotNil(history[0].ActiveUntil)
	assert.Equal(validUntil, *history[0].ActiveUntil)
}

func TestApplyPersonFactGenerationDoesNotMoveTargetResolutionBackward(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	newerResolvedAt := personFactLedgerNow.Add(2 * time.Hour)
	newerClaim := personFactProjectionClaim(personID, target, `"email"`, "newer-first")
	newerClaim.ValidFrom = &newerResolvedAt
	newerClaim.Confidence.ReportedScore = 1000
	newer := personFactProjectionInput(
		personID, "newer-first", []personfacts.ProposedClaim{newerClaim}, nil)
	newer.ResolvedAt = newerResolvedAt

	_, err := st.ApplyPersonFactGenerationContext(t.Context(), newer, nil)
	require.NoError(err)
	olderClaim := personFactProjectionClaim(personID, target, `"chat"`, "older-second")
	olderClaim.Confidence.ReportedScore = 700
	older := personFactProjectionInput(
		personID, "older-second", []personfacts.ProposedClaim{olderClaim}, nil)
	older.ResolvedAt = personFactLedgerNow
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), older, nil)
	require.NoError(err)
	require.Len(result.Resolutions, 1)
	assert.Equal(newerResolvedAt, result.Resolutions[0].ResolvedAt)

	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("email", *values[0].Value.Text)
}

func TestApplyPersonFactGenerationClampsBackdatedAttributeReplacementToCurrentStart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	initialClaim := personFactProjectionClaim(personID, target, `"chat"`, "backdated-replacement-initial")
	initialClaim.Confidence.ReportedScore = 0
	initialInput := personFactProjectionInput(
		personID, "backdated-replacement-initial", []personfacts.ProposedClaim{initialClaim}, nil)

	_, err := st.ApplyPersonFactGenerationContext(t.Context(), initialInput, nil)
	require.NoError(err)
	initial, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(initial, 1)
	currentStart := initial[0].ActiveFrom

	validFrom := currentStart.Add(-24 * time.Hour)
	correction := personFactProjectionClaim(personID, target, `"email"`, "backdated-replacement-correction")
	correction.Confidence.ReportedScore = 1000
	correction.ValidFrom = &validFrom
	correctionInput := personFactProjectionInput(
		personID, "backdated-replacement-correction", []personfacts.ProposedClaim{correction}, nil)
	correctionInput.ResolvedAt = currentStart.Add(time.Hour)

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), correctionInput, nil)
	require.NoError(err)
	assert.NotZero(result.GenerationID)
	assert.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assert.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_claims"))

	current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(current, 1)
	assert.Equal("email", *current[0].Value.Text)
	assert.Equal(currentStart, current[0].ActiveFrom)
	history, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel, IncludeHistory: true})
	require.NoError(err)
	require.Len(history, 2)
	require.NotNil(history[1].ActiveUntil)
	assert.Equal(currentStart, *history[1].ActiveUntil)
}

func TestApplyPersonFactGenerationClampsBackdatedAttributeRetirementToCurrentStart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	initialClaim := personFactProjectionClaim(personID, target, `"chat"`, "backdated-retirement-initial")
	initialClaim.Confidence.ReportedScore = 0
	initialInput := personFactProjectionInput(
		personID, "backdated-retirement-initial", []personfacts.ProposedClaim{initialClaim}, nil)

	_, err := st.ApplyPersonFactGenerationContext(t.Context(), initialInput, nil)
	require.NoError(err)
	initial, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(initial, 1)
	currentStart := initial[0].ActiveFrom

	validFrom := currentStart.Add(-24 * time.Hour)
	correction := personFactProjectionClaim(personID, target, `"chat"`, "backdated-retirement-correction")
	correction.Relation = personfacts.RelationSupersede
	correction.Confidence.ReportedScore = 1000
	correction.ValidFrom = &validFrom
	correctionInput := personFactProjectionInput(
		personID, "backdated-retirement-correction", []personfacts.ProposedClaim{correction}, nil)
	correctionInput.ResolvedAt = currentStart.Add(time.Hour)

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), correctionInput, nil)
	require.NoError(err)
	assert.NotZero(result.GenerationID)
	assert.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assert.Equal(int64(2), personFactProjectionRowCount(t, st, "person_fact_claims"))

	current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(current)
	history, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel, IncludeHistory: true})
	require.NoError(err)
	require.Len(history, 1)
	require.NotNil(history[0].ActiveUntil)
	assert.Equal(currentStart, *history[0].ActiveUntil)
}

func TestApplyPersonFactGenerationRejectsUntrackedPersonWithoutWrites(t *testing.T) {
	st, personID := newPersonFactLedgerStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugPrimaryChannel)
	input := personFactProjectionInput(personID, "untracked", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "untracked"),
	}, nil)

	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.ErrorIs(t, err, ErrPersonFactPersonNotTracked)
	assert.Equal(t, int64(0), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assert.Equal(t, int64(0), personFactProjectionRowCount(t, st, "person_fact_claims"))
}

func TestPersonTrackingUntrackWaitsForInFlightFactGeneration(t *testing.T) {
	type generationOutcome struct {
		result *personfacts.GenerationResult
		err    error
	}
	type trackingOutcome struct {
		state *PersonTracking
		err   error
	}
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "tracking-race", []personfacts.ProposedClaim{
		personFactProjectionClaim(
			personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "tracking-race"),
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	maintenanceReady := make(chan struct{})
	releaseMaintenance := make(chan struct{})
	released := false
	release := func() {
		if !released {
			close(releaseMaintenance)
			released = true
		}
	}
	defer release()
	runner := personFactTxRunner(func(ctx context.Context, fn func(*loggedTx) error) error {
		return st.withTxContext(ctx, func(tx *loggedTx) error {
			if err := fn(tx); err != nil {
				return err
			}
			close(maintenanceReady)
			select {
			case <-releaseMaintenance:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	})
	generationDone := make(chan generationOutcome, 1)
	go func() {
		result, err := st.applyPersonFactGenerationContext(ctx, input, nil, runner)
		generationDone <- generationOutcome{result: result, err: err}
	}()
	select {
	case <-maintenanceReady:
	case <-ctx.Done():
		require.FailNow("generation maintenance did not reach the commit gate", ctx.Err())
	}

	untrackDone := make(chan trackingOutcome, 1)
	go func() {
		state, err := st.SetPersonTrackingContext(ctx, personID, false)
		untrackDone <- trackingOutcome{state: state, err: err}
	}()
	var early *trackingOutcome
	select {
	case outcome := <-untrackDone:
		early = &outcome
	case <-time.After(150 * time.Millisecond):
	}
	release()

	var generation generationOutcome
	select {
	case generation = <-generationDone:
	case <-ctx.Done():
		require.FailNow("in-flight generation did not finish", ctx.Err())
	}
	require.NoError(generation.err)
	require.NotNil(generation.result)
	require.Len(generation.result.Projections, 1)

	var untracked trackingOutcome
	if early != nil {
		untracked = *early
	} else {
		select {
		case untracked = <-untrackDone:
		case <-ctx.Done():
			require.FailNow("untracking did not finish after generation commit", ctx.Err())
		}
	}
	assert.Nil(early, "untracking returned while generation maintenance was still in flight")
	require.NoError(untracked.err)
	require.NotNil(untracked.state)
	assert.False(untracked.state.Tracked)

	next := personFactProjectionInput(personID, "tracking-race-after", []personfacts.ProposedClaim{
		personFactProjectionClaim(
			personID, targets[AttributeSlugPrimaryChannel], `"email"`, "tracking-race-after"),
	}, nil)
	_, err := st.ApplyPersonFactGenerationContext(ctx, next, nil)
	require.ErrorIs(err, ErrPersonFactPersonNotTracked)
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_attribute_values"))
}

func TestApplyPersonFactPreparationCompletesBeforeTransactionBegin(t *testing.T) {
	st, personID, targets := newPersonFactProjectionStore(t)
	transactionStarted := false
	alignments := 0
	aligner := projectionAlignerFunc(func(_ context.Context, evidence personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
		assert.False(t, transactionStarted)
		alignments++
		return personfacts.AlignmentResult{
			Accepted: true, SourceVersion: evidence.SourceVersion,
			ContentSHA256: strings.Repeat("b", 64),
		}, nil
	})
	claim := personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "alignment")
	claim.Evidence[0].SourceClass = personfacts.EvidenceArchive
	claim.Evidence[0].SourceURL = ""
	claim.Evidence[0].SourceRef = "message:synthetic"
	claim.Evidence[0].ContentSHA256 = strings.Repeat("a", 64)
	spanStart, spanEnd := int64(0), int64(len(claim.Evidence[0].Excerpt))
	claim.Evidence[0].SpanStart, claim.Evidence[0].SpanEnd = &spanStart, &spanEnd
	input := personFactProjectionInput(personID, "alignment", []personfacts.ProposedClaim{claim}, nil)
	runnerCalls := 0
	runner := personFactTxRunner(func(ctx context.Context, fn func(*loggedTx) error) error {
		transactionStarted = true
		runnerCalls++
		return st.withTxContext(ctx, fn)
	})

	_, err := st.applyPersonFactGenerationContext(t.Context(), input, aligner, runner)
	require.NoError(t, err)
	assert.Equal(t, 1, alignments)
	assert.Equal(t, 1, runnerCalls)
}

func TestApplyPersonFactOperationalAlignmentFailureWritesNothing(t *testing.T) {
	st, personID, targets := newPersonFactProjectionStore(t)
	claim := personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "alignment-error")
	claim.Evidence[0].SourceClass = personfacts.EvidenceArchive
	claim.Evidence[0].SourceURL = ""
	claim.Evidence[0].SourceRef = "message:synthetic-error"
	claim.Evidence[0].ContentSHA256 = strings.Repeat("a", 64)
	spanStart, spanEnd := int64(0), int64(len(claim.Evidence[0].Excerpt))
	claim.Evidence[0].SpanStart, claim.Evidence[0].SpanEnd = &spanStart, &spanEnd
	input := personFactProjectionInput(personID, "alignment-error", []personfacts.ProposedClaim{claim}, nil)
	runnerCalled := false

	_, err := st.applyPersonFactGenerationContext(t.Context(), input,
		projectionAlignerFunc(func(context.Context, personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
			return personfacts.AlignmentResult{}, errors.New("injected alignment failure")
		}), func(context.Context, func(*loggedTx) error) error {
			runnerCalled = true
			return nil
		})
	require.ErrorContains(t, err, "injected alignment failure")
	assert.False(t, runnerCalled)
	assert.Equal(t, int64(0), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestApplyPreparedPersonFactGenerationDoesNotEndCallerTransaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(),
		personFactProjectionInput(personID, "caller-tx", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "caller-tx"),
		}, nil), nil)
	require.NoError(err)
	injected := errors.New("owner rolls back")

	err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
		_, applyErr := st.applyPreparedPersonFactGenerationTx(t.Context(), tx, prepared)
		require.NoError(applyErr)
		_, sentinelErr := tx.ExecContext(t.Context(),
			`UPDATE persons SET display_name = ? WHERE id = ?`, "sentinel", personID)
		require.NoError(sentinelErr)
		return injected
	})
	require.ErrorIs(err, injected)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	assert.Nil(person.DisplayName)
	assert.Equal(int64(0), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestApplyPreparedPersonFactGenerationIgnoresCallerMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "immutable", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "immutable"),
	}, nil)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), input, nil)
	require.NoError(err)
	wantCanonical := prepared.CanonicalJSON()
	wantKey := prepared.GenerationKey()

	input.ProgramID = "mutated-original"
	input.Claims[0].SubmittedValue[1] = 'X'
	canonical := prepared.CanonicalJSON()
	canonical[0] = 'X'
	copyInput := prepared.Input()
	copyInput.ProgramID = "mutated-accessor"
	claims := prepared.Claims()
	claims[0].SubmittedValue[1] = 'Y'
	statuses := prepared.EvidenceStatusChanges()
	statuses = append(statuses, personfacts.PreparedEvidenceStatusChange{})
	assert.Len(statuses, 1)

	var got *personfacts.GenerationResult
	err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
		got, err = st.applyPreparedPersonFactGenerationTx(t.Context(), tx, prepared)
		return err
	})
	require.NoError(err)
	assert.Equal(wantKey, got.GenerationKey)
	assert.Equal(wantCanonical, prepared.CanonicalJSON())
	stored := persistLoadedPersonFactGeneration(t, st, personID, wantKey)
	assert.Equal("fact-projection-fixture", stored.Generation.ProgramID)
}

func TestApplyPreparedPersonFactGenerationRejectsZeroValue(t *testing.T) {
	st, _, _ := newPersonFactProjectionStore(t)
	err := st.withTxContext(t.Context(), func(tx *loggedTx) error {
		_, applyErr := st.applyPreparedPersonFactGenerationTx(t.Context(), tx, personfacts.PreparedGeneration{})
		return applyErr
	})
	require.Error(t, err)
	assert.Equal(t, int64(0), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestApplyPersonFactReplayHydratesByteIdenticalResolutionResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "replay", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout], `"sailing"`, "z-last"),
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "a-first"),
	}, nil)

	fresh, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	beforeCounts := personFactProjectionCounts(t, st)
	beforeGenerationCounts := personFactGenerationRowCounts(t, st, fresh.GenerationID)
	beforeRevision := personFactProjectionRevision(t, st, personID)
	input.ResolvedAt = input.ResolvedAt.Add(24 * time.Hour)
	replayed, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	freshJSON, err := json.Marshal(fresh)
	require.NoError(err)
	replayJSON, err := json.Marshal(replayed)
	require.NoError(err)
	assert.True(bytes.Equal(freshJSON, replayJSON), "generation replay bytes differ")
	assert.NotNil(fresh.Resolutions)
	for _, resolution := range fresh.Resolutions {
		assert.NotZero(resolution.ID)
		assert.NotNil(resolution.Projections)
	}
	assert.NotContains(string(freshJSON), "operation")
	assert.Equal(beforeCounts, personFactProjectionCounts(t, st))
	assert.Equal(beforeGenerationCounts, personFactGenerationRowCounts(t, st, fresh.GenerationID))
	assert.Equal(beforeRevision, personFactProjectionRevision(t, st, personID))
}

func TestApplyPersonFactRechecksEveryTouchedDescriptorInTransaction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "descriptor-race", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "descriptor-race-a"),
		personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout], `"sailing"`, "descriptor-race-b"),
	}, nil)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), input, nil)
	require.NoError(err)
	wantClaimKeys := make(map[string]string)
	for _, claim := range prepared.Claims() {
		claimKey, keyErr := personfacts.ClaimKey(prepared.GenerationKey(), claim)
		require.NoError(keyErr)
		wantClaimKeys[claim.Target.Key] = claimKey
	}
	definitions := make([]*AttributeDefinition, 0, 2)
	for _, slug := range []string{AttributeSlugPrimaryChannel, AttributeSlugAskMeAbout} {
		definition, loadErr := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, slug)
		require.NoError(loadErr)
		definitions = append(definitions, definition)
	}
	runner := personFactTxRunner(func(ctx context.Context, fn func(*loggedTx) error) error {
		for index, definition := range definitions {
			update := AttributeDefinitionUpdate{}
			if index == 0 {
				changed := fmt.Sprintf("Changed inference description %d", index)
				changedPtr := &changed
				update.Description = &changedPtr
			} else {
				inactive := false
				update.IsActive = &inactive
			}
			_, updateErr := st.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision, update)
			require.NoError(updateErr)
		}
		return st.withTxContext(ctx, fn)
	})

	result, err := st.applyPersonFactGenerationContext(t.Context(), input, nil, runner)
	require.NoError(err)
	require.Len(result.Decisions, 2)
	reasons := make([]personfacts.DecisionReason, 0, len(result.Decisions))
	for _, decision := range result.Decisions {
		assert.Equal(personfacts.DecisionInvalid, decision.Action)
		reasons = append(reasons, decision.Reason)
	}
	assert.ElementsMatch([]personfacts.DecisionReason{
		personfacts.ReasonStaleTargetRevision, personfacts.ReasonUnsupportedTarget,
	}, reasons)
	assert.Empty(result.Projections)
	rows, err := st.db.Query(`
		SELECT target_key, claim_key, submitted_value_json, normalized_value_json, value_fingerprint
		FROM person_fact_claims ORDER BY target_key`)
	require.NoError(err)
	defer func() {
		require.NoError(rows.Close())
	}()
	storedSubmitted := map[string]string{
		targets[AttributeSlugPrimaryChannel].Key: `"chat"`,
		targets[AttributeSlugAskMeAbout].Key:     `"sailing"`,
	}
	seen := 0
	for rows.Next() {
		var targetKey, claimKey, submitted string
		var normalized, fingerprint sql.NullString
		require.NoError(rows.Scan(&targetKey, &claimKey, &submitted, &normalized, &fingerprint))
		assert.Equal(wantClaimKeys[targetKey], claimKey)
		assert.JSONEq(storedSubmitted[targetKey], submitted)
		assert.False(normalized.Valid)
		assert.False(fingerprint.Valid)
		seen++
	}
	require.NoError(rows.Err())
	assert.Equal(2, seen)
}

func TestApplyPersonFactProgramFingerprintChangesGenerationIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "program", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "program"),
	}, nil)
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	input.ProgramFingerprint = strings.Repeat("b", 64)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	assert.NotEqual(first.GenerationKey, second.GenerationKey)
	assert.NotEqual(first.GenerationID, second.GenerationID)
}

func TestApplyPersonFactMultipleTargetsBumpVCardOnce(t *testing.T) {
	st, personID, targets := newPersonFactProjectionStore(t)
	before := personFactProjectionRevision(t, st, personID)
	input := personFactProjectionInput(personID, "one-bump", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "one-bump-a"),
		personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout], `"sailing"`, "one-bump-b"),
	}, nil)

	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(t, err)
	after := personFactProjectionRevision(t, st, personID)
	assert.Equal(t, before+1, after)
}

func TestApplyPersonFactProjectionChangesSemanticRenderHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	before, err := st.LoadPersonSemanticDocumentContext(t.Context(), personID)
	require.NoError(err)
	input := personFactProjectionInput(personID, "semantic", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout], `"sailing"`, "semantic"),
	}, nil)

	_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	after, err := st.LoadPersonSemanticDocumentContext(t.Context(), personID)
	require.NoError(err)
	assert.NotEqual(before.Revision, after.Revision)
}

func TestApplyPersonFactProjectionFailureRollsBackAllTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite trigger supplies a deterministic projection failure; PostgreSQL atomicity is covered by caller-owned rollback")
	}
	_, err := st.db.Exec(`
		CREATE TRIGGER fail_ask_me_about_projection
		BEFORE INSERT ON person_attribute_values
		WHEN NEW.definition_id = (SELECT id FROM attribute_definitions WHERE slug = 'ask_me_about')
		BEGIN SELECT RAISE(ABORT, 'injected projection failure'); END`)
	require.NoError(err)
	beforeCounts := personFactProjectionCounts(t, st)
	beforeRevision := personFactProjectionRevision(t, st, personID)
	input := personFactProjectionInput(personID, "rollback", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "rollback-a"),
		personFactProjectionClaim(personID, targets[AttributeSlugAskMeAbout], `"sailing"`, "rollback-b"),
	}, nil)

	_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.ErrorContains(err, "injected projection failure")
	assert.Equal(beforeCounts, personFactProjectionCounts(t, st))
	assert.Equal(beforeRevision, personFactProjectionRevision(t, st, personID))
	values, listErr := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{})
	require.NoError(listErr)
	assert.Empty(values)
}

func TestApplyPersonFactStatusOnlyUnsupportedReresolvesTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, target, first, evidenceKey := seedProjectedPersonFact(t)
	beforeRevision := personFactProjectionRevision(t, st, personID)
	status := personFactProjectionInput(personID, "status-unsupported", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	require.Len(result.Resolutions, 1)
	require.Len(result.Decisions, 1)
	assert.Equal(target.Key, result.Resolutions[0].Target.Key)
	assert.Equal(personfacts.ReasonEvidenceUnsupported, result.Decisions[0].Reason)
	assert.NotEqual(first.Resolutions[0].InputFingerprint, result.Resolutions[0].InputFingerprint)
	assert.Equal(beforeRevision+1, personFactProjectionRevision(t, st, personID))
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(values)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(err)
	assert.Len(claims, 1, "status changes must not synthesize contradiction claims")
}

func TestApplyPersonFactStatusPersistsForRejectedClaimAfterTargetDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	description := "Disposable rejected fact target"
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), AttributeDefinitionInput{
		UniversalID: "test-deleted-rejected-target", ObjectType: AttributeObjectPerson,
		Slug: "deleted_rejected_target", Label: "Deleted rejected target", Description: &description,
		ValueType: AttributeValueText, FieldType: AttributeFieldText,
		Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser,
		APIMutable: true, IsDeletable: true,
	})
	require.NoError(err)
	target := projectionTargetBySlug(t, st, definition.Slug)
	seed := personFactProjectionInput(personID, "deleted-rejected-seed", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `123`, "deleted-rejected-seed"),
	}, nil)
	seedResult, err := st.ApplyPersonFactGenerationContext(t.Context(), seed, nil)
	require.NoError(err)
	require.Len(seedResult.Decisions, 1)
	assert.Equal(personfacts.ReasonMalformedValue, seedResult.Decisions[0].Reason)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	require.Len(evidence, 1)
	require.NoError(st.DeleteAttributeDefinitionContext(
		t.Context(), definition.ID, definition.Revision))

	status := personFactProjectionInput(personID, "deleted-rejected-status", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidence[0].Key, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	require.Len(result.Resolutions, 1)
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_fact_evidence_status_events"))
}

func TestApplyPersonFactStatusUsesGenerationChronology(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _, _, evidenceKey := seedProjectedPersonFact(t)
	newerResolvedAt := personFactLedgerNow.Add(2 * time.Hour)
	newer := personFactProjectionInput(personID, "status-newer-first", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	newer.ResolvedAt = newerResolvedAt
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), newer, nil)
	require.NoError(err)
	older := personFactProjectionInput(personID, "status-older-second", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: true,
			Reason: personfacts.EvidenceStatusSourceReimported,
		}})
	older.ResolvedAt = personFactLedgerNow.Add(time.Hour)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), older, nil)
	require.NoError(err)
	require.Len(result.Resolutions, 1)
	assert.Equal(newerResolvedAt, result.Resolutions[0].ResolvedAt)

	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(values)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	require.Len(evidence, 1)
	assert.False(evidence[0].Supported)
	require.NotNil(evidence[0].LatestStatus)
	assert.Equal(personfacts.EvidenceStatusSourceDeleted, evidence[0].LatestStatus.Reason)
}

func TestApplyPersonFactStatusOnlySupportedReactivatesTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugAskMeAbout]
	seed := personFactProjectionInput(personID, "multi-status-seed", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"sailing"`, "multi-status-sailing"),
		personFactProjectionClaim(personID, target, `"ceramics"`, "multi-status-ceramics"),
	}, nil)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), seed, nil)
	require.NoError(err)
	initial, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugAskMeAbout})
	require.NoError(err)
	require.Len(initial, 2)
	initialByValue := make(map[string]PersonAttributeValue, len(initial))
	for _, value := range initial {
		initialByValue[*value.Value.Text] = value
	}
	require.Contains(initialByValue, "sailing")
	require.Contains(initialByValue, "ceramics")
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	var evidenceKey string
	for _, item := range evidence {
		if strings.HasSuffix(item.Input.SourceURL, "/multi-status-sailing") {
			evidenceKey = item.Key
		}
	}
	require.NotEmpty(evidenceKey)
	unsupported := personFactProjectionInput(personID, "status-off", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusScopeUnlinked,
		}})
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), unsupported, nil)
	require.NoError(err)
	remaining, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugAskMeAbout})
	require.NoError(err)
	require.Len(remaining, 1)
	assert.Equal("ceramics", *remaining[0].Value.Text)
	assert.Equal(initialByValue["ceramics"].ID, remaining[0].ID,
		"the matching current fingerprint must retain its row and ordinal")
	assert.Equal(initialByValue["ceramics"].Ordinal, remaining[0].Ordinal)
	beforeRevision := personFactProjectionRevision(t, st, personID)
	supported := personFactProjectionInput(personID, "status-on", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: true,
			Reason: personfacts.EvidenceStatusScopeRelinked,
		}})

	result, err := st.ApplyPersonFactGenerationContext(t.Context(), supported, nil)
	require.NoError(err)
	require.Len(result.Decisions, 2)
	require.Len(result.Projections, 1)
	for _, decision := range result.Decisions {
		assert.Equal(personfacts.DecisionApplied, decision.Action)
	}
	assert.Equal(beforeRevision+1, personFactProjectionRevision(t, st, personID))
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugAskMeAbout, IncludeHistory: true})
	require.NoError(err)
	require.Len(values, 3)
	current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugAskMeAbout})
	require.NoError(err)
	require.Len(current, 2)
	currentByValue := make(map[string]PersonAttributeValue, len(current))
	for _, value := range current {
		currentByValue[*value.Value.Text] = value
	}
	assert.Equal(initialByValue["ceramics"].ID, currentByValue["ceramics"].ID)
	assert.Equal(initialByValue["ceramics"].Ordinal, currentByValue["ceramics"].Ordinal)
	assert.Equal(int64(2), currentByValue["sailing"].Ordinal,
		"reactivation allocates after the historical maximum instead of reusing ordinal zero")
	assert.Equal(currentByValue["sailing"].ID, result.Projections[0].RowID)
}

func TestApplyPersonFactStatusReplayIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _, _, evidenceKey := seedProjectedPersonFact(t)
	status := personFactProjectionInput(personID, "status-replay", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceEdited,
		}})
	first, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	status.ResolvedAt = status.ResolvedAt.Add(time.Hour)
	second, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	firstJSON, err := json.Marshal(first)
	require.NoError(err)
	secondJSON, err := json.Marshal(second)
	require.NoError(err)
	assert.True(bytes.Equal(firstJSON, secondJSON), "status replay bytes differ")
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_fact_evidence_status_events"))
}

func TestApplyPersonFactStatusRejectsUnknownEvidence(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	status := personFactProjectionInput(personID, "status-unknown", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: "missing-evidence", SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.ErrorContains(t, err, "unknown evidence")
	assert.Equal(t, int64(0), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestApplyPersonFactStatusRejectsSourceVersionMismatch(t *testing.T) {
	st, personID, _, _, evidenceKey := seedProjectedPersonFact(t)
	before := personFactProjectionRowCount(t, st, "person_fact_generations")
	status := personFactProjectionInput(personID, "status-version", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "wrong-version", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.ErrorContains(t, err, "source version")
	assert.Equal(t, before, personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestApplyPersonFactClaimsAndStatusesCommitAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	input := personFactProjectionInput(personID, "claims-status-atomic", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, targets[AttributeSlugPrimaryChannel], `"chat"`, "atomic"),
	}, []personfacts.EvidenceStatusChange{{
		EvidenceKey: "missing-evidence", SourceVersion: "source-v1", Supported: false,
		Reason: personfacts.EvidenceStatusIdentityReassigned,
	}})
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.ErrorContains(err, "unknown evidence")
	assert.Equal(int64(0), personFactProjectionRowCount(t, st, "person_fact_generations"))
	assert.Equal(int64(0), personFactProjectionRowCount(t, st, "person_fact_claims"))
	values, listErr := st.ListPersonAttributeValuesContext(t.Context(), personID, PersonAttributeQuery{})
	require.NoError(listErr)
	assert.Empty(values)
}

type projectionAlignerFunc func(context.Context, personfacts.EvidenceInput) (personfacts.AlignmentResult, error)

func (f projectionAlignerFunc) Align(ctx context.Context, input personfacts.EvidenceInput) (personfacts.AlignmentResult, error) {
	return f(ctx, input)
}

func newPersonFactProjectionStore(t *testing.T) (*Store, int64, map[string]personfacts.TargetDescriptor) {
	t.Helper()
	st, personID := newPersonFactLedgerStore(t)
	_, err := st.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(t, err)
	return st, personID, map[string]personfacts.TargetDescriptor{
		AttributeSlugPrimaryChannel: projectionTargetBySlug(t, st, AttributeSlugPrimaryChannel),
		AttributeSlugAskMeAbout:     projectionTargetBySlug(t, st, AttributeSlugAskMeAbout),
	}
}

func projectionTargetBySlug(t *testing.T, st *Store, slug string) personfacts.TargetDescriptor {
	t.Helper()
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	for _, target := range catalog.Targets {
		if target.Slug == slug {
			return target
		}
	}
	require.FailNow(t, "target not found", slug)
	return personfacts.TargetDescriptor{}
}

func personFactProjectionInput(
	personID int64, suffix string, claims []personfacts.ProposedClaim,
	statuses []personfacts.EvidenceStatusChange,
) personfacts.GenerationInput {
	return personfacts.GenerationInput{
		PersonID:      personID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: suffix, End: suffix + "-end"}},
		ProgramID:     "fact-projection-fixture", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: "catalog-fixture",
		Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: personFactLedgerNow,
		Policy:     personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"},
		Claims:     claims, EvidenceStatusChanges: statuses,
	}
}

func personFactProjectionClaim(
	personID int64, target personfacts.TargetDescriptor, submitted, suffix string,
) personfacts.ProposedClaim {
	subject := personID
	return personfacts.ProposedClaim{
		Target: target, Relation: personfacts.RelationSupport,
		SubmittedValue: json.RawMessage(submitted),
		Evidence: []personfacts.EvidenceInput{{
			PersonID: personID, SourceClass: personfacts.EvidencePublic,
			Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
			SourceURL: "https://example.com/" + suffix, SubjectPersonID: &subject,
			SubjectRef: "synthetic-person", Excerpt: "Synthetic evidence " + suffix,
			SourceVersion: "source-v1", EventTime: personFactLedgerNow.Add(-time.Hour),
			RecordedTime: personFactLedgerNow, IdentityScore: 990,
		}},
		Origin:     personfacts.OriginExtraction,
		Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
	}
}

func personFactProjectionRowCount(t *testing.T, st *Store, table string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, st.db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
	return count
}

func personFactProjectionCounts(t *testing.T, st *Store) map[string]int64 {
	t.Helper()
	tables := []string{
		"person_fact_generations",
		"person_fact_evidence",
		"person_fact_claims",
		"person_fact_claim_evidence",
		"person_fact_evidence_status_events",
		"person_fact_resolutions",
		"person_fact_decisions",
		"person_fact_pin_events",
		"person_attribute_values",
	}
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		counts[table] = personFactProjectionRowCount(t, st, table)
	}
	return counts
}

func personFactGenerationRowCounts(t *testing.T, st *Store, generationID int64) map[string]int64 {
	t.Helper()
	queries := map[string]string{
		"generations":    `SELECT COUNT(*) FROM person_fact_generations WHERE id = ?`,
		"claims":         `SELECT COUNT(*) FROM person_fact_claims WHERE generation_id = ?`,
		"claim_evidence": `SELECT COUNT(*) FROM person_fact_claim_evidence ce JOIN person_fact_claims c ON c.id = ce.claim_id WHERE c.generation_id = ?`,
		"evidence":       `SELECT COUNT(DISTINCT ce.evidence_id) FROM person_fact_claim_evidence ce JOIN person_fact_claims c ON c.id = ce.claim_id WHERE c.generation_id = ?`,
		"status_events":  `SELECT COUNT(*) FROM person_fact_evidence_status_events WHERE generation_id = ?`,
		"resolutions":    `SELECT COUNT(*) FROM person_fact_resolutions WHERE generation_id = ?`,
		"decisions":      `SELECT COUNT(*) FROM person_fact_decisions d JOIN person_fact_resolutions r ON r.id = d.resolution_id WHERE r.generation_id = ?`,
	}
	counts := make(map[string]int64, len(queries))
	for name, query := range queries {
		var count int64
		require.NoError(t, st.db.QueryRow(query, generationID).Scan(&count))
		counts[name] = count
	}
	return counts
}

func personFactProjectionRevision(t *testing.T, st *Store, personID int64) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, st.db.QueryRow(
		`SELECT vcard_projection_revision FROM persons WHERE id = ?`, personID).Scan(&revision))
	return revision
}

func seedProjectedPersonFact(t *testing.T) (
	*Store, int64, personfacts.TargetDescriptor, *personfacts.GenerationResult, string,
) {
	t.Helper()
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	input := personFactProjectionInput(personID, "seed-"+AttributeSlugPrimaryChannel, []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "seed-"+AttributeSlugPrimaryChannel),
	}, nil)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(t, err)
	claims, err := st.ListPersonFactClaimsContext(t.Context(), personID, personfacts.ClaimFilter{})
	require.NoError(t, err)
	require.Len(t, claims, 1)
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID,
		personfacts.EvidenceFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, evidence, 1)
	return st, personID, target, result, evidence[0].Key
}

func persistLoadedPersonFactGeneration(
	t *testing.T, st *Store, personID int64, generationKey string,
) personFactLedgerGeneration {
	t.Helper()
	var stored personFactLedgerGeneration
	require.NoError(t, st.withTxContext(t.Context(), func(tx *loggedTx) error {
		var err error
		stored, err = st.loadPersonFactGenerationTx(t.Context(), tx, personID, generationKey)
		return err
	}))
	return stored
}
