package store_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestStandalonePersonDuplicateRefusal(t *testing.T) {
	email, phone := store.ContactAddressEmail, store.ContactAddressPhone
	identifier := func(identifierType string) func(*storetest.Fixture, store.ContactAddressKind, string) int64 {
		return func(f *storetest.Fixture, _ store.ContactAddressKind, value string) int64 {
			_, err := f.Store.EnsureParticipantByIdentifier(identifierType, value, "Existing Example")
			require.NoError(f.T, err)
			return 0
		}
	}
	for _, tc := range []struct {
		name             string
		kind             store.ContactAddressKind
		value, duplicate string
		existing         func(f *storetest.Fixture, kind store.ContactAddressKind, value string) int64
	}{
		{"curated email", email, "alex@example.com", "ALEX@example.com", curatedContact},
		{"curated phone", phone, "+12025550123", "+1 (202) 555-0123", curatedContact},
		{"observed email", email, "alex@example.com", "ALEX@example.com",
			func(f *storetest.Fixture, _ store.ContactAddressKind, value string) int64 {
				f.EnsureParticipant(value, "Existing Example", "example.com")
				return 0
			}},
		{"observed phone", phone, "+12025550123", "+1 (202) 555-0123",
			func(f *storetest.Fixture, _ store.ContactAddressKind, value string) int64 {
				_, err := f.Store.EnsurePhoneParticipantContext(f.T.Context(), value, "Existing Example")
				require.NoError(f.T, err)
				return 0
			}},
		{"sms phone identifier", phone, "+12025550124", "+1 (202) 555-0124", identifier("synctech_sms")},
		{"mms email identifier", email, "mms@example.com", "MMS@example.com", identifier("synctech_sms")},
		{"beeper phone alias", phone, "+12025550125", "+1 202 555 0125", identifier("beeper")},
		{"observation-only email", email, "observed@example.com", "Observed@Example.com", observedContact},
		{"observation-only phone", phone, "+12025550126", "+1 (202) 555-0126", observedContact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			existingID := tc.existing(f, tc.kind, tc.value)
			input := store.PersonCreateInput{Name: "Duplicate Example"}
			if tc.kind == store.ContactAddressEmail {
				input.Emails = []store.PersonCreateContact{{Value: tc.duplicate}}
			} else {
				input.Phones = []store.PersonCreateContact{{Value: tc.duplicate}}
			}
			person, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
			assert.Nil(person)
			require.ErrorIs(err, store.ErrPersonContactExists)
			var conflict *store.PersonContactExistsError
			require.ErrorAs(err, &conflict)
			assert.Equal(existingID, conflict.PersonID)
			assert.Contains(err.Error(), "Existing Example")
		})
	}
}

func TestStandalonePersonConcurrentDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.com"}}})
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	var created, duplicates int
	for err := range errs {
		if err == nil {
			created++
		} else {
			require.ErrorIs(err, store.ErrPersonContactExists)
			duplicates++
		}
	}
	assert.Equal(1, created)
	assert.Equal(1, duplicates)
	people, err := f.Store.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Len(people, 1)
}

func TestStandalonePersonRefusesContactInPromotedCluster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.EnsureParticipant("first@example.com", "First Example", "example.com")
	second := f.EnsureParticipant("second@example.com", "Second Example", "example.com")
	_, err := f.Store.LinkParticipants(first, second)
	require.NoError(err)
	existing, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), second)
	require.NoError(err)
	_, err = f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{Name: "Duplicate", Emails: []store.PersonCreateContact{{Value: "first@example.com"}}})
	var conflict *store.PersonContactExistsError
	require.ErrorAs(err, &conflict)
	assert.Equal(existing.ID, conflict.PersonID)
}

func TestStandalonePersonReusesCompanyAndRefusesAmbiguousOrg(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	company, err := f.Store.CreateOrganizationContext(t.Context(),
		store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(),
		store.PersonCreateInput{Name: "Alex Example", Org: " example  company ", Title: " Engineer "})
	require.NoError(err)
	employments, err := f.Store.ListEmploymentsContext(t.Context(),
		store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Equal(company.ID, employments[0].OrganizationID)
	require.NotNil(employments[0].Title)
	assert.Equal("Engineer", *employments[0].Title)

	_, err = f.Store.CreateOrganizationContext(t.Context(),
		store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	_, err = f.Store.CreateStandalonePersonContext(t.Context(),
		store.PersonCreateInput{Name: "Sam Example", Org: "Example Company"})
	require.ErrorIs(err, store.ErrPersonCreateInvalid)
	assert.Contains(err.Error(), "matches several organizations")
	people, err := f.Store.ListPersonsContext(t.Context())
	require.NoError(err)
	assert.Len(people, 1)
}

func TestValidatePersonCreateInput(t *testing.T) {
	email := func(value, kind string) []store.PersonCreateContact {
		return []store.PersonCreateContact{{Value: value, Type: kind}}
	}
	for _, tc := range []struct {
		name  string
		input store.PersonCreateInput
		want  string
	}{
		{"valid", store.PersonCreateInput{Name: "Alex", Emails: email("a@example.com", "work"),
			Org: "Example", Title: "Engineer"}, ""},
		{"vendor type", store.PersonCreateInput{Name: "Alex", Emails: email("a@example.com", "x-assistant")}, ""},
		{"blank name", store.PersonCreateInput{Name: " "}, "name is required"},
		{"long name", store.PersonCreateInput{Name: strings.Repeat("n", store.MaxPersonCreateNameLength+1)},
			"name exceeds"},
		{"name at limit counts runes", store.PersonCreateInput{
			Name: strings.Repeat("é", store.MaxPersonCreateNameLength)}, ""},
		{"long note", store.PersonCreateInput{Name: "Alex",
			Note: strings.Repeat("y", store.MaxPersonCreateNoteLength+1)}, "note exceeds"},
		{"long address", store.PersonCreateInput{Name: "Alex",
			Address: strings.Repeat("z", store.MaxPersonCreateAddressLength+1)}, "address exceeds"},
		{"long title", store.PersonCreateInput{Name: "Alex", Org: "Example",
			Title: strings.Repeat("t", store.MaxPersonCreateTitleLength+1)}, "title exceeds"},
		{"title without org", store.PersonCreateInput{Name: "Alex", Title: "Engineer"},
			"title requires org"},
		{"enrichment source", store.PersonCreateInput{Name: "Alex", Source: store.ProvenanceEnrichment}, ""},
		{"extraction source", store.PersonCreateInput{Name: "Alex", Source: store.ProvenanceExtraction},
			"source must be user or enrichment"},
		{"type with space", store.PersonCreateInput{Name: "Alex", Emails: email("a@example.com", "x y")},
			"must be 1-64 letters"},
		{"type with quote", store.PersonCreateInput{Name: "Alex", Emails: email("a@example.com", `w"`)},
			"must be 1-64 letters"},
		{"email with display name", store.PersonCreateInput{Name: "Alex",
			Emails: email("Alex <a@example.com>", "")}, "bare email address"},
		{"repeated email", store.PersonCreateInput{Name: "Alex", Emails: []store.PersonCreateContact{
			{Value: "a@example.com"}, {Value: "A@example.com"}}}, "repeated email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := store.ValidatePersonCreateInput(tc.input)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, store.ErrPersonCreateInvalid)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// curatedContact saves value on an existing person and returns that person.
func curatedContact(f *storetest.Fixture, kind store.ContactAddressKind, value string) int64 {
	existing, err := f.Store.CreateStandalonePersonContext(f.T.Context(),
		store.PersonCreateInput{Name: "Existing Example"})
	require.NoError(f.T, err)
	_, err = f.Store.AddPersonContactPointContext(f.T.Context(), existing.ID, store.PersonContactPointInput{
		AddressKind: kind, OriginalValue: value,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(f.T, err)
	return existing.ID
}

// observedContact records value only as a contact observation on a
// participant whose primary address is something else, as Beeper does.
func observedContact(f *storetest.Fixture, kind store.ContactAddressKind, value string) int64 {
	participantID := f.EnsureParticipant("primary@example.org", "Existing Example", "example.org")
	_, err := f.Store.RecordContactObservationContext(f.T.Context(), participantID,
		store.ParticipantContactObservationInput{
			AddressKind: kind, OriginalValue: value,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		})
	require.NoError(f.T, err)
	return 0
}

func TestStandalonePersonEnrichmentSourceRequiresExportReview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.com"}},
		Org: "Example Company", Title: "Engineer", Note: "Met at a conference",
		Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	notes, err := f.Store.ListPersonAttributeValuesContext(t.Context(), person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugNotes})
	require.NoError(err)
	require.Len(notes, 1)
	assert.Equal(store.ProvenanceEnrichment, notes[0].Source)
	employments, err := f.Store.ListEmploymentsContext(t.Context(), store.EmploymentFilter{PersonID: person.ID})
	require.NoError(err)
	require.Len(employments, 1)
	assert.Equal(store.ProvenanceEnrichment, employments[0].Source)
	points, err := f.Store.ListPersonContactPointsContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal(store.ProvenanceEnrichment, points[0].Envelope.Source)
	assert.Equal(int64(1), inferenceRevision(t, f.Store, person.ID))

	contactOnly, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Riley Example", Phones: []store.PersonCreateContact{{Value: "+12025550199"}},
		Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	assert.Equal(int64(1), inferenceRevision(t, f.Store, contactOnly.ID))

	user, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Sam Example", Org: "Example Company", Note: "Met at a meetup",
	})
	require.NoError(err)
	assert.Equal(int64(0), inferenceRevision(t, f.Store, user.ID))
}

func inferenceRevision(t *testing.T, st *store.Store, personID int64) int64 {
	t.Helper()
	var revision int64
	err := st.DB().QueryRow(st.Rebind(`SELECT COALESCE(MAX(inference_revision), 0)
		FROM person_carddav_inference_state WHERE person_id = ?`), personID).Scan(&revision)
	require.NoError(t, err)
	return revision
}

func TestEnrichmentCreatedContactsKeepReviewThroughMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor, err := f.Store.CreateStandalonePersonContext(t.Context(),
		store.PersonCreateInput{Name: "Alex Example"})
	require.NoError(err)
	absorbed, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Agent Copy", Emails: []store.PersonCreateContact{{Value: "alex@example.com"}},
		Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	names, err := f.Store.ListPersonNamesContext(t.Context(), absorbed.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NoError(f.Store.SupersedePersonNameContext(t.Context(), absorbed.ID, names[0].Envelope.ID, nil))
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	require.Equal(int64(0), inferenceRevision(t, f.Store, survivor.ID))
	_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "merge-enrichment-contacts", Actor: "test",
	})
	require.NoError(err)
	points, err := f.Store.ListPersonContactPointsContext(t.Context(), survivor.ID, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal("alex@example.com", points[0].OriginalValue)
	assert.Equal(store.ProvenanceEnrichment, points[0].Envelope.Source)
	assert.Equal(int64(1), inferenceRevision(t, f.Store, survivor.ID))
}

func TestEnrichmentCreatedNameKeepsReviewThroughMergeAndSplit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor, err := f.Store.CreateStandalonePersonContext(t.Context(),
		store.PersonCreateInput{Name: "Alex Example"})
	require.NoError(err)
	absorbed, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Agent Copy", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	require.Equal(int64(1), inferenceRevision(t, f.Store, absorbed.ID))
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "merge-enrichment-name", Actor: "test",
	})
	require.NoError(err)
	assert.Equal(int64(1), inferenceRevision(t, f.Store, survivor.ID))
	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: survivor.ID, MergeID: merged.Merge.ID,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-enrichment-name", Actor: "test",
	})
	require.NoError(err)
	require.NotNil(split.NewPerson.DisplayName)
	assert.Equal("Alex Agent Copy", *split.NewPerson.DisplayName)
	assert.Equal(int64(1), inferenceRevision(t, f.Store, split.NewPerson.ID))
}

func TestInferredReplacementChangingOnlyARenderedParameterNeedsReview(t *testing.T) {
	enrichment := store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment}
	street := "1 Example Street"
	name := "Alex Example"
	for _, tc := range []struct {
		name  string
		patch func(profile *store.PersonProfile) store.PersonProfilePatch
	}{
		{"address GEO", func(profile *store.PersonProfile) store.PersonProfilePatch {
			return store.PersonProfilePatch{Addresses: &store.PersonAddressPatch{
				Supersede: []int64{profile.Addresses[0].Envelope.ID},
				Add: []store.PersonAddressInput{{
					AddressKind: store.PersonAddressPostal, StreetAddress: &street, OriginalValue: street,
					GeoURI: new("geo:40.7,-74.0"), Envelope: enrichment,
				}},
			}}
		}},
		{"contact URI", func(profile *store.PersonProfile) store.PersonProfilePatch {
			return store.PersonProfilePatch{ContactPoints: &store.PersonContactPointPatch{
				Supersede: []int64{profile.ContactPoints[0].Envelope.ID},
				Add: []store.PersonContactPointInput{{
					AddressKind: store.ContactAddressEmail, OriginalValue: "alex@example.com",
					URI: new("mailto:alex+agent@example.com"), Envelope: enrichment,
				}},
			}}
		}},
		{"name SORT-AS", func(profile *store.PersonProfile) store.PersonProfilePatch {
			return store.PersonProfilePatch{Names: &store.PersonNamePatch{
				Supersede: []int64{profile.Names[0].Envelope.ID},
				Add: []store.PersonNameInput{{
					NameKind: store.PersonNameFormatted, Formatted: &name, OriginalValue: name,
					SortAs: new("Example,Alex"), Envelope: enrichment,
				}},
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
				Name: name, Emails: []store.PersonCreateContact{{Value: "alex@example.com"}},
				Address: street, Source: store.ProvenanceEnrichment,
			})
			require.NoError(err)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE person_carddav_inference_state
				SET approved_revision = inference_revision WHERE person_id = ?`), person.ID)
			require.NoError(err)
			approved := inferenceRevision(t, f.Store, person.ID)
			profile, err := f.Store.GetPersonProfileContext(t.Context(), person.ID)
			require.NoError(err)
			_, err = f.Store.ApplyPersonProfilePatchContext(t.Context(), person.ID, profile.Person.Revision, tc.patch(profile))
			require.NoError(err)
			assert.Greater(inferenceRevision(t, f.Store, person.ID), approved)
		})
	}
}
