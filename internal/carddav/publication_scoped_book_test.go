package carddav

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestScopedPublicationImportedNameReplacementPreservesResourceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
		fn, n   []string
	}{
		{name: "exact_replacement", replace: true, fn: []string{"Corrected Export Name"}, n: []string{"Example;Corrected;;;"}},
		{name: "unbound_addition", replace: false, fn: []string{"Original Export Name", "Corrected Export Name"}, n: []string{"Example;Original;;;", "Example;Corrected;;;"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			body := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:person\r\nFN:Original Export Name\r\nN:Example;Original;;;\r\nX-USER-DETAIL:Keep synthetic detail\r\nEND:VCARD\r\n")
			fixture := &mutationFixture{body: body, etag: `"base"`}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, _, defaultBook := seededMutationServiceForServer(t, server)
			account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "synthetic-selected", BaseURL: server.URL, Username: "alice", PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: defaultBook.CanonicalURL, DisplayName: "Synthetic selected", CanCreate: new(true)}}})
			requirements.NoError(err)
			requirements.Len(books, 1)
			book := books[0]
			requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true, IsWriteTarget: true}))
			books, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
			requirements.NoError(err)
			requirements.Len(books, 1)
			book = books[0]
			remote, err := parseRemoteResource(book.CanonicalURL+"person.vcf", `"base"`, body)
			requirements.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
			requirements.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
			requirements.NoError(err)
			requirements.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
			requirements.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
			service = service.ForConnection(account.ConnectionName, account.ConnectionGeneration)
			backend, ok := any(service).(scopedPublicationService)
			requirements.True(ok)
			authorize := func(_ context.Context, scope *store.IdentityGrantSelection) error {
				requirements.Len(scope.Persons, 1)
				requirements.Len(scope.AddressBooks, 1)
				if scope.AddressBooks[0].BookID != book.ID {
					return ErrConnectionMismatch
				}
				assertions.Equal(personID, scope.Persons[0].ID)
				assertions.Equal(account.ID, scope.AddressBooks[0].AccountID)
				return nil
			}
			profile, err := st.GetPersonProfileContext(t.Context(), personID)
			requirements.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, profile.Person.Revision, new("Local Triage Label"))
			requirements.NoError(err)
			profile, err = st.GetPersonProfileContext(t.Context(), personID)
			requirements.NoError(err)
			var supersede []int64
			for _, name := range profile.Names {
				if name.NameKind == store.PersonNameFormatted || name.NameKind == store.PersonNameStructured {
					supersede = append(supersede, name.Envelope.ID)
				}
			}
			// Replacing imported properties retains their exact resource association.
			// An unbound addition intentionally appends instead of claiming residue.
			imported, err := vcard.ParseResourceEnvelope(body)
			requirements.NoError(err)
			sourceRef := fmt.Sprintf("carddav:%d", book.ID)
			replacementNames := []store.PersonNameInput{}
			for _, occurrence := range imported.PropertyTree {
				envelope := store.ValueEnvelopeInput{Source: store.ProvenanceUser}
				if tc.replace {
					envelope.SourceRef = &sourceRef
					envelope.SourceResourceUID = &remote.Href
					envelope.VCard = cardDAVVCardIdentity(occurrence)
				}
				switch occurrence.Property.Name {
				case "FN":
					replacementNames = append(replacementNames, store.PersonNameInput{NameKind: store.PersonNameFormatted, Formatted: new("Corrected Export Name"), Envelope: envelope})
				case "N":
					replacementNames = append(replacementNames, store.PersonNameInput{NameKind: store.PersonNameStructured, FamilyName: new("Example"), GivenName: new("Corrected"), Envelope: envelope})
				}
			}
			requirements.Len(replacementNames, 2)
			_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{Names: &store.PersonNamePatch{Supersede: supersede, Add: replacementNames}})
			requirements.NoError(err)
			beforeMapping, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			fixture.mu.Lock()
			putsBefore, getsBefore := fixture.puts, fixture.gets
			fixture.mu.Unlock()
			preview, err := backend.PreviewPublicationAuthorized(t.Context(), personID, authorize)
			requirements.NoError(err)
			requirements.NotNil(preview)
			assertions.Equal(book.ID, preview.AddressBook.ID)
			properties := publicationNameProperties(t, []byte(preview.VCard))
			assertions.Equal(tc.fn, properties["FN"])
			assertions.Equal(tc.n, properties["N"])
			afterPreview, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			requirements.NoError(err)
			beforeJSON, err := json.Marshal(beforeMapping)
			requirements.NoError(err)
			afterJSON, err := json.Marshal(afterPreview)
			requirements.NoError(err)
			assertions.JSONEq(string(beforeJSON), string(afterJSON), "preview must not change native rendering, approval, mapping or publication evidence")
			_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
			requirements.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
			fixture.mu.Lock()
			assertions.Equal(putsBefore, fixture.puts)
			assertions.Equal(getsBefore, fixture.gets)
			fixture.mu.Unlock()
			requirements.NoError(backend.PublishReviewedPersonAuthorized(t.Context(), personID, preview.ApprovalToken, authorize))
			canonical, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
			requirements.NoError(err)
			properties = publicationNameProperties(t, canonical.RemoteBody)
			assertions.Equal(tc.fn, properties["FN"])
			assertions.Equal(tc.n, properties["N"])
			assertions.Equal([]string{"-//Server//EN"}, properties["PRODID"], "native settlement must store the actual normalized canonical GET")
			parsed, err := vcard.ParseResourceEnvelope(canonical.RemoteBody)
			requirements.NoError(err)
			var preserved []string
			for _, occurrence := range parsed.PropertyTree {
				if occurrence.Property.Name == "X-USER-DETAIL" {
					preserved = append(preserved, occurrence.Property.RawValue)
				}
			}
			assertions.Equal([]string{"Keep synthetic detail"}, preserved)
			person, err := st.GetPersonContext(t.Context(), personID)
			requirements.NoError(err)
			assertions.Equal(new("Local Triage Label"), person.DisplayName)
			fixture.mu.Lock()
			assertions.Equal(putsBefore+1, fixture.puts)
			assertions.Equal(getsBefore+1, fixture.gets)
			fixture.mu.Unlock()
		})
	}
}
