package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector/visual"
)

func TestFuseFileRanksUsesRRFAndKeepsSignalExplain(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	lexical := &query.FileSearchResponse{Files: []query.FileRow{{ID: 11}, {ID: 22}}}
	visualFiles := &query.FileSearchResponse{Files: []query.FileRow{{ID: 22}, {ID: 33}}}
	rows, explain := fuseFileRanks(lexical, visualFiles, map[int64]int{22: 1, 33: 2})

	requirements.Len(rows, 3)
	assertions.Equal([]int64{22, 11, 33}, []int64{rows[0].ID, rows[1].ID, rows[2].ID})
	requirements.NotNil(explain[22].FilenameRank)
	requirements.NotNil(explain[22].VisualRank)
	assertions.Equal(2, *explain[22].FilenameRank)
	assertions.Equal(1, *explain[22].VisualRank)
	assertions.InDelta(1.0/62+1.0/61, explain[22].RRF, 1e-12)
	assertions.Nil(explain[11].VisualRank)
	assertions.Nil(explain[33].FilenameRank)
}

func TestFuseFileRanksBreaksTiesByAttachmentID(t *testing.T) {
	visualFiles := &query.FileSearchResponse{Files: []query.FileRow{{ID: 8}, {ID: 4}}}
	rows, _ := fuseFileRanks(nil, visualFiles, map[int64]int{8: 1, 4: 1})
	require.Len(t, rows, 2)
	assert.Equal(t, []int64{4, 8}, []int64{rows[0].ID, rows[1].ID})
}

func TestApplyVisualSearchScopeMapsHardFilters(t *testing.T) {
	assert := assert.New(t)
	after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	request := query.FileSearchRequest{
		FilenameQuery: " diagram ",
		MIMEFamilies:  []query.FileMIMEFamily{query.FileMIMEImage},
		Explore: query.ExploreRequest{Context: query.Context{
			SourceIDs: []int64{7}, After: &after, Before: &before,
		}},
	}
	request.Person = &query.PersonFileScope{
		ParticipantIDs: []int64{4}, Directions: []query.PersonFileDirection{query.PersonFileFromPerson},
	}
	visualQuery := visual.SearchQuery{Text: "red square", Limit: 100}
	applyVisualSearchScope(&visualQuery, request)
	assert.Equal(int64(7), visualQuery.SourceID, "a single-account scope rides into the vector search")
	assert.Equal(&after, visualQuery.After)
	assert.Equal(&before, visualQuery.Before)
	assert.Equal("image/", visualQuery.MIMEPrefix)
	assert.Equal("diagram", visualQuery.Filename)
	assert.Equal(request.Person, visualQuery.Person)

	multi := query.FileSearchRequest{Explore: query.ExploreRequest{Context: query.Context{SourceIDs: []int64{1, 2}}}}
	visualQuery = visual.SearchQuery{Text: "x", Limit: 100}
	applyVisualSearchScope(&visualQuery, multi)
	assert.Zero(visualQuery.SourceID, "multiple accounts cannot map to the single-source predicate")
}
