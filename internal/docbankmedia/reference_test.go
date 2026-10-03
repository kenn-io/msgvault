package docbankmedia

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReferenceWire(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	var body map[string]any
	var getPath string
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if !assert.NoError(json.UnmarshalRead(r.Body, &body)) {
				return
			}
		} else {
			getPath = r.URL.EscapedPath()
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"vault_uid":"vault","source_id":"source","operation_id":"operation","occurrence_id":"occurrence","operation_state":"succeeded","coverage_state":"unprocessed","outcome":"access_required"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, nil)
	require.NoError(err)
	receipt, err := client.SubmitReference(t.Context(), ReferenceRequest{OperationID: "operation", ReferenceURL: "https://loom.com/share/abc?token=secret", Acquire: false, Occurrence: Occurrence{Ref: "ref", Revision: "revision"}})
	require.NoError(err)
	assert.Equal("occurrence", receipt.OccurrenceID)
	assert.Equal("access_required", receipt.Outcome)
	assert.Equal("unprocessed", receipt.CoverageState)
	assert.Len(body, 4)
	assert.Equal(false, body["acquire"])
	assert.Equal("operation", body["operation_id"])
	assert.Equal("https://loom.com/share/abc?token=secret", body["reference_url"])
	assert.NotContains(body, "processing")
	assert.NotContains(body, "credential_binding")
	_, err = client.OperationReceipt(t.Context(), "operation")
	require.NoError(err)
	assert.Equal("/api/v1/media/operations/operation", getPath)
	status = http.StatusNotFound
	_, err = client.OperationReceipt(t.Context(), "operation")
	assert.True(IsNotFound(err))
}
