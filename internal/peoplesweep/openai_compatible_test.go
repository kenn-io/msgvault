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
	profile, err := (peoplesweep.Config{
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
	}).Profile()
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

func TestOpenAICompatibleTransportGeneratesStructuredJSON(t *testing.T) {
	assert := assert.New(t)
	var captured capturedChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/v1/chat/completions", r.URL.Path)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		assert.Equal("Bearer test-key", r.Header.Get("Authorization"))
		assert.NoError(json.NewDecoder(r.Body).Decode(&captured))
		w.Header().Set("x-request-id", "req-1")
		_, err := io.WriteString(w,
			`{"choices":[{"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	request := structuredTestRequest()
	got, err := peoplesweep.NewOpenAICompatibleTransport(server.Client()).GenerateJSON(
		t.Context(), providerTestProfile(t, server.URL+"/v1", false),
		"test-key", request,
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
			`{"choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	_, err := peoplesweep.NewOpenAICompatibleTransport(server.Client()).GenerateJSON(
		t.Context(), providerTestProfile(t, server.URL+"/v1", true),
		"", structuredTestRequest(),
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", "req-secret-safe")
				w.WriteHeader(status)
				_, err := io.WriteString(w, `{"error":{"message":"provider-secret-body"}}`)
				assert.NoError(err)
			}))
			defer server.Close()

			_, err := peoplesweep.NewOpenAICompatibleTransport(server.Client()).GenerateJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				"test-key", structuredTestRequest(),
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

func TestOpenAICompatibleTransportDoesNotFollowRedirects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var redirectedRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		_, err := io.WriteString(w,
			`{"choices":[{"message":{"content":"{\"ok\":true}"}}],"usage":{}}`)
		assert.NoError(err)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/capture")
		w.Header().Set("x-request-id", "redirect-req")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, err := peoplesweep.NewOpenAICompatibleTransport(origin.Client()).GenerateJSON(
		t.Context(), providerTestProfile(t, origin.URL+"/v1", false),
		"test-key", structuredTestRequest(),
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, test.body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := peoplesweep.NewOpenAICompatibleTransport(server.Client()).GenerateJSON(
				t.Context(), providerTestProfile(t, server.URL+"/v1", false),
				"test-key", structuredTestRequest(),
			)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "provider-secret-body")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestOpenAICompatibleTransportBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, strings.Repeat("x", (1<<20)+1))
		assert.NoError(t, err)
	}))
	defer server.Close()

	_, err := peoplesweep.NewOpenAICompatibleTransport(server.Client()).GenerateJSON(
		t.Context(), providerTestProfile(t, server.URL+"/v1", false),
		"test-key", structuredTestRequest(),
	)
	require.ErrorContains(t, err, "too large")
	assert.NotContains(t, err.Error(), strings.Repeat("x", 100))
}

func TestOpenAICompatibleTransportHonorsCancellationAndClientTimeout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var requests atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
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
	_, err := peoplesweep.NewOpenAICompatibleTransport(server.Client()).GenerateJSON(
		cancelled, profile, "test-key", structuredTestRequest(),
	)
	require.ErrorIs(err, context.Canceled)
	assert.Zero(requests.Load())

	timeoutClient := server.Client()
	timeoutClient.Timeout = 100 * time.Millisecond
	_, err = peoplesweep.NewOpenAICompatibleTransport(timeoutClient).GenerateJSON(
		t.Context(), profile, "test-key", structuredTestRequest(),
	)
	require.Error(err)
	assert.Truef(
		errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout"),
		"timeout error = %T: %v", err, err)
	assert.Equal(int64(1), requests.Load())
}
