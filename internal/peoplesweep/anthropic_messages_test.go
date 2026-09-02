package peoplesweep_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func anthropicTestProfile(
	t *testing.T,
	endpoint, model string,
	mode peoplesweep.OutputMode,
) peoplesweep.ProviderProfile {
	t.Helper()
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolAnthropicMessages, Endpoint: endpoint, Model: model,
		Auth: peoplesweep.AuthXAPIKey, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode: mode, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
	})
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}

func generateAnthropicMessages(
	t *testing.T,
	server *httptest.Server,
	profile peoplesweep.ProviderProfile,
) (peoplesweep.DriverResponse, error) {
	t.Helper()
	driver := peoplesweep.NewAnthropicMessagesDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(t, err)
	return driver.GeneratePrepared(
		t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, "test-key"), prepared,
	)
}

func TestAnthropicMessagesEmitsForcedToolRequestExactly(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	const want = `{"model":"claude-test","system":"Return one JSON value that strictly matches the supplied JSON Schema.","messages":[{"role":"user","content":"Synthetic input only."}],"max_tokens":32,"tools":[{"name":"person_facts","input_schema":{"type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}}],"tool_choice":{"type":"tool","name":"person_facts"}}`
	var calls atomic.Int64
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert := newAssert(t)
		calls.Add(1)
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/messages", r.URL.Path)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		assert.Equal("test-key", r.Header.Get("X-Api-Key"))
		assert.Equal("2023-06-01", r.Header.Get("Anthropic-Version"))
		assert.Empty(r.Header.Get("Authorization"))
		var err error
		received, err = io.ReadAll(r.Body)
		assert.NoError(err)
		w.Header().Set("Request-Id", "req-anthropic-1")
		_, err = io.WriteString(w, `{"id":"msg_123","type":"message","role":"assistant","model":"claude-test-build","content":[{"type":"tool_use","id":"toolu_123","name":"person_facts","input":{"ok":true}}],"stop_reason":"tool_use","usage":{"input_tokens":17,"output_tokens":5}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	profile := anthropicTestProfile(t, server.URL+"/api", "claude-test", peoplesweep.OutputModeNativeJSONSchema)
	driver := peoplesweep.NewAnthropicMessagesDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(err)
	assert.JSONEq(want, string(prepared.WireRequest()))

	response, err := driver.GeneratePrepared(
		t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, "test-key"), prepared,
	)
	require.NoError(err)
	assert.JSONEq(want, string(received))
	assert.JSONEq(`{"ok":true}`, string(response.CandidateJSON))
	assert.Equal("req-anthropic-1", response.ProviderRequestID)
	assert.Equal("anthropic-messages-v1", response.ProviderVersion)
	assert.Equal("claude-test-build", response.ModelVersion)
	assert.True(response.UsageKnown)
	assert.Equal(peoplesweep.TokenUsage{InputTokens: 17, OutputTokens: 5}, response.Usage)
	assert.Equal(int64(1), calls.Load())
}

func TestAnthropicMessagesPromptModeUsesOneCompleteJSONText(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	const want = `{"model":"claude-test","system":"Return only one JSON value that strictly matches this JSON Schema:\n{\"type\":\"object\",\"properties\":{\"ok\":{\"type\":\"boolean\",\"const\":true}},\"required\":[\"ok\"],\"additionalProperties\":false}","messages":[{"role":"user","content":"Synthetic input only."}],"max_tokens":32}`
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert := newAssert(t)
		var err error
		received, err = io.ReadAll(r.Body)
		assert.NoError(err)
		_, err = io.WriteString(w, `{"id":"msg_123","type":"message","role":"assistant","model":"claude-test-build","content":[{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn","usage":{}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	profile := anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON)
	response, err := generateAnthropicMessages(t, server, profile)
	require.NoError(err)
	assert.JSONEq(want, string(received))
	assert.JSONEq(`{"ok":true}`, string(response.CandidateJSON))
	assert.True(response.UsageKnown)
	assert.Equal(peoplesweep.TokenUsage{}, response.Usage)
}

func TestKimiAnthropicFixtureUsesGenericMessagesProtocol(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	want, err := os.ReadFile("testdata/providers/kimi-k3-anthropic.json")
	require.NoError(err)
	want = bytes.TrimSpace(want)
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert := newAssert(t)
		var readErr error
		received, readErr = io.ReadAll(r.Body)
		assert.NoError(readErr)
		_, readErr = io.WriteString(w, `{"id":"msg_compatible","type":"message","role":"assistant","model":"compatible-build","content":[{"type":"tool_use","id":"toolu_compatible","name":"person_facts","input":{"ok":true}}]}`)
		assert.NoError(readErr)
	}))
	defer server.Close()

	profile := anthropicTestProfile(t, server.URL, "kimi-k3", peoplesweep.OutputModeNativeJSONSchema)
	response, err := generateAnthropicMessages(t, server, profile)
	require.NoError(err)
	assert.Equal(string(want), string(received))
	assert.JSONEq(`{"ok":true}`, string(response.CandidateJSON))
}

func TestAnthropicMessagesRejectsJSONObjectModeBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModeNativeJSONSchema)
	profile.OutputMode = peoplesweep.OutputModeJSONObject
	_, err := peoplesweep.NewAnthropicMessagesDriver(server.Client()).Prepare(profile, structuredTestRequest())
	require.ErrorContains(t, err, "not supported by anthropic_messages")
	assert.Zero(t, calls.Load())
}

func TestAnthropicMessagesRequiresXAPIKeyProfileBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON)
	profile.Auth = peoplesweep.AuthBearer
	_, err := peoplesweep.NewAnthropicMessagesDriver(server.Client()).Prepare(profile, structuredTestRequest())
	require.ErrorContains(t, err, "x_api_key")
	assert.Zero(t, calls.Load())
}

func TestAnthropicMessagesRejectsUnsupportedReasoningBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON)
	profile.ReasoningEffort = "high"
	_, err := peoplesweep.NewAnthropicMessagesDriver(server.Client()).Prepare(profile, structuredTestRequest())
	require.ErrorContains(t, err, "reasoning settings are not supported by anthropic_messages")
	assert.Zero(t, calls.Load())
}

func TestAnthropicMessagesRequiresExactlyOneMatchingToolBlock(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "missing", content: ``},
		{name: "text only", content: `{"type":"text","text":"{\"ok\":true}"}`},
		{name: "empty input", content: `{"type":"tool_use","name":"person_facts","input":null}`},
		{name: "missing input", content: `{"type":"tool_use","name":"person_facts"}`},
		{name: "wrong tool", content: `{"type":"tool_use","name":"other_tool","input":{"ok":true}}`},
		{name: "duplicate", content: `{"type":"tool_use","name":"person_facts","input":{"ok":true}},{"type":"tool_use","name":"person_facts","input":{"ok":true}}`},
		{name: "conflicting", content: `{"type":"tool_use","name":"person_facts","input":{"ok":true}},{"type":"tool_use","name":"person_facts","input":{"ok":false}}`},
		{name: "text before valid", content: `{"type":"text","text":"provider-secret-body"},{"type":"tool_use","name":"person_facts","input":{"ok":true}}`},
		{name: "valid before text", content: `{"type":"tool_use","name":"person_facts","input":{"ok":true}},{"type":"text","text":"provider-secret-body"}`},
		{name: "refusal before valid", content: `{"type":"refusal","refusal":"provider-secret-body"},{"type":"tool_use","name":"person_facts","input":{"ok":true}}`},
		{name: "valid before error", content: `{"type":"tool_use","name":"person_facts","input":{"ok":true}},{"type":"tool_error","error":"provider-secret-body"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"id":"msg_123","type":"message","role":"assistant","model":"claude-test-build","content":[` + test.content + `],"secret":"provider-secret-body"}`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateAnthropicMessages(t, server,
				anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModeNativeJSONSchema))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-secret-body")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestAnthropicMessagesPromptRequiresExactlyOneNonemptyCompleteJSONText(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "missing"},
		{name: "empty", content: `{"type":"text","text":""}`},
		{name: "whitespace", content: `{"type":"text","text":" \\n\\t "}`},
		{name: "invalid JSON", content: `{"type":"text","text":"provider-secret-body"}`},
		{name: "trailing JSON", content: `{"type":"text","text":"{\"ok\":true} provider-secret-body"}`},
		{name: "duplicate", content: `{"type":"text","text":"{\"ok\":true}"},{"type":"text","text":"{\"ok\":true}"}`},
		{name: "conflicting", content: `{"type":"text","text":"{\"ok\":true}"},{"type":"text","text":"{\"ok\":false}"}`},
		{name: "tool before valid", content: `{"type":"tool_use","name":"person_facts","input":{"ok":false}},{"type":"text","text":"{\"ok\":true}"}`},
		{name: "valid before tool", content: `{"type":"text","text":"{\"ok\":true}"},{"type":"tool_use","name":"person_facts","input":{"ok":false}}`},
		{name: "refusal", content: `{"type":"refusal","refusal":"provider-secret-body"}`},
		{name: "error before valid", content: `{"type":"error","error":"provider-secret-body"},{"type":"text","text":"{\"ok\":true}"}`},
		{name: "valid before error", content: `{"type":"text","text":"{\"ok\":true}"},{"type":"error","error":"provider-secret-body"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"id":"msg_123","type":"message","role":"assistant","model":"claude-test-build","content":[` + test.content + `],"secret":"provider-secret-body"}`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateAnthropicMessages(t, server,
				anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-secret-body")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestAnthropicMessagesRejectsMalformedEnvelopeWithoutEchoingBody(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{name: "invalid envelope", body: `{provider-secret-body`},
		{name: "trailing envelope", body: `{"type":"message","role":"assistant","model":"claude-test-build","content":[{"type":"text","text":"{\"ok\":true}"}]} provider-secret-body`},
		{name: "error envelope", body: `{"type":"error","error":{"message":"provider-secret-body"}}`},
		{name: "wrong role", body: `{"type":"message","role":"user","model":"claude-test-build","content":[{"type":"text","text":"{\"ok\":true}"}],"secret":"provider-secret-body"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, test.body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateAnthropicMessages(t, server,
				anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-secret-body")
		})
	}
}

func TestAnthropicMessagesDistinguishesUsagePresence(t *testing.T) {
	tests := []struct {
		name, usage string
		known       bool
		want        peoplesweep.TokenUsage
	}{
		{name: "missing"},
		{name: "reported zero", usage: `,"usage":{}`, known: true},
		{name: "reported values", usage: `,"usage":{"input_tokens":11,"output_tokens":4}`, known: true, want: peoplesweep.TokenUsage{InputTokens: 11, OutputTokens: 4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-test-build","content":[{"type":"text","text":"{\"ok\":true}"}]`+test.usage+`}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			response, err := generateAnthropicMessages(t, server,
				anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
			require.NoError(t, err)
			assert.Equal(t, test.known, response.UsageKnown)
			assert.Equal(t, test.want, response.Usage)
		})
	}
}

func TestAnthropicMessagesRejectsInvalidUsageAndUnsafeMetadata(t *testing.T) {
	t.Run("invalid usage", func(t *testing.T) {
		for _, usage := range []string{
			`"input_tokens":-1,"output_tokens":2`,
			`"input_tokens":2,"output_tokens":-1`,
			`"input_tokens":9223372036854775808,"output_tokens":2`,
			`"input_tokens":2,"output_tokens":9223372036854775808`,
		} {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-test-build","content":[{"type":"text","text":"{\"ok\":true}"}],"usage":{`+usage+`},"secret":"provider-secret-body"}`)
				assert.NoError(t, err)
			}))
			_, err := generateAnthropicMessages(t, server,
				anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
			server.Close()
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-secret-body")
		}
	})

	for _, model := range []string{"model with claim text", strings.Repeat("m", 129)} {
		t.Run("unsafe model", func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"type": "message", "role": "assistant", "model": model,
				"content": []any{map[string]any{"type": "text", "text": `{"ok":true}`}},
			})
			require.NoError(t, err)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, writeErr := w.Write(body)
				assert.NoError(t, writeErr)
			}))
			defer server.Close()

			_, err = generateAnthropicMessages(t, server,
				anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), model)
		})
	}

	t.Run("unsafe request ID is discarded", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Request-Id", strings.Repeat("r", 129))
			_, err := io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-test-build","content":[{"type":"text","text":"{\"ok\":true}"}]}`)
			assert.NoError(t, err)
		}))
		defer server.Close()

		response, err := generateAnthropicMessages(t, server,
			anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
		require.NoError(t, err)
		assert.Empty(t, response.ProviderRequestID)
	})
}

func TestAnthropicMessagesSanitizesHTTPFailureAndMakesOneAttempt(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert := newAssert(t)
		calls.Add(1)
		w.Header().Set("Request-Id", "req-safe")
		w.WriteHeader(http.StatusInternalServerError)
		_, err := io.WriteString(w, `{"error":{"message":"provider-secret-body"}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	_, err := generateAnthropicMessages(t, server,
		anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON))
	require.Error(err)
	var providerErr *peoplesweep.ProviderError
	require.ErrorAs(err, &providerErr)
	assert.Equal(http.StatusInternalServerError, providerErr.StatusCode)
	assert.Equal("req-safe", providerErr.RequestID)
	assert.NotContains(err.Error(), "provider-secret-body")
	assert.NotContains(err.Error(), "test-key")
	assert.Equal(int64(1), calls.Load())
}

func TestAnthropicMessagesRejectsMismatchedCredentialAndForgedWireBeforeIO(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := anthropicTestProfile(t, server.URL, "claude-test", peoplesweep.OutputModePromptJSON)
	driver := peoplesweep.NewAnthropicMessagesDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(err)

	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthBearer, "typed-secret-canary"), prepared)
	require.ErrorContains(err, "scheme does not match")
	assert.NotContains(err.Error(), "typed-secret-canary")
	assert.Zero(calls.Load())

	forged, err := peoplesweep.NewPreparedStructuredRequest(
		structuredTestRequest(), []byte(`{"different":"wire"}`))
	require.NoError(err)
	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, "test-key"), forged)
	require.ErrorContains(err, "does not match deterministic provider encoding")
	assert.Zero(calls.Load())
}
