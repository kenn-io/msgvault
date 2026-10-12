package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxTagsHTTPStore struct{ tagsHTTPStore }

func (s *inboxTagsHTTPStore) ResolveMessageTagTarget(context.Context, int64, string) (inboxcontrol.Target, error) {
	return inboxcontrol.Target{SourceID: 2, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "native-7"}, nil
}
func TestInboxEmailTagsGmailLegacyWriteRequiresSignedControl(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := &inboxTagsHTTPStore{}
	srv := NewServer(&config.Config{}, st, newMockScheduler(), testLogger())
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		return response
	}
	response := call(http.MethodPost, "/api/v1/messages/7/tags", `{"add":["Label_next"]}`)
	require.Equal(http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "inbox_preview_required")
	assert.Equal(0, st.calls, "legacy write rejection must precede all provider access")
	response = call(http.MethodGet, "/api/v1/messages/7/tags", "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Target *inboxcontrol.Target `json:"target"`
	}
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.NotNil(result.Target)
	assert.Equal("native-7", result.Target.ProviderID)
}
