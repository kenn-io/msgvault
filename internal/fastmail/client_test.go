package fastmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "fm_synthetic_token_never_log"

func TestListIdentityRecordsIncludesHistoricalMaskedAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	methodRequests := make(chan requestCapture, 1)
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL: baseURL + "/jmap",
			Capabilities: capabilitySet(
				CoreCapability,
				MaskedEmailCapability,
				SubmissionCapability,
			),
			Accounts: map[string]sessionAccount{
				"masked-account": {
					AccountCapabilities: capabilitySet(MaskedEmailCapability),
				},
				"submission-account": {
					AccountCapabilities: capabilitySet(SubmissionCapability),
				},
			},
			PrimaryAccounts: map[string]string{
				MaskedEmailCapability: "masked-account",
				SubmissionCapability:  "submission-account",
			},
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		var request jmapRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		methodRequests <- requestCapture{request: request, err: err}
		if err != nil {
			http.Error(w, "invalid JMAP request", http.StatusBadRequest)
			return
		}
		assert.NoError(writeJSON(w, map[string]any{
			"methodResponses": []any{
				[]any{"MaskedEmail/get", map[string]any{
					"accountId": "masked-account",
					"state":     "masked-state",
					"list": []any{
						map[string]any{"id": "4", "email": "waiting@example.test", "state": "pending"},
						map[string]any{"id": "2", "email": "old@example.test", "state": "disabled"},
						map[string]any{"id": "1", "email": "Active@example.test", "state": "enabled"},
						map[string]any{"id": "3", "email": "deleted@example.test", "state": "deleted"},
					},
				}, "masked"},
				[]any{"Identity/get", map[string]any{
					"accountId": "submission-account",
					"state":     "identity-state",
					"list": []any{
						map[string]any{"id": "identity-2", "email": "send-as@example.test", "name": "Send As"},
						map[string]any{"id": "identity-1", "email": "*@example.test", "name": "Wildcard"},
					},
				}, "identity"},
			},
		}))
	})

	got, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	require.NoError(err)
	methodRequest := <-methodRequests
	require.NoError(methodRequest.err)
	assert.Equal([]Record{
		{Identifier: "*@example.test", State: "enabled", Kind: "identity"},
		{Identifier: "Active@example.test", State: "enabled", Kind: "masked-email"},
		{Identifier: "deleted@example.test", State: "deleted", Kind: "masked-email"},
		{Identifier: "old@example.test", State: "disabled", Kind: "masked-email"},
		{Identifier: "send-as@example.test", State: "enabled", Kind: "identity"},
		{Identifier: "waiting@example.test", State: "pending", Kind: "masked-email"},
	}, got)

	require.Len(methodRequest.request.MethodCalls, 2)
	assert.Equal([]string{
		CoreCapability,
		MaskedEmailCapability,
		SubmissionCapability,
	}, methodRequest.request.Using)
	assertMethodCall(t, methodRequest.request.MethodCalls[0], "MaskedEmail/get", "masked-account", "masked")
	assertMethodCall(t, methodRequest.request.MethodCalls[1], "Identity/get", "submission-account", "identity")
}

func TestListIdentityRecordsWorksWithMaskedEmailCapabilityOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	methodRequests := make(chan requestCapture, 1)
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL:       baseURL + "/jmap",
			Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability),
			Accounts: map[string]sessionAccount{
				"masked-only": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
			},
			PrimaryAccounts: map[string]string{MaskedEmailCapability: "masked-only"},
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		var request jmapRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		methodRequests <- requestCapture{request: request, err: err}
		if err != nil {
			http.Error(w, "invalid JMAP request", http.StatusBadRequest)
			return
		}
		assert.NoError(writeJSON(w, map[string]any{
			"methodResponses": []any{
				[]any{"MaskedEmail/get", map[string]any{
					"accountId": "masked-only",
					"list": []any{
						map[string]any{"id": "1", "email": "masked@example.test", "state": "enabled"},
					},
				}, "masked"},
			},
		}))
	})

	got, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	require.NoError(err)
	methodRequest := <-methodRequests
	require.NoError(methodRequest.err)
	assert.Equal([]Record{
		{Identifier: "masked@example.test", State: "enabled", Kind: "masked-email"},
	}, got)
	require.Len(methodRequest.request.MethodCalls, 1)
	assert.Equal([]string{CoreCapability, MaskedEmailCapability}, methodRequest.request.Using)
	assertMethodCall(t, methodRequest.request.MethodCalls[0], "MaskedEmail/get", "masked-only", "masked")
}

func TestListIdentityRecordsSkipsSubmissionWithoutAccessibleAccount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	methodRequests := make(chan requestCapture, 1)
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL: baseURL + "/jmap",
			Capabilities: capabilitySet(
				CoreCapability,
				MaskedEmailCapability,
				SubmissionCapability,
			),
			Accounts: map[string]sessionAccount{
				"masked-only": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
			},
			PrimaryAccounts: map[string]string{MaskedEmailCapability: "masked-only"},
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		var request jmapRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		methodRequests <- requestCapture{request: request, err: err}
		if err != nil {
			http.Error(w, "invalid JMAP request", http.StatusBadRequest)
			return
		}
		assert.NoError(writeJSON(w, map[string]any{
			"methodResponses": []any{
				[]any{"MaskedEmail/get", map[string]any{
					"accountId": "masked-only",
					"list": []any{
						map[string]any{"id": "1", "email": "historical@example.test", "state": "disabled"},
					},
				}, "masked"},
			},
		}))
	})

	got, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	require.NoError(err)
	assert.Equal([]Record{
		{Identifier: "historical@example.test", State: "disabled", Kind: "masked-email"},
	}, got)
	methodRequest := <-methodRequests
	require.NoError(methodRequest.err)
	require.Len(methodRequest.request.MethodCalls, 1)
	assert.Equal([]string{CoreCapability, MaskedEmailCapability}, methodRequest.request.Using)
	assertMethodCall(t, methodRequest.request.MethodCalls[0], "MaskedEmail/get", "masked-only", "masked")
}

func TestListIdentityRecordsUsesUniqueAccountCapabilityFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	methodRequests := make(chan requestCapture, 1)
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL:       baseURL + "/jmap",
			Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability, SubmissionCapability),
			Accounts: map[string]sessionAccount{
				"mail-account": {AccountCapabilities: capabilitySet("urn:ietf:params:jmap:mail")},
				"masked-account": {
					AccountCapabilities: capabilitySet(MaskedEmailCapability),
				},
				"submission-account": {
					AccountCapabilities: capabilitySet(SubmissionCapability),
				},
			},
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		var request jmapRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		methodRequests <- requestCapture{request: request, err: err}
		if err != nil {
			http.Error(w, "invalid JMAP request", http.StatusBadRequest)
			return
		}
		assert.NoError(writeJSON(w, map[string]any{
			"methodResponses": []any{
				[]any{"MaskedEmail/get", map[string]any{"accountId": "masked-account", "list": []any{}}, "masked"},
				[]any{"Identity/get", map[string]any{"accountId": "submission-account", "list": []any{}}, "identity"},
			},
		}))
	})

	got, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	require.NoError(err)
	assert.Empty(got)
	methodRequest := <-methodRequests
	require.NoError(methodRequest.err)
	require.Len(methodRequest.request.MethodCalls, 2)
	assertMethodCall(t, methodRequest.request.MethodCalls[0], "MaskedEmail/get", "masked-account", "masked")
	assertMethodCall(t, methodRequest.request.MethodCalls[1], "Identity/get", "submission-account", "identity")
}

func TestListIdentityRecordsReturnsTypedCapabilityError(t *testing.T) {
	var methodRequests atomic.Int32
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL:          baseURL + "/jmap",
			Capabilities:    capabilitySet(CoreCapability),
			Accounts:        map[string]sessionAccount{},
			PrimaryAccounts: map[string]string{},
		}
	}, func(w http.ResponseWriter, _ *http.Request) {
		methodRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	var capabilityErr *CapabilityError
	require.ErrorAs(t, err, &capabilityErr)
	assert.Equal(t, MaskedEmailCapability, capabilityErr.Capability)
	assert.Equal(t, int32(0), methodRequests.Load())
}

func TestListIdentityRecordsRejectsAmbiguousCapabilityAccount(t *testing.T) {
	var methodRequests atomic.Int32
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL:       baseURL + "/jmap",
			Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability),
			Accounts: map[string]sessionAccount{
				"masked-a": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
				"masked-b": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
			},
		}
	}, func(w http.ResponseWriter, _ *http.Request) {
		methodRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	require.ErrorContains(t, err, "ambiguous")
	assert.NotContains(t, err.Error(), testToken)
	assert.Equal(t, int32(0), methodRequests.Load())
}

func TestListIdentityRecordsRejectsCrossOriginAPIURLBeforeSendingToken(t *testing.T) {
	var crossOriginRequests atomic.Int32
	crossOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		crossOriginRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(crossOrigin.Close)

	session := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assertBearer(t, r) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		assert.NoError(t, writeJSON(w, sessionResponse{
			APIURL:       crossOrigin.URL + "/jmap",
			Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability),
			Accounts: map[string]sessionAccount{
				"masked": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
			},
			PrimaryAccounts: map[string]string{MaskedEmailCapability: "masked"},
		}))
	}))
	t.Cleanup(session.Close)

	_, err := newClient(testToken, session.Client(), session.URL+"/session").ListIdentityRecords(context.Background())
	require.ErrorContains(t, err, "cross-origin")
	assert.Equal(t, int32(0), crossOriginRequests.Load())
	assert.NotContains(t, err.Error(), testToken)
}

func TestListIdentityRecordsDoesNotFollowAuthenticatedRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(redirectTarget.Close)

	session := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	t.Cleanup(session.Close)

	_, err := newClient(testToken, session.Client(), session.URL+"/session").ListIdentityRecords(context.Background())
	require.Error(t, err)
	assert.Equal(t, int32(0), redirectedRequests.Load())
	assert.NotContains(t, err.Error(), testToken)
}

func TestJMAPHTTPErrorsRedactTokenAndBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{
			APIURL:       baseURL + "/jmap",
			Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability),
			Accounts: map[string]sessionAccount{
				"masked": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
			},
			PrimaryAccounts: map[string]string{MaskedEmailCapability: "masked"},
		}
	}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"detail":"private-address@example.test","token":"fm_body_secret"}`))
	})

	_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
	require.Error(err)
	require.ErrorContains(err, "MaskedEmail/get")
	require.ErrorContains(err, "502")
	assert.NotContains(err.Error(), testToken)
	assert.NotContains(err.Error(), "private-address@example.test")
	assert.NotContains(err.Error(), "fm_body_secret")
}

func TestListIdentityRecordsFailsClosedForInvalidMethodResponses(t *testing.T) {
	tests := []struct {
		name            string
		methodResponses []any
		wantError       string
	}{
		{
			name: "method error",
			methodResponses: []any{
				[]any{"error", map[string]any{"type": "serverFail", "description": "private@example.test"}, "masked"},
			},
			wantError: "MaskedEmail/get",
		},
		{
			name: "wrong call id",
			methodResponses: []any{
				[]any{"MaskedEmail/get", map[string]any{"accountId": "masked", "list": []any{}}, "other"},
			},
			wantError: "unexpected JMAP call ID",
		},
		{
			name: "wrong method",
			methodResponses: []any{
				[]any{"Identity/get", map[string]any{"accountId": "masked", "list": []any{}}, "masked"},
			},
			wantError: "unexpected JMAP method",
		},
		{
			name: "mismatched response account",
			methodResponses: []any{
				[]any{"MaskedEmail/get", map[string]any{"accountId": "private-account@example.test", "list": []any{}}, "masked"},
			},
			wantError: "unexpected account in MaskedEmail/get response",
		},
		{
			name: "duplicate expected call id",
			methodResponses: []any{
				[]any{"MaskedEmail/get", map[string]any{"accountId": "masked", "list": []any{}}, "masked"},
				[]any{"MaskedEmail/get", map[string]any{"accountId": "masked", "list": []any{}}, "masked"},
			},
			wantError: "duplicate JMAP call ID for MaskedEmail/get",
		},
		{
			name: "missing expected response",
			methodResponses: []any{
				[]any{"MaskedEmail/get", map[string]any{"accountId": "masked", "list": []any{}}, "masked"},
			},
			wantError: "missing Identity/get response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := newTestServer(t, func(baseURL string) sessionResponse {
				return sessionResponse{
					APIURL: baseURL + "/jmap",
					Capabilities: capabilitySet(
						CoreCapability,
						MaskedEmailCapability,
						SubmissionCapability,
					),
					Accounts: map[string]sessionAccount{
						"masked":     {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
						"submission": {AccountCapabilities: capabilitySet(SubmissionCapability)},
					},
					PrimaryAccounts: map[string]string{
						MaskedEmailCapability: "masked",
						SubmissionCapability:  "submission",
					},
				}
			}, func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(writeJSON(w, map[string]any{"methodResponses": tt.methodResponses}))
			})

			_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
			require.Error(err)
			require.ErrorContains(err, tt.wantError)
			assert.NotContains(err.Error(), "private@example.test")
			assert.NotContains(err.Error(), "private-account@example.test")
			assert.NotContains(err.Error(), testToken)
		})
	}
}

func TestListIdentityRecordsClassifiesObjectLimitErrors(t *testing.T) {
	tests := []struct {
		name            string
		coreCapability  json.RawMessage
		methodResponses []any
		wantMethod      string
		wantLimit       int64
	}{
		{
			name:           "masked email inventory over limit",
			coreCapability: json.RawMessage(`{"maxObjectsInGet": 2}`),
			methodResponses: []any{
				[]any{"error", map[string]any{
					"type":        "requestTooLarge",
					"description": "private@example.test",
				}, "masked"},
			},
			wantMethod: "MaskedEmail/get",
			wantLimit:  2,
		},
		{
			name:           "identity inventory over limit",
			coreCapability: json.RawMessage(`{"maxObjectsInGet": 3}`),
			methodResponses: []any{
				[]any{"MaskedEmail/get", map[string]any{"accountId": "masked", "list": []any{}}, "masked"},
				[]any{"error", map[string]any{
					"type":        "requestTooLarge",
					"description": "private@example.test",
				}, "identity"},
			},
			wantMethod: "Identity/get",
			wantLimit:  3,
		},
		{
			name:           "limit not advertised",
			coreCapability: json.RawMessage(`{}`),
			methodResponses: []any{
				[]any{"error", map[string]any{"type": "requestTooLarge"}, "masked"},
			},
			wantMethod: "MaskedEmail/get",
			wantLimit:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := newTestServer(t, func(baseURL string) sessionResponse {
				capabilities := capabilitySet(CoreCapability, MaskedEmailCapability, SubmissionCapability)
				capabilities[CoreCapability] = tt.coreCapability
				return sessionResponse{
					APIURL:       baseURL + "/jmap",
					Capabilities: capabilities,
					Accounts: map[string]sessionAccount{
						"masked":     {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
						"submission": {AccountCapabilities: capabilitySet(SubmissionCapability)},
					},
					PrimaryAccounts: map[string]string{
						MaskedEmailCapability: "masked",
						SubmissionCapability:  "submission",
					},
				}
			}, func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(writeJSON(w, map[string]any{"methodResponses": tt.methodResponses}))
			})

			_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(context.Background())
			var limitErr *ObjectLimitError
			require.ErrorAs(err, &limitErr)
			assert.Equal(tt.wantMethod, limitErr.Method)
			assert.Equal(tt.wantLimit, limitErr.MaxObjectsInGet)
			require.ErrorContains(err, "requestTooLarge")
			assert.NotContains(err.Error(), "private@example.test")
			assert.NotContains(err.Error(), testToken)
		})
	}
}

func TestListIdentityRecordsReturnsContextCancellation(t *testing.T) {
	t.Run("session request", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(ctx)
			done <- err
		}()
		waitForRequest(t, started)
		cancel()
		assert.ErrorIs(t, waitForError(t, done), context.Canceled)
	})

	t.Run("method request", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		methodRequests := make(chan requestCapture, 1)
		srv := newTestServer(t, func(baseURL string) sessionResponse {
			return sessionResponse{
				APIURL:       baseURL + "/jmap",
				Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability),
				Accounts: map[string]sessionAccount{
					"masked": {AccountCapabilities: capabilitySet(MaskedEmailCapability)},
				},
				PrimaryAccounts: map[string]string{MaskedEmailCapability: "masked"},
			}
		}, func(w http.ResponseWriter, r *http.Request) {
			var request jmapRequest
			err := json.NewDecoder(r.Body).Decode(&request)
			methodRequests <- requestCapture{request: request, err: err}
			if err != nil {
				http.Error(w, "invalid JMAP request", http.StatusBadRequest)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := newClient(testToken, srv.Client(), srv.URL+"/session").ListIdentityRecords(ctx)
			done <- err
		}()
		waitForRequest(t, started)
		cancel()
		require.ErrorIs(t, waitForError(t, done), context.Canceled)
		require.NoError(t, (<-methodRequests).err)
	})
}

func TestNewClientDefaultsNilHTTPClient(t *testing.T) {
	client := NewClient(testToken, nil)
	require.NotNil(t, client)
	assert.NotNil(t, client.httpClient)
	assert.Equal(t, defaultRequestTimeout, client.httpClient.Timeout)
}

func TestNewClientCapsRequestTimeoutWithoutLengtheningShorterCustomTimeout(t *testing.T) {
	tests := []struct {
		name        string
		timeout     time.Duration
		wantTimeout time.Duration
	}{
		{name: "unset", timeout: 0, wantTimeout: defaultRequestTimeout},
		{name: "shorter", timeout: 2 * time.Second, wantTimeout: 2 * time.Second},
		{name: "longer", timeout: 2 * defaultRequestTimeout, wantTimeout: defaultRequestTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := &http.Client{Timeout: tt.timeout}

			client := NewClient(testToken, original)

			require.NotNil(t, client)
			assert.Equal(t, tt.wantTimeout, client.httpClient.Timeout)
			assert.Equal(t, tt.timeout, original.Timeout, "caller-owned client must not be mutated")
		})
	}
}

type testSessionFactory func(baseURL string) sessionResponse

type requestCapture struct {
	request jmapRequest
	err     error
}

func newTestServer(
	t *testing.T,
	session testSessionFactory,
	methodHandler func(w http.ResponseWriter, r *http.Request),
) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			if !assertBearer(t, r) {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			assert.NoError(t, writeJSON(w, session(srv.URL)))
		case "/jmap":
			if !assertBearer(t, r) {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			methodHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func assertBearer(t *testing.T, r *http.Request) bool {
	t.Helper()
	return assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
}

func assertMethodCall(t *testing.T, call []json.RawMessage, method, accountID, callID string) {
	t.Helper()
	require.Len(t, call, 3)
	var gotMethod string
	require.NoError(t, json.Unmarshal(call[0], &gotMethod))
	assert.Equal(t, method, gotMethod)
	var args struct {
		AccountID string `json:"accountId"`
	}
	require.NoError(t, json.Unmarshal(call[1], &args))
	assert.Equal(t, accountID, args.AccountID)
	var gotCallID string
	require.NoError(t, json.Unmarshal(call[2], &gotCallID))
	assert.Equal(t, callID, gotCallID)
}

func capabilitySet(capabilities ...string) map[string]json.RawMessage {
	set := make(map[string]json.RawMessage, len(capabilities))
	for _, capability := range capabilities {
		set[capability] = json.RawMessage(`{}`)
	}
	return set
}

func writeJSON[T any](w http.ResponseWriter, value T) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(value)
}

func waitForRequest(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for request")
	}
}

func waitForError(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		require.Fail(t, "timed out waiting for client result")
		return nil
	}
}
