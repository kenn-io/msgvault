package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExploreFilesRejectsUnboundedLimit(t *testing.T) {
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	response := postExploreJSON(t, srv, "/api/v1/explore/files", `{"predicate":{},"limit":101}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "limit must be between")
}

func TestPrepareExplorePredicateCanonicalizesWithoutMutatingCaller(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	input := ExploreHTTPRequest{
		Query:      " alpha ",
		SearchMode: " FULL_TEXT ",
		Filters: []ExploreFilter{
			{Dimension: " source ", Values: []string{"1"}},
			{Dimension: " identity ", Values: []string{"1", " BOB@MEMBERS.EXAMPLE ", ""}},
		},
		Grouping: []ExploreGroupDimension{" SOURCE "},
		Sort:     []ExploreSort{{Field: " OCCURRED_AT ", Direction: " DESC "}},
	}
	wantInput := ExploreHTTPRequest{
		Query:      " alpha ",
		SearchMode: " FULL_TEXT ",
		Filters: []ExploreFilter{
			{Dimension: " source ", Values: []string{"1"}},
			{Dimension: " identity ", Values: []string{"1", " BOB@MEMBERS.EXAMPLE ", ""}},
		},
		Grouping: []ExploreGroupDimension{" SOURCE "},
		Sort:     []ExploreSort{{Field: " OCCURRED_AT ", Direction: " DESC "}},
	}

	prepared, err := prepareExplorePredicate(input)
	requirements.NoError(err)
	assertions.Equal(wantInput, input, "canonicalization must not mutate the caller through shared slices")
	assertions.Equal(ExploreFilter{
		Dimension: "identity",
		Values:    []string{"1", "BOB@MEMBERS.EXAMPLE", "any"},
	}, prepared.request.Filters[0])
	assertions.Equal("alpha", prepared.request.Query)
	assertions.Equal(exploreSearchModeFullText, prepared.request.SearchMode)
	assertions.Equal([]ExploreGroupDimension{"source"}, prepared.request.Grouping)
	assertions.Equal([]ExploreSort{{Field: "occurred_at", Direction: "desc"}}, prepared.request.Sort)
}
