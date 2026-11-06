package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

func TestExploreSenderLabelsKeepConversationRoster(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	rows := []query.EntryRow{
		{Kind: query.EntryEmail, AnchorMessageID: new(int64(1))},
		{Kind: query.EntryConversation, AnchorMessageID: new(int64(5)), ParticipantLabels: []string{"Alice Recipient", "Zoe Sender"}},
	}
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{SenderLabelsByID: map[int64]string{
		1: "Zoe Sender", 5: "Conversation Sender",
	}}, timelineResult: &query.ExploreResponse{Rows: rows, TotalCount: 2}}
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

func TestExploreSenderLookupFailureKeepsRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{SenderLabelsErr: errors.New("sender lookup failed")}, timelineResult: &query.ExploreResponse{Rows: []query.EntryRow{{Kind: query.EntryEmail, AnchorMessageID: new(int64(1)), ParticipantLabels: []string{"Example Sender"}}}, TotalCount: 1}}
	opts := testServerOptions(t, nil)
	opts.Engine = engine
	response := postExploreJSON(t, NewServerWithOptions(opts), "/api/v1/explore", `{}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result ExploreHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(result.Rows, 1)
	assert.Empty(result.Rows[0].SenderLabel)
	assert.Equal([]string{"Example Sender"}, result.Rows[0].ParticipantLabels)
}

func TestFilesSenderLookupFailureKeepsRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := &fileSearchEngine{MockEngine: &querytest.MockEngine{SenderLabelsErr: errors.New("sender lookup failed")}, result: &query.FileSearchResponse{Files: []query.FileRow{{ID: 1, MessageID: 11, Filename: "example.pdf", ParticipantLabels: []string{"Example Sender"}}}, TotalCount: 1}}
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{1: {ID: 1, MessageID: 11, Filename: "example.pdf"}}}
	opts := testServerOptions(t, nil)
	opts.Engine, opts.Store = engine, catalog
	response := postExploreJSON(t, NewServerWithOptions(opts), "/api/v1/files/search", `{}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result FileSearchHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(result.Files, 1)
	assert.Empty(result.Files[0].SenderLabel)
	assert.Equal([]string{"Example Sender"}, result.Files[0].ParticipantLabels)
}

type engineWithoutSenderLabeler struct{ query.Engine }

func TestSenderLabelsWithoutCapability(t *testing.T) {
	srv := NewServerWithOptions(testServerOptions(t, nil))
	ctx := context.WithValue(t.Context(), analyticsEngineContextKey{}, &analyticsEngineState{engine: engineWithoutSenderLabeler{&querytest.MockEngine{}}})
	var labels map[int64]string
	var err error
	require.NotPanics(t, func() { labels, err = srv.pageSenderLabels(ctx, []int64{1}) })
	require.NoError(t, err)
	assert.Empty(t, labels[1])
}
