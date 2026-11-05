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
	t.Parallel()
	rows := []query.EntryRow{
		{Kind: query.EntryEmail, AnchorMessageID: new(int64(1))},
		{Kind: query.EntryEmail, AnchorMessageID: new(int64(4))},
		{Kind: query.EntryConversation, AnchorMessageID: new(int64(5)), ParticipantLabels: []string{"Alice Recipient", "Zoe Sender"}},
	}
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{SenderLabelsByID: map[int64]string{
		1: "Zoe Sender", 5: "Conversation Sender",
	}}, timelineResult: &query.ExploreResponse{Rows: rows, TotalCount: 3}}
	opts := testServerOptions(t, nil)
	opts.Engine = engine
	response := postExploreJSON(t, NewServerWithOptions(opts), "/api/v1/explore", `{}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result ExploreHTTPResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Rows, 3)
	assert.Equal(t, "Zoe Sender", result.Rows[0].SenderLabel)
	assert.Empty(t, result.Rows[1].SenderLabel)
	assert.Empty(t, result.Rows[2].SenderLabel)
	assert.Equal(t, []string{"Alice Recipient", "Zoe Sender"}, result.Rows[2].ParticipantLabels)
}
