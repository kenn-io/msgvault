package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestRunArchiveQueryReturnsRowsAndAcceptedBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/query/archive", r.URL.Path)
		var request struct {
			Fresh bool `json:"fresh"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if request.Fresh {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued","job_id":"synthetic-build"}`))
			return
		}
		_, _ = w.Write([]byte(`{"columns":["subject"],"rows":[["synthetic archive row"]],"row_count":1}`))
	}))
	t.Cleanup(server.Close)
	c, err := New(server.URL)
	require.NoError(err)
	options := &generated.RunArchiveQueryRequestOptions{Body: &generated.RunArchiveQueryBody{SQL: "SELECT subject FROM messages"}}
	rows, err := c.RunArchiveQuery(t.Context(), options)
	require.NoError(err)
	encoded, err := json.Marshal(rows)
	require.NoError(err)
	assert.JSONEq(`{"columns":["subject"],"rows":[["synthetic archive row"]],"row_count":1}`, string(encoded))

	options.Body.Fresh = new(true)
	accepted, err := c.RunArchiveQueryWithResponse(t.Context(), options)
	require.NoError(err)
	require.NotNil(accepted.JSON202)
	assert.Equal("synthetic-build", accepted.JSON202.JobID)
}
