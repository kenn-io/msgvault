package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestExploreSenderLabelsKeepConversationRoster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	rows := []query.EntryRow{
		{Kind: query.EntryEmail, AnchorMessageID: new(int64(1)), SenderLabel: "Zoe Sender"},
		{Kind: query.EntryConversation, AnchorMessageID: new(int64(5)), ParticipantLabels: []string{"Alice Recipient", "Zoe Sender"}},
	}
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, timelineResult: &query.ExploreResponse{Rows: rows, TotalCount: 2}}
	opts := testServerOptions(t, nil)
	opts.Engine = engine
	response := postExploreJSON(t, NewServerWithOptions(opts), "/api/v1/explore", `{}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result ExploreHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(result.Rows, 2)
	assert.Equal("Zoe Sender", result.Rows[0].SenderLabel)
	assert.Empty(result.Rows[1].SenderLabel)
	assert.Equal([]string{"Alice Recipient", "Zoe Sender"}, result.Rows[1].ParticipantLabels)
}
