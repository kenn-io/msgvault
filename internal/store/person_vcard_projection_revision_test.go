package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// vcardProjectionFixture is one person with a value in every table the vCard
// snapshot reads, so a test case can mutate any single input in place.
type vcardProjectionFixture struct {
	store        *store.Store
	person       *store.Person
	counterpart  *store.Person
	name         *store.PersonName
	contactPoint *store.PersonContactPoint
	organization *store.Organization
	employment   *store.Employment
	relationship *store.PersonRelationship
	definition   *store.AttributeDefinition
	attributeID  int64
	review       *store.RelationshipReview
}

func newVCardProjectionFixture(t *testing.T) *vcardProjectionFixture {
	t.Helper()
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	fixture := &vcardProjectionFixture{
		store:       st,
		person:      createEnvelopePerson(t, st, "alice@example.com"),
		counterpart: createEnvelopePerson(t, st, "bob@example.com"),
	}

	var err error
	fixture.name, err = st.AddPersonNameContext(ctx, fixture.person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		OriginalValue: "Alice Example",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	fixture.contactPoint, err = st.AddPersonContactPointContext(
		ctx, fixture.person.ID, store.PersonContactPointInput{
			AddressKind:   store.ContactAddressEmail,
			OriginalValue: "alice@example.com",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
	require.NoError(err)

	definitionInput := personTextDefinition("projection_revision_probe")
	definitionInput.UniversalID = "test-vcard-projection-revision-probe"
	definitionInput.VCardProperty = new("X-PROJECTION-PROBE")
	fixture.definition, err = st.CreateAttributeDefinitionContext(ctx, definitionInput)
	require.NoError(err)
	attribute, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: fixture.person.ID, DefinitionSlug: fixture.definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueText, Text: new("ambient"),
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	fixture.attributeID = attribute.Value.ID

	fixture.organization, err = st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	fixture.employment, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: fixture.person.ID, OrganizationID: fixture.organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	fixture.relationship, err = st.AddPersonRelationshipContext(
		ctx, store.PersonRelationshipInput{
			SourcePersonID: fixture.person.ID, TargetPersonID: fixture.counterpart.ID,
			TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
		})
	require.NoError(err)

	staged, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: fixture.person.ID, RawValue: "Unresolved Person",
		RawType: "unknown-relation", ValueKind: store.RelatedValueKindText,
		Source: store.ProvenanceVCardImport, Actor: "test",
		VCardIdentity: store.VCardIdentity{Property: "RELATED"},
	})
	require.NoError(err)
	require.NotNil(staged.Review)
	fixture.review = staged.Review
	return fixture
}

func (f *vcardProjectionFixture) snapshot(t *testing.T) *store.PersonVCardSnapshot {
	t.Helper()
	snapshot, err := f.store.LoadPersonVCardSnapshotContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	return snapshot
}

// TestPersonVCardProjectionRevisionAdvancesOnEveryProjectingWrite is the
// mechanism's contract: the envelope commit serializes on this one row, so a
// write that can change what the snapshot reads and does not move it would be
// invisible to a concurrent commit. Every case mutates exactly one snapshot
// input, including the ones that reach the person only indirectly, and every
// case must move the revision. Most also change the fingerprint, which pins
// the other half: the fingerprint view still sees each projected input, so
// nothing a card renders has been stripped out of it. The cases that leave the
// projected content where it was say so; the revision moves regardless.
func TestPersonVCardProjectionRevisionAdvancesOnEveryProjectingWrite(t *testing.T) {
	cases := []struct {
		name        string
		sameContent bool
		mutate      func(t *testing.T, f *vcardProjectionFixture)
	}{{
		name: "person name",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.AddPersonNameContext(
				t.Context(), f.person.ID, store.PersonNameInput{
					NameKind: store.PersonNameFormatted, Formatted: new("Alice A. Example"),
					OriginalValue: "Alice A. Example",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person contact point",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.AddPersonContactPointContext(
				t.Context(), f.person.ID, store.PersonContactPointInput{
					AddressKind:   store.ContactAddressEmail,
					OriginalValue: "Alice@Example.com",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person name superseded",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			require.NoError(t, f.store.SupersedePersonNameContext(
				t.Context(), f.person.ID, f.name.Envelope.ID, nil))
		},
	}, {
		name: "person contact point superseded",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			require.NoError(t, f.store.SupersedePersonContactPointContext(
				t.Context(), f.person.ID, f.contactPoint.Envelope.ID, nil))
		},
	}, {
		name: "person address",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.AddPersonAddressContext(
				t.Context(), f.person.ID, store.PersonAddressInput{
					AddressKind: store.PersonAddressPostal, Locality: new("Springfield"),
					OriginalValue: "Springfield",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person date",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			date, err := store.ParsePartialDate("1990-04-01")
			require.NoError(t, err)
			_, err = f.store.AddPersonDateContext(
				t.Context(), f.person.ID, store.PersonDateInput{
					DateKind: store.PersonDateBirthday, Date: date,
					OriginalValue: "1990-04-01",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person category",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.AddPersonCategoryContext(
				t.Context(), f.person.ID, store.PersonCategoryInput{
					OriginalValue: "Colleagues",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person media",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.AddPersonMediaContext(
				t.Context(), f.person.ID, store.PersonMediaInput{
					MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"),
					Data:     []byte("synthetic-photo"),
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person profile patch",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			person, err := f.store.GetPersonContext(t.Context(), f.person.ID)
			require.NoError(t, err)
			_, err = f.store.ApplyPersonProfilePatchContext(
				t.Context(), f.person.ID, person.Revision, store.PersonProfilePatch{
					Names: &store.PersonNamePatch{Add: []store.PersonNameInput{{
						NameKind: store.PersonNameFormatted, Formatted: new("Patched Name"),
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					}}},
				})
			require.NoError(t, err)
		},
	}, {
		name: "person display name",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			person, err := f.store.GetPersonContext(t.Context(), f.person.ID)
			require.NoError(t, err)
			_, err = f.store.UpdatePersonDisplayNameContext(
				t.Context(), f.person.ID, person.Revision, new("Renamed"))
			require.NoError(t, err)
		},
	}, {
		name: "relationship counterpart display name",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			counterpart, err := f.store.GetPersonContext(
				t.Context(), f.counterpart.ID,
			)
			require.NoError(t, err)
			_, err = f.store.UpdatePersonDisplayNameContext(
				t.Context(), counterpart.ID, counterpart.Revision,
				new("Renamed Counterpart"),
			)
			require.NoError(t, err)
		},
	}, {
		name: "person attribute value set",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.SetPersonAttributeValueContext(
				t.Context(), store.PersonAttributeValueInput{
					PersonID: f.person.ID, DefinitionSlug: f.definition.Slug,
					Value: store.AttributeValue{
						Type: store.AttributeValueText, Text: new("jazz"),
					},
					Source: store.ProvenanceUser,
				})
			require.NoError(t, err)
		},
	}, {
		name: "person attribute value supersede",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.SupersedePersonAttributeValueContext(
				t.Context(), store.PersonAttributeSupersedeInput{
					PersonID: f.person.ID, DefinitionSlug: f.definition.Slug,
					ExpectedValueID: &f.attributeID,
				})
			require.NoError(t, err)
		},
	}, {
		name: "employment add",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			other, err := f.store.CreateOrganizationContext(
				t.Context(), store.OrganizationInput{
					Name: "Second Org", Kind: store.OrganizationKindCompany,
				})
			require.NoError(t, err)
			_, err = f.store.AddEmploymentContext(t.Context(), store.EmploymentInput{
				PersonID: f.person.ID, OrganizationID: other.ID,
				Title: new("Advisor"), Source: store.ProvenanceUser,
			})
			require.NoError(t, err)
		},
	}, {
		name: "employment update",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.UpdateEmploymentContext(
				t.Context(), f.employment.ID, f.employment.Revision,
				store.EmploymentInput{
					PersonID: f.person.ID, OrganizationID: f.organization.ID,
					Title: new("Staff Engineer"), Source: store.ProvenanceUser,
				})
			require.NoError(t, err)
		},
	}, {
		name: "employment end",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			endDate, err := store.ParseRelationshipDate("2024-06")
			require.NoError(t, err)
			_, err = f.store.EndEmploymentContext(
				t.Context(), f.employment.ID, f.employment.Revision, endDate)
			require.NoError(t, err)
		},
	}, {
		// The fixture's only current employment became primary when it was
		// added, so re-affirming it changes no projected value.
		name:        "employment set primary",
		sameContent: true,
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.SetPrimaryEmploymentContext(
				t.Context(), f.employment.ID, f.employment.Revision)
			require.NoError(t, err)
		},
	}, {
		name: "employment delete",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			require.NoError(t, f.store.DeleteEmploymentContext(
				t.Context(), f.employment.ID, f.employment.Revision))
		},
	}, {
		name: "employer record replaced",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.ReplaceOrganizationContext(
				t.Context(), f.organization.ID, f.organization.Revision,
				store.OrganizationInput{
					Name: "Example Group", Kind: store.OrganizationKindCompany,
				}, false)
			require.NoError(t, err)
		},
	}, {
		name: "employer profile replaced",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.ReplaceOrganizationProfileContext(
				t.Context(), f.organization.ID, f.organization.Revision,
				store.OrganizationProfileInput{
					Media: []store.OrganizationMediaInput{{
						MediaKind: store.PersonMediaLogo,
						URI:       new("https://example.test/logo.png"),
						Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					}},
				})
			require.NoError(t, err)
		},
	}, {
		name: "employer merged away",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			survivor, err := f.store.CreateOrganizationContext(
				t.Context(), store.OrganizationInput{
					Name: "Surviving Org", Kind: store.OrganizationKindCompany,
				})
			require.NoError(t, err)
			_, err = f.store.MergeOrganizationsContext(
				t.Context(), survivor.ID, survivor.Revision,
				f.organization.ID, f.organization.Revision)
			require.NoError(t, err)
		},
	}, {
		name: "relationship add",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			third := createEnvelopePerson(t, f.store, "carol@example.com")
			_, err := f.store.AddPersonRelationshipContext(
				t.Context(), store.PersonRelationshipInput{
					SourcePersonID: f.person.ID, TargetPersonID: third.ID,
					TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
				})
			require.NoError(t, err)
		},
	}, {
		name: "relationship patch",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			notes := "met at the block party"
			_, err := f.store.PatchPersonRelationshipContext(
				t.Context(), f.relationship.ID, f.relationship.Revision,
				store.PersonRelationshipPatch{Notes: &notes, UpdateNotes: true},
				"user")
			require.NoError(t, err)
		},
	}, {
		name: "relationship delete",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			require.NoError(t, f.store.DeletePersonRelationshipContext(
				t.Context(), f.relationship.ID, f.relationship.Revision))
		},
	}, {
		name: "relationship review staged",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			staged, err := f.store.ResolveRelatedValueContext(
				t.Context(), store.RelatedImport{
					PersonID: f.person.ID, RawValue: "Another Unresolved Person",
					RawType: "unknown-relation", ValueKind: store.RelatedValueKindText,
					Source: store.ProvenanceVCardImport, Actor: "test",
					VCardIdentity: store.VCardIdentity{Property: "RELATED"},
				})
			require.NoError(t, err)
			require.NotNil(t, staged.Review)
		},
	}, {
		name: "relationship review accepted",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.AcceptRelationshipReviewContext(
				t.Context(), f.review.ID, "acquaintance", f.counterpart.ID, "user")
			require.NoError(t, err)
		},
	}, {
		name: "relationship review rejected",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.RejectRelationshipReviewContext(t.Context(), f.review.ID, "user")
			require.NoError(t, err)
		},
	}, {
		name: "attribute definition created",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			input := personTextDefinition("another_projection_probe")
			input.UniversalID = "test-vcard-another-projection-probe"
			input.VCardProperty = new("X-ANOTHER-PROBE")
			_, err := f.store.CreateAttributeDefinitionContext(t.Context(), input)
			require.NoError(t, err)
		},
	}, {
		name: "attribute definition deactivated",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			_, err := f.store.UpdateAttributeDefinitionContext(
				t.Context(), f.definition.ID, f.definition.Revision,
				store.AttributeDefinitionUpdate{IsActive: new(false)})
			require.NoError(t, err)
		},
	}, {
		name:        "attribute definition created and deleted",
		sameContent: true,
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			input := personTextDefinition("disposable_projection_probe")
			input.UniversalID = "test-vcard-disposable-projection-probe"
			input.VCardProperty = new("X-DISPOSABLE-PROBE")
			disposable, err := f.store.CreateAttributeDefinitionContext(t.Context(), input)
			require.NoError(t, err)
			require.NoError(t, f.store.DeleteAttributeDefinitionContext(
				t.Context(), disposable.ID, disposable.Revision))
		},
	}, {
		name: "relationship type updated",
		mutate: func(t *testing.T, f *vcardProjectionFixture) {
			t.Helper()
			friend, err := f.store.GetRelationshipTypeBySlugContext(t.Context(), "friend")
			require.NoError(t, err)
			renamed := "mate"
			_, err = f.store.UpdateRelationshipTypeContext(
				t.Context(), friend.ID, friend.Revision,
				store.RelationshipTypeUpdate{
					ForwardLabel: &renamed, ReverseLabel: &renamed,
				})
			require.NoError(t, err)
		},
	}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newVCardProjectionFixture(t)
			before := fixture.snapshot(t)
			testCase.mutate(t, fixture)
			after := fixture.snapshot(t)
			assert.Greater(t, after.ProjectionRevision, before.ProjectionRevision,
				"a write that changes a snapshot input must move the projection revision")
			if testCase.sameContent {
				assert.Equal(t, before.Fingerprint, after.Fingerprint,
					"projected content is unchanged, so the fingerprint must not move")
				return
			}
			assert.NotEqual(t, before.Fingerprint, after.Fingerprint,
				"the fingerprint view must still see the projected input this write changed")
		})
	}
}

// TestPersonVCardProjectionRevisionSkipsNonProjectingCatalogWrites is the
// narrowing half of the catalog contract: definitions and relationship types
// are shared, but a write to one only reaches the cards that resolve through
// it. Presentation-only definition edits and unmapped definitions reach no
// card at all; a relationship type rename reaches only the endpoints of its
// edges.
func TestPersonVCardProjectionRevisionSkipsNonProjectingCatalogWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newVCardProjectionFixture(t)
	ctx := t.Context()
	stranger := createEnvelopePerson(t, fixture.store, "dana@example.com")
	strangerSnapshot := func() *store.PersonVCardSnapshot {
		snapshot, err := fixture.store.LoadPersonVCardSnapshotContext(ctx, stranger.ID)
		require.NoError(err)
		return snapshot
	}

	before := fixture.snapshot(t)
	definition, err := fixture.store.UpdateAttributeDefinitionContext(
		ctx, fixture.definition.ID, fixture.definition.Revision,
		store.AttributeDefinitionUpdate{
			Label: new("Projection Probe"), Description: new(new("Presentation only")),
			DisplayOrder: new(int64(7)),
		})
	require.NoError(err)
	after := fixture.snapshot(t)
	assert.Equal(before.ProjectionRevision, after.ProjectionRevision,
		"a presentation-only definition edit must not move the projection revision")
	assert.Equal(before.Fingerprint, after.Fingerprint,
		"a presentation-only definition edit must not change the fingerprint")

	unmapped := personTextDefinition("unmapped_probe")
	unmapped.UniversalID = "test-vcard-unmapped-probe"
	_, err = fixture.store.CreateAttributeDefinitionContext(ctx, unmapped)
	require.NoError(err)
	after = fixture.snapshot(t)
	assert.Equal(before.ProjectionRevision, after.ProjectionRevision,
		"a definition without a vCard property is never projected")
	assert.Equal(before.Fingerprint, after.Fingerprint)

	_, err = fixture.store.UpdateAttributeDefinitionContext(
		ctx, definition.ID, definition.Revision,
		store.AttributeDefinitionUpdate{IsActive: new(false)})
	require.NoError(err)
	after = fixture.snapshot(t)
	assert.Greater(after.ProjectionRevision, before.ProjectionRevision,
		"deactivating a mapped definition removes it from every snapshot")
	assert.NotEqual(before.Fingerprint, after.Fingerprint)

	personBefore, strangerBefore := fixture.snapshot(t), strangerSnapshot()
	friend, err := fixture.store.GetRelationshipTypeBySlugContext(ctx, "friend")
	require.NoError(err)
	_, err = fixture.store.UpdateRelationshipTypeContext(
		ctx, friend.ID, friend.Revision,
		store.RelationshipTypeUpdate{ForwardLabel: new("mate"), ReverseLabel: new("mate")})
	require.NoError(err)
	personAfter, strangerAfter := fixture.snapshot(t), strangerSnapshot()
	assert.Greater(personAfter.ProjectionRevision, personBefore.ProjectionRevision,
		"a person with an edge of the renamed type projects its labels")
	assert.NotEqual(personBefore.Fingerprint, personAfter.Fingerprint)
	assert.Equal(strangerBefore.ProjectionRevision, strangerAfter.ProjectionRevision,
		"a person with no edge of the type has nothing to re-render")
	assert.Equal(strangerBefore.Fingerprint, strangerAfter.Fingerprint)
}

// TestPersonVCardFingerprintIgnoresWatermarks pins what the fingerprint is
// for: it has to change when projected content changes and only then. Linking
// a participant advances the person record's revision, updated_at, and binding
// set — none of which any card renders — so a render made just before the link
// must still commit rather than conflict and re-render an identical card.
func TestPersonVCardFingerprintIgnoresWatermarks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newVCardProjectionFixture(t)
	ctx := t.Context()
	created, before, prepared := renderVCardEnvelope(t, fixture.store, fixture.person.ID,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
		"watermarks", "Alice Example")
	other, err := fixture.store.EnsureParticipant("alice@example.org", "Alice", "example.org")
	require.NoError(err)
	_, err = fixture.store.LinkParticipants(fixture.person.ParticipantIDs[0], other)
	require.NoError(err)
	person, err := fixture.store.GetPersonContext(ctx, fixture.person.ID)
	require.NoError(err)
	require.Greater(person.Revision, fixture.person.Revision,
		"the link must have moved the person record's own revision")
	require.Len(person.ParticipantIDs, 2)

	linked := fixture.snapshot(t)
	assert.Equal(before.Fingerprint, linked.Fingerprint,
		"a binding change renders nothing and must not change the fingerprint")
	committed, err := fixture.store.CommitVCardResourceEnvelopeContext(
		ctx, "book", "watermarks", created.Revision, before.Fingerprint, prepared)
	require.NoError(err, "a render made before a watermark-only write must still commit")
	assert.Equal(created.Revision+1, committed.Revision)

	_, err = fixture.store.AddPersonNameContext(ctx, fixture.person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Renamed"),
		OriginalValue: "Alice Renamed",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	renamed := fixture.snapshot(t)
	assert.NotEqual(linked.Fingerprint, renamed.Fingerprint,
		"a projected value change must change the fingerprint")
}

// TestPersonVCardProjectionRevisionAdvancesForDeletedCounterparts covers the
// half of the contract a person's own writes cannot: deleting a person removes
// rows from SOMEONE ELSE's snapshot through the schema's cascades, so the
// deletion has to move that person's projection revision itself. Each case
// leaves the survivor with exactly one row naming the deleted person.
func TestPersonVCardProjectionRevisionAdvancesForDeletedCounterparts(t *testing.T) {
	cases := []struct {
		name   string
		relate func(t *testing.T, st *store.Store, deleted, survivor *store.Person)
	}{{
		name: "deleted person is the relationship source",
		relate: func(t *testing.T, st *store.Store, deleted, survivor *store.Person) {
			t.Helper()
			_, err := st.AddPersonRelationshipContext(
				t.Context(), store.PersonRelationshipInput{
					SourcePersonID: deleted.ID, TargetPersonID: survivor.ID,
					TypeSlug: "parent", Source: store.ProvenanceUser, Actor: "test",
				})
			require.NoError(t, err)
		},
	}, {
		name: "deleted person is the relationship target",
		relate: func(t *testing.T, st *store.Store, deleted, survivor *store.Person) {
			t.Helper()
			_, err := st.AddPersonRelationshipContext(
				t.Context(), store.PersonRelationshipInput{
					SourcePersonID: survivor.ID, TargetPersonID: deleted.ID,
					TypeSlug: "parent", Source: store.ProvenanceUser, Actor: "test",
				})
			require.NoError(t, err)
		},
	}, {
		// An exact UID with an unrecognized TYPE records the match but leaves
		// the review pending, which is the state the survivor's card renders.
		name: "deleted person is a pending review's match",
		relate: func(t *testing.T, st *store.Store, deleted, survivor *store.Person) {
			t.Helper()
			staged, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{
				PersonID: survivor.ID, RawValue: "urn:uuid:" + deleted.VCardUID,
				RawType: "unknown-relation", ValueKind: store.RelatedValueKindURI,
				Source: store.ProvenanceVCardImport, Actor: "test",
				VCardIdentity: store.VCardIdentity{Property: "RELATED"},
			})
			require.NoError(t, err)
			require.NotNil(t, staged.Review)
			require.Equal(t, store.RelationshipReviewPending, staged.Review.Status)
			require.NotNil(t, staged.Review.MatchedPersonID)
			require.Equal(t, deleted.ID, *staged.Review.MatchedPersonID)
		},
	}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := t.Context()
			st := testutil.NewTestStore(t)
			deleted := createEnvelopePerson(t, st, "alice@example.com")
			survivor := createEnvelopePerson(t, st, "bob@example.com")
			testCase.relate(t, st, deleted, survivor)

			before, err := st.LoadPersonVCardSnapshotContext(ctx, survivor.ID)
			require.NoError(err)
			doomed, err := st.GetPersonContext(ctx, deleted.ID)
			require.NoError(err)
			require.NoError(st.DeletePersonContext(ctx, deleted.ID, doomed.Revision))

			after, err := st.LoadPersonVCardSnapshotContext(ctx, survivor.ID)
			require.NoError(err)
			assert.NotEqual(before.Fingerprint, after.Fingerprint,
				"the cascade must have changed what the survivor's card projects")
			assert.Greater(after.ProjectionRevision, before.ProjectionRevision,
				"a cascade that changes a survivor's snapshot must move their projection revision")
		})
	}
}

// TestPersonVCardProjectionRevisionIgnoresUnrelatedPeople keeps the bump from
// degenerating into "touch everything": a card that shares no semantic row
// with the write must stay commitable.
func TestPersonVCardProjectionRevisionIgnoresUnrelatedPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newVCardProjectionFixture(t)
	ctx := t.Context()
	stranger := createEnvelopePerson(t, fixture.store, "dana@example.com")

	before, err := fixture.store.LoadPersonVCardSnapshotContext(ctx, stranger.ID)
	require.NoError(err)
	_, err = fixture.store.AddPersonNameContext(
		ctx, fixture.person.ID, store.PersonNameInput{
			NameKind: store.PersonNameFormatted, Formatted: new("Alice Renamed"),
			OriginalValue: "Alice Renamed",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
	require.NoError(err)
	after, err := fixture.store.LoadPersonVCardSnapshotContext(ctx, stranger.ID)
	require.NoError(err)

	assert.Equal(before.ProjectionRevision, after.ProjectionRevision)
	assert.Equal(before.Fingerprint, after.Fingerprint)
}
