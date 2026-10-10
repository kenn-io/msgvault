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
	for _, tc := range []struct {
		name             string
		kind             store.ContactAddressKind
		value, duplicate string
		promoted         bool
	}{
		{"curated email", store.ContactAddressEmail, "alex@example.com", "ALEX@example.com", true},
		{"curated phone", store.ContactAddressPhone, "+12025550123", "+1 (202) 555-0123", true},
		{"observed email", store.ContactAddressEmail, "alex@example.com", "ALEX@example.com", false},
		{"observed phone", store.ContactAddressPhone, "+12025550123", "+1 (202) 555-0123", false},
		{"observed sms identifier", store.ContactAddressPhone, "+12025550124", "+1 (202) 555-0124", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			var existingID int64
			if tc.promoted {
				existing, err := f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{Name: "Existing Example"})
				require.NoError(err)
				existingID = existing.ID
				_, err = f.Store.AddPersonContactPointContext(t.Context(), existing.ID, store.PersonContactPointInput{AddressKind: tc.kind, OriginalValue: tc.value, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				require.NoError(err)
			} else {
				switch {
				case strings.HasPrefix(tc.name, "observed sms"):
					_, err := f.Store.EnsureParticipantByIdentifier("synctech_sms", tc.value, "Existing Example")
					require.NoError(err)
				case tc.kind == store.ContactAddressPhone:
					_, err := f.Store.EnsurePhoneParticipantContext(t.Context(), tc.value, "Existing Example")
					require.NoError(err)
				default:
					f.EnsureParticipant(tc.value, "Existing Example", "example.com")
				}
			}
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
