package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxTagsHTTPStore struct{ tagsHTTPStore }

func (s *inboxTagsHTTPStore) ResolveMessageTagTarget(context.Context, int64, string) (inboxcontrol.Target, error) {
	return inboxcontrol.Target{SourceID: 2, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "native-7"}, nil
}
func TestInboxEmailTagsGmailLegacyWriteRequiresSignedControl(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

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
	requirements.Equal(http.StatusConflict, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "inbox_preview_required")
	assertions.Equal(0, st.calls, "legacy write rejection must precede all provider access")
	response = call(http.MethodGet, "/api/v1/messages/7/tags", "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Target *inboxcontrol.Target `json:"target"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	requirements.NotNil(result.Target)
	assertions.Equal("native-7", result.Target.ProviderID)
}
