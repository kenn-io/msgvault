package carddav

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func removePublishedScalarNameOwner(t *testing.T, st *store.Store, personID int64, book store.CardDAVAddressBook) {
	t.Helper()
	resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(t, err)
	record, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:"+strconv.FormatInt(book.ID, 10), resource.Href)
	require.NoError(t, err)
	require.True(t, hasScalarNameMapping(record.NativeMappings, personID))
	// Recreate the legacy metadata through the real envelope persistence API.
	record.NativeMappings = nil
	record.Residue = vcard.ResidueWithMappings(record.PropertyTree, nil)
	_, err = st.PutVCardResourceEnvelopeContext(t.Context(), store.VCardResourceEnvelopeInput{
		PersonID: personID, ExpectedRevision: &record.Revision,
		ProjectionFingerprint: record.ProjectionFingerprint, Envelope: record.ResourceEnvelope,
	})
	require.NoError(t, err)
}

func TestLegacyPublicationNameRepair(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rename", true: "typed-name"}[typed], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := &mutationFixture{}
			service, st, personID, book := seededMutationService(t, fixture)
			require.NoError(service.PublishPerson(t.Context(), personID))
			removePublishedScalarNameOwner(t, st, personID, book)
			want := "New Example"
			if typed {
				want = "Typed Example"
				_, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
					NameKind: store.PersonNameFormatted, Formatted: &want,
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
				require.NoError(err)
			} else {
				person, err := st.GetPersonContext(t.Context(), personID)
				require.NoError(err)
				_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, &want)
				require.NoError(err)
			}
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Equal([]string{want}, publicationFullNames(t, []byte(preview.VCard)))
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			assert.Equal([]string{want}, publicationFullNames(t, fixture.body))
			assert.Equal(2, fixture.puts)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(2, fixture.puts)
		})
	}
}

func TestLegacyPublicationNamePreservesImportedHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	removePublishedScalarNameOwner(t, st, personID, book)
	resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(err)
	sourceRef := "carddav:" + strconv.FormatInt(book.ID, 10)
	name, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceCardDAVImport, SourceRef: &sourceRef, SourceResourceUID: &resource.Href},
	})
	require.NoError(err)
	require.NoError(st.SupersedePersonNameContext(t.Context(), personID, name.Envelope.ID, nil))
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local label"))
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Equal([]string{"Alice Example"}, publicationFullNames(t, []byte(preview.VCard)))
}

func TestLegacyPublicationNameSurvivesFirstRemoteEditBeforeReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{exactGet: true}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	removePublishedScalarNameOwner(t, st, personID, book)
	resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(err)
	require.True(resource.LocallyPublished)

	fixture.mu.Lock()
	fixture.body = []byte(strings.Replace(string(fixture.body), "END:VCARD\r\n", "NOTE:Remote edit\r\nEND:VCARD\r\n", 1))
	fixture.etag = `"remote-edit"`
	fixture.mu.Unlock()
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)

	hasHistory, err := st.HasCardDAVImportedNameHistoryContext(t.Context(), book.ID, personID, resource.Href)
	require.NoError(err)
	assert.False(hasHistory)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("New Example"))
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Equal([]string{"New Example"}, publicationFullNames(t, []byte(preview.VCard)))
}

func TestRemoteMatchedCardKeepsUnownedFullNameAfterPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{exactGet: true}
	service, st, personID, book := seededMutationService(t, fixture)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Remote Example"))
	require.NoError(err)

	href := book.CanonicalURL + "person.vcf"
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Remote Example\r\nEND:VCARD\r\n")
	semanticHash, err := SemanticHash(body)
	require.NoError(err)
	fixture.body, fixture.etag, fixture.href = append([]byte(nil), body...), `"one"`, "/books/personal/person.vcf"
	account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision,
		Upserts: []store.CardDAVRemoteResource{{
			Href: href, RemoteUID: "person", RemoteETag: fixture.etag, RemoteBody: body,
			SemanticHash: semanticHash, DisplayName: "Remote Example",
		}},
	})
	require.NoError(err)
	hasImportedNameHistory, err := st.HasCardDAVImportedNameHistoryContext(t.Context(), book.ID, personID, href)
	require.NoError(err)
	assert.False(hasImportedNameHistory)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	assert.Equal(store.CardDAVGovernanceLocal, resource.Governance)
	assert.False(resource.LocallyPublished)

	require.NoError(service.PublishPerson(t.Context(), personID))
	resource, err = st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	assert.False(resource.LocallyPublished)
	assert.Equal(0, fixture.puts, string(fixture.body))
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(0, fixture.puts)
	assert.Equal([]string{"Remote Example"}, publicationFullNames(t, fixture.body))
	_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts)
	resource, err = st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	assert.False(resource.LocallyPublished)
	person, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
	require.NoError(err)
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(1, fixture.puts)
	assert.Equal([]string{"Remote Example"}, publicationFullNames(t, fixture.body))
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:"+strconv.FormatInt(book.ID, 10), href)
	require.NoError(err)
	assert.False(hasScalarNameMapping(envelope.NativeMappings, personID))
}

func TestLegacyPublicationNameAdoptionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, original, current    string
		version                    vcard.Version
		localPublication           bool
		pending, remote, undesired bool
		want                       []string
	}{
		{name: "bare", original: "FN:Initial Example", current: "FN:Initial Example", localPublication: true, want: []string{"New Example"}},
		{name: "legacy-lowercase", original: "fn:Initial Example", current: "fn:Initial Example", version: vcard.Version30, localPublication: true, want: []string{"New Example"}},
		{name: "escaped", original: "FN:Initial\\, Example", current: "FN:Initial\\, Example", localPublication: true, want: []string{"New Example"}},
		{name: "remote-change", original: "FN:Initial Example", current: "FN:Remote Example", localPublication: true, want: []string{"Remote Example"}},
		{name: "decorated", original: "FN;LANGUAGE=en:Initial Example", current: "FN;LANGUAGE=en:Initial Example", localPublication: true, want: []string{"Initial Example"}},
		{name: "grouped", original: "item1.FN:Initial Example", current: "item1.FN:Initial Example", localPublication: true, want: []string{"Initial Example"}},
		{name: "secondary", original: "FN:Initial Example\r\nFN:Secondary Example", current: "FN:Initial Example\r\nFN:Secondary Example", localPublication: true, want: []string{"Secondary Example"}},
		{name: "ambiguous-duplicates", original: "FN:Initial Example\r\nFN:Initial Example", current: "FN:Initial Example\r\nFN:Initial Example", localPublication: true, want: []string{"Initial Example", "Initial Example"}},
		{name: "pending", original: "FN:Initial Example", current: "FN:Initial Example", localPublication: true, pending: true, want: []string{"Initial Example"}},
		{name: "remote-governance", original: "FN:Initial Example", current: "FN:Initial Example", localPublication: true, remote: true, want: []string{"Initial Example"}},
		{name: "remote-match-local-governance", original: "FN:Initial Example", current: "FN:Initial Example", want: []string{"Initial Example"}},
		{name: "undesired", original: "FN:Initial Example", current: "FN:Initial Example", localPublication: true, undesired: true, want: []string{"Initial Example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			origin, err := url.Parse("https://contacts.example")
			require.NoError(err)
			service := &Service{remote: &davRemote{client: &Client{origin: originURL(origin)}}}
			version := tc.version
			if version == "" {
				version = vcard.Version40
			}
			body := func(fn string) []byte {
				return []byte("BEGIN:VCARD\r\nVERSION:" + string(version) + "\r\nUID:person\r\n" + fn + "\r\nX-KEEP:opaque\r\nEND:VCARD\r\n")
			}
			envelope, err := vcard.ParseResourceEnvelope(body(tc.current))
			require.NoError(err)
			envelope.OriginalRawBytes = body(tc.original)
			envelope.SourceRef, envelope.SourceResourceUID = "carddav:1", "https://contacts.example/books/personal/person.vcf"
			person := store.Person{ID: 1, VCardUID: "person", DisplayName: new("New Example")}
			publication := &store.CardDAVPublication{Desired: !tc.undesired, PersonID: 1, AddressBookID: 1, Href: envelope.SourceResourceUID}
			if tc.pending {
				publication.PendingOperation = store.CardDAVMutationUpdate
			}
			governance := store.CardDAVGovernanceLocal
			if tc.remote {
				governance = store.CardDAVGovernanceRemote
			}
			source := &store.CardDAVPublicationReviewSource{
				Person: person, Snapshot: &store.PersonVCardSnapshot{Profile: store.PersonProfile{Person: person}},
				Book: store.CardDAVAddressBook{ID: 1, SupportedVCardVersions: []string{"4.0"}}, Publication: publication,
				Resource: &store.CardDAVResource{Governance: governance, Href: envelope.SourceResourceUID, LocallyPublished: tc.localPublication},
				Envelope: &store.VCardResourceEnvelopeRecord{ResourceEnvelope: envelope},
			}
			prepared, err := service.preparePublicationEnvelope(source)
			require.NoError(err)
			assert.Equal(tc.want, publicationFullNames(t, prepared.StoredBody))
			assert.Contains(string(prepared.StoredBody), "UID:person\r\n")
			assert.Contains(string(prepared.StoredBody), "X-KEEP:opaque\r\n")
			source.Envelope.ResourceEnvelope = prepared
			again, err := service.preparePublicationEnvelope(source)
			require.NoError(err)
			assert.Equal(prepared.StoredBody, again.StoredBody)
		})
	}
}

func TestPublicationBlankNameRemainsValid(t *testing.T) {
	for _, version := range []vcard.Version{vcard.Version30, vcard.Version40} {
		t.Run(string(version), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			origin, err := url.Parse("https://contacts.example")
			require.NoError(err)
			service := &Service{remote: &davRemote{client: &Client{origin: originURL(origin)}}}
			person := store.Person{ID: 1, VCardUID: "person", DisplayName: new("Initial Example")}
			source := &store.CardDAVPublicationReviewSource{
				Person: person, Snapshot: &store.PersonVCardSnapshot{Profile: store.PersonProfile{Person: person}},
				Book: store.CardDAVAddressBook{ID: 1, CanonicalURL: "https://contacts.example/books/personal/", SupportedVCardVersions: []string{string(version)}},
			}
			first, err := service.preparePublicationEnvelope(source)
			require.NoError(err)
			source.Resource = &store.CardDAVResource{}
			source.Envelope = &store.VCardResourceEnvelopeRecord{ResourceEnvelope: first}
			source.Person.DisplayName, source.Snapshot.Profile.Person.DisplayName = nil, nil
			cleared, err := service.preparePublicationEnvelope(source)
			require.NoError(err)
			names := publicationFullNames(t, cleared.StoredBody)
			require.Len(names, 1)
			assert.NotEmpty(names[0])
			assert.NotEqual("Initial Example", names[0])
			assert.Equal(version, cleared.RenderMetadata.StoredVersion)
		})
	}
}

func TestLegacyPublicationNameRepairWithEqualTypedName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	origin, err := url.Parse("https://contacts.example")
	require.NoError(err)
	service := &Service{remote: &davRemote{client: &Client{origin: originURL(origin)}}}
	envelope, err := vcard.ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Initial Example\r\nFN:Initial Example\r\nEND:VCARD\r\n"))
	require.NoError(err)
	envelope.SourceRef, envelope.SourceResourceUID = "carddav:1", "https://contacts.example/books/personal/person.vcf"
	var names []vcard.PropertyOccurrence
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Property.Name == "FN" {
			names = append(names, occurrence)
		}
	}
	require.Len(names, 2)
	envelope.NativeMappings = []vcard.NativeMapping{{Identity: names[1].Identity, SourceRef: envelope.SourceRef, Table: "person_names", RowID: 10, Field: "formatted", Kind: vcard.HandlingNative}}
	person := store.Person{ID: 1, VCardUID: "person", DisplayName: new("Initial Example")}
	source := &store.CardDAVPublicationReviewSource{
		Person: person, Book: store.CardDAVAddressBook{ID: 1, SupportedVCardVersions: []string{"4.0"}},
		Publication: &store.CardDAVPublication{PersonID: 1, AddressBookID: 1, Href: envelope.SourceResourceUID, Desired: true},
		Resource:    &store.CardDAVResource{Governance: store.CardDAVGovernanceLocal, LocallyPublished: true, Href: envelope.SourceResourceUID},
		Envelope:    &store.VCardResourceEnvelopeRecord{ResourceEnvelope: envelope},
		Snapshot: &store.PersonVCardSnapshot{Profile: store.PersonProfile{Person: person, Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{ID: 10, Source: store.ProvenanceUser}, NameKind: store.PersonNameFormatted, Formatted: new("Initial Example"),
		}}}},
	}
	prepared, err := service.preparePublicationEnvelope(source)
	require.NoError(err)
	assert.Equal([]string{"Initial Example"}, publicationFullNames(t, prepared.StoredBody))
	source.Envelope.ResourceEnvelope = prepared
	again, err := service.preparePublicationEnvelope(source)
	require.NoError(err)
	assert.Equal(prepared.StoredBody, again.StoredBody)
}

func TestLegacyPublicationOriginMigrationRepairsPublishedCard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	removePublishedScalarNameOwner(t, st, personID, book)
	publishedResource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(err)
	// Use unambiguous ordering evidence; same-second ambiguity is covered in store tests.
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET created_at = ?
		WHERE person_id = ? AND address_book_id = ? AND href = ?`),
		"2000-01-01 00:00:00", personID, book.ID, publishedResource.Href)
	require.NoError(err)
	// Recreate a pre-upgrade archive with no origin marker.
	_, err = st.DB().Exec(`UPDATE carddav_resources SET locally_published = FALSE`)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), "carddav_local_publication_origin_v1")
	require.NoError(err)
	require.NoError(st.InitSchemaContext(t.Context()))
	resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(err)
	assert.True(resource.LocallyPublished)

	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("New Example"))
	require.NoError(err)
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal([]string{"New Example"}, publicationFullNames(t, fixture.body))
}

func TestLegacyPublicationNameRemovalBeforeAdoptionRequiresConflict(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replacement string
	}{
		{name: "blank", replacement: "FN: \r\n"},
		{name: "removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			removePublishedScalarNameOwner(t, st, personID, book)

			fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example\r\n"), []byte(tc.replacement), 1)
			fixture.etag = `"remote-removal"`
			_, err := service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
			require.NoError(err)
			conflict, err := st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, resource.Href)
			require.NoError(err, "a pull must not let the label replace the removed name")

			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			assert.Nil(person.DisplayName)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.NotContains(publicationFullNames(t, fixture.body), "Alice Example")
		})
	}
}

func TestRemoteMatchedCardNameRemovalRequiresConflict(t *testing.T) {
	require := require.New(t)
	fixture := &mutationFixture{exactGet: true, reportCurrent: true}
	service, st, personID, book := seededMutationService(t, fixture)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
	require.NoError(err)

	href := book.CanonicalURL + "person.vcf"
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Remote Example\r\nEND:VCARD\r\n")
	semanticHash, err := SemanticHash(body)
	require.NoError(err)
	fixture.body, fixture.etag, fixture.href = append([]byte(nil), body...), `"one"`, "/books/personal/person.vcf"
	account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision,
		Upserts: []store.CardDAVRemoteResource{{
			Href: href, RemoteUID: "person", RemoteETag: fixture.etag, RemoteBody: body,
			SemanticHash: semanticHash, DisplayName: "Remote Example",
		}},
	})
	require.NoError(err)
	require.NoError(service.PublishPerson(t.Context(), personID))
	require.Equal([]string{"Remote Example"}, publicationFullNames(t, fixture.body))

	fixture.mu.Lock()
	fixture.body = bytes.Replace(fixture.body, []byte("FN:Remote Example\r\n"), nil, 1)
	fixture.etag = `"remote-removal"`
	fixture.mu.Unlock()
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	_, err = st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, href)
	require.NoError(err, "a pull must not let the local label replace the removed name")
}

func TestLegacyPublicationNameKeepRemote(t *testing.T) {
	t.Run("unrelated-conflict-keeps-seed", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		fixture, service, st, personID, book := legacyKeepRemoteConflict(t, func(st *store.Store, personID int64) {
			_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "local@example.test",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(err)
		})
		resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
		require.NoError(err)
		hasHistory, err := st.HasCardDAVImportedNameHistoryContext(t.Context(), book.ID, personID, resource.Href)
		require.NoError(err)
		assert.False(hasHistory, "keep_remote must not import an unchanged legacy name")

		person, err := st.GetPersonContext(t.Context(), personID)
		require.NoError(err)
		_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Renamed Example"))
		require.NoError(err)
		require.NoError(service.ReconcilePublications(t.Context()))
		assert.Equal([]string{"Renamed Example"}, publicationFullNames(t, fixture.body))
	})
	t.Run("rejected-rename-imports-card-name", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		fixture, service, st, personID, book := legacyKeepRemoteConflict(t, func(st *store.Store, personID int64) {
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
			require.NoError(err)
		})
		resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
		require.NoError(err)
		hasHistory, err := st.HasCardDAVImportedNameHistoryContext(t.Context(), book.ID, personID, resource.Href)
		require.NoError(err)
		assert.True(hasHistory)
		puts := fixture.puts
		require.NoError(service.ReconcilePublications(t.Context()))
		assert.Equal(puts, fixture.puts, "the rejected rename must not be uploaded")
		assert.Equal([]string{"Alice Example"}, publicationFullNames(t, fixture.body))
	})
}

// legacyKeepRemoteConflict publishes a legacy card, applies a local edit and a
// remote NOTE edit, and resolves the resulting conflict with keep_remote.
func legacyKeepRemoteConflict(
	t *testing.T, localEdit func(*store.Store, int64),
) (*conflictMutationServer, *Service, *store.Store, int64, store.CardDAVAddressBook) {
	t.Helper()
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	require.NoError(t, service.PublishPerson(t.Context(), personID))
	removePublishedScalarNameOwner(t, st, personID, book)
	localEdit(st, personID)
	fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
	fixture.etag, fixture.force412 = `"remote-edit"`, true
	var conflict *ConflictError
	require.ErrorAs(t, service.PublishPerson(t.Context(), personID), &conflict)
	require.NoError(t, service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
	return fixture, service, st, personID, book
}
