package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPDaemonAPIErrorPreservesMachineCodeAndVectorSemantics(t *testing.T) {
	tests := []struct {
		code     string
		status   int
		sentinel error
	}{
		{code: "vector_not_enabled", status: http.StatusServiceUnavailable, sentinel: vector.ErrNotEnabled},
		{code: "index_stale", status: http.StatusServiceUnavailable, sentinel: vector.ErrIndexStale},
		{code: "index_building", status: http.StatusServiceUnavailable, sentinel: vector.ErrIndexBuilding},
		{code: "embedding_timeout", status: http.StatusServiceUnavailable, sentinel: vector.ErrEmbeddingTimeout},
		{code: "index_scope_mismatch", status: http.StatusBadRequest, sentinel: vector.ErrIndexScopeMismatch},
	}

	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			message := "daemon message for " + test.code
			body, err := json.Marshal(map[string]string{"error": test.code, "message": message})
			must.NoError(err)
			response := &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(string(body)))}

			err = HandleErrorResponse(response)
			must.EqualError(err, fmt.Sprintf("API error (%d): %s", test.status, message))
			var coded interface{ APIErrorCode() string }
			must.ErrorAs(err, &coded)
			checks.Equal(test.code, coded.APIErrorCode())
			checks.ErrorIs(err, test.sentinel)
		})
	}

	t.Run("unknown code stays typed but is not a vector sentinel", func(t *testing.T) {
		checks := assert.New(t)
		must := require.New(t)
		response := &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body: io.NopCloser(strings.NewReader(
				`{"error":"private_backend_failure","message":"sanitized daemon message"}`,
			)),
		}

		err := HandleErrorResponse(response)
		must.EqualError(err, "API error (500): sanitized daemon message")
		var coded interface{ APIErrorCode() string }
		must.ErrorAs(err, &coded)
		checks.Equal("private_backend_failure", coded.APIErrorCode())
		must.NotErrorIs(err, vector.ErrNotEnabled)
		must.NotErrorIs(err, vector.ErrIndexStale)
		must.NotErrorIs(err, vector.ErrIndexBuilding)
		must.NotErrorIs(err, vector.ErrEmbeddingTimeout)
		must.NotErrorIs(err, vector.ErrIndexScopeMismatch)
	})
}

func TestMCPTracePropagationDaemonClientInjectsW3CHeaders(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const (
		traceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		traceState  = "vendor=value"
		baggage     = "tenant=test"
	)

	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{
		URL:           server.URL,
		AllowInsecure: true,
		HTTPClient:    server.Client(),
	})
	must.NoError(err)

	carrier := propagation.HeaderCarrier(http.Header{
		"Traceparent": []string{traceParent},
		"Tracestate":  []string{traceState},
		"Baggage":     []string{baggage},
	})
	ctx := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	).Extract(context.Background(), carrier)
	response, err := client.DoGeneratedRequestWithContext(
		ctx,
		http.MethodGet,
		"/trace",
		&generated.RunCLIRequestOptions{},
	)
	must.NoError(err)
	must.NotNil(response)
	must.NoError(response.Body.Close())

	got := <-headers
	checks.Equal(traceParent, got.Get("Traceparent"))
	checks.Equal(traceState, got.Get("Tracestate"))
	checks.Equal(baggage, got.Get("Baggage"))
}

func TestCLIIdentityDiscoverProviderStreamsConvertedEventsAndRequest(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/cli/identities/discover", r.URL.Path)
		var req identityops.DiscoverRequest
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&req), "decode discovery request") {
			http.Error(w, "bad discovery request", http.StatusBadRequest)
			return
		}
		assertions.Equal(identityops.SourceSelector{SourceID: 14}, req.SourceSelector)
		assertions.True(req.Apply)
		assertions.True(req.Provider)
		assertions.Equal([]string{"weak@example.test"}, req.Confirm)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte(
			`{"type":"progress","progress":{"done":2,"total":2,"candidates":1}}` + "\n" +
				`{"type":"result","result":{"account":"primary@example.test","source_id":14,"source_type":"imap","scanned_messages":2,"candidates":[{"identifier":"alias@example.test","normalized_identifier":"alias@example.test","classification":"strong","already_confirmed":false,"signals":["provider-alias"],"provider_states":["disabled","enabled"],"sent_message_count":0,"received_message_count":0,"first_seen_at":"0001-01-01T00:00:00Z","last_seen_at":"0001-01-01T00:00:00Z"}],"rejected":[],"applied":[]}}` + "\n",
		))
		assertions.NoError(err)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Config{URL: srv.URL, AllowInsecure: true})
	requirements.NoError(err)
	var events []identityops.DiscoverEvent
	err = client.DiscoverCLIIdentities(t.Context(), identityops.DiscoverRequest{
		SourceSelector: identityops.SourceSelector{SourceID: 14},
		Apply:          true,
		Provider:       true,
		Confirm:        []string{"weak@example.test"},
	}, func(event identityops.DiscoverEvent) error {
		events = append(events, event)
		return nil
	})

	requirements.NoError(err)
	requirements.Len(events, 2)
	assertions.Equal("progress", events[0].Type)
	requirements.NotNil(events[0].Progress)
	assertions.Equal(int64(2), events[0].Progress.Done)
	assertions.Equal("result", events[1].Type)
	requirements.NotNil(events[1].Result)
	assertions.Equal(int64(14), events[1].Result.SourceID)
	encoded, err := json.Marshal(events[1].Result.Candidates[0])
	requirements.NoError(err)
	var reported struct {
		ProviderStates []string `json:"provider_states"`
	}
	requirements.NoError(json.Unmarshal(encoded, &reported))
	assertions.Equal([]string{"disabled", "enabled"}, reported.ProviderStates,
		"generated-client conversion must retain provider state metadata")
}

func TestCLIIdentityDiscoverRejectsStreamWithoutResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte(
			`{"type":"progress","progress":{"done":1,"total":2,"candidates":1}}` + "\n",
		))
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	client, err := New(Config{URL: srv.URL, AllowInsecure: true})
	require.NoError(t, err)

	err = client.DiscoverCLIIdentities(t.Context(), identityops.DiscoverRequest{
		SourceSelector: identityops.SourceSelector{SourceID: 14},
	}, nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "without result")
}

func TestCLIIdentityDiscoverConsumesSanitizedTerminalError(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	const secret = "provider-token-private-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte(
			`{"type":"progress","progress":{"done":1,"total":2,"candidates":1}}` + "\n" +
				`{"type":"error","error":{"code":"internal_error","message":"Failed to discover identities"}}` + "\n",
		))
		assertions.NoError(err)
	}))
	t.Cleanup(srv.Close)
	client, err := New(Config{URL: srv.URL, AllowInsecure: true})
	requirements.NoError(err)

	var events []identityops.DiscoverEvent
	err = client.DiscoverCLIIdentities(t.Context(), identityops.DiscoverRequest{
		SourceSelector: identityops.SourceSelector{SourceID: 14},
	}, func(event identityops.DiscoverEvent) error {
		events = append(events, event)
		return nil
	})

	requirements.Error(err)
	requirements.ErrorContains(err, "Failed to discover identities")
	assertions.NotContains(err.Error(), secret)
	var discoverErr *identityops.DiscoverError
	requirements.ErrorAs(err, &discoverErr)
	assertions.Equal("internal_error", discoverErr.Code)
	assertions.Equal("Failed to discover identities", discoverErr.Message)
	requirements.Len(events, 1)
	assertions.Equal("progress", events[0].Type)
}

func TestCLIIdentityImportSendsParsedEntriesAndConvertsResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodPost, r.Method)
		assertions.Equal("/api/v1/cli/identities/import", r.URL.Path)
		var raw map[string]json.RawMessage
		if !assertions.NoError(json.NewDecoder(r.Body).Decode(&raw), "decode import request") {
			http.Error(w, "bad import request", http.StatusBadRequest)
			return
		}
		assertions.NotContains(raw, "file")
		assertions.NotContains(raw, "data")
		encoded, err := json.Marshal(raw)
		if !assertions.NoError(err) {
			http.Error(w, "encode import request", http.StatusInternalServerError)
			return
		}
		var req identityops.ImportRequest
		if !assertions.NoError(json.Unmarshal(encoded, &req)) {
			http.Error(w, "decode import request", http.StatusBadRequest)
			return
		}
		assertions.Empty(req.Account)
		assertions.Equal(int64(14), req.SourceID)
		assertions.Equal("bulk-import", req.Signal)
		assertions.True(req.Apply)
		assertions.Equal([]identityops.ImportEntry{{
			Identifier: "alias@example.test", State: "disabled",
		}}, req.Entries)

		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.NewEncoder(w).Encode(identityops.ImportResult{
			Account:  "primary@example.test",
			SourceID: 14,
			Signal:   "bulk-import",
			Candidates: []identityops.Candidate{{
				Identifier:           "alias@example.test",
				NormalizedIdentifier: "alias@example.test",
				Classification:       "strong",
				Signals:              []string{"bulk-import"},
				ProviderStates:       []string{"disabled"},
			}},
			Applied: []store.IdentityConfirmationOutcome{{
				Identifier: "alias@example.test", Added: true, Signals: []string{"bulk-import"},
			}},
		}))
	}))
	t.Cleanup(srv.Close)

	client, err := New(Config{URL: srv.URL, AllowInsecure: true})
	requirements.NoError(err)
	result, err := client.ImportCLIIdentities(t.Context(), identityops.ImportRequest{
		SourceSelector: identityops.SourceSelector{SourceID: 14},
		Entries: []identityops.ImportEntry{{
			Identifier: "alias@example.test", State: "disabled",
		}},
		Signal: "bulk-import",
		Apply:  true,
	})

	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.Equal("primary@example.test", result.Account)
	assertions.Equal([]string{"disabled"}, result.Candidates[0].ProviderStates)
	assertions.Equal([]store.IdentityConfirmationOutcome{{
		Identifier: "alias@example.test", Added: true, Signals: []string{"bulk-import"},
	}}, result.Applied)
}

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

func TestNewCLIModeDisablesWholeRequestTimeoutAndPreservesTransport(t *testing.T) {
	assert := assert.New(t)
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 7 * time.Second}).DialContext,
		TLSHandshakeTimeout: 11 * time.Second,
	}
	base := &http.Client{Transport: transport, Timeout: 45 * time.Second}

	c, err := New(Config{
		URL:         "https://nas.example:8443",
		HTTPClient:  base,
		RequestMode: RequestModeCLI,
	})
	require.NoError(t, err, "New")

	assert.Zero(c.Timeout(), "CLI operations are governed by their context")
	assert.Same(transport, c.httpClient.Transport, "transport-level bounds are retained")
	assert.Equal(11*time.Second, transport.TLSHandshakeTimeout)
	assert.Equal(45*time.Second, base.Timeout, "caller-owned client is not mutated")
}

func TestCLIModeGeneratedRequestCarriesClassAndAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, apiprotocol.ClientClassCLI, r.Header.Get(apiprotocol.ClientClassHeader))
		assert.Equal(t, "secret-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n"],"rows":[[1]],"row_count":1}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		URL: srv.URL, APIKey: "secret-key", AllowInsecure: true,
		RequestMode: RequestModeCLI,
	})
	require.NoError(t, err, "New")
	_, err = c.RunSQLQuery(context.Background(), "SELECT 1")
	require.NoError(t, err, "RunSQLQuery")
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

func TestRebuildCLIFTSRetriesWhileOperationInProgress(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Millisecond
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/rebuild-fts", r.URL.Path, "path")
		if hits.Add(1) <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write([]byte(`{"error":"operation_in_progress","message":"msgvault embeddings build has been running for 42m"}`))
			assert.NoError(err, "write busy response")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte(`{"type":"progress","done":5,"total":10}` + "\n" + `{"type":"complete","indexed":10}` + "\n"))
		assert.NoError(err, "write stream")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "key", AllowInsecure: true})
	require.NoError(err, "New")

	var progressCalls int
	indexed, err := c.RebuildCLIFTS(context.Background(), func(_, _ int64) { progressCalls++ })
	require.NoError(err, "RebuildCLIFTS")

	assert.Equal(int64(10), indexed, "indexed count after retries")
	assert.Equal(1, progressCalls, "progress streamed")
	assert.Equal(int64(3), hits.Load(), "two busy responses then success")
}

func TestGeneratedNonStreamingBusyRetryReturnsRootContextCancellation(t *testing.T) {
	require := require.New(t)
	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Second
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, err := w.Write([]byte(
			`{"error":"operation_in_progress","message":"a scheduled sync has been running for 5m"}`,
		))
		assert.NoError(t, err, "write busy response")
	}))
	t.Cleanup(srv.Close)

	rootCtx, cancel := context.WithCancel(context.Background())
	c, err := New(Config{
		URL:           srv.URL,
		APIKey:        "key",
		AllowInsecure: true,
		Context:       rootCtx,
	})
	require.NoError(err, "New")

	waiting := make(chan struct{})
	c.SetBusyNotifier(func(string) { close(waiting) })
	done := make(chan error, 1)
	go func() {
		_, err := c.GetStats()
		done <- err
	}()

	select {
	case <-waiting:
	case <-time.After(time.Second):
		require.FailNow("generated response did not enter busy retry wait")
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(time.Second):
		require.FailNow("generated response did not return after root context cancellation")
	}
}

func TestRawAndStreamingRequestsUseClientRootContext(t *testing.T) {
	tests := []struct {
		name    string
		request func(*Client) (*http.Response, error)
	}{
		{
			name: "raw",
			request: func(c *Client) (*http.Response, error) {
				return c.DoGeneratedRequestWithContext(
					context.Background(),
					http.MethodGet,
					"/api/v1/stats",
					&generated.RunCLIRequestOptions{},
				)
			},
		},
		{
			name: "streaming",
			request: func(c *Client) (*http.Response, error) {
				return c.DoGeneratedStreamingRequestWithContext(
					context.Background(),
					http.MethodPost,
					"/api/v1/cli/run",
					&generated.RunCLIRequestOptions{},
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			requestStarted := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				close(requestStarted)
				<-release
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})

			rootCtx, cancelRoot := context.WithCancel(context.Background())
			t.Cleanup(cancelRoot)
			c, err := New(Config{
				URL:           srv.URL,
				AllowInsecure: true,
				HTTPClient:    srv.Client(),
				Context:       rootCtx,
				RequestMode:   RequestModeCLI,
			})
			require.NoError(err, "New")

			done := make(chan error, 1)
			go func() {
				resp, err := tt.request(c)
				if resp != nil {
					_ = resp.Body.Close()
				}
				done <- err
			}()

			select {
			case <-requestStarted:
			case <-time.After(time.Second):
				require.FailNow("request did not start")
			}
			cancelRoot()

			select {
			case err = <-done:
			case <-time.After(time.Second):
				close(release)
				err = <-done
			}
			require.ErrorIs(err, context.Canceled)
		})
	}
}

func TestRunCLICommandStopsRetryingWhenContextCancelled(t *testing.T) {
	require := require.New(t)

	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Second
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
	waiting := make(chan struct{})
	c.SetBusyNotifier(func(string) { close(waiting) })
	done := make(chan error, 1)
	go func() {
		done <- c.RunCLICommand(ctx, CLIRunRequest{Args: []string{"sync"}}, nil)
	}()

	select {
	case <-waiting:
	case <-time.After(time.Second):
		require.FailNow("streaming request did not enter busy retry wait")
	}
	cancel()

	select {
	case err = <-done:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(time.Second):
		require.FailNow("streaming busy retry did not return after context cancellation")
	}
}
