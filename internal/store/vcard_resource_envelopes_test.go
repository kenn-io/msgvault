package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestVCardResourceEnvelopeRoundTripsExactBodyAndMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"item1.EMAIL;TYPE=home;X-KEEP=one:alice@example.com\r\n" +
		"item1.X-ABLABEL:Home\r\nEND:VCARD\r\n")
	envelope := parseStoreEnvelope(t, raw, "book", "source-1")
	envelope.CanonicalPersonUID = person.VCardUID

	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: envelope,
		},
	)
	require.NoError(err)
	assert.Equal(int64(1), created.Revision)
	assert.Equal(raw, created.OriginalRawBytes)
	assert.Equal(raw, created.StoredBody)
	assert.Equal(vcard.ContentHash(raw), created.ContentHash)
	assert.Equal(vcard.ETagForBody(raw), created.ETag)
	assert.Equal(envelope.PropertyTree, created.PropertyTree)

	loaded, err := st.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "source-1",
	)
	require.NoError(err)
	assert.Equal(created.ID, loaded.ID)
	assert.Equal(created.PropertyTree, loaded.PropertyTree)
	assert.Equal(created.Residue, loaded.Residue)

	v3, err := st.RenderVCardResourceViewContext(
		t.Context(), "book", "source-1", vcard.Version30,
	)
	require.NoError(err)
	assert.Contains(string(v3), "VERSION:3.0")
	unchanged, err := st.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "source-1",
	)
	require.NoError(err)
	assert.Equal(int64(1), unchanged.Revision)
	assert.Equal(raw, unchanged.StoredBody)
}

func TestVCardResourceCommitPersistsBodyTreeMappingsAndResidueAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"EMAIL;X-KEEP=opaque:old@example.com\r\n" +
		"X-VENDOR;X-FUTURE=yes:keep\r\nEND:VCARD\r\n")
	envelope := parseStoreEnvelope(t, raw, "book", "mapped")
	email := findStoreProperty(t, envelope.PropertyTree, "EMAIL")
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: email.Identity, SourceRef: "book",
		Table: "person_contact_points", RowID: 10, Field: "original_value",
		Kind: vcard.HandlingNative,
	}}
	envelope.Residue = vcard.ResidueWithMappings(
		envelope.PropertyTree, envelope.NativeMappings,
	)
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: envelope,
		},
	)
	require.NoError(err)

	replacement, err := vcard.NewProperty("", "EMAIL", "new@example.com")
	require.NoError(err)
	merged, err := created.MergeProperties(
		[]vcard.PropertyEdit{{Identity: email.Identity, Property: replacement}},
	)
	require.NoError(err)
	prepared, err := merged.PrepareCanonicalRender()
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	committed, err := st.CommitVCardResourceEnvelopeContext(
		t.Context(), "book", "mapped", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.NoError(err)

	assert.Equal(int64(2), committed.Revision)
	assert.Equal(snapshot.Fingerprint, committed.ProjectionFingerprint)
	assert.Contains(string(committed.StoredBody), "EMAIL;X-KEEP=opaque:new@example.com")
	assert.Contains(string(committed.StoredBody), "X-VENDOR;X-FUTURE=yes:keep")
	assert.Equal(vcard.ContentHash(committed.StoredBody), committed.ContentHash)
	assert.Equal(vcard.ETagForBody(committed.StoredBody), committed.ETag)
	require.Len(committed.NativeMappings, 1)
	assert.Equal(email.Identity, committed.NativeMappings[0].Identity)
	require.Len(committed.Residue, 2)
	assert.Equal("FN", committed.Residue[0].Property.Name)
	assert.Equal("X-VENDOR", committed.Residue[1].Property.Name)

	reloaded, err := st.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "mapped",
	)
	require.NoError(err)
	assert.Equal(committed.StoredBody, reloaded.StoredBody)
	assert.Equal(committed.PropertyTree, reloaded.PropertyTree)
	assert.Equal(committed.NativeMappings, reloaded.NativeMappings)
	assert.Equal(committed.Residue, reloaded.Residue)
}

func TestVCardResourceNoOpDoesNotAdvanceRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	envelope := parseStoreEnvelope(t,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
		"book", "noop",
	)
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, ProjectionFingerprint: "same",
			Envelope: envelope,
		},
	)
	require.NoError(err)

	expectedRevision := created.Revision
	noOp, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, ExpectedRevision: &expectedRevision,
			ProjectionFingerprint: "same", Envelope: created.ResourceEnvelope,
		},
	)
	require.NoError(err)
	assert.Equal(created.Revision, noOp.Revision)
	assert.Equal(created.UpdatedAt, noOp.UpdatedAt)

	advanced, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, ExpectedRevision: &expectedRevision,
			ProjectionFingerprint: "newer", Envelope: created.ResourceEnvelope,
		},
	)
	require.NoError(err)
	assert.Equal(created.Revision+1, advanced.Revision)
	staleRevision := created.Revision
	_, err = st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, ExpectedRevision: &staleRevision,
			ProjectionFingerprint: "newer", Envelope: advanced.ResourceEnvelope,
		},
	)
	require.ErrorIs(err, store.ErrVCardResourceWriteConflict)
}

func TestVCardResourceUpdateRejectsPersonReassignment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	originalOwner := createEnvelopePerson(t, st, "original-owner@example.com")
	otherPerson := createEnvelopePerson(t, st, "other-person@example.com")
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: originalOwner.ID,
			Envelope: parseStoreEnvelope(t,
				[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Owner\r\nEND:VCARD\r\n"),
				"book", "owned-resource",
			),
		},
	)
	require.NoError(err)

	replacement := created.ResourceEnvelope
	// Reproduce the bypass: an omitted canonical UID is normally filled from
	// PersonID, but must not make an existing resource transferable.
	replacement.CanonicalPersonUID = ""
	_, err = st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: otherPerson.ID, ExpectedRevision: &created.Revision,
			Envelope: replacement,
		},
	)
	require.ErrorIs(err, store.ErrVCardResourceInvalid)

	unchanged, err := st.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "owned-resource",
	)
	require.NoError(err)
	assert.Equal(originalOwner.ID, unchanged.PersonID)
	assert.Equal(originalOwner.VCardUID, unchanged.CanonicalPersonUID)
	assert.Equal(created.Revision, unchanged.Revision)
}

// TestVCardResourceCommitRequiresProjectionFingerprint pins the one argument
// that carries the whole concurrency guarantee. Without a fingerprint there is
// nothing for the projection lock and recheck to serialize against, so a commit
// that omitted it would write a render of semantic state that may already be
// gone — silently, and only under concurrency. It is refused as invalid input
// rather than accepted as a weaker commit.
func TestVCardResourceCommitRequiresProjectionFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	created, err := st.PutVCardResourceEnvelopeContext(
		ctx, store.VCardResourceEnvelopeInput{
			PersonID: person.ID,
			Envelope: parseStoreEnvelope(t, raw, "book", "unfingerprinted"),
		},
	)
	require.NoError(err)
	prepared := replaceStoreFormattedName(
		t, created.ResourceEnvelope, "Alice Example",
	)

	for _, fingerprint := range []string{"", "   "} {
		_, err = st.CommitVCardResourceEnvelopeContext(
			ctx, "book", "unfingerprinted", created.Revision,
			fingerprint, prepared,
		)
		require.ErrorIs(err, store.ErrVCardResourceInvalid)

		untouched, getErr := st.GetVCardResourceEnvelopeContext(
			ctx, "book", "unfingerprinted",
		)
		require.NoError(getErr)
		assert.Equal(created.Revision, untouched.Revision)
		assert.Equal(raw, untouched.StoredBody)
		assert.Equal(created.UpdatedAt, untouched.UpdatedAt)
	}

	snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	committed, err := st.CommitVCardResourceEnvelopeContext(
		ctx, "book", "unfingerprinted", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.NoError(err)
	assert.Equal(created.Revision+1, committed.Revision)
	assert.Equal(snapshot.Fingerprint, committed.ProjectionFingerprint)
	assert.Contains(string(committed.StoredBody), "FN:Alice Example")
}

func TestVCardResourceIdentityAndHrefAreScopedBySource(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	first := parseStoreEnvelope(t, raw, "book-a", "shared-uid")
	first.Href = "/people/alice.vcf"
	_, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: first,
		},
	)
	require.NoError(t, err)

	secondSource := parseStoreEnvelope(t, raw, "book-b", "shared-uid")
	secondSource.Href = first.Href
	_, err = st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: secondSource,
		},
	)
	require.NoError(t, err)

	duplicateHref := parseStoreEnvelope(t, raw, "book-a", "different-uid")
	duplicateHref.Href = first.Href
	_, err = st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: duplicateHref,
		},
	)
	require.ErrorIs(t, err, store.ErrVCardResourceIdentityExists)
}

func TestVCardResourceRejectsStaleWriterAcrossHandles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "vcard-cas.db")
	first, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = first.Close() })
	require.NoError(first.InitSchema())
	person := createEnvelopePerson(t, first, "alice@example.com")
	second, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	envelope := parseStoreEnvelope(t,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Initial\r\nEND:VCARD\r\n"),
		"book", "cas",
	)
	created, err := first.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: envelope,
		},
	)
	require.NoError(err)
	firstRead, err := first.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "cas",
	)
	require.NoError(err)
	secondRead, err := second.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "cas",
	)
	require.NoError(err)

	// Both handles render from the same unchanged projection, so the loser is
	// rejected by the envelope's own revision compare-and-swap rather than by
	// the projection guard.
	snapshot, err := first.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	firstPrepared := replaceStoreFormattedName(
		t, firstRead.ResourceEnvelope, "First",
	)
	winner, err := first.CommitVCardResourceEnvelopeContext(
		t.Context(), "book", "cas", firstRead.Revision,
		snapshot.Fingerprint, firstPrepared,
	)
	require.NoError(err)
	secondPrepared := replaceStoreFormattedName(
		t, secondRead.ResourceEnvelope, "Stale",
	)
	_, err = second.CommitVCardResourceEnvelopeContext(
		t.Context(), "book", "cas", secondRead.Revision,
		snapshot.Fingerprint, secondPrepared,
	)
	require.ErrorIs(err, store.ErrVCardResourceWriteConflict)
	var conflict *store.VCardResourceWriteConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(secondRead.Revision, conflict.ExpectedRevision)

	loaded, err := second.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "cas",
	)
	require.NoError(err)
	assert.Equal(winner.Revision, loaded.Revision)
	assert.Contains(string(loaded.StoredBody), "FN:First")
	assert.NotContains(string(loaded.StoredBody), "FN:Stale")
	assert.Equal(created.OriginalRawBytes, loaded.OriginalRawBytes)
}

func TestVCardResourceSourceUIDRewriteUsesCASAndKeepsCanonicalIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	envelope := parseStoreEnvelope(t,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
		"book", "old-source",
	)
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: envelope,
		},
	)
	require.NoError(err)

	rewritten, err := st.RewriteVCardResourceSourceUIDContext(
		t.Context(), "book", "old-source", "new-source", created.Revision,
	)
	require.NoError(err)
	assert.Equal(created.Revision+1, rewritten.Revision)
	assert.Equal("new-source", rewritten.SourceResourceUID)
	assert.Equal(person.VCardUID, rewritten.CanonicalPersonUID)
	assert.Equal(created.StoredBody, rewritten.StoredBody)
	assert.Equal(created.ETag, rewritten.ETag)

	_, err = st.RewriteVCardResourceSourceUIDContext(
		t.Context(), "book", "new-source", "stale-source", created.Revision,
	)
	require.ErrorIs(err, store.ErrVCardResourceWriteConflict)
}

func TestVCardResourceSourceUIDRewriteMigratesProvenanceAndProjectionRevisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice-rewrite@example.com")
	related := createEnvelopePerson(t, st, "bob-rewrite@example.com")
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID,
			Envelope: parseStoreEnvelope(t,
				[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
				"book", "old-source",
			),
		},
	)
	require.NoError(err)

	oldUID := "old-source"
	newUID := "new-source"
	sourceRef := "book"
	propID := "name-1"
	formatted := "Alice Rewrite"
	_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind:      store.PersonNameFormatted,
		Formatted:     &formatted,
		OriginalValue: formatted,
		Envelope: store.ValueEnvelopeInput{
			Source:            store.ProvenanceVCardImport,
			SourceRef:         &sourceRef,
			SourceResourceUID: &oldUID,
			VCard: store.VCardIdentity{
				Property: "FN",
				PropID:   &propID,
			},
		},
	})
	require.NoError(err)

	resolution, err := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{
		PersonID:          person.ID,
		RawValue:          related.VCardUID,
		RawType:           "agent",
		ValueKind:         store.RelatedValueKindText,
		Source:            store.ProvenanceVCardImport,
		SourceRef:         &sourceRef,
		SourceResourceUID: &oldUID,
		VCardIdentity:     store.VCardIdentity{Property: "RELATED"},
		Actor:             "system",
	})
	require.NoError(err)
	require.NotNil(resolution.Relationship)
	require.NotNil(resolution.Review)

	personBefore, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	relatedBefore, err := st.LoadPersonVCardSnapshotContext(t.Context(), related.ID)
	require.NoError(err)

	_, err = st.RewriteVCardResourceSourceUIDContext(
		t.Context(), sourceRef, oldUID, newUID, created.Revision,
	)
	require.NoError(err)

	names, err := st.ListPersonNamesContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NotNil(names[0].Envelope.SourceResourceUID)
	assert.Equal(newUID, *names[0].Envelope.SourceResourceUID)

	relationship, err := st.GetPersonRelationshipContext(
		t.Context(), resolution.Relationship.ID,
	)
	require.NoError(err)
	require.NotNil(relationship.SourceResourceUID)
	assert.Equal(newUID, *relationship.SourceResourceUID)

	reviews, err := st.ListRelationshipReviewsContext(
		t.Context(), store.RelationshipReviewListOptions{PersonID: person.ID},
	)
	require.NoError(err)
	require.Len(reviews, 1)
	require.NotNil(reviews[0].SourceResourceUID)
	assert.Equal(newUID, *reviews[0].SourceResourceUID)

	personAfter, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	relatedAfter, err := st.LoadPersonVCardSnapshotContext(t.Context(), related.ID)
	require.NoError(err)
	assert.Greater(personAfter.ProjectionRevision, personBefore.ProjectionRevision)
	assert.Greater(relatedAfter.ProjectionRevision, relatedBefore.ProjectionRevision)
}

func TestVCardResourceSourceUIDRewriteAdvancesProfileCASRevisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "profile-cas@example.com")
	created, err := st.PutVCardResourceEnvelopeContext(
		ctx, store.VCardResourceEnvelopeInput{
			PersonID: person.ID,
			Envelope: parseStoreEnvelope(t,
				[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Profile CAS\r\nEND:VCARD\r\n"),
				"book", "old-source",
			),
		},
	)
	require.NoError(err)

	sourceRef := "book"
	oldUID := "old-source"
	newUID := "new-source"
	personPropID := "person-name"
	formatted := "Profile CAS"
	_, err = st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind:      store.PersonNameFormatted,
		Formatted:     &formatted,
		OriginalValue: formatted,
		Envelope: store.ValueEnvelopeInput{
			Source:            store.ProvenanceVCardImport,
			SourceRef:         &sourceRef,
			SourceResourceUID: &oldUID,
			VCard: store.VCardIdentity{
				Property: "FN",
				PropID:   &personPropID,
			},
		},
	})
	require.NoError(err)

	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Profile CAS Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	organizationPropID := "organization-name"
	organizationInput := store.OrganizationProfileInput{
		Names: []store.OrganizationNameInput{{
			Name: "Profile CAS Organization", NameKind: store.OrganizationNameKindAlias,
			Envelope: store.ValueEnvelopeInput{
				Source:            store.ProvenanceVCardImport,
				SourceRef:         &sourceRef,
				SourceResourceUID: &oldUID,
				VCard: store.VCardIdentity{
					Property: "ORG",
					PropID:   &organizationPropID,
				},
			},
		}},
	}
	organizationProfile, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, organizationInput,
	)
	require.NoError(err)
	personBefore, err := st.GetPersonContext(ctx, person.ID)
	require.NoError(err)
	organizationBefore := organizationProfile.Organization

	_, err = st.RewriteVCardResourceSourceUIDContext(
		ctx, sourceRef, oldUID, newUID, created.Revision,
	)
	require.NoError(err)

	personAfter, err := st.GetPersonContext(ctx, person.ID)
	require.NoError(err)
	organizationAfter, err := st.GetOrganizationContext(ctx, organization.ID)
	require.NoError(err)
	assert.Equal(personBefore.Revision+1, personAfter.Revision)
	assert.Equal(organizationBefore.Revision+1, organizationAfter.Revision)

	_, err = st.ApplyPersonProfilePatchContext(
		ctx, person.ID, personBefore.Revision, store.PersonProfilePatch{
			Categories: &store.PersonCategoryPatch{Add: []store.PersonCategoryInput{{
				OriginalValue: "stale",
				Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}}},
		},
	)
	require.ErrorIs(err, store.ErrPersonRevisionConflict)
	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organizationBefore.Revision, organizationInput,
	)
	require.ErrorIs(err, store.ErrOrganizationRevisionConflict)

	rewrittenProfile, err := st.GetOrganizationProfileContext(
		ctx, organization.ID, false,
	)
	require.NoError(err)
	require.Len(rewrittenProfile.Names, 1)
	require.NotNil(rewrittenProfile.Names[0].Envelope.SourceResourceUID)
	assert.Equal(newUID, *rewrittenProfile.Names[0].Envelope.SourceResourceUID)
}

func TestVCardResourceSourceUIDRewriteRollsBackOnProvenanceCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "rewrite-collision@example.com")
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID,
			Envelope: parseStoreEnvelope(t,
				[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Collision\r\nEND:VCARD\r\n"),
				"book", "old-source",
			),
		},
	)
	require.NoError(err)

	sourceRef := "book"
	propID := "name-1"
	for _, uid := range []string{"old-source", "new-source"} {
		formatted := "Name from " + uid
		_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
			NameKind:      store.PersonNameFormatted,
			Formatted:     &formatted,
			OriginalValue: formatted,
			Envelope: store.ValueEnvelopeInput{
				Source:            store.ProvenanceVCardImport,
				SourceRef:         &sourceRef,
				SourceResourceUID: &uid,
				VCard: store.VCardIdentity{
					Property: "FN",
					PropID:   &propID,
				},
			},
		})
		require.NoError(err)
	}
	before, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)

	_, err = st.RewriteVCardResourceSourceUIDContext(
		t.Context(), sourceRef, "old-source", "new-source", created.Revision,
	)
	require.ErrorIs(err, store.ErrVCardResourceIdentityExists)

	unchanged, err := st.GetVCardResourceEnvelopeContext(
		t.Context(), sourceRef, "old-source",
	)
	require.NoError(err)
	assert.Equal(created.Revision, unchanged.Revision)
	_, err = st.GetVCardResourceEnvelopeContext(t.Context(), sourceRef, "new-source")
	require.ErrorIs(err, store.ErrVCardResourceNotFound)

	names, err := st.ListPersonNamesContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.Len(names, 2)
	uids := make([]string, 0, len(names))
	for _, name := range names {
		require.NotNil(name.Envelope.SourceResourceUID)
		uids = append(uids, *name.Envelope.SourceResourceUID)
	}
	assert.ElementsMatch([]string{"old-source", "new-source"}, uids)
	after, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal(before.ProjectionRevision, after.ProjectionRevision)
}

func TestVCardResourcePreservesInitialRawBytesOnReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	initialRaw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Initial\r\nEND:VCARD\r\n")
	initial := parseStoreEnvelope(t, initialRaw, "book", "replace")
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: initial,
		},
	)
	require.NoError(err)

	replacementRaw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Replacement\r\nEND:VCARD\r\n")
	replacement := parseStoreEnvelope(t, replacementRaw, "book", "replace")
	updated, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, ExpectedRevision: &created.Revision,
			Envelope: replacement,
		},
	)
	require.NoError(err)
	assert.Equal(initialRaw, updated.OriginalRawBytes)
	assert.Equal(replacementRaw, updated.StoredBody)
}

func TestCanonicalAndSourceUIDNamespacesRemainSeparate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	alias, err := st.RetirePersonUIDAliasContext(
		t.Context(), "retired-canonical", &person.ID, "merge",
	)
	require.NoError(err)
	assert.Equal("retired-canonical", alias.RetiredUID)
	_, err = st.ResolveRetiredPersonUIDContext(
		t.Context(), "retired-canonical",
	)
	require.NoError(err)
	_, err = st.ResolvePersonByVCardUIDContext(
		t.Context(), "retired-canonical",
	)
	require.ErrorIs(err, store.ErrPersonNotFound)

	envelope := parseStoreEnvelope(t,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
		"book", "retired-canonical",
	)
	_, err = st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: envelope,
		},
	)
	require.NoError(err)
	records, err := st.GetVCardResourceEnvelopeByCanonicalUIDContext(
		t.Context(), person.VCardUID,
	)
	require.NoError(err)
	require.Len(records, 1)
	assert.Equal("retired-canonical", records[0].SourceResourceUID)
}

func TestVCardResourceReadRejectsCorruptHash(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "alice@example.com")
	envelope := parseStoreEnvelope(t,
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
		"book", "corrupt",
	)
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: person.ID, Envelope: envelope,
		},
	)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(),
		st.Rebind(`UPDATE vcard_resource_envelopes SET content_hash = 'bad' WHERE id = ?`),
		created.ID,
	)
	require.NoError(t, err)

	_, err = st.GetVCardResourceEnvelopeContext(
		t.Context(), "book", "corrupt",
	)
	require.ErrorIs(t, err, store.ErrVCardResourceInvalid)
}

func createEnvelopePerson(
	t *testing.T, st *store.Store, email string,
) *store.Person {
	t.Helper()
	participantID, err := st.EnsureParticipant(email, "Test User", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	return person
}

func parseStoreEnvelope(
	t *testing.T, raw []byte, sourceRef, sourceUID string,
) vcard.ResourceEnvelope {
	t.Helper()
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(t, err)
	envelope.SourceRef = sourceRef
	envelope.SourceResourceUID = sourceUID
	return envelope
}

func findStoreProperty(
	t *testing.T, properties []vcard.PropertyOccurrence, name string,
) vcard.PropertyOccurrence {
	t.Helper()
	for _, property := range properties {
		if property.Property.Name == name {
			return property
		}
	}
	require.FailNow(t, "property not found", name)
	return vcard.PropertyOccurrence{}
}

func replaceStoreFormattedName(
	t *testing.T,
	envelope vcard.ResourceEnvelope,
	value string,
) vcard.ResourceEnvelope {
	t.Helper()
	current := findStoreProperty(t, envelope.PropertyTree, "FN")
	replacement, err := vcard.NewProperty(
		current.Property.Group, current.Property.OriginalName, value,
	)
	require.NoError(t, err)
	merged, err := envelope.MergeProperties([]vcard.PropertyEdit{{
		Identity: current.Identity, Property: replacement,
	}})
	require.NoError(t, err)
	prepared, err := merged.PrepareCanonicalRender()
	require.NoError(t, err)
	return prepared
}

// TestVCardResourceWritesRetrySQLiteSnapshotContention pins the SQLite half of
// the write path's concurrency story. Envelope writes are deferred read-then-
// write transactions: by the time one asks for the WAL writer lock it already
// holds a read snapshot, so an unrelated write that committed in between makes
// SQLite fail it with SQLITE_BUSY at once instead of waiting on the busy
// handler. Without the contended-write retry roughly a tenth of these Puts
// surfaced "database is locked" to the caller under one concurrent writer.
func TestVCardResourceWritesRetrySQLiteSnapshotContention(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	created, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: person.ID,
		Envelope: parseStoreEnvelope(t,
			[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
			"book", "contended"),
	})
	require.NoError(err)

	// The writer pauses between transactions: the retry policy is bounded and
	// backs off, so it is built for a busy neighbour, not one that never lets
	// go of the file. Without the retry a single collision fails the Put, and
	// at this duty cycle two hundred Puts collide many times over.
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			_, _ = st.AddPersonCategoryContext(context.Background(), person.ID,
				store.PersonCategoryInput{
					OriginalValue: "contention",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-writerDone
	})

	revision := created.Revision
	for i := range 200 {
		body := fmt.Appendf(nil, "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice %d\r\nEND:VCARD\r\n", i)
		record, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
			PersonID: person.ID, ExpectedRevision: &revision,
			Envelope: parseStoreEnvelope(t, body, "book", "contended"),
		})
		require.NoError(err, "put %d must absorb SQLite snapshot contention", i)
		revision = record.Revision
	}
	rewritten, err := st.RewriteVCardResourceSourceUIDContext(
		ctx, "book", "contended", "contended-renamed", revision)
	require.NoError(err)
	require.Equal(revision+1, rewritten.Revision)
}

// TestVCardSemanticCommitAbsorbsSQLiteWriterContention pins the commit path,
// which TestVCardResourceWritesRetrySQLiteSnapshotContention does not reach.
// The commit transaction reads the whole snapshot before its first write, so
// on SQLite it must take the writer lock up front: a deferred transaction that
// asks for the lock after reading loses SQLITE_BUSY_SNAPSHOT to every
// unrelated writer that committed meanwhile, and a busy neighbour then starves
// it through the whole retry budget.
func TestVCardSemanticCommitAbsorbsSQLiteWriterContention(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	neighbour := createEnvelopePerson(t, st, "bob@example.com")
	created, err := st.PutVCardResourceEnvelopeContext(ctx, store.VCardResourceEnvelopeInput{
		PersonID: person.ID,
		Envelope: parseStoreEnvelope(t,
			[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
			"book", "commit-contended"),
	})
	require.NoError(err)

	// A neighbour that writes, lets go for a moment, and writes again.
	// Before the commit took the writer lock up front, this writer won
	// nearly every collision and the commit burned its whole retry budget
	// within the first few cycles.
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Microsecond):
			}
			_, _ = st.AddPersonNameContext(context.Background(), neighbour.ID,
				store.PersonNameInput{
					NameKind:  store.PersonNameFormatted,
					Formatted: new(fmt.Sprintf("Bob %d", i)),
					Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-writerDone
	})

	revision := created.Revision
	for i := range 50 {
		snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
		require.NoError(err)
		prepared := replaceStoreFormattedName(
			t, created.ResourceEnvelope, fmt.Sprintf("Alice %d", i))
		committed, err := st.CommitVCardResourceEnvelopeContext(
			ctx, "book", "commit-contended", revision, snapshot.Fingerprint, prepared)
		require.NoError(err, "commit %d must absorb an unrelated SQLite writer", i)
		revision = committed.Revision
	}
}
