package store_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vcard"
	"go.kenn.io/msgvault/internal/vcardmap"
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

func TestStandalonePersonDuplicateRefusalSanitizesImportedName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	f.EnsureParticipant("alex@example.test", "Imported \x1b[31mExample\x1b[0m", "example.test")

	_, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.test"}},
	})
	var conflict *store.PersonContactExistsError
	require.ErrorAs(err, &conflict)
	assert.Contains(err.Error(), "(Imported Example)")
	assert.NotContains(err.Error(), "\x1b")
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
		{"name with carriage return", store.PersonCreateInput{Name: "Alex\rExample"}, "CR or NUL"},
		{"name with NUL", store.PersonCreateInput{Name: "Alex\x00Example"}, "CR or NUL"},
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

func TestStandalonePersonRejectsMalformedNamesBeforeMutation(t *testing.T) {
	for _, name := range []struct {
		label string
		value string
	}{
		{label: "carriage return", value: "Alex\rExample"},
		{label: "NUL", value: "Alex\x00Example"},
	} {
		for _, source := range []struct {
			label string
			value store.Provenance
		}{
			{label: "default provenance"},
			{label: "enrichment provenance", value: store.ProvenanceEnrichment},
		} {
			t.Run(name.label+"/"+source.label, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := storetest.New(t)

				person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
					Name: name.value, Source: source.value,
				})
				assert.Nil(person)
				require.ErrorIs(err, store.ErrPersonCreateInvalid)
				people, err := f.Store.ListPersonsContext(t.Context())
				require.NoError(err)
				assert.Empty(people, "invalid names must be rejected before a person is stored")
			})
		}
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

	names, err := f.Store.ListPersonNamesContext(t.Context(), split.NewPerson.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NoError(f.Store.SupersedePersonNameContext(
		t.Context(), split.NewPerson.ID, names[0].Envelope.ID, nil,
	))
	retired, err := f.Store.GetPersonContext(t.Context(), split.NewPerson.ID)
	require.NoError(err)
	assert.Nil(retired.DisplayName, "the restored inferred name row must retain ownership of its mirror")
}

func TestRetiringInferredNameKeepsEqualCuratedDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example",
	})
	require.NoError(err)
	inferredName, err := f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alex Example"),
		OriginalValue: "Alex Example",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
	})
	require.NoError(err)

	require.NoError(f.Store.SupersedePersonNameContext(
		t.Context(), person.ID, inferredName.Envelope.ID, nil,
	))
	updated, err := f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	require.NotNil(updated.DisplayName)
	assert.Equal("Alex Example", *updated.DisplayName,
		"matching text does not make the inferred row the owner of a curated display name")
}

func TestRetiringEnrichmentNameAfterMergeKeepsSurvivorDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example",
	})
	require.NoError(err)
	absorbed, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "merge-equal-display-name-ownership", Actor: "test",
	})
	require.NoError(err)
	names, err := f.Store.ListPersonNamesContext(t.Context(), survivor.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NoError(f.Store.SupersedePersonNameContext(
		t.Context(), survivor.ID, names[0].Envelope.ID, nil,
	))
	updated, err := f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	require.NotNil(updated.DisplayName)
	assert.Equal("Alex Example", *updated.DisplayName,
		"an absorbed inferred row must not take ownership of the survivor's scalar name")
	assert.NotZero(merged.Merge.ID)
}

func TestRetiringInferredNameByProfilePatchInvalidatesDisplayNameConsumers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	counterpart, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Riley Example",
	})
	require.NoError(err)
	_, err = f.Store.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
		SourcePersonID: person.ID, TargetPersonID: counterpart.ID, TypeSlug: "friend",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)

	profile, err := f.Store.GetPersonProfileContext(t.Context(), person.ID)
	require.NoError(err)
	require.Len(profile.Names, 1)
	nameRevisionBefore, err := f.Store.PersonDisplayNameRevision()
	require.NoError(err)
	var counterpartProjectionBefore int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), counterpart.ID).Scan(&counterpartProjectionBefore))

	updated, err := f.Store.ApplyPersonProfilePatchContext(t.Context(), person.ID, profile.Person.Revision,
		store.PersonProfilePatch{Names: &store.PersonNamePatch{
			Supersede: []int64{profile.Names[0].Envelope.ID},
		}})
	require.NoError(err)
	assert.Nil(updated.Person.DisplayName)
	nameRevisionAfter, err := f.Store.PersonDisplayNameRevision()
	require.NoError(err)
	assert.Equal(nameRevisionBefore+1, nameRevisionAfter,
		"profile-patch retirement clearing a mirrored name advances the analytics revision")
	var counterpartProjectionAfter int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), counterpart.ID).Scan(&counterpartProjectionAfter))
	assert.Equal(counterpartProjectionBefore+1, counterpartProjectionAfter,
		"profile-patch retirement invalidates related-card snapshots")
}

func TestSplitRestoresInferredDisplayNameOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivor, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example",
	})
	require.NoError(err)
	absorbed, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "merge-equal-display-name-then-split", Actor: "test",
	})
	require.NoError(err)
	split, err := f.Store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: survivor.ID, MergeID: merged.Merge.ID,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-equal-display-name-owner", Actor: "test",
	})
	require.NoError(err)
	require.NotNil(split.NewPerson.DisplayName)
	assert.Equal("Alex Example", *split.NewPerson.DisplayName)

	names, err := f.Store.ListPersonNamesContext(t.Context(), split.NewPerson.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NoError(f.Store.SupersedePersonNameContext(
		t.Context(), split.NewPerson.ID, names[0].Envelope.ID, nil,
	))
	retiredSplitPerson, err := f.Store.GetPersonContext(t.Context(), split.NewPerson.ID)
	require.NoError(err)
	assert.Nil(retiredSplitPerson.DisplayName,
		"the exact split restores ownership of the absorbed inferred name")
	currentSurvivor, err := f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	require.NotNil(currentSurvivor.DisplayName)
	assert.Equal("Alex Example", *currentSurvivor.DisplayName,
		"retiring the split inferred name leaves the curated survivor name intact")
}

func TestRetiringInferredNameInvalidatesDisplayNameConsumers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	counterpart, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Riley Example",
	})
	require.NoError(err)
	_, err = f.Store.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{
		SourcePersonID: person.ID, TargetPersonID: counterpart.ID, TypeSlug: "friend",
		Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)

	nameRevisionBefore, err := f.Store.PersonDisplayNameRevision()
	require.NoError(err)
	var counterpartProjectionBefore int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), counterpart.ID).Scan(&counterpartProjectionBefore))
	names, err := f.Store.ListPersonNamesContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NoError(f.Store.SupersedePersonNameContext(
		t.Context(), person.ID, names[0].Envelope.ID, nil,
	))

	nameRevisionAfter, err := f.Store.PersonDisplayNameRevision()
	require.NoError(err)
	assert.Equal(nameRevisionBefore+1, nameRevisionAfter,
		"clearing a mirrored display name advances the analytics revision")
	var counterpartProjectionAfter int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT vcard_projection_revision
		FROM persons WHERE id = ?`), counterpart.ID).Scan(&counterpartProjectionAfter))
	assert.Equal(counterpartProjectionBefore+1, counterpartProjectionAfter,
		"clearing a mirrored display name invalidates related-card snapshots")
}

func TestEnrichmentCreatedFormattedNameTracksRepeatedDisplayNameEdits(t *testing.T) {
	firstName := "Alex First"
	secondName := "Alex Second"
	for _, tc := range []struct {
		name    string
		changes []*string
		want    *string
	}{
		{
			name:    "second rename updates the current formatted name",
			changes: []*string{&firstName, &secondName},
			want:    &secondName,
		},
		{
			name:    "clearing retires the rename-created formatted name",
			changes: []*string{&firstName, nil},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
				Name: "Alex Enrichment", Source: store.ProvenanceEnrichment,
			})
			require.NoError(err)
			for _, change := range tc.changes {
				person, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, change)
				require.NoError(err)
			}

			names, err := f.Store.ListPersonNamesContext(t.Context(), person.ID, true)
			require.NoError(err)
			if tc.want == nil {
				assert.Empty(names)
				return
			}
			require.Len(names, 1)
			assert.Equal(store.PersonNameFormatted, names[0].NameKind)
			require.NotNil(names[0].Formatted)
			assert.Equal(*tc.want, *names[0].Formatted)
		})
	}
}

func TestDisplayNameEditPreservesIndependentInferredAlternateNames(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Enrichment", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	alternate, err := f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alexandre Sample"),
		Language: new("fr"), OriginalValue: "Alexandre Sample",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
	})
	require.NoError(err)
	person, err = f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)

	_, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, new("Alex Renamed"))
	require.NoError(err)

	names, err := f.Store.ListPersonNamesContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.Len(names, 2)
	var preserved *store.PersonName
	for index := range names {
		if names[index].Envelope.ID == alternate.Envelope.ID {
			preserved = &names[index]
			break
		}
	}
	require.NotNil(preserved)
	assert.Equal(store.ProvenanceEnrichment, preserved.Envelope.Source)
	assert.Equal("Alexandre Sample", preserved.OriginalValue)
	require.NotNil(preserved.Formatted)
	assert.Equal("Alexandre Sample", *preserved.Formatted)
	require.NotNil(preserved.Language)
	assert.Equal("fr", *preserved.Language)
}

func TestDisplayNameEditPreservesMatchingNameMetadataAndLanguageVariants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Source: store.ProvenanceUser,
	})
	require.NoError(err)

	englishPref := 1
	englishLanguage, englishPropID, englishAltID := "en", "fn-en", "name-1"
	originalEnglish, err := f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alex Example"),
		Language: &englishLanguage, OriginalValue: "Alex Example",
		Envelope: store.ValueEnvelopeInput{
			Pref:   &englishPref,
			VCard:  store.VCardIdentity{Property: "FN", PropID: &englishPropID, AltID: &englishAltID},
			Source: store.ProvenanceEnrichment,
		},
	})
	require.NoError(err)
	pref := 2
	language, script, sortAs := "fr", "Latn", "Example,Alex"
	typeLabel, group, propID, altID := "work", "item2", "fn-fr", "name-2"
	localized, err := f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alex Example"),
		Language: &language, Script: &script, SortAs: &sortAs, OriginalValue: "Alex Example",
		Envelope: store.ValueEnvelopeInput{
			Pref: &pref, TypeLabel: &typeLabel, TypeTokens: []string{"work", "voice"},
			VCard: store.VCardIdentity{
				Property: "FN", Group: &group, PropID: &propID,
				PID: []string{"1.2"}, AltID: &altID,
			}, Source: store.ProvenanceEnrichment,
		},
	})
	require.NoError(err)
	person, err = f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)

	_, err = f.Store.UpdatePersonDisplayNameContext(
		t.Context(), person.ID, person.Revision, new("Alex Renamed"),
	)
	require.NoError(err)

	snapshot, err := f.Store.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	require.Len(snapshot.Profile.Names, 2)
	var english, localizedSuccessor *store.PersonName
	for index := range snapshot.Profile.Names {
		name := &snapshot.Profile.Names[index]
		require.NotNil(name.Formatted)
		require.NotNil(name.Language)
		assert.Equal("Alex Renamed", *name.Formatted)
		assert.Equal(store.ProvenanceUser, name.Envelope.Source)
		switch *name.Language {
		case "en":
			english = name
		case "fr":
			localizedSuccessor = name
		default:
			assert.Fail("unexpected language variant", *name.Language)
		}
	}
	require.NotNil(english)
	require.NotNil(localizedSuccessor)
	assert.Equal(originalEnglish.Envelope.VCard, english.Envelope.VCard)
	require.NotNil(english.Envelope.Pref)
	assert.Equal(1, *english.Envelope.Pref)
	assert.Equal(localized.Envelope.VCard, localizedSuccessor.Envelope.VCard)
	require.NotNil(localizedSuccessor.Envelope.Pref)
	assert.Equal(2, *localizedSuccessor.Envelope.Pref)
	require.NotNil(localizedSuccessor.Envelope.TypeLabel)
	assert.Equal("work", *localizedSuccessor.Envelope.TypeLabel)
	assert.Equal([]string{"work", "voice"}, localizedSuccessor.Envelope.TypeTokens)
	require.NotNil(localizedSuccessor.Script)
	assert.Equal("Latn", *localizedSuccessor.Script)
	require.NotNil(localizedSuccessor.SortAs)
	assert.Equal("Example,Alex", *localizedSuccessor.SortAs)
}

func TestDisplayNameEditKeepsScopedVCardNameIdentityOnItsResource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	person, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Source: store.ProvenanceUser,
	})
	require.NoError(err)

	group, propID := "item1", "name-1"
	sourceRef, sourceUID := "book-origin", "origin.vcf"
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alex Example"),
		OriginalValue: "Alex Example",
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceUser, SourceRef: &sourceRef, SourceResourceUID: &sourceUID,
			VCard: store.VCardIdentity{Property: "FN", Group: &group, PropID: &propID},
		},
	})
	require.NoError(err)
	person, err = f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	_, err = f.Store.UpdatePersonDisplayNameContext(
		t.Context(), person.ID, person.Revision, new("Alex Renamed"),
	)
	require.NoError(err)

	snapshot, err := f.Store.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	require.Len(snapshot.Profile.Names, 1)
	name := snapshot.Profile.Names[0]
	assert.Equal(&sourceRef, name.Envelope.SourceRef)
	assert.Equal(&sourceUID, name.Envelope.SourceResourceUID)

	otherResource, err := vcard.ParseResourceEnvelope([]byte(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nitem1.FN;PROP-ID=name-1:Unrelated Card Name\r\nEND:VCARD\r\n",
	))
	require.NoError(err)
	otherResource.SourceRef = "book-other"
	otherResource.SourceResourceUID = "other.vcf"
	projected, err := vcardmap.ProjectPersonEnvelope(*snapshot, otherResource)
	require.NoError(err)
	assert.Contains(string(projected.StoredBody), "item1.FN;PROP-ID=name-1:Unrelated Card Name\r\n")
	assert.Contains(string(projected.StoredBody), "FN:Alex Renamed\r\n")
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
