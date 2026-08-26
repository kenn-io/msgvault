package peoplesweep_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

type capturedChatRequest struct {
	Model               string                 `json:"model"`
	Messages            []capturedChatMessage  `json:"messages"`
	ResponseFormat      capturedResponseFormat `json:"response_format"`
	MaxCompletionTokens int                    `json:"max_completion_tokens"`
	MaxTokens           *int                   `json:"max_tokens"`
}

type capturedChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type capturedResponseFormat struct {
	Type       string             `json:"type"`
	JSONSchema capturedJSONSchema `json:"json_schema"`
}

type capturedJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

func providerTestProfile(t *testing.T, endpoint string, anonymous bool) peoplesweep.ProviderProfile {
	t.Helper()
	apiKeyEnv := "TEST_KEY"
	if anonymous {
		apiKeyEnv = ""
	}
	config := peoplesweep.Config{
		Enabled: true,
		Provider: peoplesweep.ProviderConfig{
			Kind:             peoplesweep.ProviderOpenAICompatible,
			Endpoint:         endpoint,
			Model:            "gpt-test",
			APIKeyEnv:        apiKeyEnv,
			AllowAnonymous:   anonymous,
			RetentionPosture: "zero_retention",
			TrainingPosture:  "no_training",
			AllowedSources: []peoplesweep.SourceClass{
				peoplesweep.SourceConversationText,
			},
			SourceSince:    "2025-01-01",
			RequestTimeout: time.Minute,
		},
	}
	config.ApplyDefaults()
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}

func structuredTestRequest() peoplesweep.StructuredRequest {
	return peoplesweep.StructuredRequest{
		ProgramID: "person-facts", ProgramVersion: "1.0.0",
		Sources: []peoplesweep.SourceDescriptor{{
			Class: peoplesweep.SourceConversationText, ObservedOn: "2025-08-20",
		}},
		InputText:       "Synthetic input only.",
		SchemaName:      "person_facts",
		JSONSchema:      json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}`),
		MaxOutputTokens: 32,
	}
}

func generateOpenAICompatibleJSON(
	ctx context.Context,
	profile peoplesweep.ProviderProfile,
	transport *peoplesweep.OpenAICompatibleTransport,
	credential string,
	request peoplesweep.StructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	prepared, err := transport.PrepareJSON(profile, request)
	if err != nil {
		return peoplesweep.StructuredResponse{}, err
	}
	return transport.GeneratePreparedJSON(ctx, profile, credential, prepared)
}

func TestOpenAICompatibleTransportGeneratesStructuredJSON(t *testing.T) {
	assert := assert.New(t)
	var captured capturedChatRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/v1/chat/completions", r.URL.Path)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		assert.Equal("Bearer test-key", r.Header.Get("Authorization"))
		assert.NoError(json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("x-request-id", "req-1")
		_, err := io.WriteString(w,
			`{"model":"gpt-test","choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	request := structuredTestRequest()
	got, err := generateOpenAICompatibleJSON(
		t.Context(), providerTestProfile(t, server.URL+"/v1", false),
		peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", request,
	)
	require.NoError(t, err)
	assert.JSONEq(`{"ok":true}`, string(got.Output))
	assert.Equal("req-1", got.ProviderRequestID)
	assert.Equal(int64(7), got.Usage.InputTokens)
	assert.Equal(int64(3), got.Usage.OutputTokens)

	assert.Equal("gpt-test", captured.Model)
	assert.Equal(32, captured.MaxCompletionTokens)
	assert.Nil(captured.MaxTokens, "deprecated max_tokens must not be sent")
	require.Len(t, captured.Messages, 2)
	assert.Equal("system", captured.Messages[0].Role)
	assert.Equal("Return one JSON value that strictly matches the supplied JSON Schema.",
		captured.Messages[0].Content)
	assert.Equal("user", captured.Messages[1].Role)
	assert.Equal(request.InputText, captured.Messages[1].Content)
	assert.Equal("json_schema", captured.ResponseFormat.Type)
	assert.Equal(request.SchemaName, captured.ResponseFormat.JSONSchema.Name)
	assert.True(captured.ResponseFormat.JSONSchema.Strict)
	assert.JSONEq(string(request.JSONSchema),
		string(captured.ResponseFormat.JSONSchema.Schema))
}

func TestOpenAICompatibleTransportOmitsAuthorizationForAnonymousLoopback(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, err := io.WriteString(w,
			`{"model":"gpt-test","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	_, err := generateOpenAICompatibleJSON(
		t.Context(), providerTestProfile(t, server.URL+"/v1", true),
		peoplesweep.NewOpenAICompatibleTransport(server.Client()), "", structuredTestRequest(),
	)
	require.NoError(t, err)
	assert.Empty(t, authorization)
}

func TestOpenAICompatibleTransportSanitizesHTTPFailures(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", "req-secret-safe")
				w.WriteHeader(status)
				_, err := io.WriteString(w, `{"error":{"message":"provider-secret-body"}}`)
				assert.NoError(err)
			}))
			defer server.Close()

			_, err := generateOpenAICompatibleJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", structuredTestRequest(),
			)
			require.Error(t, err)
			var providerErr *peoplesweep.ProviderError
			require.ErrorAs(t, err, &providerErr)
			assert.Equal(status, providerErr.StatusCode)
			assert.Equal("req-secret-safe", providerErr.RequestID)
			assert.NotContains(err.Error(), "provider-secret-body")
			assert.NotContains(err.Error(), "test-key")
		})
	}
}

func TestOpenAICompatibleTransportDiscardsUnsafeRequestIDs(t *testing.T) {
	for _, requestID := range []string{
		"archive claim text",
		strings.Repeat("a", 129),
	} {
		t.Run(requestID[:1], func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", requestID)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()

			_, err := generateOpenAICompatibleJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", structuredTestRequest(),
			)
			requirements.Error(err)
			var providerErr *peoplesweep.ProviderError
			requirements.ErrorAs(err, &providerErr)
			checks.Empty(providerErr.RequestID)
			checks.NotContains(err.Error(), requestID)
		})
	}
}

func TestOpenAICompatibleTransportRejectsUnsafeModelVersion(t *testing.T) {
	for _, model := range []string{
		"model with claim text",
		strings.Repeat("m", 129),
	} {
		t.Run(model[:1], func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, `{"model":"`+model+`","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateOpenAICompatibleJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", structuredTestRequest(),
			)
			require.ErrorContains(t, err, "model version")
			assert.NotContains(t, err.Error(), model)
		})
	}
}

func TestOpenAICompatibleTransportRejectsInvalidTokenUsage(t *testing.T) {
	tests := []struct {
		name       string
		usage      string
		wantInput  int64
		wantOutput int64
	}{
		{name: "negative prompt tokens", usage: `"prompt_tokens":-1,"completion_tokens":2`, wantInput: -1, wantOutput: 2},
		{name: "negative completion tokens", usage: `"prompt_tokens":2,"completion_tokens":-1`, wantInput: 2, wantOutput: -1},
		{name: "overflowing prompt tokens", usage: `"prompt_tokens":9223372036854775808,"completion_tokens":2`},
		{name: "overflowing completion tokens", usage: `"prompt_tokens":2,"completion_tokens":9223372036854775808`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			body := `{"model":"gpt-test","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{` +
				test.usage + `},"unsafe":"provider-secret-body"}`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, body)
				assert.NoError(err)
			}))
			defer server.Close()

			response, err := generateOpenAICompatibleJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", structuredTestRequest(),
			)
			require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(err.Error(), "provider-secret-body")
			assert.Equal(test.wantInput, response.Usage.InputTokens)
			assert.Equal(test.wantOutput, response.Usage.OutputTokens)
		})
	}
}

func TestOpenAICompatibleTransportDoesNotFollowRedirects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var redirectedRequests atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		_, err := io.WriteString(w,
			`{"model":"gpt-test","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`)
		assert.NoError(err)
	}))
	defer target.Close()

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/capture")
		w.Header().Set("x-request-id", "redirect-req")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, err := generateOpenAICompatibleJSON(
		t.Context(), providerTestProfile(t, origin.URL+"/v1", false),
		peoplesweep.NewOpenAICompatibleTransport(origin.Client()), "test-key", structuredTestRequest(),
	)
	require.Error(err)
	var providerErr *peoplesweep.ProviderError
	require.ErrorAs(err, &providerErr)
	assert.Equal(http.StatusTemporaryRedirect, providerErr.StatusCode)
	assert.Equal("redirect-req", providerErr.RequestID)
	assert.Zero(redirectedRequests.Load(), "redirect target must receive no provider request")
}

func TestOpenAICompatibleTransportRejectsMalformedResponsesWithoutEchoingThem(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"invalid envelope", `{provider-secret-body`},
		{"missing choices", `{"choices":[],"secret":"provider-secret-body"}`},
		{"empty content", `{"choices":[{"message":{"content":""}}],"secret":"provider-secret-body"}`},
		{"invalid content JSON", `{"choices":[{"message":{"content":"provider-secret-body"}}]}`},
		{"trailing content JSON", `{"choices":[{"message":{"content":"{\"ok\":true} provider-secret-body"}}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, test.body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateOpenAICompatibleJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", structuredTestRequest(),
			)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "provider-secret-body")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestOpenAICompatibleTransportBoundsResponseBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, strings.Repeat("x", (1<<20)+1))
		assert.NoError(t, err)
	}))
	defer server.Close()

	_, err := generateOpenAICompatibleJSON(
		t.Context(), providerTestProfile(t, server.URL+"/v1", false),
		peoplesweep.NewOpenAICompatibleTransport(server.Client()), "test-key", structuredTestRequest(),
	)
	require.ErrorContains(t, err, "too large")
	assert.NotContains(t, err.Error(), strings.Repeat("x", 100))
}

func TestOpenAICompatibleTransportHonorsCancellationAndClientTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var requests atomic.Int64
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	profile := providerTestProfile(t, server.URL+"/v1", false)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := generateOpenAICompatibleJSON(
		cancelled, profile, peoplesweep.NewOpenAICompatibleTransport(server.Client()),
		"test-key", structuredTestRequest(),
	)
	require.ErrorIs(err, context.Canceled)
	assert.Zero(requests.Load())

	timeoutClient := server.Client()
	timeoutClient.Timeout = 100 * time.Millisecond
	_, err = generateOpenAICompatibleJSON(
		t.Context(), profile, peoplesweep.NewOpenAICompatibleTransport(timeoutClient),
		"test-key", structuredTestRequest(),
	)
	require.Error(err)
	assert.Truef(
		errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout"),
		"timeout error = %T: %v", err, err)
	assert.Equal(int64(1), requests.Load())
}

func TestOpenAICompatiblePreparedRequestUsesExactHTTPBody(t *testing.T) {
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		received, err = io.ReadAll(r.Body)
		assert.NoError(t, err)
		_, err = io.WriteString(w, `{"model":"gpt-test","choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	transport := peoplesweep.NewOpenAICompatibleTransport(server.Client())
	profile := providerTestProfile(t, server.URL+"/v1", false)
	prepared, err := transport.PrepareJSON(profile, structuredTestRequest())
	require.NoError(t, err)
	want := prepared.WireRequest()

	_, err = transport.GeneratePreparedJSON(t.Context(), profile, "test-key", prepared)
	require.NoError(t, err)
	assert.Equal(t, want, received)
}

func TestOpenAICompatibleTransportRejectsForgedPreparedWire(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	transport := peoplesweep.NewOpenAICompatibleTransport(server.Client())
	profile := providerTestProfile(t, server.URL+"/v1", false)
	forged, err := peoplesweep.NewPreparedStructuredRequest(
		structuredTestRequest(), []byte(`{"different":"wire"}`))
	require.NoError(t, err)

	_, err = transport.GeneratePreparedJSON(t.Context(), profile, "test-key", forged)
	require.ErrorContains(t, err, "does not match deterministic provider encoding")
	assert.Zero(t, requests.Load())
}
