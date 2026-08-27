package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPersonFactImplicitPinUnpinRerunsAndManualSetRepins(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	activeFrom := personFactLedgerNow.Add(-24 * time.Hour)
	manual, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
		Value:      AttributeValue{Type: AttributeValueText, Text: new("email")},
		ActiveFrom: &activeFrom, Source: ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM person_fact_pin_events WHERE person_id = ?`, personID)
	require.NoError(err, "simulate a declared value that predates the pin ledger")
	input := personFactProjectionInput(personID, "implicit-pin", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "implicit-pin"),
	}, nil)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)
	assert.Empty(result.Projections)

	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	require.Len(pins, 1)
	assert.True(pins[0].Pinned)
	assert.Nil(pins[0].EventID)

	unpinned, err := st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, false, "test")
	require.NoError(err)
	assert.False(unpinned.State.Pinned)
	assert.NotNil(unpinned.State.EventID)
	assert.NotEmpty(unpinned.Projections)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("chat", *values[0].Value.Text)
	assert.NotEqual(manual.Value.ID, values[0].ID)

	_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
		Value:  AttributeValue{Type: AttributeValueText, Text: new("phone")},
		Source: ProvenanceUser,
	})
	require.NoError(err)
	pins, err = st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	require.Len(pins, 1)
	assert.True(pins[0].Pinned)
	assert.NotNil(pins[0].EventID)
}

func TestPersonFactManualNoteAppendRepinsAfterExplicitUnpin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	target := projectionTargetBySlug(t, st, AttributeSlugNotes)

	_, err := st.AppendPersonNoteContext(t.Context(), PersonNoteAppendInput{
		PersonID: personID, Text: "first curated note", Source: ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
		false, "test")
	require.NoError(err)
	_, err = st.AppendPersonNoteContext(t.Context(), PersonNoteAppendInput{
		PersonID: personID, Text: "second curated note", Source: ProvenanceUser,
	})
	require.NoError(err)

	input := personFactProjectionInput(personID, "manual-note-repin", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"automatic replacement"`, "manual-note-repin"),
	}, nil)
	input.Policy.AllowSensitive = true
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)

	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugNotes})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.Text)
	assert.Equal("first curated note\nsecond curated note", *values[0].Value.Text)
}

func TestPersonFactExplicitUnpinOverridesUnownedDerivedAttributeProtection(t *testing.T) {
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
				target := targets[AttributeSlugPrimaryChannel]
				activeFrom := personFactLedgerNow.Add(-24 * time.Hour)
				confidence := 0.8
				unowned, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
					PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
					Value:      AttributeValue{Type: AttributeValueText, Text: new("chat")},
					ActiveFrom: &activeFrom, Source: source, Confidence: &confidence,
				})
				require.NoError(err)

				pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
				require.NoError(err)
				require.Len(pins, 1)
				assert.True(pins[0].Pinned)
				assert.Nil(pins[0].EventID)

				unpinned, err := st.SetPersonFactPinContext(t.Context(), personID,
					personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
					false, "test")
				require.NoError(err)
				assert.False(unpinned.State.Pinned)
				assert.NotNil(unpinned.State.EventID)
				assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_fact_pin_events"))
				pins, err = st.ListPersonFactPinsContext(t.Context(), personID)
				require.NoError(err)
				require.Len(pins, 1)
				assert.False(pins[0].Pinned)
				assert.Equal(unpinned.State.EventID, pins[0].EventID)

				claim := personFactProjectionClaim(personID, target, operation.value,
					"unowned-unpin-"+string(source)+"-"+operation.name)
				claim.Relation = operation.relation
				result, err := st.ApplyPersonFactGenerationContext(t.Context(),
					personFactProjectionInput(personID, "unowned-unpin-"+string(source)+"-"+operation.name,
						[]personfacts.ProposedClaim{claim}, nil), nil)
				require.NoError(err)
				require.Len(result.Decisions, 1)
				assert.Equal(personfacts.DecisionApplied, result.Decisions[0].Action)
				assert.NotEmpty(result.Projections)

				current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
					PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
				require.NoError(err)
				if operation.relation == personfacts.RelationSupport {
					require.Len(current, 1)
					assert.NotEqual(unowned.Value.ID, current[0].ID)
					assert.Equal("email", *current[0].Value.Text)
				} else {
					assert.Empty(current)
				}
			})
		}
	}
}

func TestListPersonFactPinsDoesNotInferPinForFactOwnedDerivedAttribute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, target, result, evidenceKey := seedProjectedPersonFact(t)
	assert.Equal(personfacts.TargetAttribute, target.Kind)
	require.NotEmpty(result.Projections)
	assert.NotEmpty(evidenceKey)

	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(pins)
}

func TestListPersonFactPinsAttributeDiscoveryUsesCurrentIndex(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite query-plan coverage; PostgreSQL uses the equivalent partial current-row index")
	}
	plan := explainPlan(t, st, personFactPinAttributeTargetCandidatesSQL, personID,
		ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport, "person_attribute")
	t.Logf("query plan:\n%s", plan)
	assert.Contains(t, plan, "USING INDEX idx_person_attribute_values_current (person_id=?)",
		"pin discovery must be bounded to one person's current attribute rows:\n%s", plan)
	assert.Contains(t, plan, "USING COVERING INDEX idx_person_fact_decisions_projection",
		"projection ownership checks must use the composite decision index:\n%s", plan)
	assert.NotContains(t, plan, "TEMP B-TREE",
		"unordered target discovery must not sort its bounded candidate set:\n%s", plan)
}

func TestListPersonFactPinsEmploymentDiscoveryUsesCurrentAndOwnershipIndexes(t *testing.T) {
	st, personID, _ := newPersonFactProjectionStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite query-plan coverage; PostgreSQL has equivalent person/current and ownership indexes")
	}
	plan := explainPlan(t, st, personFactPinEmploymentTargetCandidateSQL,
		personID, ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport,
		personID, "employment", personFactDecisionSourceRefPrefix)
	t.Logf("query plan:\n%s", plan)
	assert.Contains(t, plan, "idx_employments_person",
		"employment pin discovery must be bounded to one person's rows:\n%s", plan)
	assert.Contains(t, plan, "idx_person_fact_decisions_projection",
		"employment ownership checks must use the composite decision index:\n%s", plan)
}

func TestPersonFactManualSupersedePinsAbsence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	write, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
		Value: AttributeValue{Type: AttributeValueText, Text: new("email")}, Source: ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: targets[AttributeSlugPrimaryChannel].Kind,
			Key: targets[AttributeSlugPrimaryChannel].Key, Revision: targets[AttributeSlugPrimaryChannel].Revision},
		false, "test")
	require.NoError(err)

	_, err = st.SupersedePersonAttributeValueContext(t.Context(), PersonAttributeSupersedeInput{
		PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
		ExpectedValueID: &write.Value.ID,
	})
	require.NoError(err)
	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	require.Len(pins, 1)
	assert.True(pins[0].Pinned)
	assert.NotNil(pins[0].EventID)
}

func TestPersonFactManualSupersedePinsInactiveAttributeAbsence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	seed := personFactProjectionInput(personID, "inactive-clear-seed", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "inactive-clear-seed"),
	}, nil)
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), seed, nil)
	require.NoError(err)

	definition, err := st.GetAttributeDefinitionBySlugContext(
		t.Context(), AttributeObjectPerson, AttributeSlugPrimaryChannel)
	require.NoError(err)
	inactive := false
	definition, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision,
		AttributeDefinitionUpdate{IsActive: &inactive})
	require.NoError(err)
	current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(current, 1)
	_, err = st.SupersedePersonAttributeValueContext(t.Context(), PersonAttributeSupersedeInput{
		PersonID: personID, DefinitionSlug: AttributeSlugPrimaryChannel,
		ExpectedValueID: &current[0].ID,
	})
	require.NoError(err)

	active := true
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision,
		AttributeDefinitionUpdate{IsActive: &active})
	require.NoError(err)
	target = projectionTargetBySlug(t, st, AttributeSlugPrimaryChannel)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(),
		personFactProjectionInput(personID, "inactive-clear-resolve", []personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, `"email"`, "inactive-clear-resolve"),
		}, nil), nil)
	require.NoError(err)
	require.NotEmpty(result.Decisions)
	for _, decision := range result.Decisions {
		assert.Equal(personfacts.ReasonPinRetained, decision.Reason)
	}
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(values)
}

func TestPersonFactExplicitPinSurvivesAutomaticValueChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	pin, err := st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, true, "test")
	require.NoError(err)
	require.NotNil(pin.State.EventID)

	input := personFactProjectionInput(personID, "explicit-pin", []personfacts.ProposedClaim{
		personFactProjectionClaim(personID, target, `"chat"`, "explicit-pin"),
	}, nil)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Decisions, 1)
	assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)
	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	require.Len(pins, 1)
	assert.Equal(pin.State.EventID, pins[0].EventID)
}

func TestPersonFactPinnedAutomaticProjectionSurvivesStatusInvalidationUntilUnpinned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, target, _, evidenceKey := seedProjectedPersonFact(t)
	current, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(current, 1)
	pinnedRowID := current[0].ID
	_, err = st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, true, "test")
	require.NoError(err)
	beforeRevision := personFactProjectionRevision(t, st, personID)

	status := personFactProjectionInput(personID, "pinned-status-off", nil,
		[]personfacts.EvidenceStatusChange{{
			EvidenceKey: evidenceKey, SourceVersion: "source-v1", Supported: false,
			Reason: personfacts.EvidenceStatusSourceDeleted,
		}})
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	current, err = st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	require.Len(current, 1)
	assert.Equal(pinnedRowID, current[0].ID)
	assert.Equal(beforeRevision, personFactProjectionRevision(t, st, personID))

	unpinned, err := st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, false, "test")
	require.NoError(err)
	assert.NotEmpty(unpinned.Projections)
	current, err = st.ListPersonAttributeValuesContext(t.Context(), personID,
		PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(err)
	assert.Empty(current)
	assert.Equal(beforeRevision+1, personFactProjectionRevision(t, st, personID))
}

func TestPersonFactPinReresolutionUsesHostGenerationAndPreservesProviderReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, target, providerResult, _ := seedProjectedPersonFact(t)
	providerJSON, err := json.Marshal(providerResult)
	require.NoError(err)
	providerCounts := personFactGenerationRowCounts(t, st, providerResult.GenerationID)
	ref := personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}

	pinned, err := st.SetPersonFactPinContext(t.Context(), personID, ref, true, "test")
	require.NoError(err)
	require.Len(pinned.Resolutions, 1)
	assertPersonFactPinHostGeneration(t, st, pinned.Resolutions[0].ID,
		providerResult.GenerationID, target, *pinned.State.EventID, true)
	unpinned, err := st.SetPersonFactPinContext(t.Context(), personID, ref, false, "test")
	require.NoError(err)
	require.Len(unpinned.Resolutions, 1)
	assertPersonFactPinHostGeneration(t, st, unpinned.Resolutions[0].ID,
		providerResult.GenerationID, target, *unpinned.State.EventID, false)
	assert.NotEqual(pinned.Resolutions[0].ID, unpinned.Resolutions[0].ID)

	providerInput := personFactProjectionInput(personID, "seed-"+AttributeSlugPrimaryChannel,
		[]personfacts.ProposedClaim{
			personFactProjectionClaim(personID, target, `"chat"`, "seed-"+AttributeSlugPrimaryChannel),
		}, nil)
	replayed, err := st.ApplyPersonFactGenerationContext(t.Context(), providerInput, nil)
	require.NoError(err)
	replayedJSON, err := json.Marshal(replayed)
	require.NoError(err)
	assert.True(bytes.Equal(providerJSON, replayedJSON), "provider replay bytes differ")
	assert.Equal(providerCounts, personFactGenerationRowCounts(t, st, providerResult.GenerationID))
	assert.Equal(int64(3), personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func TestPersonFactExplicitPinSurvivesDescriptorRevisionChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	pin, err := st.SetPersonFactPinContext(t.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, true, "test")
	require.NoError(err)

	_, err = st.db.Exec(`UPDATE attribute_definitions SET description = ? WHERE universal_id = ?`,
		"Updated extraction guidance", target.Key)
	require.NoError(err)
	updated := projectionTargetBySlug(t, st, AttributeSlugPrimaryChannel)
	require.NotEqual(target.Revision, updated.Revision)

	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	require.Len(pins, 1)
	assert.True(pins[0].Pinned)
	assert.Equal(pin.State.EventID, pins[0].EventID)
	assert.Equal(updated.Revision, pins[0].Target.Revision)
}

func TestListPersonFactPinsRetainsEventForDeletedAttributeDefinition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, _ := newPersonFactProjectionStore(t)
	description := "Disposable inference target"
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), AttributeDefinitionInput{
		UniversalID: "test-deleted-pin-target", ObjectType: AttributeObjectPerson,
		Slug: "deleted_pin_target", Label: "Deleted pin target", Description: &description,
		ValueType: AttributeValueText, FieldType: AttributeFieldText,
		Cardinality: AttributeCardinalitySingle, Ownership: AttributeOwnershipUser,
		APIMutable: true, IsDeletable: true,
	})
	require.NoError(err)
	_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: definition.Slug,
		Value:  AttributeValue{Type: AttributeValueText, Text: new("temporary")},
		Source: ProvenanceExtraction,
	})
	require.NoError(err)
	target := projectionTargetBySlug(t, st, definition.Slug)
	_, err = st.SetPersonFactPinContext(t.Context(), personID, personfacts.TargetRef{
		Kind: target.Kind, Key: target.Key, Revision: target.Revision,
	}, false, "test")
	require.NoError(err)
	pin, err := st.SetPersonFactPinContext(t.Context(), personID, personfacts.TargetRef{
		Kind: target.Kind, Key: target.Key, Revision: target.Revision,
	}, true, "test")
	require.NoError(err)

	_, err = st.db.Exec(`DELETE FROM person_attribute_values WHERE definition_id = ?`, definition.ID)
	require.NoError(err)
	require.NoError(st.DeleteAttributeDefinitionContext(
		t.Context(), definition.ID, definition.Revision))

	pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	require.Len(pins, 1)
	assert.Equal(pin.State, pins[0])
}

func TestPersonFactHistoricalDeclaredEmploymentIsEffectivelyPinnedUntilExplicitUnpin(t *testing.T) {
	for _, source := range []Provenance{ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport} {
		t.Run(string(source), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			sourceSlug := strings.ReplaceAll(string(source), "_", "-")
			declaredOrganization := createPersonFactOrganization(t, st, "Declared "+string(source), "declared-"+sourceSlug+".example")
			endDate := PartialDate{Year: new(2024), Month: new(6)}
			declared, err := st.AddEmploymentContext(t.Context(), EmploymentInput{
				PersonID: personID, OrganizationID: declaredOrganization.ID,
				Title: new("Engineer"), EndDate: &endDate, Source: source,
			})
			require.NoError(err)
			assert.False(declared.IsCurrent)
			_, err = st.db.Exec(`DELETE FROM person_fact_pin_events WHERE person_id = ?`, personID)
			require.NoError(err, "simulate declared employment predating the pin ledger")

			pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
			require.NoError(err)
			require.Len(pins, 1)
			assert.Equal(personfacts.TargetEmployment, pins[0].Target.Kind)
			assert.True(pins[0].Pinned)
			assert.Nil(pins[0].EventID)

			automaticOrganization := createPersonFactOrganization(t, st, "Automatic "+string(source), "automatic-"+sourceSlug+".example")
			submitted := fmt.Sprintf(`{"organization":{"id":%d,"name":"Automatic %s","domain":"automatic-%s.example"},"title":"Advisor"}`,
				automaticOrganization.ID, source, sourceSlug)
			input := personFactProjectionInput(personID, "declared-pin-"+string(source), []personfacts.ProposedClaim{
				personFactProjectionClaim(personID, target, submitted, "declared-pin"),
			}, nil)
			retained, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(retained.Decisions, 1)
			assert.Equal(personfacts.ReasonPinRetained, retained.Decisions[0].Reason)
			assert.Empty(retained.Projections)

			unpinned, err := st.SetPersonFactPinContext(t.Context(), personID,
				personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
				false, "test")
			require.NoError(err)
			assert.False(unpinned.State.Pinned)
			assert.NotEmpty(unpinned.Projections)
			pins, err = st.ListPersonFactPinsContext(t.Context(), personID)
			require.NoError(err)
			require.Len(pins, 1)
			assert.False(pins[0].Pinned)
			assert.NotNil(pins[0].EventID)
		})
	}
}

func TestPersonFactUnownedDerivedEmploymentIsImplicitlyPinned(t *testing.T) {
	for _, source := range []Provenance{
		ProvenanceExtraction,
		ProvenanceEnrichment,
		ProvenanceSystem,
	} {
		for _, operation := range []struct {
			name     string
			relation personfacts.ClaimRelation
			role     string
		}{
			{name: "replacement", relation: personfacts.RelationSupport, role: "Infrastructure"},
			{name: "end", relation: personfacts.RelationSupersede, role: "Platform"},
		} {
			t.Run(string(source)+"/"+operation.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, personID, _ := newPersonFactProjectionStore(t)
				target := projectionTargetBySlug(t, st, "employment")
				organization := createPersonFactOrganization(
					t, st, "Unowned Employment", "unowned-employment.example")
				start := PartialDate{Year: new(2020)}
				unowned, err := st.AddEmploymentContext(t.Context(), EmploymentInput{
					PersonID: personID, OrganizationID: organization.ID,
					Title: new("Engineer"), Role: new("Platform"), StartDate: &start,
					Source: source,
				})
				require.NoError(err)

				pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
				require.NoError(err)
				assert.Len(pins, 1)
				if len(pins) == 1 {
					assert.Equal(personfacts.TargetEmployment, pins[0].Target.Kind)
					assert.True(pins[0].Pinned)
					assert.Nil(pins[0].EventID)
				}

				submitted := fmt.Sprintf(
					`{"organization":{"id":%d,"name":"Unowned Employment","domain":"unowned-employment.example"},"title":"Engineer","role":%q,"start_date":{"year":2020}}`,
					organization.ID, operation.role)
				claim := personFactProjectionClaim(
					personID, target, submitted, "unowned-employment-"+operation.name)
				claim.Relation = operation.relation
				result, err := st.ApplyPersonFactGenerationContext(t.Context(),
					personFactProjectionInput(personID,
						"unowned-employment-"+string(source)+"-"+operation.name,
						[]personfacts.ProposedClaim{claim}, nil), nil)
				require.NoError(err)
				require.Len(result.Decisions, 1)
				assert.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
				assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)
				assert.Empty(result.Projections)

				current, err := st.GetEmploymentContext(t.Context(), unowned.ID)
				require.NoError(err)
				assert.True(current.IsCurrent)
				require.NotNil(current.Role)
				assert.Equal("Platform", *current.Role)
				assert.Equal(int64(1), current.Revision)
			})
		}
	}
}

func TestPersonFactExplicitUnpinOverridesUnownedDerivedEmploymentProtection(t *testing.T) {
	for _, operation := range []struct {
		name     string
		relation personfacts.ClaimRelation
		role     string
	}{
		{name: "replacement", relation: personfacts.RelationSupport, role: "Infrastructure"},
		{name: "end", relation: personfacts.RelationSupersede, role: "Platform"},
	} {
		t.Run(operation.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(
				t, st, "Unpinned Employment", "unpinned-employment.example")
			start := PartialDate{Year: new(2020)}
			unowned, err := st.AddEmploymentContext(t.Context(), EmploymentInput{
				PersonID: personID, OrganizationID: organization.ID,
				Title: new("Engineer"), Role: new("Platform"), StartDate: &start,
				Source: ProvenanceExtraction,
			})
			require.NoError(err)

			unpinned, err := st.SetPersonFactPinContext(t.Context(), personID,
				personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
				false, "test")
			require.NoError(err)
			assert.False(unpinned.State.Pinned)
			assert.NotNil(unpinned.State.EventID)
			assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_fact_pin_events"))

			submitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Unpinned Employment","domain":"unpinned-employment.example"},"title":"Engineer","role":%q,"start_date":{"year":2020}}`,
				organization.ID, operation.role)
			claim := personFactProjectionClaim(
				personID, target, submitted, "unpinned-employment-"+operation.name)
			claim.Relation = operation.relation
			result, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "unpinned-employment-"+operation.name,
					[]personfacts.ProposedClaim{claim}, nil), nil)
			require.NoError(err)
			require.Len(result.Decisions, 1)
			assert.Equal(personfacts.DecisionApplied, result.Decisions[0].Action)
			assert.NotEmpty(result.Projections)

			current, err := st.GetEmploymentContext(t.Context(), unowned.ID)
			require.NoError(err)
			assert.Equal(int64(2), current.Revision)
			if operation.relation == personfacts.RelationSupport {
				assert.True(current.IsCurrent)
				require.NotNil(current.Role)
				assert.Equal("Infrastructure", *current.Role)
			} else {
				assert.False(current.IsCurrent)
				require.NotNil(current.EndDate)
			}
		})
	}
}

func TestPersonFactOwnedEmploymentRemainsAutomaticallyMutable(t *testing.T) {
	for _, operation := range []struct {
		name     string
		relation personfacts.ClaimRelation
		role     string
	}{
		{name: "replacement", relation: personfacts.RelationSupport, role: "Infrastructure"},
		{name: "end", relation: personfacts.RelationSupersede, role: "Platform"},
	} {
		t.Run(operation.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(
				t, st, "Owned Employment", "owned-employment.example")
			platform := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Owned Employment","domain":"owned-employment.example"},"title":"Engineer","role":"Platform","start_date":{"year":2020}}`,
				organization.ID)
			seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "owned-employment-seed",
					[]personfacts.ProposedClaim{
						personFactProjectionClaim(personID, target, platform, "owned-employment-seed"),
					}, nil), nil)
			require.NoError(err)
			require.Len(seed.Projections, 1)
			rowID := seed.Projections[0].RowID

			pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
			require.NoError(err)
			assert.Empty(pins)

			submitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Owned Employment","domain":"owned-employment.example"},"title":"Engineer","role":%q,"start_date":{"year":2020}}`,
				organization.ID, operation.role)
			claim := personFactProjectionClaim(
				personID, target, submitted, "owned-employment-"+operation.name)
			claim.Relation = operation.relation
			input := personFactProjectionInput(personID, "owned-employment-"+operation.name,
				[]personfacts.ProposedClaim{claim}, nil)
			input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Decisions, 1)
			assert.Equal(personfacts.DecisionApplied, result.Decisions[0].Action)
			assert.NotEmpty(result.Projections)

			current, err := st.GetEmploymentContext(t.Context(), rowID)
			require.NoError(err)
			assert.Equal(int64(2), current.Revision)
			if operation.relation == personfacts.RelationSupport {
				assert.True(current.IsCurrent)
				require.NotNil(current.Role)
				assert.Equal("Infrastructure", *current.Role)
			} else {
				assert.False(current.IsCurrent)
				require.NotNil(current.EndDate)
			}
		})
	}
}

func TestPersonFactEmploymentUpdatesPinProjectedRows(t *testing.T) {
	for _, operation := range []struct {
		name, updatedRole  string
		relation           personfacts.ClaimRelation
		role               string
		preserveProvenance bool
	}{
		{name: "replacement", updatedRole: "Externally Enriched", relation: personfacts.RelationSupport, role: "Automatic Replacement"},
		{name: "end", updatedRole: "Externally Enriched", relation: personfacts.RelationSupersede, role: "Externally Enriched"},
		{name: "preserved provenance", updatedRole: "Manual Role", relation: personfacts.RelationSupport,
			role: "Automatic Replacement", preserveProvenance: true},
	} {
		t.Run(operation.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, personID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(
				t, st, "Externally Updated Employment", "external-update.example")
			seedValue := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Externally Updated Employment","domain":"external-update.example"},"title":"Engineer","role":"Platform","start_date":{"year":2020}}`,
				organization.ID)
			seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(personID, "external-update-seed-"+operation.name,
					[]personfacts.ProposedClaim{
						personFactProjectionClaim(personID, target, seedValue,
							"external-update-seed-"+operation.name),
					}, nil), nil)
			require.NoError(err)
			require.Len(seed.Projections, 1)
			projected, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
			require.NoError(err)

			externalSourceRef := "external-enrichment:" + operation.name
			externalConfidence := 0.95
			source := ProvenanceEnrichment
			sourceRef := &externalSourceRef
			confidence := &externalConfidence
			if operation.preserveProvenance {
				source = projected.Source
				sourceRef = projected.SourceRef
				confidence = projected.Confidence
			}
			externallyUpdated, err := st.UpdateEmploymentContext(
				t.Context(), projected.ID, projected.Revision, EmploymentInput{
					PersonID: personID, OrganizationID: organization.ID,
					Title: projected.Title, Role: new(operation.updatedRole),
					StartDate: projected.StartDate, Source: source,
					SourceRef: sourceRef, Confidence: confidence,
				})
			require.NoError(err)
			assert.Equal(projected.ID, externallyUpdated.ID)
			assert.Equal(projected.Revision+1, externallyUpdated.Revision)

			pins, err := st.ListPersonFactPinsContext(t.Context(), personID)
			require.NoError(err)
			assert.Len(pins, 1)
			if len(pins) == 1 {
				assert.Equal(personfacts.TargetEmployment, pins[0].Target.Kind)
				assert.True(pins[0].Pinned)
				assert.NotNil(pins[0].EventID)
			}

			claimValue := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Externally Updated Employment","domain":"external-update.example"},"title":"Engineer","role":%q,"start_date":{"year":2020}}`,
				organization.ID, operation.role)
			claim := personFactProjectionClaim(
				personID, target, claimValue, "external-update-claim-"+operation.name)
			claim.Relation = operation.relation
			input := personFactProjectionInput(personID, "external-update-generation-"+operation.name,
				[]personfacts.ProposedClaim{claim}, nil)
			input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
			result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			require.Len(result.Decisions, 1)
			assert.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
			assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)
			assert.Empty(result.Projections)

			retained, err := st.GetEmploymentContext(t.Context(), projected.ID)
			require.NoError(err)
			assert.Equal(externallyUpdated.Revision, retained.Revision)
			assert.True(retained.IsCurrent)
			require.NotNil(retained.Role)
			assert.Equal(operation.updatedRole, *retained.Role)
		})
	}
}

func TestPersonFactDerivedEmploymentDetachmentDurablyPinsAffectedPeople(t *testing.T) {
	for _, operation := range []struct {
		name     string
		reassign bool
	}{
		{name: "end"},
		{name: "reassign", reassign: true},
	} {
		t.Run(operation.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, originalPersonID, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			organization := createPersonFactOrganization(
				t, st, "Detached Employment", "detached-employment.example")
			submitted := fmt.Sprintf(
				`{"organization":{"id":%d,"name":"Detached Employment","domain":"detached-employment.example"},"title":"Engineer","role":"Platform","start_date":{"year":2020}}`,
				organization.ID)
			seed, err := st.ApplyPersonFactGenerationContext(t.Context(),
				personFactProjectionInput(originalPersonID, "detachment-seed-"+operation.name,
					[]personfacts.ProposedClaim{
						personFactProjectionClaim(originalPersonID, target, submitted,
							"detachment-seed-"+operation.name),
					}, nil), nil)
			require.NoError(err)
			require.Len(seed.Projections, 1)
			projected, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
			require.NoError(err)

			destinationPersonID := originalPersonID
			affectedPersonIDs := []int64{originalPersonID}
			var endDate *PartialDate
			if operation.reassign {
				participantID, ensureErr := st.EnsureParticipant(
					"detached-destination@example.com", "Detached Destination", "example.com")
				require.NoError(ensureErr)
				destination, _, createErr := st.CreatePersonFromParticipant(participantID)
				require.NoError(createErr)
				_, trackingErr := st.SetPersonTrackingContext(t.Context(), destination.ID, true)
				require.NoError(trackingErr)
				destinationPersonID = destination.ID
				affectedPersonIDs = append(affectedPersonIDs, destinationPersonID)
			} else {
				endDate = &PartialDate{Year: new(2025), Month: new(6)}
			}

			externalSourceRef := "external-enrichment:" + operation.name
			externalConfidence := 0.95
			updated, err := st.UpdateEmploymentContext(
				t.Context(), projected.ID, projected.Revision, EmploymentInput{
					PersonID: destinationPersonID, OrganizationID: organization.ID,
					Title: projected.Title, Role: projected.Role, StartDate: projected.StartDate,
					EndDate: endDate, Source: ProvenanceEnrichment,
					SourceRef: &externalSourceRef, Confidence: &externalConfidence,
				})
			require.NoError(err)
			assert.Equal(projected.ID, updated.ID)
			assert.Equal(projected.Revision+1, updated.Revision)

			for index, affectedPersonID := range affectedPersonIDs {
				suffix := fmt.Sprintf("detachment-replay-%s-%d", operation.name, index)
				input := personFactProjectionInput(affectedPersonID, suffix,
					[]personfacts.ProposedClaim{
						personFactProjectionClaim(affectedPersonID, target, submitted, suffix),
					}, nil)
				input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
				result, applyErr := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
				require.NoError(applyErr)
				require.Len(result.Decisions, 1)
				assert.Equal(personfacts.DecisionRetained, result.Decisions[0].Action)
				assert.Equal(personfacts.ReasonPinRetained, result.Decisions[0].Reason)
				assert.Empty(result.Projections)

				pins, listErr := st.ListPersonFactPinsContext(t.Context(), affectedPersonID)
				require.NoError(listErr)
				if assert.Len(pins, 1) {
					assert.Equal(personfacts.TargetEmployment, pins[0].Target.Kind)
					assert.True(pins[0].Pinned)
					assert.NotNil(pins[0].EventID, "detachment protection must be durable")
				}
			}

			originalEmployments, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{
				PersonID: originalPersonID,
			})
			require.NoError(err)
			if operation.reassign {
				assert.Empty(originalEmployments)
				destinationEmployments, listErr := st.ListEmploymentsContext(
					t.Context(), EmploymentFilter{PersonID: destinationPersonID})
				require.NoError(listErr)
				require.Len(destinationEmployments, 1)
				assert.Equal(projected.ID, destinationEmployments[0].ID)
				assert.Equal(updated.Revision, destinationEmployments[0].Revision)
			} else {
				require.Len(originalEmployments, 1)
				assert.Equal(projected.ID, originalEmployments[0].ID)
				assert.Equal(updated.Revision, originalEmployments[0].Revision)
				assert.False(originalEmployments[0].IsCurrent)
			}
		})
	}
}

func TestSetPersonFactPinRejectsBlankActorAndSkipsDuplicateEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	ref := personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
	_, err := st.SetPersonFactPinContext(t.Context(), personID, ref, true, " ")
	require.Error(err)
	first, err := st.SetPersonFactPinContext(t.Context(), personID, ref, true, "test")
	require.NoError(err)
	second, err := st.SetPersonFactPinContext(t.Context(), personID, ref, true, "test-again")
	require.NoError(err)
	assert.Equal(first.State.EventID, second.State.EventID)
	assert.Equal(int64(1), personFactProjectionRowCount(t, st, "person_fact_pin_events"))
}

func TestSetPersonFactPinRejectsUntrackedPersonWithoutWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, personID, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugPrimaryChannel]
	ref := personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
	pinned, err := st.SetPersonFactPinContext(t.Context(), personID, ref, true, "test")
	requirements.NoError(err)
	requirements.True(pinned.State.Pinned)
	beforeEvents := personFactProjectionRowCount(t, st, "person_fact_pin_events")
	beforeGenerations := personFactProjectionRowCount(t, st, "person_fact_generations")

	_, err = st.SetPersonTrackingContext(t.Context(), personID, false)
	requirements.NoError(err)
	_, err = st.SetPersonFactPinContext(t.Context(), personID, ref, false, "test")
	requirements.ErrorIs(err, ErrPersonFactPersonNotTracked)
	assertions.Equal(beforeEvents, personFactProjectionRowCount(t, st, "person_fact_pin_events"))
	assertions.Equal(beforeGenerations, personFactProjectionRowCount(t, st, "person_fact_generations"))
}

func assertPersonFactPinHostGeneration(
	t *testing.T, st *Store, resolutionID, providerGenerationID int64,
	target personfacts.TargetDescriptor, eventID int64, pinned bool,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	var generationID int64
	var programID, provider, providerVersion, catalogFingerprint, policyFingerprint, cursorsJSON string
	require.NoError(st.db.QueryRow(`
		SELECT g.id, g.program_id, g.provider, g.provider_version, g.catalog_fingerprint,
		       g.provider_policy_fingerprint, g.source_cursors_json
		FROM person_fact_resolutions r
		JOIN person_fact_generations g ON g.id = r.generation_id
		WHERE r.id = ?`, resolutionID).Scan(
		&generationID, &programID, &provider, &providerVersion, &catalogFingerprint,
		&policyFingerprint, &cursorsJSON))
	assert.NotEqual(providerGenerationID, generationID)
	assert.Equal("msgvault-person-fact-pin", programID)
	assert.Equal("msgvault", provider)
	assert.Equal("person-fact-pin-v1", providerVersion)
	wantCatalog, err := st.BuildPersonFactCatalogContext(t.Context(), target.Sensitive)
	require.NoError(err)
	assert.Equal(wantCatalog.Fingerprint, catalogFingerprint)
	assert.Equal("policy-v1", policyFingerprint)
	var cursors []personfacts.SourceCursor
	require.NoError(json.Unmarshal([]byte(cursorsJSON), &cursors))
	require.Len(cursors, 1)
	assert.Equal("person-fact-pin:"+string(target.Kind)+":"+target.Key, cursors[0].Lane)
	assert.Equal(fmt.Sprintf("event:%d", eventID), cursors[0].Start)
	assert.Equal(fmt.Sprintf("pinned:%t@%s", pinned, target.Revision), cursors[0].End)
}
