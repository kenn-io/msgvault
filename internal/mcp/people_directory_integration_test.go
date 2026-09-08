package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPListDirectoryPeopleRecentContacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	recentAt := time.Date(2026, 8, 22, 12, 30, 0, 123456789, time.UTC)
	equalAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	olderAt := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	recent := createDirectoryIntegrationPerson(t, st, "Recent Person", "recent@example.test", "friend", "Example Org", &recentAt)
	equalFirst := createDirectoryIntegrationPerson(t, st, "Equal First", "equal-first@example.test", "friend", "Example Org", &equalAt)
	equalSecond := createDirectoryIntegrationPerson(t, st, "Equal Second", "equal-second@example.test", "friend", "Example Org", &equalAt)
	older := createDirectoryIntegrationPerson(t, st, "Older Person", "older@example.test", "colleague", "Other Org", &olderAt)
	never := createDirectoryIntegrationPerson(t, st, "Never Contacted", "never@example.test", "friend", "Example Org", nil)
	observedOnly, err := st.EnsureParticipantByIdentifier("email", "observed@example.test", "Observed Only")
	require.NoError(err)
	require.NotZero(observedOnly)
	require.NoError(st.RefreshDirectoryProjectionContext(t.Context()))

	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st,
		Logger: slog.New(slog.DiscardHandler),
	})
	daemonHTTP := httptest.NewServer(daemon.Router())
	t.Cleanup(daemonHTTP.Close)
	engine, err := daemonclient.NewEngine(daemonclient.Config{URL: daemonHTTP.URL, AllowInsecure: true})
	require.NoError(err)
	people := daemonclient.NewPeopleBrowser(engine)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PeopleBackend: people, DirectoryBackend: people}

	first := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"sort": "last_contact_desc", "limit": 1,
		},
	})
	firstPage := decodeDirectoryToolPage(t, first.Result)
	require.Len(firstPage.People, 1)
	assert.Equal(recent.ID, firstPage.People[0].ID)
	assert.Equal("Recent Person", *firstPage.People[0].DisplayName)
	assert.Equal("active", firstPage.People[0].ContactState)
	assert.Equal(recentAt, *firstPage.People[0].LastContactAt)
	assert.Equal([]string{"friend"}, firstPage.People[0].Categories)
	assert.Equal([]string{"Example Org"}, firstPage.People[0].Organizations)
	require.NotEmpty(firstPage.NextCursor)

	second := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"sort": "last_contact_desc", "limit": 1, "cursor": firstPage.NextCursor,
		},
	})
	secondPage := decodeDirectoryToolPage(t, second.Result)
	require.Len(secondPage.People, 1)
	assert.Contains([]int64{equalFirst.ID, equalSecond.ID}, secondPage.People[0].ID)
	require.NotEmpty(secondPage.NextCursor)

	changed := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"sort": "last_contact_desc", "limit": 1, "cursor": firstPage.NextCursor, "category": "colleague",
		},
	})
	changedIsError, ok := changed.Result["isError"].(bool)
	require.True(ok)
	assert.True(changedIsError)
	assert.Contains(toolErrorTextFromResult(t, changed.Result), "invalid_cursor")

	inclusive := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"sort": "last_contact_desc", "last_contact_after": recentAt.Format(time.RFC3339Nano),
			"last_contact_before": recentAt.Format(time.RFC3339Nano),
		},
	})
	inclusivePage := decodeDirectoryToolPage(t, inclusive.Result)
	require.Len(inclusivePage.People, 1)
	assert.Equal(recent.ID, inclusivePage.People[0].ID)

	dateOnly := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"last_contact_after": "2026-08-20", "last_contact_before": "2026-08-22", "limit": 1,
		},
	})
	dateOnlyPage := decodeDirectoryToolPage(t, dateOnly.Result)
	require.Len(dateOnlyPage.People, 1)
	assert.Contains([]int64{equalFirst.ID, equalSecond.ID}, dateOnlyPage.People[0].ID)
	require.NotEmpty(dateOnlyPage.NextCursor)

	dateOnlyNext := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"last_contact_after": "2026-08-20T00:00:00Z", "last_contact_before": "2026-08-22T00:00:00Z",
			"limit": 1, "cursor": dateOnlyPage.NextCursor,
		},
	})
	dateOnlyNextPage := decodeDirectoryToolPage(t, dateOnlyNext.Result)
	require.Len(dateOnlyNextPage.People, 1)
	assert.ElementsMatch([]int64{equalFirst.ID, equalSecond.ID}, []int64{dateOnlyPage.People[0].ID, dateOnlyNextPage.People[0].ID})
	assert.Empty(dateOnlyNextPage.NextCursor)

	reversed := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"last_contact_after":  recentAt.Add(time.Hour).Format(time.RFC3339Nano),
			"last_contact_before": recentAt.Format(time.RFC3339Nano),
		},
	})
	reversedIsError, ok := reversed.Result["isError"].(bool)
	require.True(ok)
	assert.True(reversedIsError)
	assert.Contains(toolErrorTextFromResult(t, reversed.Result), "invalid_query")

	empty := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{"query": "no-such-person"},
	})
	emptyPage := decodeDirectoryToolPage(t, empty.Result)
	assert.Empty(emptyPage.People)

	all := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{
		"name": "list_directory_people", "arguments": map[string]any{
			"sort": "last_contact_desc", "limit": 100,
		},
	})
	allPage := decodeDirectoryToolPage(t, all.Result)
	ids := make([]int64, 0, len(allPage.People))
	for _, person := range allPage.People {
		ids = append(ids, person.ID)
	}
	for i := 1; i < len(allPage.People); i++ {
		previous, current := allPage.People[i-1].LastContactAt, allPage.People[i].LastContactAt
		if previous == nil {
			assert.Nil(current)
			continue
		}
		if current != nil {
			assert.False(current.After(*previous))
		}
	}
	assert.Contains(ids, recent.ID)
	assert.Contains(ids, older.ID)
	assert.Contains(ids, never.ID)
	assert.ElementsMatch([]int64{recent.ID, equalFirst.ID, equalSecond.ID, older.ID, never.ID}, ids)
	assert.Equal("inactive", findDirectoryPerson(allPage.People, never.ID).ContactState)
	assert.Nil(findDirectoryPerson(allPage.People, never.ID).LastContactAt)
}

func createDirectoryIntegrationPerson(
	t *testing.T,
	st *store.Store,
	displayName, email, category, organizationName string,
	lastContactAt *time.Time,
) *store.Person {
	t.Helper()
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier("email", email, displayName)
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
	require.NoError(t, err)
	person, err = st.UpdatePersonDisplayNameContext(ctx, person.ID, person.Revision, &displayName)
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
	if lastContactAt != nil {
		_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO person_contact_state (
			person_id, last_contact_at, interaction_count
		) VALUES (?, ?, 1)`), person.ID, lastContactAt)
		require.NoError(t, err)
	}
	return person
}

func decodeDirectoryToolPage(t *testing.T, result map[string]any) store.DirectoryPeoplePage {
	t.Helper()
	assert.NotEqual(t, true, result["isError"], "result: %#v", result)
	structured := toolStructuredContent(t, result)
	data, err := json.Marshal(structured)
	require.NoError(t, err)
	var page store.DirectoryPeoplePage
	require.NoError(t, json.Unmarshal(data, &page))
	if page.People == nil {
		page.People = []store.DirectoryPersonSummary{}
	}
	return page
}

func findDirectoryPerson(people []store.DirectoryPersonSummary, id int64) store.DirectoryPersonSummary {
	for _, person := range people {
		if person.ID == id {
			return person
		}
	}
	return store.DirectoryPersonSummary{}
}
