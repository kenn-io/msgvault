package store_test

import (
	"encoding/json/v2"
	"fmt"
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
	f := storetest.New(t)
	input := returnPersonInput("alex@example.test")
	first, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	require.NoError(t, err)
	require.NotNil(t, first.Ensure)
	assert.Equal(t, store.PersonEnsureCreated, first.Ensure.Status)
	assert.Empty(t, first.Ensure.MatchBasis)
	counts := personCreateCounts(t, f.Store)
	input.Name, input.Org, input.Title, input.Note = "Ignored Name", "Ignored Company", "Ignored Title", "Ignored Note"
	input.Phones = []store.PersonCreateContact{{Value: "+12025550123", Type: "cell"}}
	second, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	require.NoError(t, err)
	require.NotNil(t, second.Ensure)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.DisplayName, second.DisplayName)
	assert.Equal(t, first.Revision, second.Revision)
	assert.Equal(t, first.UpdatedAt, second.UpdatedAt)
	assert.Equal(t, store.PersonEnsureMatchedExisting, second.Ensure.Status)
	assert.Equal(t, []store.PersonUnappliedContact{{Kind: store.ContactAddressPhone, Value: "+12025550123", Type: "cell"}}, second.Ensure.UnappliedContacts)
	assert.Equal(t, counts, personCreateCounts(t, f.Store))
}

func TestCreateIfExistsDefaultCompatibility(t *testing.T) {
	f := storetest.New(t)
	for _, mode := range []store.PersonIfExists{"", store.PersonIfExistsError} {
		input := store.PersonCreateInput{Name: "Alex Example", IfExists: mode}
		person, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
		require.NoError(t, err)
		require.Nil(t, person.Ensure)
		saved, err := f.Store.GetPersonContext(t.Context(), person.ID)
		require.NoError(t, err)
		got, err := json.Marshal(person)
		require.NoError(t, err)
		want, err := json.Marshal(saved)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got))
		assert.NotContains(t, string(got), "ensure")
	}
	id := curatedContact(f, store.ContactAddressEmail, "existing@example.test")
	input := returnPersonInput("existing@example.test")
	input.IfExists = store.PersonIfExistsError
	_, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	var conflict *store.PersonContactExistsError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, id, conflict.PersonID)
	assert.Equal(t, `email "existing@example.test" matches person `+fmt.Sprint(id)+` (Existing Example); use msgvault person get `+fmt.Sprint(id)+`; link observed participants explicitly through POST /api/v1/identity/links`, err.Error())
	_, err = f.Store.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{Name: "Alex", IfExists: "merge"})
	assert.ErrorIs(t, err, store.ErrPersonCreateInvalid)
}

func TestCreateIfExistsReturnConcurrent(t *testing.T) {
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
		require.NoError(t, result.err)
		require.NotNil(t, result.person.Ensure)
		statuses[result.person.Ensure.Status]++
	}
	assert.Equal(t, map[store.PersonEnsureStatus]int{store.PersonEnsureCreated: 1, store.PersonEnsureMatchedExisting: 1}, statuses)
	counts := personCreateCounts(t, f.Store)
	assert.Equal(t, []int{1, 1, 0, 0, 0}, counts)
}

func TestCreateIfExistsAllContactRepresentations(t *testing.T) {
	for _, representation := range []string{"contact_point", "participant", "identifier", "observation"} {
		for _, kind := range []store.ContactAddressKind{store.ContactAddressEmail, store.ContactAddressPhone} {
			t.Run(representation+"/"+string(kind), func(t *testing.T) {
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
						require.NoError(t, err)
					}
				case "identifier":
					var err error
					participantID, err = f.Store.EnsureParticipantByIdentifier("beeper", value, "Alex Example")
					require.NoError(t, err)
				case "observation":
					participantID = f.EnsureParticipant("primary@example.test", "Alex Example", "example.test")
					_, err := f.Store.RecordContactObservationContext(t.Context(), participantID, store.ParticipantContactObservationInput{AddressKind: kind, OriginalValue: value, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
					require.NoError(t, err)
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
					require.ErrorAs(t, err, &conflict)
					assert.Equal(t, participantID, conflict.ParticipantID)
					assert.Equal(t, counts, personCreateCounts(t, f.Store))
					person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
					require.NoError(t, err)
					personID = person.ID
				}
				counts := personCreateCounts(t, f.Store)
				person, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
				require.NoError(t, err)
				assert.Equal(t, personID, person.ID)
				require.NotNil(t, person.Ensure)
				basisFound := false
				for _, basis := range person.Ensure.MatchBasis {
					if basis.Matched == representation {
						basisFound = true
					}
					assert.Equal(t, personID, *basis.PersonID)
				}
				assert.True(t, basisFound)
				assert.Equal(t, counts, personCreateCounts(t, f.Store))
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
					f := storetest.New(t)
					a := f.EnsureParticipant("a@example.test", "Alex Example", "example.test")
					b := f.EnsureParticipant("b@example.test", "Blair Example", "example.test")
					if scenario.linked {
						_, err := f.Store.LinkParticipants(a, b)
						require.NoError(t, err)
					}
					var existing *store.Person
					if scenario.people > 0 {
						var err error
						existing, _, err = f.Store.CreatePersonFromParticipantContext(t.Context(), a)
						require.NoError(t, err)
					}
					if scenario.people == 2 {
						_, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), b)
						require.NoError(t, err)
					}
					counts := personCreateCounts(t, f.Store)
					person, err := f.Store.CreateStandalonePersonContext(t.Context(), returnPersonInput("a@example.test", "b@example.test"))
					switch {
					case group.matched:
						require.NoError(t, err)
						assert.Equal(t, existing.ID, person.ID)
					case scenario.unbound:
						var conflict *store.PersonMatchesParticipantError
						require.ErrorAs(t, err, &conflict)
						assert.Equal(t, a, conflict.ParticipantID)
						assert.GreaterOrEqual(t, len(conflict.MatchBasis), 2)
						matchedParticipants := map[int64]bool{}
						for _, match := range conflict.MatchBasis {
							matchedParticipants[*match.ParticipantID] = true
						}
						assert.Equal(t, map[int64]bool{a: true, b: true}, matchedParticipants)
						assert.Equal(t, store.PersonCreateMCPNext{Tool: "promote_person", Arguments: map[string]int64{"participant_id": a}}, conflict.Next.MCP)
						assert.Equal(t, []string{"msgvault", "person", "promote", fmt.Sprint(a), "--json"}, conflict.Next.CLI)
					default:
						var conflict *store.PersonAmbiguousError
						require.ErrorAs(t, err, &conflict)
						assert.Len(t, conflict.Candidates, 2)
						assert.GreaterOrEqual(t, len(conflict.MatchBasis), 2)
					}
					assert.Equal(t, counts, personCreateCounts(t, f.Store))
				})
			}
		})
	}
}

func TestCreateIfExistsReturnEmailAndPhoneAmbiguous(t *testing.T) {
	f := storetest.New(t)
	emailPerson := curatedContact(f, store.ContactAddressEmail, "alex@example.test")
	phonePerson := curatedContact(f, store.ContactAddressPhone, "+12025550123")
	counts := personCreateCounts(t, f.Store)
	input := returnPersonInput("alex@example.test")
	input.Phones = []store.PersonCreateContact{{Value: "+1 (202) 555-0123"}}
	_, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
	var conflict *store.PersonAmbiguousError
	require.ErrorAs(t, err, &conflict)
	require.Len(t, conflict.Candidates, 2)
	assert.Equal(t, emailPerson, *conflict.Candidates[0].PersonID)
	assert.Equal(t, phonePerson, *conflict.Candidates[1].PersonID)
	assert.Equal(t, counts, personCreateCounts(t, f.Store))
}

func TestCreateIfExistsCuratedObservedOverlap(t *testing.T) {
	for _, scenario := range []string{"redundant unbound email", "bound to another person", "same cluster on uncurated phone"} {
		t.Run(scenario, func(t *testing.T) {
			f := storetest.New(t)
			input := returnPersonInput("alex@example.test")
			saved, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
			require.NoError(t, err)
			var participantID, otherPersonID int64
			participantID, err = f.Store.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
			require.NoError(t, err)
			if scenario == "bound to another person" {
				other, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
				require.NoError(t, err)
				otherPersonID = other.ID
			}
			if scenario == "same cluster on uncurated phone" {
				_, err := f.Store.RecordContactObservationContext(t.Context(), participantID, store.ParticipantContactObservationInput{AddressKind: store.ContactAddressPhone, OriginalValue: "+12025550123", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
				require.NoError(t, err)
			}
			if scenario == "same cluster on uncurated phone" {
				input.Phones = []store.PersonCreateContact{{Value: "+12025550123"}}
			}
			counts := personCreateCounts(t, f.Store)
			got, err := f.Store.CreateStandalonePersonContext(t.Context(), input)
			var basis []store.PersonMatchBasis
			if scenario == "redundant unbound email" {
				require.NoError(t, err)
				require.NotNil(t, got.Ensure)
				assert.Equal(t, saved.ID, got.ID)
				assert.Equal(t, store.PersonEnsureMatchedExisting, got.Ensure.Status)
				require.Len(t, got.Ensure.MatchBasis, 1)
				assert.Equal(t, "contact_point", got.Ensure.MatchBasis[0].Matched)
				assert.Empty(t, got.Ensure.UnappliedContacts)
				basis = got.Ensure.MatchBasis
			} else {
				var conflict *store.PersonAmbiguousError
				require.ErrorAs(t, err, &conflict)
				require.Len(t, conflict.Candidates, 2)
				assert.Equal(t, new(saved.ID), conflict.Candidates[0].PersonID)
				if otherPersonID != 0 {
					assert.Equal(t, new(otherPersonID), conflict.Candidates[1].PersonID)
				} else {
					assert.Equal(t, new(participantID), conflict.Candidates[1].ParticipantID)
					assert.Contains(t, conflict.MatchBasis, store.PersonMatchBasis{Kind: store.ContactAddressPhone, Value: "+12025550123", Matched: "observation", ParticipantID: new(participantID)})
				}
				basis = conflict.MatchBasis
			}
			for _, match := range basis {
				if match.Kind == store.ContactAddressEmail && match.ParticipantID != nil {
					assert.NotNil(t, match.PersonID, "curated email must not retain redundant unbound evidence")
				}
			}
			unchanged, err := f.Store.GetPersonContext(t.Context(), saved.ID)
			require.NoError(t, err)
			assert.Empty(t, unchanged.ParticipantIDs)
			assert.Equal(t, saved.Revision, unchanged.Revision)
			assert.Equal(t, saved.UpdatedAt, unchanged.UpdatedAt)
			assert.Equal(t, counts, personCreateCounts(t, f.Store))
			input.IfExists = ""
			_, err = f.Store.CreateStandalonePersonContext(t.Context(), input)
			var refusal *store.PersonContactExistsError
			require.ErrorAs(t, err, &refusal)
			assert.Equal(t, saved.ID, refusal.PersonID)
		})
	}
}
