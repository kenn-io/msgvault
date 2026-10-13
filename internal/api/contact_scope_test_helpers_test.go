package api

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func issueContactTokenRequest(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, agentTokensPath, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}
