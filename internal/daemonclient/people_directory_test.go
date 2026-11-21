package daemonclient

import (
	"fmt"
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
		if r.URL.Path == "/api/v1/health" {
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"status":"ok","api_schema_version":"3.29.0"}`)
			return
		}
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
                "id":7,"vcard_uid":"person-uid-7",
                "carddav_bindings":[{"connection":"personal","book":"Personal",
                  "href":"https://contacts.example.test/book/person.vcf",
                  "remote_uid":"remote-uid-7","mapping_status":"mapped"}],
                "display_name":"Alice Example","revision":9,
                "primary_channel":"chat","contact_state":"active",
                "last_contact_at":"2026-08-20T12:30:00.123456789Z",
                "categories":null,"organizations":null
            }]
        }`)
	}))

	page, err := engine.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{
		Query:             "Alice",
		Address:           "ÉCOLE+%_@example.test",
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
		"address":             "ÉCOLE+%_@example.test",
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
		ID: 7, VCardUID: "person-uid-7",
		CardDAVBindings: []store.CardDAVBinding{{
			Connection: "personal", Book: "Personal",
			Href:      "https://contacts.example.test/book/person.vcf",
			RemoteUID: "remote-uid-7", MappingStatus: store.CardDAVMappingMapped,
		}},
		DisplayName: new("Alice Example"), Revision: 9,
		PrimaryChannel: "chat", ContactState: "active",
		LastContactAt: new(time.Date(2026, 8, 20, 12, 30, 0, 123456789, time.UTC)),
		Categories:    []string{}, Organizations: []string{},
	}, page.People[0])
	var _ peoplebrowser.DirectoryLister = engine
}

func TestPeopleBrowserDirectoryAddressRejectsUnsupportedDaemon(t *testing.T) {
	for _, version := range []string{"3.28.0", "", "unknown"} {
		t.Run(version, func(t *testing.T) {
			var lookups int
			engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					writePeopleBrowserJSON(t, w, http.StatusOK, `{"status":"ok","api_schema_version":"`+version+`"}`)
					return
				}
				lookups++
				http.NotFound(w, r)
			}))
			_, err := engine.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{Address: "alex@example.test"})
			require.EqualError(t, err, "person directory address filtering requires daemon API schema 3.29.0 or newer; upgrade the daemon")
			assert.Zero(t, lookups)
		})
	}
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

func TestPeopleBrowserDirectoryCadenceRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writePeopleBrowserJSON(t, w, http.StatusOK, fmt.Sprintf(`{"status":"ok","api_schema_version":%q}`, DirectoryCadenceMinAPISchemaVersion))
			return
		}

		assert.Equal("due", r.URL.Query().Get("cadence"))
		assert.Equal("7", r.URL.Query().Get("due_within_days"))
		assert.Equal("next_due", r.URL.Query().Get("sort"))
		writePeopleBrowserJSON(t, w, http.StatusOK, `{"cadence_presets":[{"list":"A","days":21}],"cadence_coverage":{"total":4,"no_list":1,"unknown":1,"scheduled":2},"people":[{"id":7,"vcard_uid":"person-7","revision":1,"contact_state":"active","last_contact_channel":"email","cadence":{"frequency_days":21,"list":"A","due_at":"2026-10-20T12:00:00Z","status":"ok","stale":false},"reach_out_context":{"awaiting_reply_count":2,"needs_reply_count":1,"open_loops_stale":true,"suggested_list":"B","signals_stale":false},"categories":[],"organizations":[]}]}`)
	}))
	days := 7
	page, err := engine.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{Cadence: "due", DueWithinDays: &days, Sort: "next_due"})
	require.NoError(err)
	require.Len(page.People, 1)
	require.Len(page.CadencePresets, 1)
	assert.Equal(int64(21), page.CadencePresets[0].Days)
	require.NotNil(page.CadenceCoverage)
	assert.Equal(4, page.CadenceCoverage.Total)
	p := page.People[0]
	assert.Equal("email", p.LastContactChannel)
	assert.Equal("A", p.Cadence.List)
	require.NotNil(p.Cadence.FrequencyDays)
	assert.Equal(int64(21), *p.Cadence.FrequencyDays)
	require.NotNil(p.Cadence.DueAt)
	require.NotNil(p.ReachOutContext)
	assert.Equal(2, *p.ReachOutContext.AwaitingReplyCount)
	assert.Equal(1, *p.ReachOutContext.NeedsReplyCount)
	assert.True(*p.ReachOutContext.OpenLoopsStale)
	assert.False(*p.ReachOutContext.SignalsStale)
	assert.Equal("B", p.ReachOutContext.SuggestedList)
}

func TestPeopleBrowserDirectoryCadenceRequiresCurrentSchema(t *testing.T) {
	requests := 0
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"status":"ok","api_schema_version":"3.23.0"}`)
			return
		}
		requests++
		writePeopleBrowserJSON(t, w, http.StatusOK, `{"people":[]}`)
	}))
	_, err := engine.ListDirectoryPeople(t.Context(), store.DirectoryPeopleQuery{Cadence: "overdue"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upgrade the daemon")
	assert.Zero(t, requests, "an old daemon must not silently ignore cadence filtering")
}
