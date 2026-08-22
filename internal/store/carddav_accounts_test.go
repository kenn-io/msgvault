package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func cardDAVRoleDiscovery() store.CardDAVDiscoveryInput {
	allowed := true
	return store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{
			{
				CanonicalURL: "https://contacts.example/books/alice/personal/",
				DisplayName:  "Personal", DiscoveryIndex: 0, CanCreate: &allowed,
			},
			{
				CanonicalURL:      "https://contacts.example/books/alice/directory/",
				DiscoveryAliasURL: "https://contacts.example/books/alice/directory-alias/",
				DisplayName:       "Directory", DiscoveryIndex: 1, CanCreate: &allowed,
			},
		},
	}
}

func TestCardDAVDiscoveryPersistsEveryHomeURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := cardDAVRoleDiscovery()
	input.HomeURLs = []string{
		"https://contacts.example/books/alice/",
		"https://contacts.example/books/team/",
	}
	account, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(input.HomeURLs, account.HomeURLs)

	stored, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	assert.Equal(input.HomeURLs, stored.HomeURLs)
}

func importCardDAVRoleTestPeople(
	t *testing.T, slugs ...string,
) (*store.Store, store.CardDAVAddressBook, map[string]store.CardDAVResource) {
	t.Helper()
	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(t, err)
	require.Len(t, books, 2)
	book := books[1]
	require.NoError(t, st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: true,
	}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(t, err)
	book = books[1]
	resources := make([]store.CardDAVRemoteResource, 0, len(slugs))
	for _, slug := range slugs {
		resources = append(resources, remoteResource(
			book.CanonicalURL+slug+".vcf", slug, slug, slug+"@example.test", `"one"`,
		))
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: resources,
	})
	require.NoError(t, err)
	mappings, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(t, err)
	require.Len(t, mappings, len(slugs))
	bySlug := make(map[string]store.CardDAVResource, len(mappings))
	for _, mapping := range mappings {
		require.NotNil(t, mapping.PersonID)
		for _, slug := range slugs {
			if mapping.Href == book.CanonicalURL+slug+".vcf" {
				bySlug[slug] = mapping
				break
			}
		}
	}
	require.Len(t, bySlug, len(slugs))
	return st, book, bySlug
}

func TestCardDAVSetBookRolesAtomicallySwapsWriteTargetAndSchedulesWidening(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.Len(books, 2)
	oldRevision := books[1].SyncRevision

	err = st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
	})
	require.NoError(err)

	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.False(books[0].IsWriteTarget)
	assert.True(books[0].IsSubscribed, "the old target remains materialized")
	assert.True(books[1].IsWriteTarget)
	assert.True(books[1].IsSubscribed)
	assert.True(books[1].NeedsFullReconcile)
	assert.Greater(books[1].SyncRevision, oldRevision)

	var targets int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM carddav_address_books WHERE is_write_target = TRUE`).Scan(&targets)
	require.NoError(err)
	assert.Equal(1, targets)
}

func TestCardDAVSetBookRolesRefusesUnsubscribingWriteTarget(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)

	err = st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: false, IsLookupSource: true,
	})
	require.ErrorIs(err, store.ErrCardDAVWriteTargetSubscribed)

	after, listErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(listErr)
	assert.Equal(t, books, after)
}

func TestCardDAVSetBookRolesClearsWriteTargetAndSubscriptionTogether(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.True(books[0].IsWriteTarget)
	require.True(books[0].IsSubscribed)

	require.NoError(st.SetCardDAVBookRolesContext(
		t.Context(), books[0].ID, store.CardDAVBookRoles{},
	))
	after, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.False(after[0].IsWriteTarget)
	assert.False(after[0].IsSubscribed)
	assert.False(after[0].IsLookupSource)

	var targets int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM carddav_address_books WHERE is_write_target = TRUE`,
	).Scan(&targets))
	assert.Zero(targets)
}

func TestCardDAVSetBookRolesPreservesScheduledReconcileAcrossNarrowing(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: true,
	}))

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: false,
	}))
	after, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.True(t, after[1].NeedsFullReconcile,
		"a later narrow role edit must not cancel the pending materializing pull")
}

func TestCardDAVSetBookRolesRejectsLifecycleDeniedWriteTarget(t *testing.T) {
	for _, capability := range []string{"create", "update", "delete"} {
		t.Run(capability, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			input := cardDAVRoleDiscovery()
			denied := false
			switch capability {
			case "create":
				input.Books[1].CanCreate = &denied
			case "update":
				input.Books[1].CanUpdate = &denied
			case "delete":
				input.Books[1].CanDelete = &denied
			}
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)

			err = st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
				IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
			})
			require.ErrorIs(err, store.ErrCardDAVReadOnlyAddressBook)
			after, listErr := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(listErr)
			assert.True(after[0].IsWriteTarget)
			assert.False(after[1].IsWriteTarget)
		})
	}
}

func TestCardDAVDiscoverySkipsLifecycleDeniedAutomaticWriteTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	input := cardDAVRoleDiscovery()
	denied, allowed := false, true
	input.Books[0].CanUpdate = &denied
	input.Books[1].CanUpdate = &allowed
	input.Books[1].CanDelete = &allowed

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.Len(books, 2)
	assert.False(books[0].IsWriteTarget)
	assert.True(books[1].IsWriteTarget)
}

func TestCardDAVSetBookRolesUnsubscribeDeletesOnlyUntouchedImportedPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.Len(books, 2)
	book := books[1]
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: true,
	}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	book = books[1]

	resources := []store.CardDAVRemoteResource{
		remoteResource(book.CanonicalURL+"untouched.vcf", "untouched", "Untouched", "untouched@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"edited.vcf", "edited", "Edited", "edited@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"linked.vcf", "linked", "Linked", "linked@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"relationship-source.vcf", "relationship-source", "Relationship Source", "relationship-source@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"relationship-target.vcf", "relationship-target", "Relationship Target", "relationship-target@example.test", `"one"`),
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: resources,
	})
	require.NoError(err)
	mappings, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	require.Len(mappings, 5)
	personByHref := make(map[string]int64, len(mappings))
	for _, mapping := range mappings {
		require.NotNil(mapping.PersonID)
		personByHref[mapping.Href] = *mapping.PersonID
	}

	edited, err := st.GetPersonContext(t.Context(), personByHref[resources[1].Href])
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), edited.ID, edited.Revision, new("User edited"))
	require.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier("email", "linked@example.test", "Linked")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO person_participants (person_id, participant_id) VALUES (?, ?)`),
		personByHref[resources[2].Href], participantID)
	require.NoError(err)
	_, err = st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
		SourcePersonID: personByHref[resources[3].Href],
		TargetPersonID: personByHref[resources[4].Href],
		TypeSlug:       "agent",
		Source:         store.ProvenanceUser,
		Actor:          "user",
	})
	require.NoError(err)

	err = st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: false, IsLookupSource: true,
	})
	require.NoError(err)

	_, err = st.GetPersonContext(t.Context(), personByHref[resources[0].Href])
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.GetPersonContext(t.Context(), personByHref[resources[1].Href])
	require.NoError(err, "user-edited imported person must survive")
	_, err = st.GetPersonContext(t.Context(), personByHref[resources[2].Href])
	require.NoError(err, "participant-linked imported person must survive")
	_, err = st.GetPersonContext(t.Context(), personByHref[resources[3].Href])
	require.NoError(err, "relationship source imported person must survive")
	_, err = st.GetPersonContext(t.Context(), personByHref[resources[4].Href])
	require.NoError(err, "relationship target imported person must survive")

	mappings, err = st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	require.Len(mappings, 5)
	for _, mapping := range mappings {
		assert.Nil(mapping.PersonID)
		assert.Nil(mapping.PersonRevisionAtBind)
		assert.Equal(store.CardDAVMappingUnbound, mapping.MappingStatus)
		assert.Equal(store.CardDAVGovernanceNone, mapping.Governance)
	}
}

func TestCardDAVSetBookRolesUnsubscribePreservesUserLinkedImports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.Len(books, 2)
	book := books[1]
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: true,
	}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	book = books[1]

	resources := []store.CardDAVRemoteResource{
		remoteResource(book.CanonicalURL+"untouched.vcf", "untouched", "Untouched", "untouched@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"user-employment.vcf", "user-employment", "User Employment", "user-employment@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"remote-employment.vcf", "remote-employment", "Remote Employment", "remote-employment@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"daily-note.vcf", "daily-note", "Daily Note", "daily-note@example.test", `"one"`),
		remoteResource(book.CanonicalURL+"user-attribute.vcf", "user-attribute", "User Attribute", "user-attribute@example.test", `"one"`),
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: resources,
	})
	require.NoError(err)
	mappings, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	require.Len(mappings, 5)
	personByHref := make(map[string]int64, len(mappings))
	revisionAtBind := make(map[int64]int64, len(mappings))
	for _, mapping := range mappings {
		require.NotNil(mapping.PersonID)
		require.NotNil(mapping.PersonRevisionAtBind)
		personByHref[mapping.Href] = *mapping.PersonID
		revisionAtBind[*mapping.PersonID] = *mapping.PersonRevisionAtBind
	}

	organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	userEmploymentPersonID := personByHref[resources[1].Href]
	_, err = st.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: userEmploymentPersonID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	remoteEmploymentPersonID := personByHref[resources[2].Href]
	_, err = st.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: remoteEmploymentPersonID, OrganizationID: organization.ID,
		Title: new("Imported Role"), Source: store.ProvenanceCardDAVImport,
	})
	require.NoError(err)
	dailyNotePersonID := personByHref[resources[3].Href]
	note, err := st.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{
		LocalDate: "2026-08-19", Body: "Follow up", Author: "user",
		PersonIDs: []int64{dailyNotePersonID},
	})
	require.NoError(err)
	definition := personTextDefinition("carddav_preserved_note")
	definition.UniversalID = "test-carddav-preserved-note"
	_, err = st.CreateAttributeDefinitionContext(t.Context(), definition)
	require.NoError(err)
	userAttributePersonID := personByHref[resources[4].Href]
	attributeWrite, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: userAttributePersonID, DefinitionSlug: definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueText, Text: new("Remember this"),
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	for _, personID := range []int64{
		userEmploymentPersonID, remoteEmploymentPersonID, dailyNotePersonID, userAttributePersonID,
	} {
		person, getErr := st.GetPersonContext(t.Context(), personID)
		require.NoError(getErr)
		assert.Equal(revisionAtBind[personID], person.Revision,
			"linkage must exercise the non-person-revision deletion guards")
	}

	err = st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: false, IsLookupSource: true,
	})
	require.NoError(err)

	_, err = st.GetPersonContext(t.Context(), personByHref[resources[0].Href])
	require.ErrorIs(err, store.ErrPersonNotFound, "untouched import remains the deletion control")
	_, err = st.GetPersonContext(t.Context(), userEmploymentPersonID)
	require.NoError(err, "user employment must preserve its imported person")
	_, err = st.GetPersonContext(t.Context(), remoteEmploymentPersonID)
	require.ErrorIs(err, store.ErrPersonNotFound,
		"non-user employment must not turn a remote-only person into user-owned state")
	_, err = st.GetPersonContext(t.Context(), dailyNotePersonID)
	require.NoError(err, "daily-note target must preserve its imported person")
	_, err = st.GetPersonContext(t.Context(), userAttributePersonID)
	require.NoError(err, "user attribute must preserve its imported person")

	userEmployments, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{
		PersonID: userEmploymentPersonID,
	})
	require.NoError(err)
	assert.Len(userEmployments, 1)
	remoteEmployments, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{
		PersonID: remoteEmploymentPersonID,
	})
	require.NoError(err)
	assert.Empty(remoteEmployments)
	notes, err := st.ListDailyNoteEntriesForPersonContext(
		t.Context(), dailyNotePersonID, "2026-08-19", 10, 0,
	)
	require.NoError(err)
	require.Len(notes, 1)
	assert.Equal(note.ID, notes[0].ID)
	assert.Equal([]int64{dailyNotePersonID}, notes[0].PersonIDs)
	attributes, err := st.ListPersonAttributeValuesContext(
		t.Context(), userAttributePersonID,
		store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true},
	)
	require.NoError(err)
	require.Len(attributes, 1)
	assert.Equal(attributeWrite.Value.ID, attributes[0].ID)
}

func TestCardDAVSetBookRolesUnsubscribePreservesReviewedRelationships(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, book, mappings := importCardDAVRoleTestPeople(t, "accepted", "rejected", "pending")
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "relationship-counterpart@example.test", "Relationship Counterpart",
	)
	require.NoError(err)
	counterpart, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)

	stage := func(slug string) *store.RelationshipReview {
		t.Helper()
		mapping := mappings[slug]
		resolved, resolveErr := st.ResolveRelatedValueContext(t.Context(), store.RelatedImport{
			PersonID: *mapping.PersonID, RawValue: "Review " + slug,
			RawType: "unknown", ValueKind: store.RelatedValueKindText,
			Source: store.ProvenanceCardDAVImport, Actor: "system",
			VCardIdentity: store.VCardIdentity{Property: "RELATED"},
		})
		require.NoError(resolveErr)
		require.NotNil(resolved.Review)
		return resolved.Review
	}
	acceptedReview := stage("accepted")
	acceptedEdge, err := st.AcceptRelationshipReviewContext(
		t.Context(), acceptedReview.ID, "friend", counterpart.ID, "user",
	)
	require.NoError(err)
	assert.Equal(store.ProvenanceCardDAVImport, acceptedEdge.Source)
	rejectedReview := stage("rejected")
	rejectedReview, err = st.RejectRelationshipReviewContext(t.Context(), rejectedReview.ID, "user")
	require.NoError(err)
	require.NotNil(rejectedReview.ReviewedBy)
	require.NotNil(rejectedReview.ReviewedAt)
	pendingReview := stage("pending")

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: false, IsLookupSource: true,
	}))

	_, err = st.GetPersonContext(t.Context(), *mappings["accepted"].PersonID)
	require.NoError(err, "accepted imported relationship review must preserve its person")
	_, err = st.GetPersonContext(t.Context(), *mappings["rejected"].PersonID)
	require.NoError(err, "rejected imported relationship review must preserve its person")
	_, err = st.GetPersonContext(t.Context(), *mappings["pending"].PersonID)
	require.ErrorIs(err, store.ErrPersonNotFound,
		"an untouched pending imported review remains remote-only state")
	_, err = st.GetPersonRelationshipContext(t.Context(), acceptedEdge.ID)
	require.NoError(err, "the accepted imported edge must survive with its person")
	reviews, err := st.ListRelationshipReviewsContext(
		t.Context(), store.RelationshipReviewListOptions{},
	)
	require.NoError(err)
	statusByID := make(map[int64]store.RelationshipReviewStatus, len(reviews))
	for _, review := range reviews {
		statusByID[review.ID] = review.Status
	}
	assert.Equal(store.RelationshipReviewAccepted, statusByID[acceptedReview.ID])
	assert.Equal(store.RelationshipReviewRejected, statusByID[rejectedReview.ID])
	_, pendingExists := statusByID[pendingReview.ID]
	assert.False(pendingExists)
}

func TestCardDAVSetBookRolesUnsubscribePreservesReviewedIdentityMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, book, mappings := importCardDAVRoleTestPeople(
		t, "accepted", "rejected", "user-source", "user-evidence", "system-control",
	)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "identity-counterpart@example.test", "Identity Counterpart",
	)
	require.NoError(err)
	counterpart, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)

	personCandidate := func(
		slug string, source store.Provenance, importedOnRight bool,
	) *store.IdentityMatchCandidate {
		t.Helper()
		leftID, rightID := *mappings[slug].PersonID, counterpart.ID
		if importedOnRight {
			leftID, rightID = rightID, leftID
		}
		candidate, created, candidateErr := st.UpsertIdentityMatchCandidateContext(
			t.Context(), store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchPerson, LeftID: leftID,
				RightKind: store.IdentityMatchPerson, RightID: rightID,
				Basis: store.IdentityMatchDisplayName, NormalizedValue: new(slug),
				State: store.IdentityMatchStateCandidate, Source: source,
			})
		require.NoError(candidateErr)
		assert.True(created)
		return candidate
	}
	accepted := personCandidate("accepted", store.ProvenanceArchiveObservation, false)
	accepted, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), accepted.ID, store.IdentityMatchStateAccepted, "user", new("confirmed"),
	)
	require.NoError(err)
	rejected := personCandidate("rejected", store.ProvenanceArchiveObservation, true)
	rejected, err = st.DecideIdentityMatchCandidateContext(
		t.Context(), rejected.ID, store.IdentityMatchStateRejected, "user", new("not the same"),
	)
	require.NoError(err)
	userSource := personCandidate("user-source", store.ProvenanceUser, false)

	points, err := st.ListPersonContactPointsContext(
		t.Context(), *mappings["user-evidence"].PersonID, true,
	)
	require.NoError(err)
	require.NotEmpty(points)
	userEvidence, created, err := st.UpsertIdentityMatchCandidateContext(
		t.Context(), store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchPerson, LeftID: counterpart.ID,
			RightKind: store.IdentityMatchContactPoint, RightID: points[0].Envelope.ID,
			Basis: store.IdentityMatchEmail, NormalizedValue: new("user-evidence@example.test"),
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
		})
	require.NoError(err)
	assert.True(created)
	_, err = st.AddIdentityMatchEvidenceContext(
		t.Context(), userEvidence.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: "manual confirmation", Source: store.ProvenanceUser,
		},
	)
	require.NoError(err)
	systemControl := personCandidate("system-control", store.ProvenanceArchiveObservation, false)
	_, err = st.AddIdentityMatchEvidenceContext(
		t.Context(), systemControl.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: "generated similarity", Source: store.ProvenanceArchiveObservation,
		},
	)
	require.NoError(err)

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: false, IsLookupSource: true,
	}))

	for _, slug := range []string{"accepted", "rejected", "user-source", "user-evidence"} {
		_, getErr := st.GetPersonContext(t.Context(), *mappings[slug].PersonID)
		require.NoError(getErr, "%s identity review must preserve its person", slug)
	}
	_, err = st.GetPersonContext(t.Context(), *mappings["system-control"].PersonID)
	require.ErrorIs(err, store.ErrPersonNotFound,
		"pure generated candidate and evidence remain deletable")
	for _, candidate := range []*store.IdentityMatchCandidate{accepted, rejected, userSource, userEvidence} {
		_, getErr := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
		require.NoError(getErr, "reviewed identity candidate %d must survive", candidate.ID)
	}
	_, err = st.GetIdentityMatchCandidateContext(t.Context(), systemControl.ID)
	require.ErrorIs(err, store.ErrIdentityMatchNotFound)
}

func TestCardDAVIgnoredBookDropsLedgerOnceAndSurvivesAliasRediscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	book := books[1]
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsSubscribed: true, IsLookupSource: true,
	}))
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	book = books[1]
	input := remoteResource(book.CanonicalURL+"ignored.vcf", "ignored", "Ignored", "ignored@example.test", `"one"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{}))
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	ignoredBooks, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	ignoredBook := ignoredBooks[1]
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: ignoredBook.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: ignoredBook.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.ErrorIs(err, store.ErrCardDAVStalePlan,
		"an ignored book must reject even a revision-current apply plan")
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{}),
		"reapplying ignored roles must be idempotent")

	rediscovered := cardDAVRoleDiscovery()
	rediscovered.Books[1].CanonicalURL = rediscovered.Books[1].DiscoveryAliasURL
	rediscovered.Books[1].DiscoveryAliasURL = ""
	_, after, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), rediscovered)
	require.NoError(err)
	require.Len(after, 2)
	assert.Equal(book.ID, after[1].ID)
	assert.False(after[1].IsWriteTarget)
	assert.False(after[1].IsSubscribed)
	assert.False(after[1].IsLookupSource)
	assert.False(after[1].NeedsFullReconcile)
	ledger, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	assert.Empty(ledger)
}

func TestCardDAVIgnoredBookIdentitySurvivesTemporaryDiscoveryAbsence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	initial := cardDAVRoleDiscovery()
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), initial)
	require.NoError(err)
	require.Len(books, 2)
	ignored := books[1]
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), ignored.ID, store.CardDAVBookRoles{}))

	absent := initial
	absent.Books = initial.Books[:1]
	_, afterAbsence, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), absent)
	require.NoError(err)
	require.Len(afterAbsence, 2)
	assert.Equal(ignored.ID, afterAbsence[1].ID)
	assert.False(afterAbsence[1].IsWriteTarget)
	assert.False(afterAbsence[1].IsSubscribed)
	assert.False(afterAbsence[1].IsLookupSource)

	rediscovered := initial
	rediscovered.Books[1].CanonicalURL = initial.Books[1].DiscoveryAliasURL
	rediscovered.Books[1].DiscoveryAliasURL = ""
	_, afterRediscovery, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), rediscovered)
	require.NoError(err)
	require.Len(afterRediscovery, 2)
	assert.Equal(ignored.ID, afterRediscovery[1].ID)
	assert.False(afterRediscovery[1].IsWriteTarget)
	assert.False(afterRediscovery[1].IsSubscribed)
	assert.False(afterRediscovery[1].IsLookupSource)
}

func TestCardDAVDiscoveryPruneUsesGovernanceAwareImportedCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"removed.vcf", "removed", "Removed", "removed@example.test", `"one"`)
	preserved := remoteResource(book.CanonicalURL+"preserved.vcf", "preserved", "Preserved", "preserved@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input, preserved},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	preservedMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, preserved.Href)
	require.NoError(err)
	require.NotNil(preservedMapping.PersonID)
	preservedPersonID := *preservedMapping.PersonID
	_, err = st.AddPersonContactPointContext(t.Context(), preservedPersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "user-owned@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
	})
	require.NoError(err)
	assert.Empty(books)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", input.Href)
	require.ErrorIs(err, store.ErrVCardResourceNotFound)
	preservedPerson, err := st.GetPersonContext(t.Context(), preservedPersonID)
	require.NoError(err, "user-owned state must keep the person while pruning its remote book")
	require.NotNil(preservedPerson.DisplayName)
	assert.Equal(preserved.DisplayName, *preservedPerson.DisplayName)
	points, err := st.ListPersonContactPointsContext(t.Context(), preservedPersonID, true)
	require.NoError(err)
	assert.Contains(contactOriginalValues(points), "user-owned@example.test")
	assert.Contains(contactOriginalValues(points), preserved.Emails[0])
	names, err := st.ListPersonNamesContext(t.Context(), preservedPersonID, true)
	require.NoError(err)
	require.Len(names, 1)
	assert.Equal(preserved.DisplayName, names[0].OriginalValue)
	_, err = st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", preserved.Href)
	require.ErrorIs(err, store.ErrVCardResourceNotFound)
}

func contactOriginalValues(points []store.PersonContactPoint) []string {
	values := make([]string, 0, len(points))
	for _, point := range points {
		values = append(values, point.OriginalValue)
	}
	return values
}

func TestCardDAVDiscoveryRetainsBookWithPendingPublicationIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('pending-person', 'Pending Person') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID,
		Href:                 book.CanonicalURL + "pending-person.vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:pending-person\r\nFN:Pending Person\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "semantic-pending", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	require.NotEmpty(pending.PendingOperation)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(account)

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
	})
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(book.ID, books[0].ID)
	after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(pending.PendingOperation, after.PendingOperation)
}

func TestCardDAVDiscoveryRetainsBookWithSettledPublicationOwnership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, book := newCardDAVResourceStore(t)
	personID, settledRemote := settleCardDAVTestPublication(t, st, book, "settled-owner")

	before, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(before.Desired)
	assert.Empty(before.PendingOperation)
	resourceBefore, err := st.GetCardDAVResourceContext(t.Context(), book.ID, settledRemote.Href)
	require.NoError(err)

	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(account)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
	})
	require.NoError(err)
	require.Len(books, 1,
		"settled remote ownership must retain a temporarily absent address book")
	assert.Equal(book.ID, books[0].ID)
	after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(after.Desired)
	assert.Empty(after.PendingOperation)
	resourceAfter, err := st.GetCardDAVResourceContext(t.Context(), book.ID, settledRemote.Href)
	require.NoError(err)
	assert.Equal(resourceBefore.ID, resourceAfter.ID,
		"discovery absence must not cascade-delete settled remote state")
}

func TestCardDAVDiscoveryRetainsBookWithClearedPublicationRetryState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, _, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('retry-owner', 'Retry Owner') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	remote := remoteResource(book.CanonicalURL+"retry-owner.vcf", "retry-owner", "Retry Owner", "retry@example.test", `"retry"`)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID, Href: remote.Href,
		OutgoingBody: remote.RemoteBody, OutgoingSemanticHash: remote.SemanticHash,
		LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	retryAfter := time.Now().UTC().Add(time.Hour)
	require.NoError(st.RollbackCardDAVPublicationThrottleContext(t.Context(), pending, retryAfter))
	cleared, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(cleared.Desired)
	assert.Empty(cleared.PendingOperation)
	gateBefore, err := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(err)
	require.NotNil(gateBefore)

	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(account)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
	})
	require.NoError(err)
	require.Len(books, 1,
		"clearing a throttled mutation must not make its remote ownership disposable")
	assert.Equal(book.ID, books[0].ID)
	after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(after.PendingOperation)
	gateAfter, err := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(err)
	require.NotNil(gateAfter)
	assert.Equal(gateBefore.UTC(), gateAfter.UTC())
}

func TestCardDAVDiscoveryRetainsBookWithUnresolvedConflictIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	initial := remoteResource(book.CanonicalURL+"conflicted.vcf", "conflicted", "Initial", "initial@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{initial},
	})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, initial.Href)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	_, err = st.AddPersonContactPointContext(t.Context(), *mapping.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "user-edit@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	remote := remoteResource(initial.Href, "conflicted", "Remote", "remote@example.test", `"two"`)
	remote.SemanticHash = "semantic-conflicted-remote"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision + 1, Upserts: []store.CardDAVRemoteResource{remote},
		Conflicts: []store.CardDAVConflictCapture{{
			AddressBookID: book.ID, Href: initial.Href,
			ExpectedMappingRevision: mapping.MappingRevision,
			BaseLocalHash:           mapping.LocalHash, LocalHash: local.Fingerprint,
			BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
			RemoteETag: remote.RemoteETag, LocalBody: initial.RemoteBody, RemoteBody: remote.RemoteBody,
		}},
	})
	require.NoError(err)

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
	})
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(book.ID, books[0].ID)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true)
	require.NoError(err)
	require.Len(conflicts, 1)
	assert.Equal(initial.Href, conflicts[0].Href)
}

func TestCardDAVSetBookRolesRejectsWriteTargetSwapWithPendingMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.Len(books, 2)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('pending-role-person', 'Pending Role Person') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: books[0].ID,
		Href:                 books[0].CanonicalURL + "pending-role-person.vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:pending-role-person\r\nFN:Pending Role Person\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "semantic-pending-role", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)

	err = st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
	})
	require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
	after, listErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(listErr)
	assert.True(after[0].IsWriteTarget)
	assert.False(after[1].IsWriteTarget)
	pending, pendingErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(pendingErr)
	assert.NotEmpty(pending.PendingOperation)
}

func TestCardDAVSetBookRolesRejectsWriteTargetSwapWithSettledPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.Len(books, 2)
	personID, _ := settleCardDAVTestPublication(t, st, books[0], "settled-role-owner")

	err = st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
	})
	require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
	after, listErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(listErr)
	assert.True(after[0].IsWriteTarget)
	assert.False(after[1].IsWriteTarget)
	settled, publicationErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(publicationErr)
	assert.True(settled.Desired)
	assert.Empty(settled.PendingOperation)
}

func TestCardDAVSetBookRolesRejectsWriteTargetSwapWhenProposedTargetHasPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), cardDAVRoleDiscovery())
	require.NoError(err)
	require.Len(books, 2)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('proposed-owner', 'Proposed Owner') RETURNING id`).Scan(&personID))
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications
		(person_id, desired, address_book_id, href)
		VALUES (?, TRUE, ?, ?)`), personID, books[1].ID, books[1].CanonicalURL+"proposed-owner.vcf")
	require.NoError(err)

	err = st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
	})
	require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
	after, listErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(listErr)
	assert.True(after[0].IsWriteTarget)
	assert.False(after[1].IsWriteTarget)
	_, publicationErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(publicationErr)
}

func TestCardDAVSetBookRolesRejectsWriteTargetSwapWithPendingConflictIntent(t *testing.T) {
	for _, test := range []struct {
		name                     string
		demoteCurrentTarget      bool
		conflictOnProposedTarget bool
	}{
		{name: "swap away from current target"},
		{name: "demote current target", demoteCurrentTarget: true},
		{name: "swap to proposed target", conflictOnProposedTarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, _, conflictBook, _, conflict, remote := seededCardDAVTombstoneConflict(t, false)
			pending, err := st.PrepareCardDAVConflictLocalTombstoneContext(
				t.Context(), conflict.ID, conflict.MappingRevision, remote)
			require.NoError(err)
			require.Equal(store.CardDAVMutationDelete, pending.PendingOperation)
			allowed := true
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
				BaseURL: "https://contacts.example/dav", Username: "alice",
				PrincipalURL: "https://contacts.example/principal/alice/",
				HomeURL:      "https://contacts.example/books/alice/",
				Books: []store.CardDAVDiscoveredBook{
					{CanonicalURL: conflictBook.CanonicalURL, DisplayName: "Personal", CanCreate: &allowed},
					{CanonicalURL: "https://contacts.example/books/alice/next/", DisplayName: "Next", CanCreate: &allowed},
				},
			})
			require.NoError(err)
			require.Len(books, 2)
			nextBook := books[1]
			targetBook := nextBook
			expectedTargetID := conflictBook.ID
			roles := store.CardDAVBookRoles{
				IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
			}
			if test.demoteCurrentTarget {
				targetBook = conflictBook
				roles.IsWriteTarget = false
			}
			if test.conflictOnProposedTarget {
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
					SET is_write_target = FALSE WHERE id = ?`), conflictBook.ID)
				require.NoError(err)
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
					SET is_write_target = TRUE, is_subscribed = TRUE WHERE id = ?`), nextBook.ID)
				require.NoError(err)
				targetBook = conflictBook
				expectedTargetID = nextBook.ID
			}

			err = st.SetCardDAVBookRolesContext(t.Context(), targetBook.ID, roles)
			require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
			afterBooks, listErr := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(listErr)
			var actualTargetID int64
			for _, book := range afterBooks {
				if book.IsWriteTarget {
					actualTargetID = book.ID
				}
			}
			assert.Equal(expectedTargetID, actualTargetID)
			afterConflict, conflictErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(conflictErr)
			assert.Equal(store.CardDAVMutationDelete, afterConflict.PendingOperation)
			assert.Equal(pending.MappingRevision, afterConflict.MappingRevision)
		})
	}
}

func TestCardDAVSetBookRolesRejectsTransitionWithUnresolvedConflict(t *testing.T) {
	for _, test := range []struct {
		name                     string
		demoteCurrentTarget      bool
		conflictOnProposedTarget bool
	}{
		{name: "swap away from current target"},
		{name: "demote current target", demoteCurrentTarget: true},
		{name: "swap to proposed target", conflictOnProposedTarget: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, account, conflictBook, mapping := seededCardDAVConflictMapping(t)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
			require.NoError(err)
			require.Equal(store.CardDAVConflictUnresolved, conflict.Status)
			require.Empty(conflict.PendingOperation)
			books := addCardDAVRoleTransitionBook(t, st, account, conflictBook)
			nextBook := books[1]
			targetBook := nextBook
			expectedTargetID := conflictBook.ID
			roles := store.CardDAVBookRoles{
				IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
			}
			if test.demoteCurrentTarget {
				targetBook = conflictBook
				roles.IsWriteTarget = false
			}
			if test.conflictOnProposedTarget {
				setCardDAVRoleTestWriteTarget(t, st, conflictBook.ID, nextBook.ID)
				targetBook = conflictBook
				expectedTargetID = nextBook.ID
			}

			err = st.SetCardDAVBookRolesContext(t.Context(), targetBook.ID, roles)
			require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
			afterBooks, listErr := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(listErr)
			assert.Equal(expectedTargetID, cardDAVRoleTestWriteTargetID(afterBooks))
			afterConflict, conflictErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(conflictErr)
			assert.Equal(store.CardDAVConflictUnresolved, afterConflict.Status)
			assert.Empty(afterConflict.PendingOperation)
		})
	}
}

func TestCardDAVSetBookRolesAllowsTransitionWithResolvedConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, conflictBook, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	retained := parseCardDAVRemoteForStoreTest(mapping.Href, capture.RemoteETag, capture.RemoteBody)
	resolved, err := st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, Remote: retained,
	})
	require.NoError(err)
	require.Equal(store.CardDAVConflictResolved, resolved.Status)
	require.Empty(resolved.PendingOperation)
	books := addCardDAVRoleTransitionBook(t, st, account, conflictBook)

	err = st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true,
	})
	require.NoError(err)
	afterBooks, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.Equal(books[1].ID, cardDAVRoleTestWriteTargetID(afterBooks))
	afterConflict, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, afterConflict.Status)
}

func TestCardDAVSetBookRolesRejectsNonTargetNarrowingWithUnresolvedConflict(t *testing.T) {
	for _, test := range []struct {
		name  string
		roles store.CardDAVBookRoles
	}{
		{name: "lookup only", roles: store.CardDAVBookRoles{IsLookupSource: true}},
		{name: "ignored", roles: store.CardDAVBookRoles{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, account, conflictBook, mapping := seededCardDAVConflictMapping(t)
			capture := conflictCapture(mapping)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
			require.NoError(err)
			books := addCardDAVRoleTransitionBook(t, st, account, conflictBook)
			setCardDAVRoleTestWriteTarget(t, st, conflictBook.ID, books[1].ID)

			err = st.SetCardDAVBookRolesContext(t.Context(), conflictBook.ID, test.roles)

			require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
			afterBooks, listErr := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(listErr)
			require.Len(afterBooks, 2)
			assert.True(afterBooks[0].IsSubscribed)
			assert.True(afterBooks[0].IsLookupSource)
			afterMapping, mappingErr := st.GetCardDAVResourceContext(
				t.Context(), conflictBook.ID, mapping.Href)
			require.NoError(mappingErr)
			assert.Equal(conflict.MappingRevision, afterMapping.MappingRevision)
			afterConflict, conflictErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
			require.NoError(conflictErr)
			assert.Equal(store.CardDAVConflictUnresolved, afterConflict.Status)
			assert.Equal(capture.LocalBody, afterConflict.LocalBody)
			assert.Equal(capture.RemoteBody, afterConflict.RemoteBody)
		})
	}
}

func TestCardDAVSetBookRolesRejectsNonTargetNarrowingWithPendingPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
		VALUES ('pending-narrowing', 'Pending Narrowing') RETURNING id`).Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID,
		Href:                 book.CanonicalURL + "pending-narrowing.vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:pending-narrowing\r\nFN:Pending Narrowing\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "semantic-pending-narrowing", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	books := addCardDAVRoleTransitionBook(t, st, account, book)
	setCardDAVRoleTestWriteTarget(t, st, book.ID, books[1].ID)

	err = st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsLookupSource: true,
	})

	require.ErrorIs(err, store.ErrCardDAVRoleChangePending)
	after, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Equal(pending.PendingOperation, after.PendingOperation)
	afterBooks, listErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(listErr)
	assert.True(afterBooks[0].IsSubscribed)
}

func TestCardDAVSetBookRolesAllowsResolvedConflictNarrowingAndKeepsAudit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, conflictBook, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	remote := parseCardDAVRemoteForStoreTest(mapping.Href, capture.RemoteETag, capture.RemoteBody)
	resolved, err := st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, Remote: remote,
	})
	require.NoError(err)
	books := addCardDAVRoleTransitionBook(t, st, account, conflictBook)
	setCardDAVRoleTestWriteTarget(t, st, conflictBook.ID, books[1].ID)

	err = st.SetCardDAVBookRolesContext(t.Context(), conflictBook.ID, store.CardDAVBookRoles{})

	require.NoError(err)
	afterBooks, listErr := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(listErr)
	require.Len(afterBooks, 2)
	assert.False(afterBooks[0].IsSubscribed)
	assert.False(afterBooks[0].IsLookupSource)
	afterConflict, conflictErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(conflictErr)
	assert.Equal(store.CardDAVConflictResolved, afterConflict.Status)
	assert.Equal(resolved.ResolvedAt, afterConflict.ResolvedAt)
}

func TestCardDAVSetBookRolesAllowsOrdinaryNonTargetNarrowing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, book, mappings := importCardDAVRoleTestPeople(t, "ordinary-narrowing")
	mapping := mappings["ordinary-narrowing"]
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{
		IsLookupSource: true,
	}))
	afterDemotion, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	assert.Nil(afterDemotion.PersonID)

	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{}))
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func addCardDAVRoleTransitionBook(
	t *testing.T, st *store.Store, account store.CardDAVAccount, current store.CardDAVAddressBook,
) []store.CardDAVAddressBook {
	t.Helper()
	require := require.New(t)
	input := cardDAVRediscoveryForBook(account, current)
	input.Books = append(input.Books, store.CardDAVDiscoveredBook{
		CanonicalURL: "https://contacts.example/books/alice/next/",
		DisplayName:  "Next", CanCreate: new(true),
	})
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.Len(books, 2)
	return books
}

func setCardDAVRoleTestWriteTarget(t *testing.T, st *store.Store, oldID, newID int64) {
	t.Helper()
	require := require.New(t)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE WHERE id = ?`), oldID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = TRUE, is_subscribed = TRUE WHERE id = ?`), newID)
	require.NoError(err)
}

func cardDAVRoleTestWriteTargetID(books []store.CardDAVAddressBook) int64 {
	for _, book := range books {
		if book.IsWriteTarget {
			return book.ID
		}
	}
	return 0
}

func settleCardDAVTestPublication(
	t *testing.T, st *store.Store, book store.CardDAVAddressBook, slug string,
) (int64, store.CardDAVRemoteResource) {
	t.Helper()
	var personID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), slug, "Settled Owner").Scan(&personID))
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(t, err)
	remote := remoteResource(book.CanonicalURL+slug+".vcf", slug, "Settled Owner", slug+"@example.test", `"settled"`)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: personID, Desired: true, AddressBookID: book.ID, Href: remote.Href,
		OutgoingBody: remote.RemoteBody, OutgoingSemanticHash: remote.SemanticHash,
		LocalHash: snapshot.Fingerprint,
	})
	require.NoError(t, err)
	require.NoError(t, st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{
		Publication: *pending, Remote: remote,
	}))
	return personID, remote
}

func TestCardDAVDiscoveryPersistsRolesAndPrunesOnlyAfterCompleteReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	initial := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: "https://contacts.example/books/alice/personal/", DisplayName: "Personal", DiscoveryIndex: 0, CanCreate: new(true), CanUpdate: new(true), SupportedVCardVersions: []string{"4.0", "3.0"}},
			{CanonicalURL: "https://contacts.example/books/alice/directory/", DisplayName: "Directory", DiscoveryIndex: 1},
		},
	}
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), initial)
	require.NoError(err)
	assert.Equal(int64(1), account.ConnectionGeneration)
	require.Len(books, 2)
	assert.True(books[0].IsWriteTarget)
	assert.True(books[0].IsSubscribed)
	assert.True(books[0].IsLookupSource)
	assert.False(books[1].IsWriteTarget)
	assert.False(books[1].IsSubscribed)
	assert.True(books[1].IsLookupSource)
	assert.Equal([]string{"4.0", "3.0"}, books[0].SupportedVCardVersions)

	// A deliberately ignored book remains ignored when it is seen again.
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_subscribed = FALSE, is_lookup_source = FALSE
		WHERE canonical_url = ?`), initial.Books[1].CanonicalURL)
	require.NoError(err)
	second := initial
	second.Books = []store.CardDAVDiscoveredBook{
		initial.Books[1],
		{CanonicalURL: "https://contacts.example/books/alice/new/", DisplayName: "New", DiscoveryIndex: 1, CanCreate: new(true)},
	}
	account, books, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), second)
	require.NoError(err)
	assert.Equal(int64(1), account.ConnectionGeneration)
	require.Len(books, 2)
	assert.Equal("Directory", books[0].DisplayName)
	assert.False(books[0].IsSubscribed)
	assert.False(books[0].IsLookupSource)
	assert.False(books[1].IsWriteTarget, "later create-capable books must not silently become the write target")
	assert.False(books[1].IsSubscribed)
	assert.True(books[1].IsLookupSource)
}

func TestCardDAVDiscoveryBumpsConnectionGenerationForConnectionOrCredentialChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/alice/personal/", DisplayName: "Personal"}},
	}
	first, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	input.PrincipalURL = "https://contacts.example/principal/renamed/"
	metadataOnly, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(first.ConnectionGeneration, metadataOnly.ConnectionGeneration)

	input.Username = "alice-renamed"
	changed, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(first.ConnectionGeneration+1, changed.ConnectionGeneration)
	input.CredentialsChanged = true
	credentialChanged, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(changed.ConnectionGeneration+1, credentialChanged.ConnectionGeneration)

	loaded, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(loaded)
	assert.Equal(credentialChanged, loaded)
	listed, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.Len(listed, 1)
}

func TestCardDAVDiscoveryRejectsIncompleteSnapshotWithoutChangingStoredBooks(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/books/alice/personal/", DisplayName: "Personal"}},
	}
	_, before, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	input.Books = []store.CardDAVDiscoveredBook{
		{CanonicalURL: "https://contacts.example/books/alice/new/", DisplayName: "New"},
		{CanonicalURL: "https://contacts.example/books/alice/new/", DisplayName: "Duplicate"},
	}
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.ErrorContains(err, "duplicate")
	after, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.Equal(t, before, after)
}

func TestCardDAVDiscoveryKeepsCanonicalURLWhenServerReadvertisesItsAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL:      "https://contacts.example/books/alice/canonical/",
			DiscoveryAliasURL: "https://contacts.example/books/alice/advertised/",
			DisplayName:       "Personal",
		}},
	}
	_, first, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.Len(first, 1)
	firstID := first[0].ID

	// A canonical-only snapshot must not erase the durable discovery alias.
	input.Books[0].DiscoveryAliasURL = ""
	_, second, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal(firstID, second[0].ID)
	assert.Equal("https://contacts.example/books/alice/canonical/", second[0].CanonicalURL)
	assert.Equal("https://contacts.example/books/alice/advertised/", second[0].DiscoveryAliasURL)

	input.Books[0].CanonicalURL = input.Books[0].DiscoveryAliasURL
	if input.Books[0].CanonicalURL == "" {
		input.Books[0].CanonicalURL = "https://contacts.example/books/alice/advertised/"
	}
	input.Books[0].DiscoveryAliasURL = ""

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(firstID, books[0].ID)
	assert.Equal("https://contacts.example/books/alice/canonical/", books[0].CanonicalURL)
	assert.Equal("https://contacts.example/books/alice/advertised/", books[0].DiscoveryAliasURL)
	assert.Equal(first[0].IsWriteTarget, books[0].IsWriteTarget)
	assert.Equal(first[0].IsSubscribed, books[0].IsSubscribed)
	assert.Equal(first[0].IsLookupSource, books[0].IsLookupSource)
}

func TestCardDAVDiscoveryCanonicalURLChangeInvalidatesIncrementalSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book := newCardDAVResourceStore(t)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, NextSyncToken: "incremental-token",
	})
	require.NoError(err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	book = books[0]
	stalePlan := store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, NextSyncToken: "stale-next-token",
	}

	rediscovered := cardDAVRediscoveryForBook(account, book)
	rediscovered.Books[0].CanonicalURL = "https://contacts.example/books/alice/renamed/"
	rediscovered.Books[0].DiscoveryAliasURL = book.CanonicalURL
	_, books, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), rediscovered)

	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(book.ID, books[0].ID)
	assert.Equal(rediscovered.Books[0].CanonicalURL, books[0].CanonicalURL)
	assert.Equal(book.CanonicalURL, books[0].DiscoveryAliasURL)
	assert.Empty(books[0].SyncToken)
	assert.True(books[0].NeedsFullReconcile)
	assert.Equal(book.SyncRevision+1, books[0].SyncRevision)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), stalePlan)
	require.ErrorIs(err, store.ErrCardDAVStalePlan)
}

func TestCardDAVDiscoveryRejectsTwoBooksClaimingOneStoredURLIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/",
		HomeURL:      "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL:      "https://contacts.example/books/alice/canonical/",
			DiscoveryAliasURL: "https://contacts.example/books/alice/alias/",
			DisplayName:       "Personal",
		}},
	}
	beforeAccount, beforeBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)

	input.Books = []store.CardDAVDiscoveredBook{
		{
			CanonicalURL:      "https://contacts.example/books/alice/canonical/",
			DiscoveryAliasURL: "https://contacts.example/books/alice/first-new-alias/",
			DisplayName:       "First",
		},
		{
			CanonicalURL:      "https://contacts.example/books/alice/alias/",
			DiscoveryAliasURL: "https://contacts.example/books/alice/second-new-alias/",
			DisplayName:       "Second",
		},
	}
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.ErrorContains(err, "match the same stored book")

	afterAccount, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	require.NotNil(afterAccount)
	assert.Equal(*beforeAccount, *afterAccount)
	afterBooks, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.Equal(beforeBooks, afterBooks)
}

func TestCardDAVURLIdentityPreservesPathCase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	input := store.CardDAVDiscoveryInput{
		BaseURL: "https://CONTACTS.example:443/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: "https://contacts.example/Books/", DisplayName: "Upper", DiscoveryIndex: 0},
			{CanonicalURL: "https://contacts.example/books/", DisplayName: "Lower", DiscoveryIndex: 1},
		},
	}

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.Len(books, 2)
	assert.Equal("https://contacts.example/Books/", books[0].CanonicalURL)
	assert.Equal("https://contacts.example/books/", books[1].CanonicalURL)
}

func TestCardDAVURLIdentityUsesOneCanonicalAndAliasNamespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	base := store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example/dav", Username: "alice",
		PrincipalURL: "https://contacts.example/principal/alice/", HomeURL: "https://contacts.example/books/alice/",
	}

	selfAlias := base
	selfAlias.Books = []store.CardDAVDiscoveredBook{{
		CanonicalURL:      "https://CONTACTS.example:443/Books/",
		DiscoveryAliasURL: "https://contacts.example/Books/",
	}}
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), selfAlias)
	require.ErrorContains(err, "duplicate")

	crossColumn := base
	crossColumn.Books = []store.CardDAVDiscoveredBook{
		{CanonicalURL: "https://contacts.example/a/", DiscoveryAliasURL: "https://contacts.example/b/", DiscoveryIndex: 0},
		{CanonicalURL: "https://contacts.example/b/", DiscoveryIndex: 1},
	}
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), crossColumn)
	require.ErrorContains(err, "duplicate")

	nullAlias := base
	nullAlias.Books = []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example/only/"}}
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), nullAlias)
	require.NoError(err)
	require.Len(books, 1)
	assert.Empty(books[0].DiscoveryAliasURL)
	var identityCount int
	err = st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM carddav_address_book_urls WHERE address_book_id = ?`), books[0].ID).Scan(&identityCount)
	require.NoError(err)
	assert.Equal(1, identityCount, "NULL alias must create only the canonical identity")
}

func cardDAVRediscoveryForBook(
	account store.CardDAVAccount, book store.CardDAVAddressBook,
) store.CardDAVDiscoveryInput {
	allowed := true
	return store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
		Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: book.CanonicalURL, DisplayName: book.DisplayName,
			SupportsSyncCollection: true, SupportsMultiget: true, CanCreate: &allowed,
		}},
	}
}

func TestCardDAVDiscoveryRejectsCredentialRotationWithPendingRemoteFirstIntent(t *testing.T) {
	t.Run("publication", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		st, account, book := newCardDAVResourceStore(t)
		var personID int64
		require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name)
			VALUES ('rotation-publication', 'Rotation Publication') RETURNING id`).Scan(&personID))
		snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
		require.NoError(err)
		pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
			PersonID: personID, Desired: true, AddressBookID: book.ID,
			Href:                 book.CanonicalURL + "rotation-publication.vcf",
			OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:rotation-publication\r\nFN:Rotation Publication\r\nEND:VCARD\r\n"),
			OutgoingSemanticHash: "semantic-rotation-publication", LocalHash: snapshot.Fingerprint,
		})
		require.NoError(err)

		input := cardDAVRediscoveryForBook(account, book)
		input.CredentialsChanged = true
		_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)

		require.ErrorContains(err, "pending remote-first")
		afterAccount, getErr := st.GetCardDAVAccountContext(t.Context())
		require.NoError(getErr)
		require.NotNil(afterAccount)
		assert.Equal(account.ConnectionGeneration, afterAccount.ConnectionGeneration)
		after, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
		require.NoError(getErr)
		assert.Equal(pending.PendingOperation, after.PendingOperation)
	})

	t.Run("conflict", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		st, account, book, _, conflict, remote := seededCardDAVTombstoneConflict(t, false)
		pending, err := st.PrepareCardDAVConflictLocalTombstoneContext(
			t.Context(), conflict.ID, conflict.MappingRevision, remote)
		require.NoError(err)
		require.Equal(store.CardDAVMutationDelete, pending.PendingOperation)

		input := cardDAVRediscoveryForBook(account, book)
		input.CredentialsChanged = true
		_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)

		require.ErrorContains(err, "pending remote-first")
		afterAccount, getErr := st.GetCardDAVAccountContext(t.Context())
		require.NoError(getErr)
		require.NotNil(afterAccount)
		assert.Equal(account.ConnectionGeneration, afterAccount.ConnectionGeneration)
		after, getErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
		require.NoError(getErr)
		assert.Equal(store.CardDAVMutationDelete, after.PendingOperation)
	})
}

func TestCardDAVConnectionIdentityChangeCreatesFreshBooksAndCleansOldImportState(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, username, localDisplay string
	}{
		{name: "username with same URLs", baseURL: "https://contacts.example/dav", username: "bob"},
		{name: "base URL", baseURL: "https://other.example/dav", username: "alice", localDisplay: "Locally Renamed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, account, book := newCardDAVResourceStore(t)
			removed := remoteResource(book.CanonicalURL+"removed.vcf", "removed-identity", "Removed", "removed@example.test", `"one"`)
			preserved := remoteResource(book.CanonicalURL+"preserved.vcf", "preserved-identity", "Preserved", "preserved@example.test", `"one"`)
			_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: book.SyncRevision, NextSyncToken: "old-account-token",
				Upserts: []store.CardDAVRemoteResource{removed, preserved},
			})
			require.NoError(err)
			removedMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, removed.Href)
			require.NoError(err)
			require.NotNil(removedMapping.PersonID)
			preservedMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, preserved.Href)
			require.NoError(err)
			require.NotNil(preservedMapping.PersonID)
			_, err = st.AddPersonContactPointContext(t.Context(), *preservedMapping.PersonID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "user-owned@example.test",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(err)
			if tc.localDisplay != "" {
				person, getErr := st.GetPersonContext(t.Context(), *preservedMapping.PersonID)
				require.NoError(getErr)
				_, err = st.UpdatePersonDisplayNameContext(
					t.Context(), person.ID, person.Revision, &tc.localDisplay)
				require.NoError(err)
			}
			beforeNames, err := st.ListPersonNamesContext(t.Context(), *preservedMapping.PersonID, true)
			require.NoError(err)
			require.Len(beforeNames, 1)

			input := cardDAVRediscoveryForBook(account, book)
			input.BaseURL = tc.baseURL
			input.Username = tc.username
			input.PrincipalURL = tc.baseURL + "/principal/"
			input.HomeURL = tc.baseURL + "/books/"
			afterAccount, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)

			require.NoError(err)
			require.Len(books, 1)
			assert.Equal(account.ConnectionGeneration+1, afterAccount.ConnectionGeneration)
			assert.NotEqual(book.ID, books[0].ID, "a new connection must not reuse URL-matched book identity")
			assert.Empty(books[0].SyncToken)
			assert.True(books[0].IsWriteTarget)
			assert.True(books[0].IsSubscribed)
			_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, removed.Href)
			require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
			newResources, err := st.ListCardDAVResourcesContext(t.Context(), books[0].ID)
			require.NoError(err)
			assert.Empty(newResources)
			_, err = st.GetPersonContext(t.Context(), *removedMapping.PersonID)
			require.ErrorIs(err, store.ErrPersonNotFound)
			preservedPerson, err := st.GetPersonContext(t.Context(), *preservedMapping.PersonID)
			require.NoError(err, "user-owned state must survive old connection cleanup")
			if tc.localDisplay == "" {
				assert.Nil(preservedPerson.DisplayName, "a retired remote name must not remain as the scalar display label")
			} else {
				require.NotNil(preservedPerson.DisplayName)
				assert.Equal(tc.localDisplay, *preservedPerson.DisplayName,
					"a locally changed scalar display label must survive")
			}
			points, err := st.ListPersonContactPointsContext(t.Context(), *preservedMapping.PersonID, true)
			require.NoError(err)
			assert.Equal([]string{"user-owned@example.test"}, contactOriginalValues(points))
			names, err := st.ListPersonNamesContext(t.Context(), *preservedMapping.PersonID, true)
			require.NoError(err)
			assert.Empty(names)
			allNames, err := st.ListPersonNamesContext(t.Context(), *preservedMapping.PersonID, false)
			require.NoError(err)
			require.Len(allNames, 1)
			assert.NotNil(allNames[0].Envelope.SupersededAt)
			allPoints, err := st.ListPersonContactPointsContext(t.Context(), *preservedMapping.PersonID, false)
			require.NoError(err)
			require.Len(allPoints, 2)
			for _, point := range allPoints {
				if point.OriginalValue == preserved.Emails[0] {
					assert.NotNil(point.Envelope.SupersededAt)
				}
			}
			_, err = st.GetVCardResourceEnvelopeContext(
				t.Context(), fmt.Sprintf("carddav:%d", book.ID), preserved.Href)
			require.ErrorIs(err, store.ErrVCardResourceNotFound)

			rematch := remoteResource(books[0].CanonicalURL+"rematch.vcf", "rematch-identity", "Rematch", preserved.Emails[0], `"one"`)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: books[0].ID, ConnectionGeneration: afterAccount.ConnectionGeneration,
				SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{rematch},
			})
			require.NoError(err)
			rematched, err := st.GetCardDAVResourceContext(t.Context(), books[0].ID, rematch.Href)
			require.NoError(err)
			require.NotNil(rematched.PersonID)
			assert.NotEqual(*preservedMapping.PersonID, *rematched.PersonID,
				"retired imported email must not participate in identity matching")
		})
	}
}

func TestCardDAVConnectionIdentityChangeRejectsOwnedPublicationOrConflict(t *testing.T) {
	t.Run("publication", func(t *testing.T) {
		st, account, book := newCardDAVResourceStore(t)
		personID, _ := settleCardDAVTestPublication(t, st, book, "identity-owner")
		input := cardDAVRediscoveryForBook(account, book)
		input.Username = "bob"

		_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)

		require.ErrorContains(t, err, "owned remote state")
		_, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
		require.NoError(t, getErr)
	})

	t.Run("unresolved conflict", func(t *testing.T) {
		require := require.New(t)

		st, account, book, mapping := seededCardDAVConflictMapping(t)
		conflict, err := st.RecordCardDAVConflictContext(t.Context(), conflictCapture(mapping))
		require.NoError(err)
		input := cardDAVRediscoveryForBook(account, book)
		input.BaseURL = "https://other.example/dav"

		_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)

		require.ErrorContains(err, "owned remote state")
		after, getErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
		require.NoError(getErr)
		assert.Equal(t, store.CardDAVConflictUnresolved, after.Status)
	})
}

func TestCardDAVDiscoveryRetainsResolvedConflictUntilAuditSweep(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	remote := parseCardDAVRemoteForStoreTest(mapping.Href, capture.RemoteETag, capture.RemoteBody)
	resolved, err := st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, Remote: remote,
	})
	require.NoError(err)
	require.NotNil(resolved.ResolvedAt)
	absent := store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username,
		PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
	}

	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), absent)
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(book.ID, books[0].ID)
	retained, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, retained.Status)

	removed, err := st.SweepResolvedCardDAVConflictsContext(
		t.Context(), resolved.ResolvedAt.Add(30*24*time.Hour+time.Second))
	require.NoError(err)
	assert.Equal(int64(1), removed)
	_, books, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), absent)
	require.NoError(err)
	assert.Empty(books)
	_, err = st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.ErrorIs(err, store.ErrCardDAVConflictNotFound)
}

func TestCardDAVIdentityChangeWaitsForResolvedConflictAuditSweep(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, account, book, mapping := seededCardDAVConflictMapping(t)
	capture := conflictCapture(mapping)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	remote := parseCardDAVRemoteForStoreTest(mapping.Href, capture.RemoteETag, capture.RemoteBody)
	resolved, err := st.ResolveCardDAVConflictRemoteContext(t.Context(), store.CardDAVConflictRemoteResolution{
		ConflictID: conflict.ID, ExpectedMappingRevision: conflict.MappingRevision, Remote: remote,
	})
	require.NoError(err)
	changed := cardDAVRediscoveryForBook(account, book)
	changed.Username = "bob"

	require.ErrorIs(st.ValidateCardDAVConnectionChangeContext(
		t.Context(), changed.BaseURL, changed.Username, false), store.ErrCardDAVIdentityChangeOwned)
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), changed)
	require.ErrorIs(err, store.ErrCardDAVIdentityChangeOwned)
	after, getErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(getErr)
	assert.Equal(store.CardDAVConflictResolved, after.Status)

	removed, err := st.SweepResolvedCardDAVConflictsContext(
		t.Context(), resolved.ResolvedAt.Add(30*24*time.Hour+time.Second))
	require.NoError(err)
	assert.Equal(int64(1), removed)
	require.NoError(st.ValidateCardDAVConnectionChangeContext(
		t.Context(), changed.BaseURL, changed.Username, false))
	afterAccount, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), changed)
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(account.ConnectionGeneration+1, afterAccount.ConnectionGeneration)
	assert.NotEqual(book.ID, books[0].ID)
}
