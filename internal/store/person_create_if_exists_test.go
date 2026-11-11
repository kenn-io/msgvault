package store_test

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func returnPersonInput(emails ...string) store.PersonCreateInput {
	input := store.PersonCreateInput{Name: "Alex Example", IfExists: store.PersonIfExistsReturn}
	for _, email := range emails {
		input.Emails = append(input.Emails, store.PersonCreateContact{Value: email})
	}
	return input
}

func personCreateCounts(t *testing.T, st *store.Store) []int {
	t.Helper()
	counts := []int{}
	for _, table := range []string{"persons", "person_contact_points", "person_names", "organizations", "employments"} {
		var count int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
		counts = append(counts, count)
	}
	return counts
}

func TestCreateIfExistsReturnIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	input := returnPersonInput("alex@example.test")
	first, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	require.NoError(err)
	require.NotNil(first.Ensure)
	assert.Equal(store.PersonEnsureCreated, first.Ensure.Status)
	assert.Empty(first.Ensure.MatchBasis)
	counts := personCreateCounts(t, f.Store)
	input.Name, input.Org, input.Title, input.Note = "Ignored Name", "Ignored Company", "Ignored Title", "Ignored Note"
	input.Phones = []store.PersonCreateContact{{Value: "+12025550123", Type: "cell"}}
	second, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	require.NoError(err)
	require.NotNil(second.Ensure)
	assert.Equal(first.ID, second.ID)
	assert.Equal(first.DisplayName, second.DisplayName)
	assert.Equal(first.Revision, second.Revision)
	assert.Equal(first.UpdatedAt, second.UpdatedAt)
	assert.Equal(store.PersonEnsureMatchedExisting, second.Ensure.Status)
	assert.Equal([]store.PersonUnappliedContact{{Kind: store.ContactAddressPhone, Value: "+12025550123", Type: "cell"}}, second.Ensure.UnappliedContacts)
	assert.Equal(counts, personCreateCounts(t, f.Store))
}

func TestCreateIfExistsDefaultCompatibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	for _, mode := range []store.PersonIfExists{"", store.PersonIfExistsError} {
		input := store.PersonCreateInput{Name: "Alex Example", IfExists: mode}
		person, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
		require.NoError(err)
		require.Nil(person.Ensure)
		saved, err := f.Store.GetPersonContext(t.Context(), person.ID)
		require.NoError(err)
		got, err := json.Marshal(person)
		require.NoError(err)
		want, err := json.Marshal(saved)
		require.NoError(err)
		assert.Equal(string(want), string(got))
		assert.NotContains(string(got), "ensure")
	}
	id := curatedContact(f, store.ContactAddressEmail, "existing@example.test")
	input := returnPersonInput("existing@example.test")
	input.IfExists = store.PersonIfExistsError
	_, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	var conflict *store.PersonContactExistsError
	require.ErrorAs(err, &conflict)
	assert.Equal(id, conflict.PersonID)
	assert.Equal(`email "existing@example.test" matches person `+strconv.FormatInt(id, 10)+` (Existing Example); use msgvault person get `+strconv.FormatInt(id, 10)+`; link observed participants explicitly through POST /api/v1/identity/links`, err.Error())
	_, err = f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{Name: "Alex", IfExists: "merge"})
	assert.ErrorIs(err, store.ErrPersonCreateInvalid)
}

func TestCreateIfExistsReturnConcurrent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	var wg sync.WaitGroup
	type result struct {
		person *store.Person
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			person, err := f.Store.CreateStandalonePersonContext(t.Context(), returnPersonInput("alex@example.test"))
			results <- result{person, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	statuses := map[store.PersonEnsureStatus]int{}
	for result := range results {
		require.NoError(result.err)
		require.NotNil(result.person.Ensure)
		statuses[result.person.Ensure.Status]++
	}
	assert.Equal(map[store.PersonEnsureStatus]int{store.PersonEnsureCreated: 1, store.PersonEnsureMatchedExisting: 1}, statuses)
	counts := personCreateCounts(t, f.Store)
	assert.Equal([]int{1, 1, 0, 0, 0}, counts)
}

func TestCreateIfExistsAllContactRepresentations(t *testing.T) {
	for _, representation := range []string{"contact_point", "participant", "identifier", "observation"} {
		for _, kind := range []store.ContactAddressKind{store.ContactAddressEmail, store.ContactAddressPhone} {
			t.Run(representation+"/"+string(kind), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := storetest.New(t)
				value := "alex@example.test"
				if kind == store.ContactAddressPhone {
					value = "+12025550123"
				}
				var participantID int64
				switch representation {
				case "participant":
					if kind == store.ContactAddressEmail {
						participantID = f.EnsureParticipant(value, "Alex Example", "example.test")
					} else {
						var err error
						participantID, err = f.Store.EnsurePhoneParticipantContext(t.Context(), value, "Alex Example")
						require.NoError(err)
					}
				case "identifier":
					var err error
					participantID, err = f.Store.EnsureParticipantByIdentifier("beeper", value, "Alex Example")
					require.NoError(err)
				case "observation":
					participantID = f.EnsureParticipant("primary@example.test", "Alex Example", "example.test")
					_, err := f.Store.RecordContactObservationContext(t.Context(), participantID, store.ParticipantContactObservationInput{AddressKind: kind, OriginalValue: value, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
					require.NoError(err)
				}
				input := returnPersonInput()
				if kind == store.ContactAddressEmail {
					input.Emails = []store.PersonCreateContact{{Value: strings.ToUpper(value)}}
				} else {
					input.Phones = []store.PersonCreateContact{{Value: "+1 (202) 555-0123"}}
				}
				var personID int64
				if representation == "contact_point" {
					personID = curatedContact(f, kind, value)
				} else {
					counts := personCreateCounts(t, f.Store)
					_, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
					var conflict *store.PersonMatchesParticipantError
					require.ErrorAs(err, &conflict)
					assert.Equal(participantID, conflict.ParticipantID)
					assert.Equal(counts, personCreateCounts(t, f.Store))
					person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
					require.NoError(err)
					personID = person.ID
				}
				counts := personCreateCounts(t, f.Store)
				person, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
				require.NoError(err)
				assert.Equal(personID, person.ID)
				require.NotNil(person.Ensure)
				basisFound := false
				for _, basis := range person.Ensure.MatchBasis {
					if basis.Matched == representation {
						basisFound = true
					}
					assert.Equal(personID, *basis.PersonID)
				}
				assert.True(basisFound)
				assert.Equal(counts, personCreateCounts(t, f.Store))
			})
		}
	}
}

func TestCreateIfExistsReturnClustersAndAmbiguity(t *testing.T) {
	type scenario struct {
		name    string
		linked  bool
		people  int
		unbound bool
	}
	for _, group := range []struct {
		name    string
		matched bool
		cases   []scenario
	}{
		{name: "matched", matched: true, cases: []scenario{{name: "one bound cluster", linked: true, people: 1}}},
		{name: "conflict", cases: []scenario{
			{name: "one unbound cluster", linked: true, unbound: true},
			{name: "two unbound clusters"},
			{name: "two people", people: 2},
			{name: "person and unbound", people: 1},
		}},
	} {
		t.Run(group.name, func(t *testing.T) {
			for _, scenario := range group.cases {
				t.Run(scenario.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					f := storetest.New(t)
					a := f.EnsureParticipant("a@example.test", "Alex Example", "example.test")
					b := f.EnsureParticipant("b@example.test", "Blair Example", "example.test")
					if scenario.linked {
						_, err := f.Store.LinkParticipants(a, b)
						require.NoError(err)
					}
					var existing *store.Person
					if scenario.people > 0 {
						var err error
						existing, _, err = f.Store.CreatePersonFromParticipantContext(t.Context(), a)
						require.NoError(err)
					}
					if scenario.people == 2 {
						_, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), b)
						require.NoError(err)
					}
					counts := personCreateCounts(t, f.Store)
					person, err := f.Store.CreateStandalonePersonContext(t.Context(), returnPersonInput("a@example.test", "b@example.test"))
					switch {
					case group.matched:
						require.NoError(err)
						assert.Equal(existing.ID, person.ID)
					case scenario.unbound:
						var conflict *store.PersonMatchesParticipantError
						require.ErrorAs(err, &conflict)
						assert.Equal(a, conflict.ParticipantID)
						assert.GreaterOrEqual(len(conflict.MatchBasis), 2)
						matchedParticipants := map[int64]bool{}
						for _, match := range conflict.MatchBasis {
							matchedParticipants[*match.ParticipantID] = true
						}
						assert.Equal(map[int64]bool{a: true, b: true}, matchedParticipants)
						assert.Equal(store.PersonCreateMCPNext{Tool: "promote_person", Arguments: map[string]int64{"participant_id": a}}, conflict.Next.MCP)
						assert.Equal([]string{"msgvault", "person", "promote", strconv.FormatInt(a, 10), "--json"}, conflict.Next.CLI)
					default:
						var conflict *store.PersonAmbiguousError
						require.ErrorAs(err, &conflict)
						assert.Len(conflict.Candidates, 2)
						assert.GreaterOrEqual(len(conflict.MatchBasis), 2)
					}
					assert.Equal(counts, personCreateCounts(t, f.Store))
				})
			}
		})
	}
}

func TestCreateIfExistsReturnEmailAndPhoneAmbiguous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	emailPerson := curatedContact(f, store.ContactAddressEmail, "alex@example.test")
	phonePerson := curatedContact(f, store.ContactAddressPhone, "+12025550123")
	counts := personCreateCounts(t, f.Store)
	input := returnPersonInput("alex@example.test")
	input.Phones = []store.PersonCreateContact{{Value: "+1 (202) 555-0123"}}
	_, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	var conflict *store.PersonAmbiguousError
	require.ErrorAs(err, &conflict)
	require.Len(conflict.Candidates, 2)
	assert.Equal(emailPerson, *conflict.Candidates[0].PersonID)
	assert.Equal(phonePerson, *conflict.Candidates[1].PersonID)
	assert.Equal(counts, personCreateCounts(t, f.Store))
}

func TestCreateIfExistsCuratedObservedOverlap(t *testing.T) {
	for _, scenario := range []string{"redundant unbound email", "bound to another person", "same cluster on uncurated phone"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			input := returnPersonInput("alex@example.test")
			saved, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
			require.NoError(err)
			var participantID, otherPersonID int64
			participantID, err = f.Store.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(err)
			if scenario == "bound to another person" {
				other, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
				require.NoError(err)
				otherPersonID = other.ID
			}
			if scenario == "same cluster on uncurated phone" {
				_, err := f.Store.RecordContactObservationContext(t.Context(), participantID, store.ParticipantContactObservationInput{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
				require.NoError(err)
			}
			if scenario == "same cluster on uncurated phone" {
				input.Phones = []store.PersonCreateContact{{Value: "+12025550123"}}
			}
			counts := personCreateCounts(t, f.Store)
			got, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
			var basis []store.PersonMatchBasis
			if scenario == "redundant unbound email" {
				require.NoError(err)
				require.NotNil(got.Ensure)
				assert.Equal(saved.ID, got.ID)
				assert.Equal(store.PersonEnsureMatchedExisting, got.Ensure.Status)
				require.Len(got.Ensure.MatchBasis, 1)
				assert.Equal("contact_point", got.Ensure.MatchBasis[0].Matched)
				assert.Empty(got.Ensure.UnappliedContacts)
				basis = got.Ensure.MatchBasis
			} else {
				var conflict *store.PersonAmbiguousError
				require.ErrorAs(err, &conflict)
				require.Len(conflict.Candidates, 2)
				assert.Equal(new(saved.ID), conflict.Candidates[0].PersonID)
				if otherPersonID != 0 {
					assert.Equal(new(otherPersonID), conflict.Candidates[1].PersonID)
				} else {
					assert.Equal(new(participantID), conflict.Candidates[1].ParticipantID)
					assert.Contains(conflict.MatchBasis, store.PersonMatchBasis{Kind: store.ContactAddressPhone, Value: "+12025550123", Matched: "observation", ParticipantID: new(participantID)})
				}
				basis = conflict.MatchBasis
			}
			for _, match := range basis {
				if match.Kind == store.ContactAddressEmail && match.ParticipantID != nil {
					assert.NotNil(match.PersonID, "curated email must not retain redundant unbound evidence")
				}
			}
			unchanged, err := f.Store.GetPersonContext(t.Context(), saved.ID)
			require.NoError(err)
			assert.Empty(unchanged.ParticipantIDs)
			assert.Equal(saved.Revision, unchanged.Revision)
			assert.Equal(saved.UpdatedAt, unchanged.UpdatedAt)
			assert.Equal(counts, personCreateCounts(t, f.Store))
			input.IfExists = ""
			_, err = f.Store.CreateStandalonePersonContext(t.Context(), input)
			var refusal *store.PersonContactExistsError
			require.ErrorAs(err, &refusal)
			assert.Equal(saved.ID, refusal.PersonID)
		})
	}
}
