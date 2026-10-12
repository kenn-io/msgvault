package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A merge must not rewrite a third person's profile outside its admitted scope.
func TestPersonMergeAuthorizationIncludesNativeAffectedOwners(t *testing.T) {
	for _, kind := range []string{"relationship", "current attribute", "historical attribute", "matched review", "accepted review owner"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := storetest.New(t).Store
			survivorID := mustPromotedPerson(t, st, "merge-survivor@example.test", "Synthetic Survivor").ID
			absorbedID := mustPromotedPerson(t, st, "merge-absorbed@example.test", "Synthetic Absorbed").ID
			otherID := mustPromotedPerson(t, st, "merge-other@example.test", "Synthetic Other").ID
			absorbed, err := st.GetPerson(absorbedID)
			requirements.NoError(err)
			wantIDs := []int64{survivorID, absorbedID, otherID}
			switch kind {
			case "relationship":
				_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: absorbedID, TargetPersonID: otherID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "synthetic-owner"})
				requirements.NoError(err)
			case "current attribute", "historical attribute":
				definition := personTextDefinition("synthetic_scope_reference")
				definition.UniversalID = "synthetic-merge-scope-reference"
				definition.ValueType = store.AttributeValueRecordReference
				definition.FieldType = store.AttributeFieldPerson
				definition.RecordTarget = new("person")
				_, err = st.CreateAttributeDefinitionContext(t.Context(), definition)
				requirements.NoError(err)
				value, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: otherID, DefinitionSlug: definition.Slug, Value: store.AttributeValue{Type: store.AttributeValueRecordReference, RecordType: new("person"), RecordID: &absorbedID}, Source: store.ProvenanceUser})
				requirements.NoError(err)
				if kind == "historical attribute" {
					_, err = st.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: otherID, DefinitionSlug: definition.Slug, ExpectedValueID: &value.Value.ID})
					requirements.NoError(err)
				}
			case "accepted review owner":
				counterpart := mustPromotedPerson(t, st, "merge-counterpart@example.test", "Synthetic Counterpart")
				edge, err := st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: absorbedID, TargetPersonID: counterpart.ID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "synthetic-owner"})
				requirements.NoError(err)
				review, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: otherID, RawValue: "Unresolved synthetic relation", RawType: "synthetic-unknown-type", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceUser, Actor: "synthetic-owner"})
				requirements.NoError(err)
				requirements.NotNil(review.Review)
				// Retain a historical accepted ledger dependency whose current
				// owner is no longer an endpoint of the original relationship.
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_relationship_reviews SET status = 'accepted', accepted_relationship_id = ? WHERE id = ?`), edge.ID, review.Review.ID)
				requirements.NoError(err)
				wantIDs = append(wantIDs, counterpart.ID)
			case "matched review":
				result, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{PersonID: otherID, RawValue: absorbed.VCardUID, RawType: "synthetic-unknown-type", ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceUser, Actor: "synthetic-owner"})
				requirements.NoError(err)
				requirements.NotNil(result.Review)
				assertions.Equal(new(absorbedID), result.Review.MatchedPersonID)
			}
			survivor, err := st.GetPerson(survivorID)
			requirements.NoError(err)
			absorbed, err = st.GetPerson(absorbedID)
			requirements.NoError(err)
			other, err := st.GetPerson(otherID)
			requirements.NoError(err)
			denied := errors.New("synthetic third-person scope denied")
			called := false
			_, err = st.MergePersonsAuthorizedContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivorID, AbsorbedID: absorbedID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-affected-merge", Actor: "agent:synthetic-grant"}, func(_ context.Context, selection *store.IdentityGrantSelection) error {
				called = true
				ids := make([]int64, len(selection.Persons))
				for i, person := range selection.Persons {
					ids[i] = person.ID
				}
				assertions.ElementsMatch(wantIDs, ids)
				return denied
			})
			requirements.ErrorIs(err, denied)
			assertions.True(called)
			for _, before := range []*store.Person{survivor, absorbed, other} {
				after, err := st.GetPerson(before.ID)
				requirements.NoError(err)
				assertions.Equal(before, after)
			}
		})
	}
}
