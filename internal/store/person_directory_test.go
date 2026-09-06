package store_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// This catches a directory query that ignores its typo-tolerant lexical
// matching or any of its conjunctive profile filters.
func TestDirectoryPeoplePageContextFiltersRanksAndPaginates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")
	createDirectoryPerson(t, st, "Alicia Example", "alicia@example.test", "friend", "active", "Acme")
	createDirectoryPerson(t, st, "Alice Other", "other@example.test", "colleague", "active", "Other Co")

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
		Query: "alcie", Category: "friend", Organization: "Acme", ContactState: "active", Limit: 1,
	})
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal(alice.ID, page.People[0].ID)
	assert.Equal([]string{"friend"}, page.People[0].Categories)
	assert.Equal([]string{"Acme"}, page.People[0].Organizations)
	assert.Empty(page.NextCursor)
}

// This catches a sort regression where one-edit typo matches outrank exact or
// prefix matches, which would make a Directory result order unpredictable.
func TestDirectoryPeoplePageContextRanksExactAndPrefixBeforeFuzzyMatches(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")
	alicf := createDirectoryPerson(t, st, "Alicf Example", "alicf@example.test", "friend", "active", "Acme")

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "alice"})
	require.NoError(t, err)
	require.Len(t, page.People, 2)
	assert.Equal(t, []int64{alice.ID, alicf.ID}, directoryPersonIDs(page.People))
}

// This catches backend-specific SQL lowercasing: Directory matching must use
// the same Go-canonical Unicode token representation on every backend.
func TestDirectoryPeoplePageContextMatchesUnicodeCaseFoldedTokens(t *testing.T) {
	st := testutil.NewTestStore(t)
	emile := createDirectoryPerson(t, st, "Émile Example", "emile@example.test", "friend", "active", "Acme")

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "émile"})
	require.NoError(t, err)
	assert.Equal(t, []int64{emile.ID}, directoryPersonIDs(page.People))
}

// This catches canonical-equivalence regressions across every persisted key:
// composed and decomposed spellings must qualify, filter, sort, and resume
// through the same cursor sequence on the configured backend.
func TestDirectoryPeoplePageContextNormalizesCanonicalUnicodeAcrossKeys(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	composed := createDirectoryPerson(t, st, "Ålpha", "unicode-first@example.test", "Café", "active", "Ångström")
	decomposed := createDirectoryPerson(t, st, "A\u030Alpha", "unicode-second@example.test", "Cafe\u0301", "active", "A\u030Angstro\u0308m")

	for _, query := range []string{"ålpha", "a\u030alpha"} {
		page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
			Query: query, Category: "Cafe\u0301", Organization: "Ångström",
		})
		require.NoError(err)
		assert.Equal([]int64{composed.ID, decomposed.ID}, directoryPersonIDs(page.People))
	}

	first, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.NotEmpty(first.NextCursor)
	second, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
		Limit: 1, Cursor: first.NextCursor,
	})
	require.NoError(err)
	assert.Equal([]int64{composed.ID, decomposed.ID},
		append(directoryPersonIDs(first.People), directoryPersonIDs(second.People)...))
}

// This catches a search path that recognizes only a fixed punctuation list
// instead of the Unicode-aware lexical token boundaries used by Directory.
func TestDirectoryPeoplePageContextMatchesPunctuationDelimitedTokens(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := createDirectoryPerson(t, st, "Alice|Example", "alice@sample.test", "friend", "active", "Acme")

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "example"})
	require.NoError(t, err)
	assert.Equal(t, []int64{alice.ID}, directoryPersonIDs(page.People))
}

// This catches keyset cursors that skip or duplicate rows, and cursors that
// accidentally permit a different normalized filter set.
func TestDirectoryPeoplePageContextPaginatesAndRejectsForeignCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	alice := createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")
	bob := createDirectoryPerson(t, st, "Bob Example", "bob@example.test", "friend", "active", "Acme")
	carol := createDirectoryPerson(t, st, "Carol Example", "carol@example.test", "friend", "active", "Acme")

	first, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.Len(first.People, 1)
	assert.Equal(alice.ID, first.People[0].ID)
	require.NotEmpty(first.NextCursor)

	second, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: first.NextCursor})
	require.NoError(err)
	require.Len(second.People, 1)
	assert.Equal(bob.ID, second.People[0].ID)
	require.NotEmpty(second.NextCursor)

	third, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: second.NextCursor})
	require.NoError(err)
	require.Len(third.People, 1)
	assert.Equal(carol.ID, third.People[0].ID)
	assert.Empty(third.NextCursor)

	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "alice", Limit: 1, Cursor: first.NextCursor})
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Cursor: "not-a-directory-cursor"})
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
}

// This catches a cursor whose persisted SQL order key differs from the key
// encoded on page one. Unicode folding and repeated whitespace must still
// advance a limit-one sequence without a skipped row.
func TestDirectoryPeoplePageContextUsesCanonicalOrderKeyAcrossPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	first := createDirectoryPerson(t, st, "Ålice  a", "first@sample.test", "friend", "active", "Acme")
	second := createDirectoryPerson(t, st, "Ålice z", "second@sample.test", "friend", "active", "Acme")

	pageOne, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.NotEmpty(pageOne.NextCursor)
	pageTwo, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: pageOne.NextCursor})
	require.NoError(err)
	assert.Equal([]int64{first.ID}, directoryPersonIDs(pageOne.People))
	assert.Equal([]int64{second.ID}, directoryPersonIDs(pageTwo.People))
}

// This catches a cursor that becomes oversized when the persisted canonical
// order key is derived from an otherwise valid long display name.
func TestDirectoryPeoplePageContextLongNameCursorRoundTrip(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first := createDirectoryPerson(t, st, strings.Repeat("a", 600), "long-first@sample.test", "friend", "active", "Acme")
	second := createDirectoryPerson(t, st, "Zed", "long-second@sample.test", "friend", "active", "Acme")

	pageOne, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.NotEmpty(pageOne.NextCursor)
	pageTwo, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: pageOne.NextCursor})
	require.NoError(err)
	assert.Equal(t, []int64{first.ID, second.ID}, append(directoryPersonIDs(pageOne.People), directoryPersonIDs(pageTwo.People)...))
}

// This catches an ID-only cursor that silently resumes using an anchor whose
// persisted order key changed after the prior page was generated.
func TestDirectoryPeoplePageContextRejectsCursorAfterAnchorOrderMutation(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first := createDirectoryPerson(t, st, "Alice", "mutation-first@sample.test", "friend", "active", "Acme")
	createDirectoryPerson(t, st, "Bob", "mutation-second@sample.test", "friend", "active", "Acme")

	pageOne, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.NotEmpty(pageOne.NextCursor)
	current, err := st.GetPersonContext(t.Context(), first.ID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), first.ID, current.Revision, new("Zed"))
	require.NoError(err)

	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: pageOne.NextCursor})
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
}

// This catches a projection that is not refreshed after direct bulk mutation
// paths which the dirty triggers centralize for Store reads.
func TestDirectoryPeoplePageContextRefreshesOrganizationAndContactState(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createDirectoryPerson(t, st, "Alice Example", "alice@sample.test", "friend", "inactive", "Acme")
	ctx := t.Context()
	_, err := st.DB().ExecContext(ctx, `UPDATE organizations SET name = 'Renamed Org' WHERE name = 'Acme'`)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_contact_state (person_id, last_contact_at, last_contact_channel, interaction_count) VALUES (?, CURRENT_TIMESTAMP, 'chat', 1)`), person.ID)
	require.NoError(err)

	page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Organization: "renamed org", ContactState: "active", PrimaryChannel: "chat"})
	require.NoError(err)
	assert.Equal(t, []int64{person.ID}, directoryPersonIDs(page.People))
}

// This catches a projection refresh that inserts one organization filter per
// employment row instead of one normalized filter per person and organization.
func TestDirectoryProjectionDeduplicatesOrganizationAcrossCurrentEmployments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	person := createDirectoryPerson(t, st, "Alice Example", "alice@sample.test", "friend", "inactive", "Acme")
	employments, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{
		PersonID: person.ID, CurrentOnly: true,
	})
	require.NoError(err)
	require.Len(employments, 1)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO employments (
		person_id, organization_id, title, title_normalized, is_current, source
	) VALUES (?, ?, 'Advisor', 'advisor', ?, 'user')`), person.ID, employments[0].OrganizationID, true)
	require.NoError(err)
	require.NoError(st.RefreshDirectoryProjectionContext(t.Context()))
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Organization: "Acme"})
	require.NoError(err)
	assert.Equal([]int64{person.ID}, directoryPersonIDs(page.People))
	var filterCount int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM directory_person_filters
		WHERE person_id = ? AND filter_kind = 'organization'`), person.ID).Scan(&filterCount))
	assert.Equal(int64(1), filterCount)
}

// This catches a query path that treats an empty qualified result as an error
// or emits a cursor without a corresponding person.
func TestDirectoryPeoplePageContextReturnsEmptyPage(t *testing.T) {
	st := testutil.NewTestStore(t)
	createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")

	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "no-such-person"})
	require.NoError(t, err)
	assert.Empty(t, page.People)
	assert.Empty(t, page.NextCursor)
}

// This catches a directory filter that uses a contact-point service instead
// of the primary activity channel from the contact-state projection.
func TestDirectoryPeoplePageContextFiltersPrimaryChannel(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	alice := createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")
	bob := createDirectoryPerson(t, st, "Bob Example", "bob@example.test", "friend", "active", "Acme")
	_, err := st.DB().ExecContext(ctx,
		st.Rebind(`UPDATE person_contact_state SET last_contact_channel = 'email' WHERE person_id = ?`), alice.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx,
		st.Rebind(`UPDATE person_contact_state SET last_contact_channel = 'chat' WHERE person_id = ?`), bob.ID)
	require.NoError(err)

	page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{PrimaryChannel: " email "})
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal(t, alice.ID, page.People[0].ID)
}

// This catches cursors that decode as JSON but do not contain the normalized
// ordering tuple emitted by the store.
func TestDirectoryPeoplePageContextRejectsMalformedOrderingCursor(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	createDirectoryPerson(t, st, "Alice Example", "alice@example.test", "friend", "active", "Acme")
	createDirectoryPerson(t, st, "Bob Example", "bob@example.test", "friend", "active", "Acme")
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1})
	require.NoError(err)
	require.NotEmpty(page.NextCursor)

	encoded, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	require.NoError(err)
	var cursor map[string]any
	require.NoError(json.Unmarshal(encoded, &cursor))
	cursor["display_name"] = " Alice Example "
	encoded, err = json.Marshal(cursor)
	require.NoError(err)
	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
		Limit: 1, Cursor: base64.RawURLEncoding.EncodeToString(encoded),
	})
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
}

// This catches a cursor whose relevance tier is changed while its person and
// display-name ordering anchor remain valid. The complete ordering tuple must
// still describe the current search projection before pagination resumes.
func TestDirectoryPeoplePageContextRejectsChangedMatchQualityCursor(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	createDirectoryPerson(t, st, "Alice", "alice-exact@example.test", "friend", "active", "Acme")
	createDirectoryPerson(t, st, "Alicef", "alice-prefix@example.test", "friend", "active", "Acme")
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Query: "alice", Limit: 1})
	require.NoError(err)
	require.NotEmpty(page.NextCursor)

	encoded, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	require.NoError(err)
	var cursor map[string]any
	require.NoError(json.Unmarshal(encoded, &cursor))
	cursor["quality"] = float64(2)
	encoded, err = json.Marshal(cursor)
	require.NoError(err)
	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{
		Query: "alice", Limit: 1, Cursor: base64.RawURLEncoding.EncodeToString(encoded),
	})
	require.ErrorIs(err, store.ErrInvalidDirectoryCursor)
}

func directoryPersonIDs(people []store.DirectoryPersonSummary) []int64 {
	ids := make([]int64, 0, len(people))
	for _, person := range people {
		ids = append(ids, person.ID)
	}
	return ids
}

func createDirectoryPerson(
	t *testing.T,
	st *store.Store,
	displayName, email, category, contactState, organizationName string,
) *store.Person {
	t.Helper()
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier("email", email, displayName)
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
	require.NoError(t, err)
	person, err = st.UpdatePersonDisplayNameContext(ctx, person.ID, person.Revision, &displayName)
	require.NoError(t, err)
	_, err = st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: &displayName, OriginalValue: displayName,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	_, err = st.AddPersonContactPointContext(ctx, person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: email,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	_, err = st.AddPersonCategoryContext(ctx, person.ID, store.PersonCategoryInput{
		OriginalValue: category, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: organizationName, Kind: store.OrganizationKindCompany,
	})
	require.NoError(t, err)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceUser,
	})
	require.NoError(t, err)
	if contactState == "active" {
		_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_contact_state (
			person_id, last_contact_at, interaction_count
		) VALUES (?, CURRENT_TIMESTAMP, 1)`), person.ID)
		require.NoError(t, err)
	}
	return person
}
