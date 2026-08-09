package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestResolvePersonByVCardUIDMatchesExactlyAndDoesNotCreate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, _ := mustTwoPersons(t, f)
	alice, err := f.Store.GetPerson(aliceID)
	require.NoError(err)

	got, err := f.Store.ResolvePersonByVCardUIDContext(ctx, alice.VCardUID)
	require.NoError(err)
	assert.Equal(alice.ID, got.ID)
	for _, uid := range []string{"", "  ", strings.ToUpper(alice.VCardUID), alice.VCardUID + " ", "urn:uuid:" + alice.VCardUID, "no-such-uid"} {
		_, err := f.Store.ResolvePersonByVCardUIDContext(ctx, uid)
		require.ErrorIsf(err, store.ErrPersonNotFound, "UID %q must compare exactly", uid)
	}
	persons, err := f.Store.ListPersons()
	require.NoError(err)
	assert.Len(persons, 2)
}

func TestResolveRelatedValueLinksExactUIDAndReturnsExactCanonicalDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, bobID := mustTwoPersons(t, f)
	bob, err := f.Store.GetPerson(bobID)
	require.NoError(err)
	ref := "carddav-resource-1"
	in := store.RelatedImport{PersonID: aliceID, RawValue: bob.VCardUID, RawType: "agent", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, SourceRef: &ref, Actor: "system"}
	first, err := f.Store.ResolveRelatedValueContext(ctx, in)
	require.NoError(err)
	require.NotNil(first.Relationship)
	assert.Equal("agent", first.Relationship.TypeSlug)
	assert.Equal(aliceID, first.Relationship.SourcePersonID)
	assert.Equal(bobID, first.Relationship.TargetPersonID)

	// agent is asymmetric, so its reciprocal edge is distinct. Re-importing
	// alice's assertion must find the first exact canonical triple, not search
	// both orientations and accidentally return Bob's reciprocal assertion.
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{SourcePersonID: bobID, TargetPersonID: aliceID, TypeSlug: "agent", Source: store.ProvenanceUser, Actor: "user"})
	require.NoError(err)
	again, err := f.Store.ResolveRelatedValueContext(ctx, in)
	require.NoError(err)
	require.NotNil(again.Relationship)
	assert.Equal(first.Relationship.ID, again.Relationship.ID)
}

func TestResolveRelatedValueStagesUnresolvedAndSelfUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, bobID := mustTwoPersons(t, f)
	bob, err := f.Store.GetPerson(bobID)
	require.NoError(err)
	alice, err := f.Store.GetPerson(aliceID)
	require.NoError(err)

	for _, in := range []store.RelatedImport{
		{PersonID: aliceID, RawValue: "mailto:bob@example.com", RawType: "friend", ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: "tel:+12025550123", RawType: "spouse", ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: "Bob from the gym", RawType: "friend", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: bob.VCardUID, RawType: "work", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: alice.VCardUID, RawType: "friend", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"},
	} {
		resolution, resolveErr := f.Store.ResolveRelatedValueContext(ctx, in)
		require.NoError(resolveErr)
		require.NotNil(resolution.Review)
		assert.Nil(resolution.Relationship)
		if in.RawValue == bob.VCardUID {
			require.NotNil(resolution.Review.MatchedPersonID)
			assert.Equal(bobID, *resolution.Review.MatchedPersonID)
		}
		if in.RawValue == alice.VCardUID {
			assert.Nil(resolution.Review.MatchedPersonID, "self UID must not violate the review self-match constraint")
		}
	}
	views, err := f.Store.ListPersonRelationshipsContext(ctx, aliceID, store.PersonRelationshipListOptions{})
	require.NoError(err)
	assert.Empty(views)
}

func TestRelationshipReviewDeduplicatesExactPropertyOccurrencesOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, _ := mustTwoPersons(t, f)
	sourceRef := "  resource-1.vcf  "
	group := "item1"
	propID := "property-1"
	altID := "alternative-1"
	base := store.RelatedImport{
		PersonID: aliceID, RawValue: "Bob from the gym", RawType: "friend",
		ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport,
		SourceRef: &sourceRef,
		VCardIdentity: store.VCardIdentity{
			Property: "RELATED", Group: &group, PropID: &propID,
			PID: []string{"1.1", "2.1"}, AltID: &altID,
		},
		Actor: "system",
	}

	first, err := f.Store.ResolveRelatedValueContext(ctx, base)
	require.NoError(err)
	require.NotNil(first.Review)
	assert.Equal("resource-1.vcf", *first.Review.SourceRef)
	assert.Equal(base.VCardIdentity, first.Review.VCardIdentity)

	secondRef := "resource-2.vcf"
	secondGroup := "item2"
	secondPropID := "property-2"
	secondAltID := "alternative-2"
	secondInput := base
	secondInput.SourceRef = &secondRef
	secondInput.VCardIdentity = store.VCardIdentity{
		Property: "X-RELATED", Group: &secondGroup, PropID: &secondPropID,
		PID: []string{"3.1", "4.1"}, AltID: &secondAltID,
	}
	second, err := f.Store.ResolveRelatedValueContext(ctx, secondInput)
	require.NoError(err)
	require.NotNil(second.Review)
	assert.NotEqual(first.Review.ID, second.Review.ID)
	assert.Equal(secondRef, *second.Review.SourceRef)
	assert.Equal(secondInput.VCardIdentity, second.Review.VCardIdentity)

	normalizedSourceRef := "resource-1.vcf"
	repeatedInput := base
	repeatedInput.SourceRef = &normalizedSourceRef
	repeated, err := f.Store.ResolveRelatedValueContext(ctx, repeatedInput)
	require.NoError(err)
	require.NotNil(repeated.Review)
	assert.Equal(first.Review.ID, repeated.Review.ID)

	reviews, err := f.Store.ListRelationshipReviewsContext(ctx, store.RelationshipReviewListOptions{PersonID: aliceID})
	require.NoError(err)
	assert.Len(reviews, 2)
}

func TestResolveRelatedValueAcceptsUrnUUIDCaseInsensitively(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, bobID := mustTwoPersons(t, f)
	bob, err := f.Store.GetPerson(bobID)
	require.NoError(err)
	resolution, err := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{PersonID: aliceID, RawValue: "URN:UUID:" + strings.ToUpper(bob.VCardUID), RawType: "sibling", ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceVCardImport, Actor: "system"})
	require.NoError(err)
	require.NotNil(resolution.Relationship)

	for _, rawValue := range []string{
		"urn:uuid:{" + strings.ToUpper(bob.VCardUID) + "}",
		"urn:uuid:" + strings.ReplaceAll(strings.ToUpper(bob.VCardUID), "-", ""),
	} {
		staged, stageErr := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{
			PersonID: aliceID, RawValue: rawValue, RawType: "friend",
			ValueKind: store.RelatedValueKindURI, Source: store.ProvenanceVCardImport, Actor: "system",
		})
		require.NoError(stageErr)
		require.NotNil(staged.Review, "only canonical hyphenated UUID URNs may auto-match")
		assert.Nil(staged.Relationship)
		assert.Nil(staged.Review.MatchedPersonID)
	}
}

func TestRelationshipReviewPreservesIdentityAndAcceptsAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, bobID := mustTwoPersons(t, f)
	identity := store.VCardIdentity{Property: "RELATED", Group: new("item1"), PropID: new("p3"), PID: []string{"1.1", "2.1"}, AltID: new("a2")}
	staged, err := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{PersonID: aliceID, RawValue: "Bob from the gym", RawType: "arch-nemesis", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceCardDAVImport, VCardIdentity: identity, Actor: "system"})
	require.NoError(err)
	require.NotNil(staged.Review)
	assert.Equal(identity, staged.Review.VCardIdentity)

	edge, err := f.Store.AcceptRelationshipReviewContext(ctx, staged.Review.ID, "acquaintance", bobID, "user")
	require.NoError(err)
	assert.Equal(identity, edge.VCardIdentity)
	reloaded, err := f.Store.GetPersonRelationshipContext(ctx, edge.ID)
	require.NoError(err)
	assert.Equal(identity, reloaded.VCardIdentity)

	_, err = f.Store.AcceptRelationshipReviewContext(ctx, staged.Review.ID, "acquaintance", bobID, "user")
	require.ErrorIs(err, store.ErrRelationshipReviewNotPending)
}

func TestRelationshipReviewAcceptRollbackAndRejectDurability(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, bobID := mustTwoPersons(t, f)
	bob, err := f.Store.GetPerson(bobID)
	require.NoError(err)
	in := store.RelatedImport{PersonID: aliceID, RawValue: bob.VCardUID, RawType: "unknown", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"}
	staged, err := f.Store.ResolveRelatedValueContext(ctx, in)
	require.NoError(err)
	require.NotNil(staged.Review)
	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{SourcePersonID: aliceID, TargetPersonID: bobID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "user"})
	require.NoError(err)
	_, err = f.Store.AcceptRelationshipReviewContext(ctx, staged.Review.ID, "friend", bobID, "user")
	require.ErrorIs(err, store.ErrPersonRelationshipDuplicate)
	reviews, err := f.Store.ListRelationshipReviewsContext(ctx, store.RelationshipReviewListOptions{})
	require.NoError(err)
	require.Len(reviews, 1)
	assert.Equal(store.RelationshipReviewPending, reviews[0].Status, "a failed edge write must roll back the claimed review")

	rejected, err := f.Store.RejectRelationshipReviewContext(ctx, staged.Review.ID, "user")
	require.NoError(err)
	assert.Equal(store.RelationshipReviewRejected, rejected.Status)
	again, err := f.Store.ResolveRelatedValueContext(ctx, in)
	require.NoError(err)
	require.NotNil(again.Review)
	assert.Equal(staged.Review.ID, again.Review.ID)
	assert.Equal(store.RelationshipReviewRejected, again.Status())
	_, err = f.Store.RejectRelationshipReviewContext(ctx, staged.Review.ID, "user")
	require.ErrorIs(err, store.ErrRelationshipReviewNotPending)
}

func TestRelationshipReviewConcurrentAcceptAndRejectHaveOneWinner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, bobID := mustTwoPersons(t, f)
	staged, err := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: aliceID, RawValue: "Bob from the gym", RawType: "unknown",
		ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system",
	})
	require.NoError(err)
	require.NotNil(staged.Review)

	type outcome struct {
		operation string
		err       error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, acceptErr := f.Store.AcceptRelationshipReviewContext(ctx, staged.Review.ID, "friend", bobID, "user")
		results <- outcome{operation: "accept", err: acceptErr}
	}()
	go func() {
		defer wg.Done()
		<-start
		_, rejectErr := f.Store.RejectRelationshipReviewContext(ctx, staged.Review.ID, "user")
		results <- outcome{operation: "reject", err: rejectErr}
	}()
	close(start)
	wg.Wait()
	close(results)

	var successes, notPending int
	for result := range results {
		if result.err == nil {
			successes++
			continue
		}
		require.ErrorIsf(result.err, store.ErrRelationshipReviewNotPending, "%s loses only because the review is no longer pending", result.operation)
		notPending++
	}
	assert.Equal(1, successes)
	assert.Equal(1, notPending)
	reviews, err := f.Store.ListRelationshipReviewsContext(ctx, store.RelationshipReviewListOptions{})
	require.NoError(err)
	require.Len(reviews, 1)
	if reviews[0].Status == store.RelationshipReviewAccepted {
		require.NotNil(reviews[0].AcceptedRelationshipID)
		_, err = f.Store.GetPersonRelationshipContext(ctx, *reviews[0].AcceptedRelationshipID)
		require.NoError(err)
		return
	}
	assert.Equal(store.RelationshipReviewRejected, reviews[0].Status)
	assert.Nil(reviews[0].AcceptedRelationshipID)
}

func TestResolveRelatedValueRejectsUnsplitTypeListsAndInvalidValues(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, _ := mustTwoPersons(t, f)
	for _, in := range []store.RelatedImport{
		{PersonID: aliceID, RawValue: "someone", RawType: "friend,co-worker", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: strings.Repeat("x", 1025), RawType: "friend", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: "  ", RawType: "friend", ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system"},
		{PersonID: aliceID, RawValue: "someone", RawType: "friend", ValueKind: store.RelatedValueKind("binary"), Source: store.ProvenanceVCardImport, Actor: "system"},
	} {
		_, err := f.Store.ResolveRelatedValueContext(ctx, in)
		require.ErrorIs(err, store.ErrRelationshipReviewInvalid)
	}
}

func TestRelationshipReviewRejectsOversizedActorsWithoutStateChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := context.Background()
	aliceID, _ := mustTwoPersons(t, f)
	invalidActors := []string{strings.Repeat("x", 129), "  "}
	for _, invalidActor := range invalidActors {
		_, err := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{
			PersonID: aliceID, RawValue: "Bob from the gym", RawType: "friend",
			ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: invalidActor,
		})
		require.ErrorIs(err, store.ErrRelationshipReviewInvalid)
	}
	reviews, err := f.Store.ListRelationshipReviewsContext(ctx, store.RelationshipReviewListOptions{})
	require.NoError(err)
	assert.Empty(reviews, "a rejected resolve input must not stage a row")

	staged, err := f.Store.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: aliceID, RawValue: "Bob from the gym", RawType: "friend",
		ValueKind: store.RelatedValueKindText, Source: store.ProvenanceVCardImport, Actor: "system",
	})
	require.NoError(err)
	require.NotNil(staged.Review)
	for _, invalidActor := range invalidActors {
		_, err = f.Store.RejectRelationshipReviewContext(ctx, staged.Review.ID, invalidActor)
		require.ErrorIs(err, store.ErrRelationshipReviewInvalid)
	}
	reviews, err = f.Store.ListRelationshipReviewsContext(ctx, store.RelationshipReviewListOptions{})
	require.NoError(err)
	require.Len(reviews, 1)
	assert.Equal(store.RelationshipReviewPending, reviews[0].Status)
	assert.Nil(reviews[0].ReviewedBy)
	assert.Nil(reviews[0].ReviewedAt)
}

func TestRelationshipReviewDatabaseConstraintsAndColumns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	aliceID, bobID := mustTwoPersons(t, f)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO person_relationship_reviews (person_id, raw_related_value, raw_related_type, value_kind, source) VALUES (?, 'bad-source', 'friend', 'text', 'guess')`), aliceID)
	require.Error(err, "the review source vocabulary must be enforced by the live database")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO person_relationship_reviews (person_id, raw_related_value, raw_related_type, value_kind, matched_person_id, source) VALUES (?, 'self', 'friend', 'text', ?, 'system')`), aliceID, aliceID)
	require.Error(err, "self matches are structurally forbidden")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO person_relationship_reviews (person_id, raw_related_value, raw_related_type, value_kind, matched_person_id, source) VALUES (?, 'valid', 'friend', 'text', ?, 'system')`), aliceID, bobID)
	require.NoError(err)

	rows, err := f.Store.DB().Query(`SELECT * FROM person_relationship_reviews WHERE 1 = 0`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	columns, err := rows.Columns()
	require.NoError(err)
	require.NoError(rows.Err())
	assert.ElementsMatch([]string{"id", "person_id", "raw_related_value", "raw_related_type", "value_kind", "matched_person_id", "status", "accepted_relationship_id", "source", "source_ref", "vcard_property", "vcard_group", "vcard_prop_id", "vcard_pid", "vcard_altid", "created_by", "reviewed_by", "reviewed_at", "created_at", "updated_at"}, columns)
}
