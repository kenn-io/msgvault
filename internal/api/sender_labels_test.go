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

type failingSenderExploreEngine struct{ *peopleAPIEngine }

func (e *failingSenderExploreEngine) SenderLabels(context.Context, []int64) (map[int64]string, error) {
	return nil, errors.New("sender lookup failed")
}

func TestExploreSenderLookupFailureKeepsRows(t *testing.T) {
	engine := &failingSenderExploreEngine{&peopleAPIEngine{MockEngine: &querytest.MockEngine{}, timelineResult: &query.ExploreResponse{Rows: []query.EntryRow{{Kind: query.EntryEmail, AnchorMessageID: new(int64(1)), ParticipantLabels: []string{"Example Sender"}}}, TotalCount: 1}}}
	opts := testServerOptions(t, nil)
	opts.Engine = engine
	response := postExploreJSON(t, NewServerWithOptions(opts), "/api/v1/explore", `{}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result ExploreHTTPResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Rows, 1)
	assert.Empty(t, result.Rows[0].SenderLabel)
	assert.Equal(t, []string{"Example Sender"}, result.Rows[0].ParticipantLabels)
}

type failingSenderFileEngine struct{ *fileSearchEngine }

func (e *failingSenderFileEngine) SenderLabels(context.Context, []int64) (map[int64]string, error) {
	return nil, errors.New("sender lookup failed")
}

func TestFilesSenderLookupFailureKeepsRows(t *testing.T) {
	engine := &failingSenderFileEngine{&fileSearchEngine{MockEngine: &querytest.MockEngine{}, result: &query.FileSearchResponse{Files: []query.FileRow{{ID: 1, MessageID: 11, Filename: "example.pdf", ParticipantLabels: []string{"Example Sender"}}}, TotalCount: 1}}}
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{1: {ID: 1, MessageID: 11, Filename: "example.pdf"}}}
	opts := testServerOptions(t, nil)
	opts.Engine, opts.Store = engine, catalog
	response := postExploreJSON(t, NewServerWithOptions(opts), "/api/v1/files/search", `{}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result FileSearchHTTPResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(t, result.Files, 1)
	assert.Empty(t, result.Files[0].SenderLabel)
	assert.Equal(t, []string{"Example Sender"}, result.Files[0].ParticipantLabels)
}
