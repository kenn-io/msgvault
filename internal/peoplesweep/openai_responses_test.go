package peoplesweep_test

import (
	"context"
	"encoding/json"
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

func responsesTestProfile(
	t *testing.T,
	endpoint string,
	mode peoplesweep.OutputMode,
	reasoningEffort string,
) peoplesweep.ProviderProfile {
	t.Helper()
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIResponses, Endpoint: endpoint, Model: "gpt-test",
		Auth: peoplesweep.AuthBearer, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode: mode, ReasoningEffort: reasoningEffort,
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
	})
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}

func generateOpenAIResponses(
	t *testing.T,
	server *httptest.Server,
	profile peoplesweep.ProviderProfile,
) (peoplesweep.DriverResponse, error) {
	t.Helper()
	driver := peoplesweep.NewOpenAIResponsesDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(t, err)
	return driver.GeneratePrepared(
		t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthBearer, "test-key"), prepared,
	)
}

func TestOpenAIResponsesEmitsSavedOutputModesExactly(t *testing.T) {
	const schema = `{"type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}`
	const escapedSchema = `{\"type\":\"object\",\"properties\":{\"ok\":{\"type\":\"boolean\",\"const\":true}},\"required\":[\"ok\"],\"additionalProperties\":false}`
	tests := []struct {
		name, reasoning string
		mode            peoplesweep.OutputMode
		want            string
	}{
		{
			name: "native schema", mode: peoplesweep.OutputModeNativeJSONSchema, reasoning: "high",
			want: `{"model":"gpt-test","input":[{"role":"system","content":"Return one JSON value that strictly matches the supplied JSON Schema."},{"role":"user","content":"Synthetic input only."}],"text":{"format":{"type":"json_schema","name":"person_facts","strict":true,"schema":` + schema + `}},"max_output_tokens":32,"reasoning":{"effort":"high"}}`,
		},
		{
			name: "JSON object", mode: peoplesweep.OutputModeJSONObject,
			want: `{"model":"gpt-test","input":[{"role":"system","content":"Return one JSON object that strictly matches this JSON Schema:\n` + escapedSchema + `"},{"role":"user","content":"Synthetic input only."}],"text":{"format":{"type":"json_object"}},"max_output_tokens":32}`,
		},
		{
			name: "prompt JSON", mode: peoplesweep.OutputModePromptJSON,
			want: `{"model":"gpt-test","input":[{"role":"system","content":"Return only one JSON value that strictly matches this JSON Schema:\n` + escapedSchema + `"},{"role":"user","content":"Synthetic input only."}],"max_output_tokens":32}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newAssert := assert.New
			assert := assert.New(t)
			require := require.New(t)
			var calls atomic.Int64
			var received []byte
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert := newAssert(t)
				calls.Add(1)
				assert.Equal(http.MethodPost, r.Method)
				assert.Equal("/v1/responses", r.URL.Path)
				assert.Equal("application/json", r.Header.Get("Content-Type"))
				assert.Equal("Bearer test-key", r.Header.Get("Authorization"))
				var err error
				received, err = io.ReadAll(r.Body)
				assert.NoError(err)
				_, err = io.WriteString(w, `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`)
				assert.NoError(err)
			}))
			defer server.Close()

			profile := responsesTestProfile(t, server.URL+"/v1", test.mode, test.reasoning)
			driver := peoplesweep.NewOpenAIResponsesDriver(server.Client())
			prepared, err := driver.Prepare(profile, structuredTestRequest())
			require.NoError(err)
			assert.Equal(test.want, string(prepared.WireRequest()))

			_, err = driver.GeneratePrepared(
				t.Context(), profile,
				peoplesweep.NewCredential(peoplesweep.AuthBearer, "test-key"), prepared,
			)
			require.NoError(err)
			assert.Equal(test.want, string(received))
			assert.Equal(int64(1), calls.Load())
		})
	}
}

func TestOpenAIResponsesTraversesOutputItemsAndBlocks(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert := newAssert(t)
		w.Header().Set("X-Request-ID", "req-responses-1")
		_, err := io.WriteString(w, `{
			"id":"resp_123","model":"gpt-test-build",
			"output":[
				{"type":"reasoning","summary":[]},
				{"type":"message","content":[{"type":"refusal","refusal":"not applicable"}]},
				{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}
			],
			"usage":{"input_tokens":17,"output_tokens":5,"total_tokens":22}
		}`)
		assert.NoError(err)
	}))
	defer server.Close()

	response, err := generateOpenAIResponses(t, server,
		responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
	require.NoError(err)
	assert.JSONEq(`{"ok":true}`, string(response.CandidateJSON))
	assert.Equal("req-responses-1", response.ProviderRequestID)
	assert.Equal("openai-responses-v1", response.ProviderVersion)
	assert.Equal("gpt-test-build", response.ModelVersion)
	assert.True(response.UsageKnown)
	assert.Equal(peoplesweep.TokenUsage{InputTokens: 17, OutputTokens: 5}, response.Usage)
}

func TestOpenAIResponsesRejectsMisplacedOrBlankOutputText(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "misplaced alone",
			output: `{"type":"reasoning","content":[{"type":"output_text","text":"{\"ok\":false}"}]}`,
		},
		{
			name: "misplaced alongside valid",
			output: `{"type":"reasoning","content":[{"type":"output_text","text":"{\"ok\":false}"}]},` +
				`{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}`,
		},
		{
			name: "valid before misplaced",
			output: `{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]},` +
				`{"type":"reasoning","content":[{"type":"output_text","text":"{\"ok\":false}"}]}`,
		},
		{
			name:   "empty alone",
			output: `{"type":"message","content":[{"type":"output_text","text":""}]}`,
		},
		{
			name: "empty alongside valid",
			output: `{"type":"message","content":[{"type":"output_text","text":""},` +
				`{"type":"output_text","text":"{\"ok\":true}"}]}`,
		},
		{
			name: "valid before empty",
			output: `{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"},` +
				`{"type":"output_text","text":""}]}`,
		},
		{
			name:   "whitespace alone",
			output: `{"type":"message","content":[{"type":"output_text","text":" \n\t "}]}`,
		},
		{
			name: "whitespace alongside valid",
			output: `{"type":"message","content":[{"type":"output_text","text":" \n\t "},` +
				`{"type":"output_text","text":"{\"ok\":true}"}]}`,
		},
		{
			name: "valid before whitespace",
			output: `{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"},` +
				`{"type":"output_text","text":" \n\t "}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w,
					`{"model":"gpt-test-build","output":[`+test.output+`],"secret":"provider-secret-body"}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateOpenAIResponses(t, server,
				responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-secret-body")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestOpenAIResponsesRequiresExactlyOneNonEmptyOutputText(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing output", `{"model":"gpt-test-build"}`},
		{"refusal only", `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"refusal","refusal":"provider-secret-body"}]}]}`},
		{"conflicting output text", `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]},{"type":"message","content":[{"type":"output_text","text":"{\"ok\":false}"}]}],"secret":"provider-secret-body"}`},
		{"duplicate output text", `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"},{"type":"output_text","text":"{\"ok\":true}"}]}]}`},
		{"invalid output JSON", `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"provider-secret-body"}]}]}`},
		{"trailing output JSON", `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true} provider-secret-body"}]}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, test.body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateOpenAIResponses(t, server,
				responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-secret-body")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestOpenAIResponsesDistinguishesUsagePresence(t *testing.T) {
	tests := []struct {
		name, usage string
		known       bool
		want        peoplesweep.TokenUsage
	}{
		{name: "missing"},
		{name: "reported zero", usage: `,"usage":{}`, known: true},
		{name: "reported values", usage: `,"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}`, known: true, want: peoplesweep.TokenUsage{InputTokens: 11, OutputTokens: 4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]`+test.usage+`}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			response, err := generateOpenAIResponses(t, server,
				responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
			require.NoError(t, err)
			assert.Equal(t, test.known, response.UsageKnown)
			assert.Equal(t, test.want, response.Usage)
		})
	}
}

func TestOpenAIResponsesRejectsInvalidUsageWithoutEchoingBody(t *testing.T) {
	tests := []struct {
		name, usage string
		want        peoplesweep.TokenUsage
	}{
		{name: "negative input", usage: `"input_tokens":-1,"output_tokens":2`, want: peoplesweep.TokenUsage{InputTokens: -1, OutputTokens: 2}},
		{name: "negative output", usage: `"input_tokens":2,"output_tokens":-1`, want: peoplesweep.TokenUsage{InputTokens: 2, OutputTokens: -1}},
		{name: "input overflow", usage: `"input_tokens":9223372036854775808,"output_tokens":2`},
		{name: "output overflow", usage: `"input_tokens":2,"output_tokens":9223372036854775808`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}],"usage":{` + test.usage + `},"secret":"provider-secret-body"}`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			response, err := generateOpenAIResponses(t, server,
				responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.Equal(t, test.want, response.Usage)
			assert.NotContains(t, err.Error(), "provider-secret-body")
		})
	}
}

func TestOpenAIResponsesBoundsSafeMetadata(t *testing.T) {
	t.Run("unsafe request ID is discarded", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Request-ID", strings.Repeat("r", 129))
			_, err := io.WriteString(w, `{"model":"gpt-test-build","output":[{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`)
			assert.NoError(t, err)
		}))
		defer server.Close()

		response, err := generateOpenAIResponses(t, server,
			responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
		require.NoError(t, err)
		assert.Empty(t, response.ProviderRequestID)
	})

	for _, model := range []string{"model with claim text", strings.Repeat("m", 129)} {
		t.Run("unsafe model", func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body, err := json.Marshal(map[string]any{
					"model":  model,
					"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": `{"ok":true}`}}}},
				})
				assert.NoError(t, err)
				_, err = w.Write(body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateOpenAIResponses(t, server,
				responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), model)
		})
	}
}

func TestOpenAIResponsesRejectsMismatchedCredentialBeforeNetwork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, "")
	driver := peoplesweep.NewOpenAIResponsesDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(err)

	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, "typed-secret-canary"), prepared)
	require.ErrorContains(err, "scheme does not match")
	assert.NotContains(err.Error(), "typed-secret-canary")
	assert.Zero(calls.Load())
}

func TestOpenAIResponsesSanitizesHTTPFailuresWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			newAssert := assert.New
			assert := assert.New(t)
			require := require.New(t)
			var calls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert := newAssert(t)
				calls.Add(1)
				w.Header().Set("X-Request-ID", "req-safe")
				w.WriteHeader(status)
				_, err := io.WriteString(w, `{"error":{"message":"provider-secret-body"}}`)
				assert.NoError(err)
			}))
			defer server.Close()

			_, err := generateOpenAIResponses(t, server,
				responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
			require.Error(err)
			var providerErr *peoplesweep.ProviderError
			require.ErrorAs(err, &providerErr)
			assert.Equal(status, providerErr.StatusCode)
			assert.Equal("req-safe", providerErr.RequestID)
			assert.NotContains(err.Error(), "provider-secret-body")
			assert.NotContains(err.Error(), "test-key")
			assert.Equal(int64(1), calls.Load())
		})
	}
}

func TestOpenAIResponsesUsesSharedBodyAndRedirectLimits(t *testing.T) {
	t.Run("oversized success body", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, err := io.WriteString(w, strings.Repeat("x", (1<<20)+1))
			assert.NoError(t, err)
		}))
		defer server.Close()

		_, err := generateOpenAIResponses(t, server,
			responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
		require.ErrorContains(t, err, "too large")
		assert.NotContains(t, err.Error(), strings.Repeat("x", 100))
	})

	t.Run("redirect", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var targetCalls atomic.Int64
		target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			targetCalls.Add(1)
		}))
		defer target.Close()
		origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", target.URL+"/capture")
			w.Header().Set("X-Request-ID", "redirect-req")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		defer origin.Close()

		_, err := generateOpenAIResponses(t, origin,
			responsesTestProfile(t, origin.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, ""))
		require.Error(err)
		var providerErr *peoplesweep.ProviderError
		require.ErrorAs(err, &providerErr)
		assert.Equal(http.StatusTemporaryRedirect, providerErr.StatusCode)
		assert.Equal("redirect-req", providerErr.RequestID)
		assert.Zero(targetCalls.Load())
	})
}

func TestOpenAIResponsesRejectsForgedPreparedWire(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, "")
	driver := peoplesweep.NewOpenAIResponsesDriver(server.Client())
	forged, err := peoplesweep.NewPreparedStructuredRequest(
		structuredTestRequest(), []byte(`{"different":"wire"}`))
	require.NoError(t, err)

	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthBearer, "test-key"), forged)
	require.ErrorContains(t, err, "does not match deterministic provider encoding")
	assert.Zero(t, calls.Load())
}

func TestOpenAIResponsesRejectsUnsupportedReasoningMode(t *testing.T) {
	profile := responsesTestProfile(t, "https://api.example.test/v1", peoplesweep.OutputModeNativeJSONSchema, "")
	profile.ReasoningMode = "enabled"
	_, err := peoplesweep.NewOpenAIResponsesDriver(nil).Prepare(profile, structuredTestRequest())
	require.ErrorContains(t, err, "not supported by openai_responses")
}

func TestOpenAIResponsesPreservesContextCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	profile := responsesTestProfile(t, server.URL+"/v1", peoplesweep.OutputModeNativeJSONSchema, "")
	driver := peoplesweep.NewOpenAIResponsesDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = driver.GeneratePrepared(ctx, profile,
		peoplesweep.NewCredential(peoplesweep.AuthBearer, "test-key"), prepared)
	require.ErrorIs(t, err, context.Canceled)
}
