package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
)

const operationTestArchive = "archive-fixture-01"

func TestOperationCursorQueryParsesDefaultsAndNormalizedFilters(t *testing.T) {
	codec := newOperationTokenCodec(operationTokenTestStore(t))
	startedFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	startedBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fractionalFrom := time.Date(2026, 8, 1, 0, 0, 0, 100000000, time.UTC)
	tests := []struct {
		name              string
		rawQuery          string
		wantKinds         []operations.Kind
		wantStates        []operations.State
		wantStartedFrom   *time.Time
		wantStartedBefore *time.Time
		wantLimit         int
	}{
		{name: "defaults", wantLimit: 25},
		{name: "kind", rawQuery: "kind=source_sync", wantKinds: []operations.Kind{operations.KindSourceSync}, wantLimit: 25},
		{name: "lane", rawQuery: "lane=messages", wantKinds: []operations.Kind{operations.KindMessageEmbedding, operations.KindSourceSync}, wantLimit: 25},
		{name: "mixed people lane", rawQuery: "lane=person_facts", wantKinds: []operations.Kind{operations.KindPersonEmbedding, operations.KindPersonEnrichment, operations.KindPersonSweep}, wantLimit: 25},
		{name: "document lane", rawQuery: "lane=documents", wantKinds: []operations.Kind{operations.KindDocumentEmbedding, operations.KindDocumentExtraction}, wantLimit: 25},
		{name: "matching kind and lane", rawQuery: "kind=carddav_sync&lane=contacts", wantKinds: []operations.Kind{operations.KindCardDAVSync}, wantLimit: 25},
		{name: "state dates and limit", rawQuery: "state=partial&started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-09-01T00%3A00%3A00Z&limit=100", wantStates: []operations.State{operations.StatePartial}, wantStartedFrom: &startedFrom, wantStartedBefore: &startedBefore, wantLimit: 100},
		{name: "canonical fractional date", rawQuery: "started_from=2026-08-01T00%3A00%3A00.1Z", wantStartedFrom: &fractionalFrom, wantLimit: 25},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/runs?"+test.rawQuery, nil)
			parsed, err := parseOperationRunsQuery(request, codec, operationTestArchive)
			require.NoError(t, err)
			assert.Equal(test.wantKinds, parsed.Query.Kinds)
			assert.Equal(test.wantStates, parsed.Query.States)
			assert.Equal(test.wantStartedFrom, parsed.Query.StartedFrom)
			assert.Equal(test.wantStartedBefore, parsed.Query.StartedBefore)
			assert.Equal(test.wantLimit, parsed.Query.Limit)
			assert.Nil(parsed.Query.Position)
			assert.NoError(parsed.Query.Validate())
		})
	}
}

func TestOperationCursorQueryRejectsDuplicateUnknownAndNoncanonicalParameters(t *testing.T) {
	codec := newOperationTokenCodec(operationTokenTestStore(t))
	tests := []struct {
		name     string
		rawQuery string
	}{
		{"duplicate kind", "kind=source_sync&kind=source_sync"},
		{"duplicate lower date", "started_from=2026-08-01T00%3A00%3A00Z&started_from=2026-08-01T00%3A00%3A00Z"},
		{"multiple cursors", "cursor=first&cursor=second"},
		{"unknown parameter", "provider=gmail"},
		{"unknown kind", "kind=provider_sync"},
		{"unknown lane", "lane=provider"},
		{"unknown state", "state=complete"},
		{"incompatible kind and lane", "kind=source_sync&lane=contacts"},
		{"empty kind", "kind="},
		{"empty cursor", "cursor="},
		{"zero limit", "limit=0"},
		{"limit above maximum", "limit=101"},
		{"nonnumeric limit", "limit=twenty"},
		{"date only", "started_from=2026-08-01"},
		{"non UTC date", "started_from=2026-08-01T02%3A00%3A00%2B02%3A00"},
		{"fractional date", "started_from=2026-08-01T00%3A00%3A00.100Z"},
		{"lower equals upper", "started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-08-01T00%3A00%3A00Z"},
		{"lower after upper", "started_from=2026-09-01T00%3A00%3A00Z&started_before=2026-08-01T00%3A00%3A00Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/runs?"+test.rawQuery, nil)
			_, err := parseOperationRunsQuery(request, codec, operationTestArchive)
			assert.Error(t, err)
		})
	}
}

func TestOperationCursorQueryExcludesLimitAndBindsCompleteNormalizedFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	codec := newOperationTokenCodec(operationTokenTestStore(t))
	first := httptest.NewRequest(http.MethodGet,
		"/api/v1/operations/runs?state=failed&limit=10&lane=messages&kind=source_sync&started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-09-01T00%3A00%3A00Z", nil)
	parsedFirst, err := parseOperationRunsQuery(first, codec, operationTestArchive)
	require.NoError(err)
	binding := operationCursorBinding{
		Position: operations.Position{
			StartedAt: time.Date(2026, 8, 28, 12, 34, 56, 123456000, time.UTC),
			ID:        mustOperationIntID(t, operations.KindSourceSync, 42),
		},
		MembershipRevision: 12,
		AvailableKinds:     []operations.Kind{operations.KindSourceSync},
		UnavailableKinds:   []operations.Kind{},
	}
	cursor, err := codec.encodeCursor(t.Context(), binding, parsedFirst.filter, operationTestArchive)
	require.NoError(err)

	second := httptest.NewRequest(http.MethodGet,
		"/api/v1/operations/runs?cursor="+cursor+"&kind=source_sync&limit=99&lane=messages&state=failed&started_before=2026-09-01T00%3A00%3A00Z&started_from=2026-08-01T00%3A00%3A00Z", nil)
	parsedSecond, err := parseOperationRunsQuery(second, codec, operationTestArchive)
	require.NoError(err)
	assert.Equal(99, parsedSecond.Query.Limit)
	require.NotNil(parsedSecond.cursor)
	assert.Equal(binding, *parsedSecond.cursor)

	for _, changed := range []string{
		"kind=source_sync&lane=messages&state=partial&started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-09-01T00%3A00%3A00Z",
		"kind=source_sync&state=failed&started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-09-01T00%3A00%3A00Z",
		"kind=source_sync&lane=messages&state=failed&started_from=2026-08-02T00%3A00%3A00Z&started_before=2026-09-01T00%3A00%3A00Z",
		"kind=source_sync&lane=messages&state=failed&started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-09-02T00%3A00%3A00Z",
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/runs?"+changed+"&cursor="+cursor, nil)
		_, err = parseOperationRunsQuery(request, codec, operationTestArchive)
		require.ErrorIs(err, errInvalidOperationCursor)
	}

	crossArchive := httptest.NewRequest(http.MethodGet,
		"/api/v1/operations/runs?kind=source_sync&lane=messages&state=failed&started_from=2026-08-01T00%3A00%3A00Z&started_before=2026-09-01T00%3A00%3A00Z&cursor="+cursor, nil)
	_, err = parseOperationRunsQuery(crossArchive, codec, "another-archive")
	assert.ErrorIs(err, errInvalidOperationCursor)
}

func mustOperationIntID(t *testing.T, kind operations.Kind, value int64) operations.StableID {
	t.Helper()
	id, err := operations.NewInt64ID(kind, value)
	require.NoError(t, err)
	return id
}

func mustOperationTextID(t *testing.T, kind operations.Kind, value string) operations.StableID {
	t.Helper()
	id, err := operations.NewTextID(kind, value)
	require.NoError(t, err)
	return id
}
