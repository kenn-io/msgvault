package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Dropping any name lane or choosing the first duplicate must fail this test.
func TestContactCandidatesPreserveDuplicatePeopleAndPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	people := []*store.Person{}
	for i := range 3 {
		pid := f.EnsureParticipant(fmt.Sprintf("candidate%d@example.test", i), "Avery Example", "example.test")
		person, _, err := f.Store.CreatePersonFromParticipant(pid)
		requirements.NoError(err)
		people = append(people, person)
	}
	_, err := f.Store.EnsureParticipantByIdentifier("email", "unbound@example.test", "Avery Example")
	requirements.NoError(err)
	page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: "Avery Example", Limit: 1})
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.Equal(people[0].VCardUID, page.Candidates[0].PersonUID)
	assertions.True(page.Ambiguous)
	assertions.True(page.HasMore)
	next, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: "Avery Example", Limit: 100, AfterID: page.NextAfterID})
	requirements.NoError(err)
	requirements.Len(next.Candidates, 2)
	assertions.Equal(people[1].ID, next.Candidates[0].PersonID)
	assertions.False(next.HasMore)
}

func TestContactCandidatesTreatWildcardsLiterally(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	for i, name := range []string{"Percent %_\\ Example", "Plain Example"} {
		id := f.EnsureParticipant(fmt.Sprintf("literal%d@example.test", i), name, "example.test")
		_, _, err := f.Store.CreatePersonFromParticipant(id)
		requirements.NoError(err)
	}
	page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: `%_\`})
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.Equal(`Percent %_\ Example`, page.Candidates[0].DisplayName)
}

func TestValidateContactCandidateQueryBoundaries(t *testing.T) {
	tokens := func(n int) string { return strings.TrimSpace(strings.Repeat("a ", n)) }
	for _, test := range []struct {
		name  string
		query store.ContactCandidateQuery
		valid bool
	}{
		{"one token", store.ContactCandidateQuery{Query: "Avery"}, true},
		{"empty", store.ContactCandidateQuery{Query: ""}, false},
		{"whitespace only", store.ContactCandidateQuery{Query: " \t "}, false},
		{"invalid UTF-8", store.ContactCandidateQuery{Query: string([]byte{0xff})}, false},
		{"256 bytes", store.ContactCandidateQuery{Query: strings.Repeat("a", 256)}, true},
		{"257 bytes", store.ContactCandidateQuery{Query: strings.Repeat("a", 257)}, false},
		{"256 bytes after trimming", store.ContactCandidateQuery{Query: "  " + strings.Repeat("a", 256) + "  "}, true},
		{"16 tokens", store.ContactCandidateQuery{Query: tokens(16)}, true},
		{"17 tokens", store.ContactCandidateQuery{Query: tokens(17)}, false},
		{"default limit", store.ContactCandidateQuery{Query: "a", Limit: 0}, true},
		{"limit 100", store.ContactCandidateQuery{Query: "a", Limit: 100}, true},
		{"limit 101", store.ContactCandidateQuery{Query: "a", Limit: 101}, false},
		{"negative limit", store.ContactCandidateQuery{Query: "a", Limit: -1}, false},
		{"negative cursor", store.ContactCandidateQuery{Query: "a", AfterID: -1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := store.ValidateContactCandidateQuery(test.query)
			if test.valid {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, store.ErrInvalidContactLookup)
			}
		})
	}
}

func TestContactCandidatesUseCurrentNamesAndBoundObservedNames(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	pid := f.EnsureParticipant("named@example.test", "Observed Alias", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), pid, new("Saved Name"))
	requirements.NoError(err)
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameNickname, Formatted: new("Curated Nickname"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	for _, query := range []string{"saved", "observed", "nickname"} {
		page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: query})
		requirements.NoError(err)
		requirements.Len(page.Candidates, 1)
		assertions.Equal(person.ID, page.Candidates[0].PersonID)
	}
}

func TestContactCandidatesMatchBoundObservedNameAfterSavedNameCleared(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	participant := f.EnsureParticipant("cleared-name@example.test", "Observed Alias", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantWithDisplayNameContext(
		t.Context(), participant, new("Saved Name"),
	)
	requirements.NoError(err)
	person, err = f.Store.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, nil)
	requirements.NoError(err)

	page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{
		Query: "Observed Alias",
	})
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.Equal(person.ID, page.Candidates[0].PersonID)
	assertions.Empty(page.Candidates[0].DisplayName)
	assertions.Contains(page.Candidates[0].MatchKinds, "bound_observed_name")
}

func TestContactCandidatesMatchStructuredNameComponents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	pid := f.EnsureParticipant("structured-name@example.test", "Participant Label", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{
		NameKind:         store.PersonNameStructured,
		Formatted:        new("Avery Example"),
		GivenName:        new("Avery"),
		AdditionalNames:  new("Jordan"),
		FamilyName:       new("Example"),
		SecondarySurname: new("Quinn"),
		Envelope:         store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)

	for _, query := range []string{"Jordan", "Quinn"} {
		page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: query})
		requirements.NoError(err)
		requirements.Len(page.Candidates, 1, "query %q", query)
		assertions.Equal(person.ID, page.Candidates[0].PersonID)
		assertions.Contains(page.Candidates[0].MatchKinds, "curated_name")
	}
}

func TestContactCandidatesPreserveUnicodeSpelling(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	pid := f.EnsureParticipant("unicode@example.test", "Éxample Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: "Éxample"})
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.Equal(person.VCardUID, page.Candidates[0].PersonUID)
}

func TestContactCandidatesReportEveryMatchingNameLane(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	pid := f.EnsureParticipant("mixed-name@example.test", "Observed Alias", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), pid, new("Saved Name"))
	requirements.NoError(err)
	_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameNickname, Formatted: new("Curated Nickname"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: "saved nickname"})
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.Contains(page.Candidates[0].MatchKinds, "saved_name")
	assertions.Contains(page.Candidates[0].MatchKinds, "curated_name")
}

// Header display names are unbounded per participant, so candidate search
// reads only saved, curated and bound participant names.
func TestContactCandidatesIgnoreNamesSeenOnlyInMessageHeaders(t *testing.T) {
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	participant := f.EnsureParticipant("header-only@example.test", "Current Name", "example.test")
	_, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participant)
	requirements.NoError(err)
	messageID := f.NewMessage().WithSourceMessageID("header-name").Create(t, f.Store)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients
		(message_id, participant_id, recipient_type, display_name) VALUES (?, ?, 'from', 'Header Nickname')`),
		messageID, participant)
	requirements.NoError(err)

	page, err := f.Store.FindContactCandidatesContext(t.Context(), store.ContactCandidateQuery{Query: "Header Nickname"})
	requirements.NoError(err)
	requirements.Empty(page.Candidates)
	requirements.False(page.Ambiguous)
}
