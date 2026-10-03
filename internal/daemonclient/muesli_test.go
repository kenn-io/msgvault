package daemonclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
)

func TestImportMuesliRetriesImmutableAuthenticatedBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var bodies []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("test-owner-key", r.Header.Get("X-Api-Key"))
		assert.Equal("/api/v1/import/muesli", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"import busy"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"registered","source_id":1,"changed":false}`))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, APIKey: "test-owner-key"})
	require.NoError(err)
	client.httpClient = server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := client.ImportMuesli(ctx, muesli.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}})
	require.NoError(err)
	assert.Equal(int64(1), result.SourceID)
	require.Len(bodies, 2)
	assert.Equal(bodies[0], bodies[1])
}

func TestImportMuesliRemoteValidationIsFatal(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnprocessableEntity, Body: io.NopCloser(strings.NewReader(`{"error":"validation_failed"}`))}
	_, err := decodeMuesliResponse(response)
	require.Error(t, err)
	assert.NotErrorIs(t, err, muesli.ErrRemoteValidation, "server rejection can be a source mismatch and must abort")
}
