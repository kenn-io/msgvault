package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func inferenceRevision(t *testing.T, st *Store, id int64) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, st.db.QueryRow(`SELECT COALESCE((SELECT inference_revision FROM person_carddav_inference_state WHERE person_id = ?), 0)`, id).Scan(&revision))
	return revision
}

func inferencePerson(t *testing.T, st *Store, email string) *Person {
	t.Helper()
	participant, err := st.EnsureParticipant(email, "Example", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return person
}

func inferenceNote(t *testing.T, st *Store, id int64, source Provenance, text string) {
	t.Helper()
	_, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: id, DefinitionSlug: AttributeSlugNotes, Value: AttributeValue{Type: AttributeValueText, Text: &text}, Source: source})
	require.NoError(t, err)
}

func TestInferenceExportEmploymentMutations(t *testing.T) {
	for _, scenario := range []string{"revise", "move", "end inferred", "nonrendering", "declared replace", "manual end", "manual delete", "primary", "primary no-op", "primary hides", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, _ := newPersonFactProjectionStore(t)
			org, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org", Kind: OrganizationKindCompany})
			require.NoError(err)
			input := EmploymentInput{PersonID: id, OrganizationID: org.ID, Title: new("Engineer"), Source: ProvenanceExtraction}
			if scenario == "primary" {
				input.IsPrimary = new(false)
			}
			emp, err := st.AddEmploymentContext(t.Context(), input)
			require.NoError(err)
			if scenario == "end inferred" || scenario == "primary hides" {
				secondary, addErr := st.AddEmploymentContext(t.Context(), EmploymentInput{
					PersonID: id, OrganizationID: org.ID, Title: new("Advisor"),
					Source: ProvenanceUser, IsPrimary: new(false),
				})
				require.NoError(addErr)
				if scenario == "primary hides" {
					emp = secondary
				}
			}
			before := inferenceRevision(t, st, id)
			want := before
			switch scenario {
			case "revise":
				input.Title = new("Director")
				want++
			case "move":
				input.PersonID = inferencePerson(t, st, "move@example.com").ID
				want++
			case "end inferred":
				input.IsCurrent = new(false)
				want++
			case "nonrendering":
				input.Description = new("Internal history")
				input.Confidence = new(0.8)
			case "declared replace":
				input.Source = ProvenanceUser
				input.Title = new("Director")
			case "manual end":
				_, err = st.EndEmploymentContext(t.Context(), emp.ID, emp.Revision, PartialDate{Year: new(2026)})
			case "manual delete":
				err = st.DeleteEmploymentContext(t.Context(), emp.ID, emp.Revision)
			case "primary", "primary no-op", "primary hides":
				_, err = st.SetPrimaryEmploymentContext(t.Context(), emp.ID, emp.Revision)
				if scenario != "primary no-op" {
					want++
				}
			case "rollback":
				input.OrganizationID = -1
			}
			if scenario != "manual end" && scenario != "manual delete" && scenario != "primary" && scenario != "primary no-op" && scenario != "primary hides" {
				_, err = st.UpdateEmploymentContext(t.Context(), emp.ID, emp.Revision, input)
			}
			if scenario == "rollback" {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			assert.Equal(want, inferenceRevision(t, st, id))
			if scenario == "move" {
				assert.Equal(int64(1), inferenceRevision(t, st, input.PersonID))
			}
			if scenario == "end inferred" {
				emps, err := st.ListEmploymentsContext(t.Context(), EmploymentFilter{PersonID: id, CurrentOnly: true})
				require.NoError(err)
				require.Len(emps, 1)
				assert.False(emps[0].IsPrimary, "retirement must not invent a fallback primary")
			}
		})
	}
}

func TestInferenceExportDefinitionExposureRetriesConcurrentWrite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite snapshot contention")
	}
	inferenceNote(t, st, id, ProvenanceExtraction, "Inferred note")
	definition, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugNotes)
	require.NoError(err)
	before := inferenceRevision(t, st, id)
	writer, err := Open(st.dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(writer.Close()) })

	// Commit on another connection after the update's projection reads, before
	// its first write. This exercises a real stale SQLite snapshot.
	rebind := st.db.rebind
	t.Cleanup(func() { st.db.rebind = rebind })
	interleaved := false
	st.db.rebind = func(query string) string {
		if !interleaved && strings.Contains(query, "UPDATE attribute_definitions") {
			interleaved = true
			_, writeErr := writer.EnsureParticipant("concurrent@example.com", "Concurrent Example", "example.com")
			require.NoError(writeErr)
		}
		return rebind(query)
	}
	updated, err := st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision,
		AttributeDefinitionUpdate{IsActive: new(false)})
	require.True(interleaved)
	require.NoError(err)
	assert.False(updated.IsActive)
	assert.Equal(before+1, inferenceRevision(t, st, id))
}

func TestInferenceExportDefinitionExposure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	inferenceNote(t, st, id, ProvenanceExtraction, "Inferred")
	other := inferencePerson(t, st, "declared@example.com")
	inferenceNote(t, st, other.ID, ProvenanceUser, "Declared")
	def, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugNotes)
	require.NoError(err)
	rev := inferenceRevision(t, st, id)
	for _, active := range []bool{false, false, true} {
		old := def.IsActive
		def, err = st.UpdateAttributeDefinitionContext(t.Context(), def.ID, def.Revision, AttributeDefinitionUpdate{IsActive: &active})
		require.NoError(err)
		if active != old {
			rev++
		}
		assert.Equal(rev, inferenceRevision(t, st, id))
		assert.Zero(inferenceRevision(t, st, other.ID))
	}
	_, err = st.UpdateAttributeDefinitionContext(t.Context(), def.ID, def.Revision, AttributeDefinitionUpdate{Label: new("Renamed notes"), DisplayOrder: new(int64(999))})
	require.NoError(err)
	assert.Equal(rev, inferenceRevision(t, st, id))
}

func TestInferenceExportSeedMappingExposure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	inferenceNote(t, st, id, ProvenanceExtraction, "Inferred")
	def, err := st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugNotes)
	require.NoError(err)
	var seed AttributeDefinitionInput
	for _, item := range SeededAttributeDefinitions() {
		if item.Slug == AttributeSlugNotes {
			seed = item
		}
	}
	before := inferenceRevision(t, st, id)
	seed.VCardProperty = nil
	require.NoError(st.reconcileSeededDefinition(t.Context(), def, seed))
	assert.Equal(before+1, inferenceRevision(t, st, id))
	def, err = st.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugNotes)
	require.NoError(err)
	seed.VCardProperty = new("NOTE")
	require.NoError(st.reconcileSeededDefinition(t.Context(), def, seed))
	assert.Equal(before+2, inferenceRevision(t, st, id))
}

func TestInferenceExportOrganizationMutations(t *testing.T) {
	for _, scenario := range []string{"rename", "metadata", "merge", "same-name merge", "nonprimary", "declared"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			st, id, _ := newPersonFactProjectionStore(t)
			org, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org", Kind: OrganizationKindCompany})
			require.NoError(err)
			input := EmploymentInput{PersonID: id, OrganizationID: org.ID, Source: ProvenanceExtraction}
			if scenario == "nonprimary" {
				input.IsPrimary = new(false)
			}
			if scenario == "declared" {
				input.Source = ProvenanceUser
			}
			_, err = st.AddEmploymentContext(t.Context(), input)
			require.NoError(err)
			before := inferenceRevision(t, st, id)
			want := before
			if scenario == "merge" || scenario == "same-name merge" {
				name := "Other Org"
				if scenario == "same-name merge" {
					name = org.Name
				}
				target, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: name, Kind: org.Kind})
				require.NoError(err)
				_, err = st.MergeOrganizationsContext(t.Context(), target.ID, target.Revision, org.ID, org.Revision)
				require.NoError(err)
				if scenario == "merge" {
					want++
				}
			} else {
				name := "New Org"
				if scenario == "metadata" {
					name = org.Name
				}
				_, err = st.ReplaceOrganizationContext(t.Context(), org.ID, org.Revision, OrganizationInput{Name: name, Kind: org.Kind, Description: new("Metadata")}, false)
				require.NoError(err)
				if scenario == "rename" {
					want++
				}
			}
			assert.Equal(t, want, inferenceRevision(t, st, id))
		})
	}
}

func TestInferenceExportResolverMultipleTargetsAdvanceOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, targets := newPersonFactProjectionStore(t)
	org := createPersonFactOrganization(t, st, "Example Org", "employer.example")
	claims := []personfacts.ProposedClaim{
		personFactProjectionClaim(id, targets[AttributeSlugNotes], `"Inferred note"`, "note"),
		personFactProjectionClaim(id, projectionTargetBySlug(t, st, "employment"), fmt.Sprintf(`{"organization":{"id":%d,"name":"Example Org","domain":"employer.example"},"title":"Engineer"}`, org.ID), "employment"),
	}
	input := inferenceGenerationInput(id, "multi-target", claims, nil)
	result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	require.Len(result.Projections, 2)
	assert.Equal(int64(1), inferenceRevision(t, st, id))
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	assert.Equal(int64(1), inferenceRevision(t, st, id), "replaying the generation must not invalidate approval again")
}

func TestInferenceExportResolverStatusAndUnpin(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(map[bool]string{false: "status", true: "unpin"}[pinned], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, targets := newPersonFactProjectionStore(t)
			target := targets[AttributeSlugNotes]
			claim := personFactProjectionClaim(id, target, `"Inferred"`, "note")
			_, err := st.ApplyPersonFactGenerationContext(t.Context(), inferenceGenerationInput(id, "first", []personfacts.ProposedClaim{claim}, nil), nil)
			require.NoError(err)
			assert.Equal(int64(1), inferenceRevision(t, st, id), "one resolver transaction must bump once")
			ref := personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
			if pinned {
				_, err = st.SetPersonFactPinContext(t.Context(), id, ref, true, "test")
				require.NoError(err)
				assert.Equal(int64(1), inferenceRevision(t, st, id))
			}
			evidence, err := st.ListPersonFactEvidenceContext(t.Context(), id, personfacts.EvidenceFilter{})
			require.NoError(err)
			require.Len(evidence, 1)
			_, err = st.ApplyPersonFactGenerationContext(t.Context(), inferenceGenerationInput(id, "retire", nil, []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: "source-v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}}), nil)
			require.NoError(err)
			if pinned {
				assert.Equal(int64(1), inferenceRevision(t, st, id))
				_, err = st.SetPersonFactPinContext(t.Context(), id, ref, false, "test")
				require.NoError(err)
			}
			assert.Equal(int64(2), inferenceRevision(t, st, id))
			values, err := st.ListPersonAttributeValuesContext(t.Context(), id, PersonAttributeQuery{DefinitionSlug: AttributeSlugNotes})
			require.NoError(err)
			assert.Empty(values)
		})
	}
}

func TestInferenceExportMergeAndSplit(t *testing.T) {
	for _, kind := range []string{"attribute", "employment", "declared"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, _ := newPersonFactProjectionStore(t)
			absorbed := inferencePerson(t, st, "absorbed@example.com")
			source := ProvenanceExtraction
			if kind == "declared" {
				source = ProvenanceUser
			}
			if kind == "employment" {
				org, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org", Kind: OrganizationKindCompany})
				require.NoError(err)
				_, err = st.AddEmploymentContext(t.Context(), EmploymentInput{PersonID: absorbed.ID, OrganizationID: org.ID, Source: source})
				require.NoError(err)
			} else {
				inferenceNote(t, st, absorbed.ID, source, "Transferred")
			}
			_, err := st.db.Exec(`UPDATE person_carddav_inference_state SET approved_revision=inference_revision WHERE person_id=?`, absorbed.ID)
			require.NoError(err)
			survivor, err := st.GetPersonContext(t.Context(), id)
			require.NoError(err)
			absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
			require.NoError(err)
			merged, err := st.MergePersonsContext(t.Context(), PersonMergeRequest{SurvivorID: id, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "merge-inference", Actor: "test"})
			require.NoError(err)
			want := int64(1)
			if kind == "declared" {
				want = 0
			}
			assert.Equal(want, inferenceRevision(t, st, id))
			split, err := st.SplitPersonMergeContext(t.Context(), PersonSplitRequest{SourcePersonID: id, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "split-inference", Actor: "test"})
			require.NoError(err)
			assert.Equal(want*2, inferenceRevision(t, st, id))
			assert.Equal(want, inferenceRevision(t, st, split.NewPerson.ID))
			var approved int64
			require.NoError(st.db.QueryRow(`SELECT COALESCE((SELECT approved_revision FROM person_carddav_inference_state WHERE person_id=?),0)`, split.NewPerson.ID).Scan(&approved))
			assert.Zero(approved)
		})
	}
}

func inferenceGenerationInput(id int64, suffix string, claims []personfacts.ProposedClaim, statuses []personfacts.EvidenceStatusChange) personfacts.GenerationInput {
	input := personFactProjectionInput(id, suffix, claims, statuses)
	input.Policy.AllowSensitive = true
	return input
}

func TestInferenceExportCanonicalAttributeInputs(t *testing.T) {
	for _, scenario := range []string{"ordinal", "reserved", "json", "archive observation", "system resolver", "brief resolver", "takeover"} {
		t.Run(scenario, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, targets := newPersonFactProjectionStore(t)
			switch scenario {
			case "system resolver", "brief resolver":
				claim := personFactProjectionClaim(id, targets[AttributeSlugNotes], `"Text"`, scenario)
				claim.Origin = personfacts.OriginSystem
				if scenario == "brief resolver" {
					claim.Origin = personfacts.OriginBrief
				}
				result, err := st.ApplyPersonFactGenerationContext(t.Context(), inferenceGenerationInput(id, scenario, []personfacts.ProposedClaim{claim}, nil), nil)
				require.NoError(err)
				require.NotEmpty(result.Projections)
				want := int64(0)
				if scenario == "brief resolver" {
					want = 1
				}
				assert.Equal(want, inferenceRevision(t, st, id))
			case "takeover":
				inferenceNote(t, st, id, ProvenanceUser, "Same")
				inferenceNote(t, st, id, ProvenanceExtraction, "Same")
				assert.Equal(int64(1), inferenceRevision(t, st, id))
			case "archive observation":
				inferenceNote(t, st, id, ProvenanceArchiveObservation, "Observed")
				assert.Zero(inferenceRevision(t, st, id))
			default:
				defInput := AttributeDefinitionInput{UniversalID: "inference-scalar", ObjectType: AttributeObjectPerson, Slug: "inference_scalar", Label: "Scalar", ValueType: AttributeValueText, FieldType: AttributeFieldText, Cardinality: AttributeCardinalityMulti, Ownership: AttributeOwnershipUser, APIMutable: true, UICreatable: true, UIEditable: true, VCardProperty: new("X-SCALAR")}
				value := AttributeValue{Type: AttributeValueText, Text: new("Value")}

				if scenario == "json" {
					defInput.ValueType = AttributeValueJSON
					defInput.FieldType = AttributeFieldJSON
					value = AttributeValue{Type: AttributeValueJSON, JSON: []byte(`{"key":"value"}`)}
				}
				def, err := st.CreateAttributeDefinitionContext(t.Context(), defInput)
				require.NoError(err)
				if scenario == "reserved" {
					_, err = st.db.Exec(`UPDATE attribute_definitions SET vcard_property='UID' WHERE id=?`, def.ID)
					require.NoError(err)
				}
				written, err := st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{PersonID: id, DefinitionSlug: def.Slug, Value: value, Source: ProvenanceExtraction})
				require.NoError(err)
				if scenario != "ordinal" {
					assert.Zero(inferenceRevision(t, st, id))
					return
				}
				require.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
					before, err := st.loadPersonInferenceExportProjectionTx(t.Context(), tx, id)
					if err != nil {
						return err
					}
					_, err = tx.ExecContext(t.Context(), `UPDATE person_attribute_values SET ordinal=99 WHERE id=?`, written.Value.ID)
					if err != nil {
						return err
					}
					after, err := st.loadPersonInferenceExportProjectionTx(t.Context(), tx, id)
					if err != nil {
						return err
					}
					changed, err := inferenceExportProjectionChanged(before, after)
					require.NoError(err)
					assert.False(changed, "ordinals identify storage slots, not portable property multiplicity")
					return nil
				}))
				_, err = st.SetPersonAttributeValueContext(t.Context(), PersonAttributeValueInput{
					PersonID: id, DefinitionSlug: def.Slug, Ordinal: new(int64(1)), Value: value, Source: ProvenanceExtraction,
				})
				require.NoError(err)
				assert.Equal(int64(2), inferenceRevision(t, st, id), "adding a duplicate portable occurrence changes multiplicity")
			}
		})
	}
}

func TestInferenceExportMergeCandidateAcceptance(t *testing.T) {
	for _, source := range []Provenance{ProvenanceExtraction, ProvenanceVCardImport} {
		t.Run(string(source), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, _ := newPersonFactProjectionStore(t)
			absorbed := inferencePerson(t, st, "candidate@example.com")
			inferenceNote(t, st, id, ProvenanceUser, "Original")
			inferenceNote(t, st, absorbed.ID, source, "Candidate")
			survivor, err := st.GetPersonContext(t.Context(), id)
			require.NoError(err)
			absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
			require.NoError(err)
			merged, err := st.MergePersonsContext(t.Context(), PersonMergeRequest{SurvivorID: id, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "candidate-merge", Actor: "test"})
			require.NoError(err)
			assert.Zero(inferenceRevision(t, st, id), "hidden candidate is not rendered")
			var candidateID int64
			require.NoError(st.db.QueryRow(`SELECT id FROM person_merge_review_candidates WHERE merge_id=? AND state='pending'`, merged.Merge.ID).Scan(&candidateID))
			request := PersonMergeCandidateDecisionRequest{PersonID: id, CandidateID: candidateID, ExpectedPersonRevision: merged.Person.Revision, Decision: PersonMergeCandidateAccept, Actor: "test"}
			_, err = st.DecidePersonMergeCandidateContext(t.Context(), request)
			require.NoError(err)
			want := int64(0)
			if source == ProvenanceExtraction {
				want = 1
			}
			assert.Equal(want, inferenceRevision(t, st, id))
			_, err = st.DecidePersonMergeCandidateContext(t.Context(), request)
			require.NoError(err)
			assert.Equal(want, inferenceRevision(t, st, id), "idempotent acceptance")
		})
	}
}

func TestInferenceExportResolverRollback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, targets := newPersonFactProjectionStore(t)
	input := inferenceGenerationInput(id, "rollback", []personfacts.ProposedClaim{personFactProjectionClaim(id, targets[AttributeSlugNotes], `"Rolled back"`, "rollback")}, nil)
	prepared, err := personfacts.PreparePersonFactGeneration(t.Context(), input, nil)
	require.NoError(err)
	stopped := errors.New("stop after projection")
	err = st.withTxContext(t.Context(), func(tx *loggedTx) error {
		_, _, err := st.applyPreparedPersonFactGenerationDetailedTx(t.Context(), tx, prepared, func(stage string) error {
			if stage == "projection" {
				state, err := st.getCardDAVInferenceExportStateTx(t.Context(), tx, id)
				require.NoError(err)
				assert.Equal(int64(1), state.InferenceRevision)
				return stopped
			}
			return nil
		})
		return err
	})
	require.ErrorIs(err, stopped)
	assert.Zero(inferenceRevision(t, st, id))
	values, err := st.ListPersonAttributeValuesContext(t.Context(), id, PersonAttributeQuery{DefinitionSlug: AttributeSlugNotes})
	require.NoError(err)
	assert.Empty(values)
}

func TestInferenceExportEmploymentRetirementAndHistoricalDeletion(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(map[bool]string{false: "current retirement", true: "historical deletion"}[historical], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, id, _ := newPersonFactProjectionStore(t)
			target := projectionTargetBySlug(t, st, "employment")
			org := createPersonFactOrganization(t, st, "Example Org", "employer.example")
			suffix := ""
			if historical {
				suffix = `,"start_date":{"year":2020},"end_date":{"year":2022}`
			}
			submitted := fmt.Sprintf(`{"organization":{"id":%d,"name":"Example Org","domain":"employer.example"},"title":"Engineer"%s}`, org.ID, suffix)
			seed, err := st.ApplyPersonFactGenerationContext(t.Context(), personFactProjectionInput(id, "employment-seed", []personfacts.ProposedClaim{personFactProjectionClaim(id, target, submitted, "employment-seed")}, nil), nil)
			require.NoError(err)
			require.Len(seed.Projections, 1)
			want := int64(1)
			if historical {
				want = 0
			}
			assert.Equal(want, inferenceRevision(t, st, id))
			claim := personFactProjectionClaim(id, target, submitted, "employment-retire")
			claim.Relation = personfacts.RelationSupersede
			input := personFactProjectionInput(id, "employment-retire", []personfacts.ProposedClaim{claim}, nil)
			input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
			_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			require.NoError(err)
			if historical {
				_, err = st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
				require.ErrorIs(err, ErrEmploymentNotFound)
			} else {
				emp, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
				require.NoError(err)
				assert.False(emp.IsCurrent)
				assert.False(emp.IsPrimary)
				want++
			}
			assert.Equal(want, inferenceRevision(t, st, id))
		})
	}
}

func TestInferenceExportSystemResolverReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, targets := newPersonFactProjectionStore(t)
	target := targets[AttributeSlugNotes]
	first := personFactProjectionClaim(id, target, `"Inferred"`, "infer")
	_, err := st.ApplyPersonFactGenerationContext(t.Context(), inferenceGenerationInput(id, "inferred-seed", []personfacts.ProposedClaim{first}, nil), nil)
	require.NoError(err)
	before := inferenceRevision(t, st, id)
	require.Equal(int64(1), before)
	system := personFactProjectionClaim(id, target, `"Deterministic"`, "system")
	system.Origin = personfacts.OriginSystem
	system.Confidence.ReportedScore = 1000
	// Retire the old evidence while a supported deterministic winner replaces
	// its slot. This must not be mistaken for an inferred deletion.
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), id, personfacts.EvidenceFilter{})
	require.NoError(err)
	require.Len(evidence, 1)
	input := inferenceGenerationInput(id, "system-winner", []personfacts.ProposedClaim{system}, []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: "source-v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}})
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(err)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), id, PersonAttributeQuery{DefinitionSlug: AttributeSlugNotes})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal(ProvenanceSystem, values[0].Source)
	assert.Equal("Deterministic", *values[0].Value.Text)
	assert.Equal(before, inferenceRevision(t, st, id))
}

func TestInferenceExportCanonicalEmploymentInputs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, id, _ := newPersonFactProjectionStore(t)
	org, err := st.CreateOrganizationContext(t.Context(), OrganizationInput{Name: "Example Org", Kind: OrganizationKindCompany})
	require.NoError(err)
	input := EmploymentInput{PersonID: id, OrganizationID: org.ID, Department: new("Engineering / Platform"), Source: ProvenanceExtraction}
	emp, err := st.AddEmploymentContext(t.Context(), input)
	require.NoError(err)
	before := inferenceRevision(t, st, id)
	input.Department = new("Engineering > Platform")
	_, err = st.UpdateEmploymentContext(t.Context(), emp.ID, emp.Revision, input)
	require.NoError(err)
	assert.Equal(before, inferenceRevision(t, st, id), "equivalent ORG components must not create debt")

	// A legacy blank employer without TITLE/ROLE contributes no properties.
	_, err = st.db.Exec(`UPDATE organizations SET name = ' ' WHERE id = ?`, org.ID)
	require.NoError(err)
	empty := inferencePerson(t, st, "empty-employer@example.com")
	_, err = st.AddEmploymentContext(t.Context(), EmploymentInput{
		PersonID: empty.ID, OrganizationID: org.ID, Source: ProvenanceExtraction,
	})
	require.NoError(err)
	assert.Zero(inferenceRevision(t, st, empty.ID), "empty rendered employment creates no debt")
}

func TestInferenceExportSupportedSystemRetirement(t *testing.T) {
	for _, kind := range []string{"attribute", "employment"} {
		for _, approved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/approved=%t", kind, approved), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st, id, targets := newPersonFactProjectionStore(t)
				target := targets[AttributeSlugNotes]
				submitted := `"Inferred"`
				if kind == "employment" {
					target = projectionTargetBySlug(t, st, "employment")
					org := createPersonFactOrganization(t, st, "Example Org", "system-retirement.example")
					submitted = fmt.Sprintf(`{"organization":{"id":%d,"name":"Example Org","domain":"system-retirement.example"},"title":"Engineer"}`, org.ID)
				}
				seedClaim := personFactProjectionClaim(id, target, submitted, "system-retirement-seed")
				seedClaim.Confidence.ReportedScore = 0
				seed, err := st.ApplyPersonFactGenerationContext(t.Context(), inferenceGenerationInput(id, "system-retirement-seed", []personfacts.ProposedClaim{seedClaim}, nil), nil)
				require.NoError(err)
				require.Len(seed.Projections, 1)
				before := inferenceRevision(t, st, id)
				require.Equal(int64(1), before)
				if approved {
					_, err = st.db.Exec(`UPDATE person_carddav_inference_state SET approved_revision=inference_revision WHERE person_id=?`, id)
					require.NoError(err)
				}
				negative := personFactProjectionClaim(id, target, submitted, "system-retirement")
				negative.Origin = personfacts.OriginSystem
				negative.Confidence.ReportedScore = 1000
				negative.Relation = personfacts.RelationSupersede
				input := inferenceGenerationInput(id, "system-retirement", []personfacts.ProposedClaim{negative}, nil)
				input.ResolvedAt = personFactLedgerNow.Add(time.Hour)
				result, err := st.ApplyPersonFactGenerationContext(t.Context(), input, nil)
				require.NoError(err)
				require.NotEmpty(result.Projections)
				if kind == "attribute" {
					values, err := st.ListPersonAttributeValuesContext(t.Context(), id, PersonAttributeQuery{DefinitionSlug: AttributeSlugNotes})
					require.NoError(err)
					assert.Empty(values)
				} else {
					emp, err := st.GetEmploymentContext(t.Context(), seed.Projections[0].RowID)
					require.NoError(err)
					assert.False(emp.IsCurrent)
				}
				assert.Equal(before, inferenceRevision(t, st, id), "supported deterministic removal preserves the inference revision")
				var gotApproved int64
				require.NoError(st.db.QueryRow(`SELECT approved_revision FROM person_carddav_inference_state WHERE person_id=?`, id).Scan(&gotApproved))
				wantApproved := int64(0)
				if approved {
					wantApproved = before
				}
				assert.Equal(wantApproved, gotApproved)
			})
		}
	}
}
