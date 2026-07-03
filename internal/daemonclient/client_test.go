package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestNewRejectsHTTPWithoutAllowInsecure(t *testing.T) {
	_, err := New(Config{URL: "http://nas:8080", APIKey: "key"})
	require.Error(t, err, "New should reject http without AllowInsecure")
}

func TestNewAllowsHTTPWithAllowInsecure(t *testing.T) {
	c, err := New(Config{URL: "http://nas:8080", APIKey: "key", AllowInsecure: true})
	require.NoError(t, err, "New")
	require.NotNil(t, c, "client")
}

func TestNewAllowsHTTPS(t *testing.T) {
	c, err := New(Config{URL: "https://nas:8080", APIKey: "key"})
	require.NoError(t, err, "New")
	require.NotNil(t, c, "client")
}

func TestNewRejectsEmptyURL(t *testing.T) {
	_, err := New(Config{APIKey: "key"})
	require.Error(t, err, "New should reject empty URL")
}

func TestNewRejectsInvalidScheme(t *testing.T) {
	_, err := New(Config{URL: "ftp://nas:8080", APIKey: "key"})
	require.Error(t, err, "New should reject ftp")
	assert.ErrorContains(t, err, "http or https")
}

func TestNewRejectsEmptyHost(t *testing.T) {
	_, err := New(Config{URL: "http://", APIKey: "key", AllowInsecure: true})
	require.Error(t, err, "New should reject empty host")
	assert.ErrorContains(t, err, "must include a host")
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c, err := New(Config{URL: "http://nas:8080/", APIKey: "key", AllowInsecure: true})
	require.NoError(t, err, "New")
	assert.Equal(t, "http://nas:8080", c.BaseURL(), "base URL")
}

func TestNewDefaultTimeout(t *testing.T) {
	c, err := New(Config{URL: "https://nas:8080", APIKey: "key"})
	require.NoError(t, err, "New")
	assert.Equal(t, 30*time.Second, c.Timeout(), "timeout")
}

func TestGeneratedClientUsesTransportAndAuth(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

func TestRunSQLQueryPreservesIntegerPrecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{
			"columns": ["name", "message_count", "id"],
			"rows": [["UNREAD", 1662130, 9007199254740993]],
			"row_count": 1
		}`))
		assert.NoError(err, "write response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "key", AllowInsecure: true})
	require.NoError(err, "New")

	got, err := c.RunSQLQuery(context.Background(), "SELECT name, message_count, id FROM v_labels")
	require.NoError(err, "RunSQLQuery")

	assert.Equal([]string{"name", "message_count", "id"}, got.Columns, "columns")
	assert.Equal(1, got.RowCount, "row count")
	require.Len(got.Rows, 1, "rows")
	assert.Equal(
		[]any{"UNREAD", json.Number("1662130"), json.Number("9007199254740993")},
		got.Rows[0],
		"row cells keep exact integer values",
	)
}

func TestGeneratedClientUsesConfiguredHTTPClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(generated.QueryResult{
			Columns: []string{"n"},
			Rows:    [][]any{{float64(1)}},
		}), "encode response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", HTTPClient: srv.Client()})
	require.NoError(t, err, "New")
	apiClient, err := c.GeneratedClient()
	require.NoError(t, err, "generated client")

	_, err = apiClient.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})

	require.NoError(t, err, "RunQuery")
}

func TestGeneratedResponseErrorReturnsDecodeErrorForOKDecodeFailure(t *testing.T) {
	decodeErr := errors.New("decode response: unexpected EOF")
	err := APIResponseError(&generated.GetStatsResp{
		StatusCode: http.StatusOK,
		Body:       []byte("{"),
	}, decodeErr)

	require.ErrorIs(t, err, decodeErr, "decode error")
	assert.NotContains(t, err.Error(), "API error (200)", "decode failures are not API error bodies")
}

func TestGeneratedResponseMetadataExtractsStatusBodyAndJSON200State(t *testing.T) {
	assert := assert.
		New(t)
	require :=
		require.
			New(t)

	body := []byte(`{"total_messages": 7}`)
	meta, ok := responseMetadata(&generated.GetStatsResp{
		StatusCode: http.StatusOK,
		Body:       body,
		JSON200:    &generated.StatsResponse{TotalMessages: 7},
	})
	require.True(ok, "metadata")
	assert.Equal(http.StatusOK, meta.Status, "status")
	assert.Equal(body, meta.Body, "body")
	assert.True(meta.HasJSON200, "has JSON200")
	assert.False(meta.MissingJSON200, "missing JSON200")

	meta, ok = responseMetadata(&generated.GetCLIStatsResp{
		StatusCode: http.StatusOK,
		Body:       []byte(`{}`),
	})
	require.True(ok, "CLI metadata")
	assert.True(meta.HasJSON200, "nil JSON200 field is still present")
	assert.True(meta.MissingJSON200, "nil JSON200 pointer is missing payload")
}

func TestGeneratedResponseErrorRejectsMissingJSON200Payload(t *testing.T) {
	err := APIResponseError(&generated.GetCLIStatsResp{StatusCode: http.StatusOK}, nil)

	require.Error(t, err, "missing JSON body must fail")
	assert.ErrorContains(t, err, "200 JSON response body")
}

func TestGeneratedCLIResponseErrorReturnsBareServerMessage(t *testing.T) {
	err := CLIResponseError(&generated.CreateCLICollectionResp{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"error":"invalid_collection","message":"bad account"}`),
	}, nil)

	require.EqualError(t, err, "bad account", "CLI error")
}

func TestGeneratedResponseDecodeErrorDetection(t *testing.T) {
	err := &runtime.ResponseDecodeError{Err: errors.New("malformed")}
	assert.True(t, responseDecodeError(err), "decode error")
	assert.False(t, responseDecodeError(errors.New("other")), "other error")
}

func TestRunCLICommandRetriesWhileOperationInProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Millisecond
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/run", r.URL.Path, "path")
		if hits.Add(1) <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte(`{"error":"operation_in_progress","message":"msgvault embeddings build has been running for 42m"}`))
			assert.NoError(err, "write busy response")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte(`{"type":"stdout","data":"done\n"}` + "\n" + `{"type":"complete"}` + "\n"))
		assert.NoError(err, "write stream")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "key", AllowInsecure: true})
	require.NoError(err, "New")
	var notified []string
	c.SetBusyNotifier(func(message string) { notified = append(notified, message) })

	var stdout strings.Builder
	err = c.RunCLICommand(context.Background(), CLIRunRequest{Args: []string{"embeddings", "list"}}, func(stream, data string) error {
		if stream == "stdout" {
			stdout.WriteString(data)
		}
		return nil
	})
	require.NoError(err, "RunCLICommand")

	assert.Equal("done\n", stdout.String(), "stdout streamed after retries")
	assert.Equal(int64(3), hits.Load(), "two busy responses then success")
	require.NotEmpty(notified, "busy notifier called")
	assert.Contains(notified[0], "embeddings build", "notifier names the holder")
}

func TestRunCLICommandStopsRetryingWhenContextCancelled(t *testing.T) {
	require := require.New(t)

	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"a scheduled sync has been running for 5m"}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "key", AllowInsecure: true})
	require.NoError(err, "New")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err = c.RunCLICommand(ctx, CLIRunRequest{Args: []string{"sync"}}, nil)
	require.Error(err, "cancelled retry loop returns an error")
}
