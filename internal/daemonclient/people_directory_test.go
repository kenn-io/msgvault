package daemonclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

func TestPeopleBrowserListDirectoryMapsQueryAndResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	after := time.Date(2026, 8, 20, 12, 30, 0, 123456789, time.FixedZone("input", -5*60*60))
	before := time.Date(2026, 8, 21, 12, 30, 0, 987654321, time.UTC)
	var gotQuery map[string]string
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("/api/v1/people/directory", r.URL.Path)
		gotQuery = map[string]string{}
		for key, values := range r.URL.Query() {
			if len(values) > 0 {
				gotQuery[key] = values[0]
			}
		}
		writePeopleBrowserJSON(t, w, http.StatusOK, `{
            "next_cursor":"cursor-bytes-%2F%2B",
            "people":[{
                "id":7,"display_name":"Alice Example","revision":9,
                "primary_channel":"chat","contact_state":"active",
                "last_contact_at":"2026-08-20T12:30:00.123456789Z",
                "categories":null,"organizations":null
            }]
        }`)
	}))

	page, err := engine.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{
		Query:             "Alice",
		Cursor:            "opaque/cursor+bytes",
		Limit:             -7,
		Sort:              store.DirectoryPeopleSortLastContactDesc,
		LastContactAfter:  &after,
		LastContactBefore: &before,
		ContactState:      "active",
		Category:          "friend",
		Organization:      "Example Org",
		PrimaryChannel:    "chat",
	})
	require.NoError(err)
	require.NotNil(page)
	require.Len(page.People, 1)

	assert.Equal(map[string]string{
		"q":                   "Alice",
		"cursor":              "opaque/cursor+bytes",
		"limit":               "-7",
		"sort":                "last_contact_desc",
		"last_contact_after":  "2026-08-20T12:30:00.123456789-05:00",
		"last_contact_before": "2026-08-21T12:30:00.987654321Z",
		"contact_state":       "active",
		"category":            "friend",
		"organization":        "Example Org",
		"primary_channel":     "chat",
	}, gotQuery)
	assert.Equal("cursor-bytes-%2F%2B", page.NextCursor)
	assert.Equal(store.DirectoryPersonSummary{
		ID: 7, DisplayName: new("Alice Example"), Revision: 9,
		PrimaryChannel: "chat", ContactState: "active",
		LastContactAt: new(time.Date(2026, 8, 20, 12, 30, 0, 123456789, time.UTC)),
		Categories:    []string{}, Organizations: []string{},
	}, page.People[0])
	var _ peoplebrowser.DirectoryLister = engine
}

func TestPeopleBrowserListDirectoryReturnsDaemonError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writePeopleBrowserJSON(t, w, http.StatusBadRequest,
			`{"error":"invalid_cursor","message":"Directory cursor is invalid"}`)
	}))

	page, err := engine.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{Cursor: "bad"})
	require.Error(err)
	assert.Nil(page)
	var coded interface{ APIErrorCode() string }
	require.ErrorAs(err, &coded)
	assert.Equal("invalid_cursor", coded.APIErrorCode())
}
