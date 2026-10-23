package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonObservationsHTTPRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, _, personID, _ := newPersonFactAPIFixtureWithoutClaims(t)
	base := fmt.Sprintf("/api/v1/people/%d/observations", personID)
	added := doRequest(t, srv.Router(), http.MethodPost, base, []byte(`{"content":"[person] swims","kind":"state"}`), nil)
	require.Equal(http.StatusCreated, added.Code, added.Body.String())
	var row struct {
		ID         int64  `json:"id"`
		UpdatedAt  string `json:"updated_at"`
		ContentKey string `json:"content_key"`
	}
	require.NoError(json.Unmarshal(added.Body.Bytes(), &row))
	require.Positive(row.ID)
	require.NotEmpty(row.UpdatedAt)
	list := doRequest(t, srv.Router(), http.MethodGet, base, nil, nil)
	require.Equal(http.StatusOK, list.Code, list.Body.String())
	assert.Equal("no-store", list.Header().Get("Cache-Control"))
	var page struct {
		Observations []map[string]any `json:"observations"`
		Total        int              `json:"total"`
	}
	require.NoError(json.Unmarshal(list.Body.Bytes(), &page))
	assert.Equal(1, page.Total)
	require.Len(page.Observations, 1)
	assert.Equal("[PERSON] swims", page.Observations[0]["content"])
	detailPath := fmt.Sprintf("/api/v1/observations/%d", row.ID)
	detail := doRequest(t, srv.Router(), http.MethodGet, detailPath, nil, nil)
	require.Equal(http.StatusOK, detail.Code, detail.Body.String())
	assert.Contains(detail.Body.String(), `"events"`)
	body := []byte(fmt.Sprintf(`{"action":"reject","expected_updated_at":%q}`, row.UpdatedAt))
	decision := doRequest(t, srv.Router(), http.MethodPost, detailPath+"/decisions", body, nil)
	require.Equal(http.StatusOK, decision.Code, decision.Body.String())
	stale := doRequest(t, srv.Router(), http.MethodPost, detailPath+"/decisions", body, nil)
	assert.Equal(http.StatusConflict, stale.Code, stale.Body.String())
	suppressionPath := fmt.Sprintf("/api/v1/people/%d/observation-suppressions", personID)
	suppressed := doRequest(t, srv.Router(), http.MethodGet, suppressionPath, nil, nil)
	require.Equal(http.StatusOK, suppressed.Code, suppressed.Body.String())
	assert.Contains(suppressed.Body.String(), row.ContentKey)
	cleared := doRequest(t, srv.Router(), http.MethodDelete, suppressionPath+"/"+row.ContentKey, nil, nil)
	assert.Equal(http.StatusNoContent, cleared.Code, cleared.Body.String())
}
func TestPersonObservationsHTTPSensitivityAndValidation(t *testing.T) {
	check := assert.New(t)
	require := require.New(t)
	srv, _, personID, _ := newPersonFactAPIFixtureWithoutClaims(t)
	base := fmt.Sprintf("/api/v1/people/%d/observations", personID)
	added := doRequest(t, srv.Router(), http.MethodPost, base, []byte(`{"content":"[PERSON] has a mortgage"}`), nil)
	require.Equal(http.StatusCreated, added.Code, added.Body.String())
	var row struct {
		ID int64 `json:"id"`
	}
	require.NoError(json.Unmarshal(added.Body.Bytes(), &row))
	list := doRequest(t, srv.Router(), http.MethodGet, base, nil, nil)
	require.Equal(http.StatusOK, list.Code)
	check.JSONEq(`{"observations":[],"total":0}`, list.Body.String())
	list = doRequest(t, srv.Router(), http.MethodGet, base+"?include_sensitive=true", nil, nil)
	require.Equal(http.StatusOK, list.Code)
	check.Contains(list.Body.String(), "mortgage")
	detail := fmt.Sprintf("/api/v1/observations/%d", row.ID)
	check.Equal(http.StatusNotFound, doRequest(t, srv.Router(), http.MethodGet, detail, nil, nil).Code)
	check.Equal(http.StatusOK, doRequest(t, srv.Router(), http.MethodGet, detail+"?include_sensitive=true", nil, nil).Code)
	for _, query := range []string{"?limit=-1", "?offset=-1", "?include_sensitive=perhaps", "?level=unknown", "?status=unknown", "?kind=unknown", "?as_of=yesterday"} {
		t.Run(query, func(t *testing.T) {
			assert := assert.New(t)
			response := doRequest(t, srv.Router(), http.MethodGet, base+query, nil, nil)
			assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
		})
	}
	for _, body := range []string{`{"content":"no token"}`, `{"content":"[PERSON] runs","unknown":true}`, `{"content":"[PERSON] runs","categories":["unknown"]}`} {
		response := doRequest(t, srv.Router(), http.MethodPost, base, []byte(body), nil)
		check.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
}
func TestPersonObservationsHTTPRefusesDelegatedTokens(t *testing.T) {
	srv, secret := newDelegatedTestServer(t)
	headers := map[string]string{"Authorization": "Bearer " + secret}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/people/1/observations"}, {http.MethodGet, "/api/v1/observations/1"}, {http.MethodPost, "/api/v1/people/1/observations"}, {http.MethodPost, "/api/v1/observations/1/decisions"}, {http.MethodGet, "/api/v1/people/1/observation-suppressions"}, {http.MethodDelete, "/api/v1/people/1/observation-suppressions/" + strings.Repeat("a", 64)},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			assert := assert.New(t)
			response := doRequest(t, srv.Router(), route.method, route.path, []byte(`{}`), headers)
			assert.Equal(http.StatusUnauthorized, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), "unauthorized")
		})
	}
}
