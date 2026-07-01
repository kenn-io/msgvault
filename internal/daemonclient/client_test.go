package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestNewRejectsHTTPWithoutAllowInsecure(t *testing.T) {
	_, err := New(Config{URL: "http://nas:8080", APIKey: "key"})
	requirepkg.Error(t, err, "New should reject http without AllowInsecure")
}

func TestNewAllowsHTTPWithAllowInsecure(t *testing.T) {
	c, err := New(Config{URL: "http://nas:8080", APIKey: "key", AllowInsecure: true})
	requirepkg.NoError(t, err, "New")
	requirepkg.NotNil(t, c, "client")
}

func TestNewAllowsHTTPS(t *testing.T) {
	c, err := New(Config{URL: "https://nas:8080", APIKey: "key"})
	requirepkg.NoError(t, err, "New")
	requirepkg.NotNil(t, c, "client")
}

func TestNewRejectsEmptyURL(t *testing.T) {
	_, err := New(Config{APIKey: "key"})
	requirepkg.Error(t, err, "New should reject empty URL")
}

func TestNewRejectsInvalidScheme(t *testing.T) {
	_, err := New(Config{URL: "ftp://nas:8080", APIKey: "key"})
	requirepkg.Error(t, err, "New should reject ftp")
	assertpkg.ErrorContains(t, err, "http or https")
}

func TestNewRejectsEmptyHost(t *testing.T) {
	_, err := New(Config{URL: "http://", APIKey: "key", AllowInsecure: true})
	requirepkg.Error(t, err, "New should reject empty host")
	assertpkg.ErrorContains(t, err, "must include a host")
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c, err := New(Config{URL: "http://nas:8080/", APIKey: "key", AllowInsecure: true})
	requirepkg.NoError(t, err, "New")
	assertpkg.Equal(t, "http://nas:8080", c.BaseURL(), "base URL")
}

func TestNewDefaultTimeout(t *testing.T) {
	c, err := New(Config{URL: "https://nas:8080", APIKey: "key"})
	requirepkg.NoError(t, err, "New")
	assertpkg.Equal(t, 30*time.Second, c.Timeout(), "timeout")
}

func TestGeneratedClientUsesTransportAndAuth(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/query", r.URL.Path, "path")
		assert.Equal("secret-key", r.Header.Get("X-Api-Key"), "api key")
		assert.Equal("application/json", r.Header.Get("Accept"), "accept")
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(generated.QueryResult{
			Columns: []string{"n"},
			Rows:    [][]any{{float64(1)}},
		}), "encode response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", AllowInsecure: true})
	require.NoError(err, "New")
	apiClient, err := c.GeneratedClient()
	require.NoError(err, "generated client")

	got, err := apiClient.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})

	require.NoError(err, "RunQuery")
	assert.Equal([]string{"n"}, got.Columns, "columns")
	require.Len(got.Rows, 1, "rows")
	assert.InDelta(float64(1), got.Rows[0][0], 0, "scalar cell")
}

func TestGeneratedClientUsesConfiguredHTTPClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertpkg.Equal(t, "/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		assertpkg.NoError(t, json.NewEncoder(w).Encode(generated.QueryResult{
			Columns: []string{"n"},
			Rows:    [][]any{{float64(1)}},
		}), "encode response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", HTTPClient: srv.Client()})
	requirepkg.NoError(t, err, "New")
	apiClient, err := c.GeneratedClient()
	requirepkg.NoError(t, err, "generated client")

	_, err = apiClient.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})

	requirepkg.NoError(t, err, "RunQuery")
}

func TestGeneratedResponseErrorReturnsDecodeErrorForOKDecodeFailure(t *testing.T) {
	decodeErr := errors.New("decode response: unexpected EOF")
	err := APIResponseError(&generated.GetStatsResp{
		StatusCode: http.StatusOK,
		Body:       []byte("{"),
	}, decodeErr)

	requirepkg.ErrorIs(t, err, decodeErr, "decode error")
	assertpkg.NotContains(t, err.Error(), "API error (200)", "decode failures are not API error bodies")
}

func TestGeneratedResponseMetadataExtractsStatusBodyAndJSON200State(t *testing.T) {
	body := []byte(`{"total_messages": 7}`)
	meta, ok := responseMetadata(&generated.GetStatsResp{
		StatusCode: http.StatusOK,
		Body:       body,
		JSON200:    &generated.StatsResponse{TotalMessages: 7},
	})

	requirepkg.True(t, ok, "metadata")
	assertpkg.Equal(t, http.StatusOK, meta.Status, "status")
	assertpkg.Equal(t, body, meta.Body, "body")
	assertpkg.True(t, meta.HasJSON200, "has JSON200")
	assertpkg.False(t, meta.MissingJSON200, "missing JSON200")

	meta, ok = responseMetadata(&generated.GetCLIStatsResp{
		StatusCode: http.StatusOK,
		Body:       []byte(`{}`),
	})

	requirepkg.True(t, ok, "CLI metadata")
	assertpkg.True(t, meta.HasJSON200, "nil JSON200 field is still present")
	assertpkg.True(t, meta.MissingJSON200, "nil JSON200 pointer is missing payload")
}

func TestGeneratedResponseErrorRejectsMissingJSON200Payload(t *testing.T) {
	err := APIResponseError(&generated.GetCLIStatsResp{StatusCode: http.StatusOK}, nil)

	requirepkg.Error(t, err, "missing JSON body must fail")
	assertpkg.ErrorContains(t, err, "200 JSON response body")
}

func TestGeneratedCLIResponseErrorReturnsBareServerMessage(t *testing.T) {
	err := CLIResponseError(&generated.CreateCLICollectionResp{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"error":"invalid_collection","message":"bad account"}`),
	}, nil)

	requirepkg.EqualError(t, err, "bad account", "CLI error")
}

func TestGeneratedResponseDecodeErrorDetection(t *testing.T) {
	err := &runtime.ResponseDecodeError{Err: errors.New("malformed")}
	assertpkg.True(t, responseDecodeError(err), "decode error")
	assertpkg.False(t, responseDecodeError(errors.New("other")), "other error")
}
