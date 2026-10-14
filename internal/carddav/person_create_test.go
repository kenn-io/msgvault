package carddav

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCreateStandalonePersonAndPublish(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, _, _ := seededMutationService(t, fixture)
	person, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.com", Type: "work"}},
		Phones: []store.PersonCreateContact{{Value: "+12025550123", Type: "cell"}},
		Org:    "Example Company", Title: "Engineer", Address: "123 Example Street", Note: "Met at a conference",
	})
	require.NoError(err)
	require.Empty(person.ParticipantIDs)
	require.NoError(service.PublishPerson(t.Context(), person.ID))
	fixture.mu.Lock()
	body := string(fixture.body)
	puts := fixture.puts
	fixture.mu.Unlock()
	assert.Equal(1, puts)
	assert.Equal(1, strings.Count(body, "FN:Alex Example\r\n"))
	assert.Contains(body, "UID:"+person.VCardUID)
	assert.Contains(body, "EMAIL;TYPE=work:alex@example.com")
	assert.Contains(body, "TEL;TYPE=cell:+12025550123")
	assert.Contains(body, "ORG:Example Company")
	assert.Contains(body, "TITLE:Engineer")
	assert.Contains(body, "123 Example Street")
	assert.Contains(body, "NOTE:Met at a conference")

	current, err := st.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, current.Revision, new("Alex Renamed"))
	require.NoError(err)
	require.NoError(service.PublishPerson(t.Context(), person.ID))
	fixture.mu.Lock()
	body = string(fixture.body)
	puts = fixture.puts
	fixture.mu.Unlock()
	assert.Equal(2, puts)
	assert.Equal(1, strings.Count(body, "\r\nFN"), body)
	assert.Contains(body, "FN:Alex Renamed\r\n")
	assert.NotContains(body, "Alex Example")
}

func TestEnrichmentCreatedPersonPublishesOnlyAfterApproval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, _, _ := seededMutationService(t, fixture)
	person, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.com"}},
		Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	require.ErrorIs(service.PublishPerson(t.Context(), person.ID), store.ErrCardDAVInferenceReviewRequired)
	fixture.mu.Lock()
	assert.Zero(fixture.puts)
	fixture.mu.Unlock()
	preview, err := service.PreviewPublication(t.Context(), person.ID)
	require.NoError(err)
	assert.True(preview.ReviewRequired)
	assert.Contains(preview.VCard, "alex@example.com")
	require.NoError(service.PublishReviewedPerson(t.Context(), person.ID, preview.ApprovalToken))
	fixture.mu.Lock()
	puts, body := fixture.puts, string(fixture.body)
	fixture.mu.Unlock()
	assert.Equal(1, puts)
	assert.Contains(body, "EMAIL:alex@example.com")
}

func TestEnrichmentCreatedNameReplacementLeavesOneFN(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, _, _ := seededMutationService(t, fixture)
	person, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Agent Name", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	publishReviewed := func() string {
		preview, err := service.PreviewPublication(t.Context(), person.ID)
		require.NoError(err)
		require.NoError(service.PublishReviewedPerson(t.Context(), person.ID, preview.ApprovalToken))
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return string(fixture.body)
	}
	body := publishReviewed()
	assert.Equal(1, strings.Count(body, "\r\nFN"), body)
	assert.Contains(body, "FN:Alex Agent Name\r\n")

	profile, err := st.GetPersonProfileContext(t.Context(), person.ID)
	require.NoError(err)
	require.Len(profile.Names, 1)
	replacement := "Alex Example"
	_, err = st.ApplyPersonProfilePatchContext(t.Context(), person.ID, profile.Person.Revision, store.PersonProfilePatch{
		Names: &store.PersonNamePatch{
			Supersede: []int64{profile.Names[0].Envelope.ID},
			Add: []store.PersonNameInput{{
				NameKind: store.PersonNameFormatted, Formatted: &replacement, OriginalValue: replacement,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		},
	})
	require.NoError(err)
	if err := service.PublishPerson(t.Context(), person.ID); err != nil {
		require.ErrorIs(err, store.ErrCardDAVInferenceReviewRequired)
		body = publishReviewed()
	} else {
		fixture.mu.Lock()
		body = string(fixture.body)
		fixture.mu.Unlock()
	}
	assert.Equal(1, strings.Count(body, "\r\nFN"), body)
	assert.Contains(body, "FN:Alex Example\r\n")
	assert.NotContains(body, "Alex Agent Name")
}

func TestApprovedEnrichmentPersonNeedsReviewAfterInferredReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, _, _ := seededMutationService(t, fixture)
	person, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.com"}},
		Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), person.ID)
	require.NoError(err)
	require.NoError(service.PublishReviewedPerson(t.Context(), person.ID, preview.ApprovalToken))

	profile, err := st.GetPersonProfileContext(t.Context(), person.ID)
	require.NoError(err)
	require.Len(profile.ContactPoints, 1)
	_, err = st.ApplyPersonProfilePatchContext(t.Context(), person.ID, profile.Person.Revision, store.PersonProfilePatch{
		ContactPoints: &store.PersonContactPointPatch{
			Supersede: []int64{profile.ContactPoints[0].Envelope.ID},
			Add: []store.PersonContactPointInput{{
				AddressKind: store.ContactAddressEmail, OriginalValue: "alex.new@example.com",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
			}},
		},
	})
	require.NoError(err)
	fixture.mu.Lock()
	putsBefore := fixture.puts
	fixture.mu.Unlock()
	require.ErrorIs(service.PublishPerson(t.Context(), person.ID), store.ErrCardDAVInferenceReviewRequired)
	fixture.mu.Lock()
	assert.Equal(putsBefore, fixture.puts)
	fixture.mu.Unlock()

	_, err = st.AddPersonAddressContext(t.Context(), person.ID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("1 Inferred Street"),
		OriginalValue: "1 Inferred Street", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
	})
	require.NoError(err)
	preview, err = service.PreviewPublication(t.Context(), person.ID)
	require.NoError(err)
	assert.True(preview.ReviewRequired)
	require.NoError(service.PublishReviewedPerson(t.Context(), person.ID, preview.ApprovalToken))
	fixture.mu.Lock()
	body := string(fixture.body)
	fixture.mu.Unlock()
	assert.Contains(body, "alex.new@example.com")
	assert.NotContains(body, "alex@example.com")
}

func TestInferredContactIdentityReplacementRequiresReview(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity store.VCardIdentity
		wire     string
	}{
		{"group", store.VCardIdentity{Group: new("item1")}, "item1.EMAIL:alex@example.com"},
		{"PROP-ID", store.VCardIdentity{PropID: new("email1")}, "EMAIL;PROP-ID=email1:alex@example.com"},
		{"PID", store.VCardIdentity{PID: []string{"1"}}, "EMAIL;PID=1:alex@example.com"},
		{"ALTID", store.VCardIdentity{AltID: new("alternate1")}, "EMAIL;ALTID=alternate1:alex@example.com"},
		{"unchanged", store.VCardIdentity{}, "EMAIL:alex@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, id, book := seededMutationService(t, fixture)
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET supported_vcard_versions = '["4.0"]' WHERE id = ?`), book.ID)
			require.NoError(err)
			point, err := st.AddPersonContactPointContext(t.Context(), id, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
			})
			require.NoError(err)
			preview, err := service.PreviewPublication(t.Context(), id)
			require.NoError(err)
			require.NoError(service.PublishReviewedPerson(t.Context(), id, preview.ApprovalToken))
			person, err := st.GetPersonContext(t.Context(), id)
			require.NoError(err)
			_, err = st.ApplyPersonProfilePatchContext(t.Context(), id, person.Revision, store.PersonProfilePatch{
				ContactPoints: &store.PersonContactPointPatch{
					Supersede: []int64{point.Envelope.ID},
					Add: []store.PersonContactPointInput{{
						AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment, VCard: tc.identity},
					}},
				},
			})
			require.NoError(err)
			preview, err = service.PreviewPublication(t.Context(), id)
			require.NoError(err)
			assert.Contains(preview.VCard, tc.wire)
			if tc.name == "unchanged" {
				assert.False(preview.ReviewRequired)
				require.NoError(service.PublishPerson(t.Context(), id))
			} else {
				assert.True(preview.ReviewRequired)
				require.ErrorIs(service.PublishPerson(t.Context(), id), store.ErrCardDAVInferenceReviewRequired)
			}
			fixture.mu.Lock()
			assert.Equal(1, fixture.puts)
			fixture.mu.Unlock()
		})
	}
}

func TestNonRenderingInferredContactPreservesPublicationApproval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, id, _ := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, id, "Reviewed note")
	preview, err := service.PreviewPublication(t.Context(), id)
	require.NoError(err)
	require.NoError(service.PublishReviewedPerson(t.Context(), id, preview.ApprovalToken))
	for _, kind := range []store.ContactAddressKind{
		store.ContactAddressUsername, store.ContactAddressIMPP, store.ContactAddressURL,
		store.ContactAddressCalendar, store.ContactAddressContactURI, store.ContactAddressOrgDirectory,
	} {
		_, err = st.AddPersonContactPointContext(t.Context(), id, store.PersonContactPointInput{
			AddressKind: kind, OriginalValue: "@alice",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
		})
		require.NoError(err)
	}
	after, err := service.PreviewPublication(t.Context(), id)
	require.NoError(err)
	assert.False(after.ReviewRequired)
	assert.Equal(preview.VCard, after.VCard)
	require.NoError(service.PublishPerson(t.Context(), id))
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	view, err := service.PublicationView(t.Context(), id)
	require.NoError(err)
	assert.False(view.InferenceReviewRequired)
	fixture.mu.Lock()
	assert.Equal(1, fixture.puts)
	fixture.mu.Unlock()
}
