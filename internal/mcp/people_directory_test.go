package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type directoryPeopleBackend struct {
	peoplebrowser.Backend

	page  *store.DirectoryPeoplePage
	err   error
	query store.DirectoryPeopleQuery
}

func (b *directoryPeopleBackend) ListDirectoryPeople(
	_ context.Context,
	query store.DirectoryPeopleQuery,
) (*store.DirectoryPeoplePage, error) {
	b.query = query
	return b.page, b.err
}

func directoryToolOptions(backend peoplebrowser.Backend) ServeOptions {
	opts := ServeOptions{Engine: &querytest.MockEngine{}, PeopleBackend: backend}
	if lister, ok := backend.(peoplebrowser.DirectoryLister); ok {
		opts.DirectoryBackend = lister
	}
	return opts
}

func TestMCPListDirectoryPeopleForwardsQueryAndReturnsRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	after := time.Date(2026, 8, 20, 12, 30, 0, 123456789, time.UTC)
	before := after.Add(24 * time.Hour)
	backend := &directoryPeopleBackend{page: &store.DirectoryPeoplePage{
		People: []store.DirectoryPersonSummary{{
			ID: 7, DisplayName: new("Alice Example"), Revision: 4,
			PrimaryChannel: "chat", ContactState: "active", LastContactAt: &after,
			Categories: []string{"friend"}, Organizations: []string{"Example Org"},
		}},
		NextCursor: "opaque/cursor+bytes",
	}}
	result := rawCallTool(t, directoryToolOptions(backend), ToolListDirectoryPeople, map[string]any{
		"query": "Alice", "cursor": "opaque/cursor+bytes", "limit": 7,
		"sort": "last_contact_asc", "last_contact_after": after.Format(time.RFC3339Nano),
		"last_contact_before": before.Format(time.RFC3339Nano), "contact_state": "active",
		"category": "friend", "organization": "Example Org", "primary_channel": "chat",
	})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(store.DirectoryPeopleQuery{
		Query: "Alice", Cursor: "opaque/cursor+bytes", Limit: 7,
		Sort: store.DirectoryPeopleSortLastContactAsc, LastContactAfter: &after,
		LastContactBefore: &before, ContactState: "active", Category: "friend",
		Organization: "Example Org", PrimaryChannel: "chat",
	}, backend.query)
	structured := toolStructuredContent(t, result)
	assert.Equal("opaque/cursor+bytes", structured["next_cursor"])
	people, ok := structured["people"].([]any)
	require.True(ok)
	require.Len(people, 1)
	row, ok := people[0].(map[string]any)
	require.True(ok)
	assert.InDelta(7, row["id"], 0)
	assert.Equal("Alice Example", row["display_name"])
	assert.InDelta(4, row["revision"], 0)
	assert.Equal("2026-08-20T12:30:00.123456789Z", row["last_contact_at"])
	assert.Equal([]any{"friend"}, row["categories"])
	assert.Equal([]any{"Example Org"}, row["organizations"])
}

func TestMCPListDirectoryPeopleUsesStoreDefaultsAndAcceptsEmptyPage(t *testing.T) {
	assert := assert.New(t)
	backend := &directoryPeopleBackend{page: &store.DirectoryPeoplePage{People: []store.DirectoryPersonSummary{}}}
	result := rawCallTool(t, directoryToolOptions(backend), ToolListDirectoryPeople, map[string]any{})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Equal(store.DefaultDirectoryPeopleLimit, backend.query.Limit)
	assert.Equal(store.DirectoryPeopleSortLastContactDesc, backend.query.Sort)
	structured := toolStructuredContent(t, result)
	assert.Equal([]any{}, structured["people"])
	assert.NotContains(structured, "next_cursor")

	result = rawCallTool(t, directoryToolOptions(backend), ToolListDirectoryPeople, map[string]any{"limit": 0})
	assert.NotEqual(true, result["isError"], "result: %#v", result)
	assert.Zero(backend.query.Limit)
}

func TestMCPListDirectoryPeopleParsesDatesBeforeBackend(t *testing.T) {
	backend := &directoryPeopleBackend{page: &store.DirectoryPeoplePage{People: []store.DirectoryPersonSummary{}}}
	result := rawCallTool(t, directoryToolOptions(backend), ToolListDirectoryPeople, map[string]any{
		"last_contact_after": "not-a-timestamp",
	})
	assert.Equal(t, true, result["isError"])
	assert.Contains(t, toolErrorTextFromResult(t, result), "last_contact_after must be RFC3339")
	assert.Zero(t, backend.query)
}

func TestMCPListDirectoryPeopleMapsKnownErrors(t *testing.T) {
	for _, test := range []struct {
		code string
		want string
	}{
		{code: "invalid_cursor", want: "invalid_cursor: directory cursor is invalid"},
		{code: "invalid_query", want: "invalid_query: directory query is invalid"},
		{code: "directory_projection_stale", want: "directory_projection_stale: directory data is refreshing"},
	} {
		t.Run(test.code, func(t *testing.T) {
			backend := &directoryPeopleBackend{err: directoryPeopleError{code: test.code}}
			result := rawCallTool(t, directoryToolOptions(backend), ToolListDirectoryPeople, map[string]any{})
			assert.Equal(t, true, result["isError"])
			assert.Contains(t, toolErrorTextFromResult(t, result), test.want)
		})
	}
}

func TestMCPListDirectoryPeopleSanitizesUnavailableAndNilPages(t *testing.T) {
	for _, test := range []struct {
		name string
		page *store.DirectoryPeoplePage
		err  error
	}{
		{name: "unavailable", err: directoryPeopleError{code: "directory_unavailable"}},
		{name: "cancellation", err: context.Canceled},
		{name: "nil page", page: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &directoryPeopleBackend{page: test.page, err: test.err}
			h := &handlers{engine: &querytest.MockEngine{}, directoryBackend: backend}
			result, err := h.listDirectoryPeople(t.Context(), toolRequest{arguments: map[string]any{}})
			require.Error(t, err)
			assert.Nil(t, result)
			var internal *internalError
			assert.ErrorAs(t, err, &internal)
		})
	}
}

func TestMCPListDirectoryPeopleRequiresDirectoryLister(t *testing.T) {
	require := require.New(t)
	result, err := (&handlers{
		engine: &querytest.MockEngine{},
	}).listDirectoryPeople(t.Context(), toolRequest{arguments: map[string]any{}})
	require.Error(err)
	assert.Nil(t, result)
	var internal *internalError
	require.ErrorAs(err, &internal)
}

type directoryPeopleError struct{ code string }

func (e directoryPeopleError) Error() string        { return e.code }
func (e directoryPeopleError) APIErrorCode() string { return e.code }

type directoryCapabilityOnlyBackend struct{ peoplebrowser.Backend }

type profileOnlyPeopleBackend struct {
	peoplebrowser.Backend

	profiles []store.Person
	profile  *peoplebrowser.PersonProfile
}

func (b *profileOnlyPeopleBackend) Search(context.Context, peoplebrowser.SearchRequest) (*peoplebrowser.SearchPage, error) {
	return &peoplebrowser.SearchPage{}, nil
}

func (b *profileOnlyPeopleBackend) ListProfiles(context.Context) ([]store.Person, error) {
	return b.profiles, nil
}

func (b *profileOnlyPeopleBackend) GetPersonProfile(context.Context, int64) (*peoplebrowser.PersonProfile, error) {
	return b.profile, nil
}

func TestMCPExistingPeopleToolsWorkWithoutDirectoryBackend(t *testing.T) {
	displayName := "Profile Person"
	backend := &profileOnlyPeopleBackend{
		profiles: []store.Person{{ID: 7, DisplayName: &displayName}},
		profile:  &peoplebrowser.PersonProfile{Person: store.Person{ID: 7, DisplayName: &displayName}},
	}
	options := peopleToolOptions(backend)
	assert := assert.New(t)

	tools := toolsByName(t, rawListTools(t, options, false))
	assert.Contains(tools, ToolSearchPeople)
	assert.Contains(tools, ToolGetPersonProfile)
	assert.NotContains(tools, ToolListDirectoryPeople)

	search := rawCallTool(t, options, ToolSearchPeople, map[string]any{})
	assert.NotEqual(true, search["isError"], "search_people result: %#v", search)
	profile := rawCallTool(t, options, ToolGetPersonProfile, map[string]any{"person_id": 7})
	assert.NotEqual(true, profile["isError"], "get_person_profile result: %#v", profile)
}

var _ peoplebrowser.Backend = (*directoryPeopleBackend)(nil)
var _ peoplebrowser.DirectoryLister = (*directoryPeopleBackend)(nil)
var _ peoplebrowser.Backend = (*directoryCapabilityOnlyBackend)(nil)
var _ peoplebrowser.Backend = (*profileOnlyPeopleBackend)(nil)
var _ peoplebrowser.ProfileLister = (*profileOnlyPeopleBackend)(nil)
var _ peoplebrowser.ProfileReader = (*profileOnlyPeopleBackend)(nil)
