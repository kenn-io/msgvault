package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObservationDecisionRequiresRevision(t *testing.T) {
	srv, _, personID, _ := newPersonFactAPIFixtureWithoutClaims(t)
	added := doRequest(t, srv.Router(), http.MethodPost, fmt.Sprintf("/api/v1/people/%d/observations", personID), []byte(`{"content":"[PERSON] swims"}`), nil)
	require.Equal(t, http.StatusCreated, added.Code, added.Body.String())
	var row struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(added.Body.Bytes(), &row))
	for _, body := range []string{`{"action":"pin"}`, `{"action":"pin","expected_updated_at":null}`, `{"action":"pin","expected_updated_at":"0001-01-01T00:00:00Z"}`} {
		response := doRequest(t, srv.Router(), http.MethodPost, fmt.Sprintf("/api/v1/observations/%d/decisions", row.ID), []byte(body), nil)
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	}
}
