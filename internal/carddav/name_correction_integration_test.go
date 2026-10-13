package carddav

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func publicationNameProperties(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	document, err := vcard.Decode(bytes.NewReader(body))
	require.NoError(t, err)
	require.Len(t, document.Cards, 1)
	properties := make(map[string][]string)
	for _, property := range document.Cards[0].Properties {
		if property.Name == "FN" || property.Name == "N" || property.Name == "PRODID" {
			properties[property.Name] = append(properties[property.Name], property.RawValue)
		}
	}
	return properties
}

func TestNameCorrectionVerifiesPublicationPreviewAndRemoteProperties(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	service.dav().client.requestTimeout = 5 * time.Second
	formatted, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Original Export Name"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	structured, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameStructured, GivenName: new("Original"), FamilyName: new("Example"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	initial, err := service.PreviewPublication(t.Context(), personID)
	requirements.NoError(err)
	requirements.NoError(service.PublishReviewedPerson(t.Context(), personID, initial.ApprovalToken))
	href := book.CanonicalURL + "person.vcf"

	person, err := st.GetPersonContext(t.Context(), personID)
	requirements.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Triage Label"))
	requirements.NoError(err)
	labelPreview, err := service.PreviewPublication(t.Context(), personID)
	requirements.NoError(err)
	labelProperties := publicationNameProperties(t, []byte(labelPreview.VCard))
	assertions.Equal([]string{"Original Export Name"}, labelProperties["FN"])
	assertions.Equal([]string{"Example;Original;;;"}, labelProperties["N"])
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	requirements.NoError(err)
	remote, absent, err := service.remote.Get(t.Context(), href)
	requirements.NoError(err)
	requirements.False(absent)
	assertions.Equal(labelProperties["FN"], publicationNameProperties(t, remote.RemoteBody)["FN"])

	requirements.NoError(st.SupersedePersonNameContext(t.Context(), personID, formatted.Envelope.ID, nil))
	requirements.NoError(st.SupersedePersonNameContext(t.Context(), personID, structured.Envelope.ID, nil))
	_, err = st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Corrected Export Name"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	_, err = st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameStructured, GivenName: new("Corrected"), FamilyName: new("Example"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	corrected, err := service.PreviewPublication(t.Context(), personID)
	requirements.NoError(err)
	properties := publicationNameProperties(t, []byte(corrected.VCard))
	assertions.Equal([]string{"Corrected Export Name"}, properties["FN"])
	assertions.Equal([]string{"Example;Corrected;;;"}, properties["N"])
	requirements.ErrorIs(service.PublishReviewedPerson(t.Context(), personID, labelPreview.ApprovalToken), store.ErrCardDAVReviewStale)
	remote, absent, err = service.remote.Get(t.Context(), href)
	requirements.NoError(err)
	requirements.False(absent)
	assertions.Equal([]string{"Original Export Name"}, publicationNameProperties(t, remote.RemoteBody)["FN"], "preview and denied stale approval leave the remote contact unchanged")

	requirements.NoError(service.PublishReviewedPerson(t.Context(), personID, corrected.ApprovalToken))
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	requirements.NoError(err)
	remote, absent, err = service.remote.Get(t.Context(), href)
	requirements.NoError(err)
	requirements.False(absent)
	remoteProperties := publicationNameProperties(t, remote.RemoteBody)
	assertions.Equal(properties["FN"], remoteProperties["FN"])
	assertions.Equal(properties["N"], remoteProperties["N"])
	assertions.Equal([]string{"-//Server//EN"}, remoteProperties["PRODID"], "compare semantic properties after server normalization")
	person, err = st.GetPersonContext(t.Context(), personID)
	requirements.NoError(err)
	assertions.Equal(new("Local Triage Label"), person.DisplayName)
}
