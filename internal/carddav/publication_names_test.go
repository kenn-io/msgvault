package carddav

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func publicationFullNames(tb testing.TB, body []byte) []string {
	tb.Helper()
	envelope, err := vcard.ParseResourceEnvelope(body)
	require.NoError(tb, err)
	var names []string
	for _, occurrence := range envelope.PropertyTree {
		if strings.EqualFold(occurrence.Property.Name, "FN") {
			value, err := vcard.PropertyValue(envelope.RenderMetadata.StoredVersion, occurrence.Property)
			require.NoError(tb, err)
			names = append(names, value)
		}
	}
	return names
}

func TestPublicationDisplayNameRenameUploadsOnce(t *testing.T) {
	t.Parallel()
	for _, promoted := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "promoted"}[promoted], func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &mutationFixture{}
			service, st, personID, book := seededMutationService(t, fixture)
			if promoted {
				participantID, err := st.EnsureParticipant("promotion@example.com", "Initial Example", "example.com")
				require.NoError(err)
				person, _, err := st.CreatePersonFromParticipant(participantID)
				require.NoError(err)
				personID = person.ID
			}
			require.NoError(service.PublishPerson(t.Context(), personID))
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("New Example"))
			require.NoError(err)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal([]string{"New Example"}, publicationFullNames(t, fixture.body))
			assert.Equal(2, fixture.puts)
			resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
			require.NoError(err)
			assert.True(resource.LocallyPublished)
			envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:"+strconv.FormatInt(book.ID, 10), resource.Href)
			require.NoError(err)
			assert.Equal([]string{"New Example"}, publicationFullNames(t, envelope.StoredBody))
			assert.Contains(string(envelope.StoredBody), person.VCardUID)
			assert.True(hasScalarNameMapping(envelope.NativeMappings, personID))
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(2, fixture.puts)
		})
	}
}

func hasScalarNameMapping(mappings []vcard.NativeMapping, personID int64) bool {
	for _, mapping := range mappings {
		if mapping.Table == "persons" && mapping.RowID == personID && mapping.Field == "display_name" {
			return true
		}
	}
	return false
}

func TestPublicationTypedNameReplacesDisplayFallback(t *testing.T) {
	t.Parallel()
	for _, publishFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "replacement"}[publishFirst], func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			if publishFirst {
				require.NoError(service.PublishPerson(t.Context(), personID))
			}
			_, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
				NameKind: store.PersonNameFormatted, Formatted: new("Typed Example"),
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(err)
			require.NoError(service.PublishPerson(t.Context(), personID))
			assert.Equal([]string{"Typed Example"}, publicationFullNames(t, fixture.body))
			puts := fixture.puts
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(puts, fixture.puts)
		})
	}
}

func FuzzPublicationDisplayNameReplacement(f *testing.F) {
	f.Add("Initial Example", "New Example")
	f.Add("Comma, Semicolon;", "Back\\slash\nNewline")
	f.Add("Renée Example", "例 Example")
	f.Fuzz(func(t *testing.T, initial, renamed string) {
		assert := assert.New(t)
		require := require.New(t)
		for _, name := range []string{initial, renamed} {
			if len(name) > 2048 || !utf8.ValidString(name) || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r") {
				t.Skip()
			}
		}
		origin, err := url.Parse("https://contacts.example")
		require.NoError(err)
		service := &Service{remote: &davRemote{client: &Client{origin: originURL(origin)}}}
		person := store.Person{ID: 1, VCardUID: "person", DisplayName: &initial}
		snapshot := &store.PersonVCardSnapshot{Profile: store.PersonProfile{Person: person}}
		source := &store.CardDAVPublicationReviewSource{
			Person: person, Snapshot: snapshot,
			Book: store.CardDAVAddressBook{ID: 1, CanonicalURL: "https://contacts.example/books/personal/", SupportedVCardVersions: []string{"4.0"}},
		}
		first, err := service.preparePublicationEnvelope(source)
		require.NoError(err)
		assert.Equal([]string{initial}, publicationFullNames(t, first.StoredBody))
		snapshot.Profile.Person.DisplayName, source.Person.DisplayName = &renamed, &renamed
		source.Resource = &store.CardDAVResource{Governance: store.CardDAVGovernanceLocal}
		source.Envelope = &store.VCardResourceEnvelopeRecord{ResourceEnvelope: first}
		second, err := service.preparePublicationEnvelope(source)
		require.NoError(err)
		assert.Equal([]string{renamed}, publicationFullNames(t, second.StoredBody))
		source.Envelope.ResourceEnvelope = second
		again, err := service.preparePublicationEnvelope(source)
		require.NoError(err)
		assert.Equal(second.StoredBody, again.StoredBody)
	})
}

func TestPublicationAcceptsRemoteOnlyNameWithoutEcho(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{reportCurrent: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.body = bytes.Replace(fixture.body, []byte("Alice Example"), []byte("Remote Example"), 1)
	fixture.etag = `"remote-edit"`
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal([]string{"Remote Example"}, publicationFullNames(t, fixture.body))
	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(names, 1)
	assert.Equal(store.ProvenanceCardDAVImport, names[0].Envelope.Source)
	assert.Equal(new("Remote Example"), names[0].Formatted)
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(1, fixture.puts)
}

func TestPublicationNameConflictResolutionDoesNotReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		choice ResolutionChoice
		remote string
		want   string
	}{
		{name: "keep-local", choice: ResolutionKeepLocal, remote: "Remote Example", want: "Local Example"},
		{name: "keep-remote", choice: ResolutionKeepRemote, remote: "Remote Example", want: "Remote Example"},
		{name: "keep-remote-retained-fn", choice: ResolutionKeepRemote, remote: "Alice Example", want: "Alice Example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
			require.NoError(err)
			fixture.body = bytes.Replace(fixture.body, []byte("Alice Example"), []byte(tc.remote), 1)
			fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
			fixture.etag, fixture.force412 = `"remote-edit"`, true
			var conflict *ConflictError
			require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflict)
			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, tc.choice))
			assert.Equal([]string{tc.want}, publicationFullNames(t, fixture.body))
			puts := fixture.puts
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(puts, fixture.puts)
			assert.Equal([]string{tc.want}, publicationFullNames(t, fixture.body))
			if tc.choice == ResolutionKeepRemote {
				person, err = st.GetPersonContext(t.Context(), personID)
				require.NoError(err)
				assert.Equal(new("Local Example"), person.DisplayName, "keep the existing scalar-label ownership policy")
				_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Later label"))
				require.NoError(err)
				require.NoError(service.ReconcilePublications(t.Context()))
				assert.Equal(puts, fixture.puts, "the accepted typed name takes priority over later label edits")
			}
		})
	}
}

func TestPublicationKeepRemoteStillProtectsTypedName(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	_, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Typed Example"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.body = bytes.Replace(fixture.body, []byte("Typed Example"), []byte("Remote Example"), 1)
	fixture.etag, fixture.delete412 = `"remote-edit"`, true
	var conflict *ConflictError
	require.ErrorAs(service.UnpublishPerson(t.Context(), personID), &conflict)
	require.ErrorIs(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote), store.ErrCardDAVRemoteProtected)
	retained, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, retained.Status)
}

func TestPublicationKeepRemoteDoesNotReplayRenameOverGeneratedFullName(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
	require.NoError(err)
	require.NoError(service.PublishPerson(t.Context(), personID))
	acceptedName := publicationFullNames(t, fixture.body)
	require.Equal([]string{"Unnamed Contact"}, acceptedName)

	person, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
	require.NoError(err)
	fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
	fixture.etag, fixture.force412 = `"remote-edit"`, true
	var conflict *ConflictError
	require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflict)
	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))

	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(names, 1)
	assert.Equal(store.ProvenanceCardDAVImport, names[0].Envelope.Source)
	assert.Equal(new("Unnamed Contact"), names[0].Formatted)
	puts := fixture.puts
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(puts, fixture.puts)
	assert.Equal(acceptedName, publicationFullNames(t, fixture.body))
}

func TestPublicationKeepRemoteDoesNotReplayRenameWhenGeneratedFullNameIsCleared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remove bool
	}{
		{name: "blank"},
		{name: "removed", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
			require.NoError(err)
			require.NoError(service.PublishPerson(t.Context(), personID))
			assert.Equal([]string{"Unnamed Contact"}, publicationFullNames(t, fixture.body))

			person, err = st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
			require.NoError(err)
			if tc.remove {
				fixture.body = bytes.Replace(fixture.body, []byte("FN:Unnamed Contact\r\n"), nil, 1)
			} else {
				fixture.body = bytes.Replace(fixture.body, []byte("FN:Unnamed Contact"), []byte("FN: \r\n"), 1)
			}
			fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
			fixture.etag, fixture.force412 = `"remote-edit"`, true
			var conflict *ConflictError
			require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflict)
			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))

			person, err = st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			assert.Nil(person.DisplayName)
			assert.NotContains(publicationFullNames(t, fixture.body), "Local Example")

			require.NoError(service.ReconcilePublications(t.Context()))
			assert.NotContains(publicationFullNames(t, fixture.body), "Local Example")
			puts := fixture.puts
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(puts, fixture.puts)
		})
	}
}

func TestPublicationKeepRemoteDoesNotRestoreClearedScalarFullName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remove bool
	}{
		{name: "blank"},
		{name: "removed", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
			require.NoError(err)
			if tc.remove {
				fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example\r\n"), nil, 1)
			} else {
				fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example"), []byte("FN: \r\n"), 1)
			}
			fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
			fixture.etag, fixture.force412 = `"remote-edit"`, true
			var conflict *ConflictError
			require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflict)
			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
			person, err = st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			assert.Nil(person.DisplayName)
			assert.NotContains(publicationFullNames(t, fixture.body), "Local Example")

			require.NoError(service.ReconcilePublications(t.Context()))
			assert.NotContains(publicationFullNames(t, fixture.body), "Local Example")
			puts := fixture.puts
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(puts, fixture.puts)
		})
	}
}

func TestPublicationRemoteFullNameRemovalRequiresConflict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		replacement string
	}{
		{name: "blank", replacement: "FN: \r\n"},
		{name: "removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example\r\n"), []byte(tc.replacement), 1)
			fixture.etag = `"remote-edit"`
			_, err := service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)

			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			assert.Equal(new("Alice Example"), person.DisplayName, "a pull must not clear the local label")
			resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
			require.NoError(err)
			conflict, err := st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, resource.Href)
			require.NoError(err)

			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
			person, err = st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			assert.Nil(person.DisplayName, "keep-remote accepts the removal")
			assert.NotContains(publicationFullNames(t, fixture.body), "Alice Example")
		})
	}
}

func TestPublicationKeepRemoteWithoutRenameKeepsLabelOwnership(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		label *string
	}{
		{name: "display-label", label: new("Alice Example")},
		{name: "nameless"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, _ := seededMutationServiceForServer(t, server)
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, tc.label)
			require.NoError(err)
			require.NoError(service.PublishPerson(t.Context(), personID))
			published := publicationFullNames(t, fixture.body)
			require.Len(published, 1)

			_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "local@example.test",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(err)
			fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
			fixture.etag, fixture.force412 = `"remote-edit"`, true
			var conflict *ConflictError
			require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflict)
			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
			assert.Equal(published, publicationFullNames(t, fixture.body))
			names, err := st.ListPersonNamesContext(t.Context(), personID, true)
			require.NoError(err)
			assert.Empty(names, "an unrelated keep-remote must not import the label-owned FN")

			person, err = st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Renamed Example"))
			require.NoError(err)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal([]string{"Renamed Example"}, publicationFullNames(t, fixture.body))
		})
	}
}

func TestPublicationRejectedRenameStaysRejectedAfterRemoteNameRemoval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		unrenderableStruct bool
	}{
		{name: "no-other-name"},
		// A structured row with only its original value renders no FN, so the
		// label still replaces the removed name.
		{name: "unrenderable-structured-name", unrenderableStruct: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &conflictMutationServer{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Example"))
			require.NoError(err)
			fixture.body = bytes.Replace(fixture.body, []byte("END:VCARD"), []byte("NOTE:Remote edit\r\nEND:VCARD"), 1)
			fixture.etag, fixture.force412 = `"remote-edit"`, true
			var conflict *ConflictError
			require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflict)
			require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
			require.Equal([]string{"Alice Example"}, publicationFullNames(t, fixture.body))
			if tc.unrenderableStruct {
				_, err = st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
					NameKind: store.PersonNameStructured, OriginalValue: "Opaque Example",
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
				require.NoError(err)
				require.NoError(service.ReconcilePublications(t.Context()))
				require.Equal([]string{"Alice Example"}, publicationFullNames(t, fixture.body))
			}

			fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example\r\n"), nil, 1)
			fixture.etag = `"remote-removal"`
			_, err = service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			resource, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
			require.NoError(err)
			removal, err := st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, resource.Href)
			require.NoError(err, "a pull must not let the rejected label replace the removed name")
			require.NoError(service.ResolveConflict(t.Context(), removal.ID, ResolutionKeepRemote))

			person, err = st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			assert.Nil(person.DisplayName)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.NotContains(publicationFullNames(t, fixture.body), "Local Example")
		})
	}
}

func TestPublicationConvergedRenameKeepsLabelOwnership(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Same Example"))
	require.NoError(err)
	fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example"), []byte("FN:Same Example"), 1)
	fixture.etag = `"remote-same-rename"`
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true, store.AllCardDAVAccounts)
	require.NoError(err)
	require.Empty(conflicts)

	person, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Next Example"))
	require.NoError(err)
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal([]string{"Next Example"}, publicationFullNames(t, fixture.body))
}

func TestPublicationAcceptedRemoteNameKeepsParameters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fullName string
	}{
		{name: "language", fullName: "FN;LANGUAGE=fr:Remote Example"},
		{name: "type", fullName: "FN;TYPE=work:Remote Example"},
		// The published card is vCard 3.0, which spells preference as TYPE=pref.
		{name: "type-and-pref", fullName: "FN;TYPE=work,pref:Remote Example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			fixture := &mutationFixture{reportCurrent: true}
			service, st, personID, _ := seededMutationService(t, fixture)
			require.NoError(service.PublishPerson(t.Context(), personID))
			fixture.body = bytes.Replace(fixture.body, []byte("FN:Alice Example"), []byte(tc.fullName), 1)
			fixture.etag = `"remote-edit"`
			_, err := service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			names, err := st.ListPersonNamesContext(t.Context(), personID, true)
			require.NoError(err)
			require.Len(names, 1)
			assert.Equal(new("Remote Example"), names[0].Formatted)

			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(1, fixture.puts, string(fixture.body))
			assert.Contains(string(fixture.body), tc.fullName)
		})
	}
}
