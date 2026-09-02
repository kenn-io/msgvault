package peoplesweep_test

import (
	"bytes"
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

func googleTestProfile(
	t *testing.T,
	endpoint, model string,
	mode peoplesweep.OutputMode,
) peoplesweep.ProviderProfile {
	t.Helper()
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolGoogleGenerateContent, Endpoint: endpoint, Model: model,
		Auth: peoplesweep.AuthGoogleAPIKey, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
		OutputMode: mode, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Minute,
	})
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}

func generateGoogleContent(
	t *testing.T,
	server *httptest.Server,
	profile peoplesweep.ProviderProfile,
) (peoplesweep.DriverResponse, error) {
	t.Helper()
	driver := peoplesweep.NewGoogleGenerateContentDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(t, err)
	return driver.GeneratePrepared(
		t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthGoogleAPIKey, "test-key"), prepared,
	)
}

func TestGoogleGenerateContentEmitsNativeSchemaRequestExactly(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	const want = `{"contents":[{"role":"user","parts":[{"text":"Synthetic input only."}]}],"systemInstruction":{"parts":[{"text":"Return one JSON value that strictly matches the supplied JSON Schema."}]},"generationConfig":{"maxOutputTokens":32,"responseMimeType":"application/json","responseSchema":{"type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}}}`
	var calls atomic.Int64
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert := newAssert(t)
		calls.Add(1)
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/v1beta/models/gemini%2F2.5%25flash:generateContent", r.RequestURI)
		assert.Empty(r.URL.RawQuery)
		assert.Equal("application/json", r.Header.Get("Content-Type"))
		assert.Equal("test-key", r.Header.Get("X-Goog-Api-Key"))
		assert.Empty(r.Header.Get("Authorization"))
		assert.Empty(r.Header.Get("X-Api-Key"))
		var err error
		received, err = io.ReadAll(r.Body)
		assert.NoError(err)
		_, err = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":"},{"text":"true}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":5},"modelVersion":"gemini-test-001","responseId":"resp-google-1"}`)
		assert.NoError(err)
	}))
	defer server.Close()

	profile := googleTestProfile(t, server.URL+"/v1beta", "gemini/2.5%flash", peoplesweep.OutputModeNativeJSONSchema)
	driver := peoplesweep.NewGoogleGenerateContentDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(err)
	assert.JSONEq(want, string(prepared.WireRequest()))

	response, err := driver.GeneratePrepared(
		t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthGoogleAPIKey, "test-key"), prepared,
	)
	require.NoError(err)
	assert.JSONEq(want, string(received))
	assert.JSONEq(`{"ok":true}`, string(response.CandidateJSON))
	assert.Equal("resp-google-1", response.ProviderRequestID)
	assert.Equal("google-generate-content-v1", response.ProviderVersion)
	assert.Equal("gemini-test-001", response.ModelVersion)
	assert.True(response.UsageKnown)
	assert.Equal(peoplesweep.TokenUsage{InputTokens: 17, OutputTokens: 5}, response.Usage)
	assert.Equal(int64(1), calls.Load())
}

func TestGoogleGenerateContentPromptModeUsesSchemaInstruction(t *testing.T) {
	const want = `{"contents":[{"role":"user","parts":[{"text":"Synthetic input only."}]}],"systemInstruction":{"parts":[{"text":"Return only one JSON value that strictly matches this JSON Schema:\n{\"type\":\"object\",\"properties\":{\"ok\":{\"type\":\"boolean\",\"const\":true}},\"required\":[\"ok\"],\"additionalProperties\":false}"}]},"generationConfig":{"maxOutputTokens":32}}`
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		received, err = io.ReadAll(r.Body)
		assert.NoError(t, err)
		_, err = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	response, err := generateGoogleContent(t, server,
		googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
	require.NoError(t, err)
	assert.JSONEq(t, want, string(received))
	assert.JSONEq(t, `{"ok":true}`, string(response.CandidateJSON))
}

func TestGoogleGenerateContentRejectsUnsupportedModesAndReasoningBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	t.Run("JSON object", func(t *testing.T) {
		profile := googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModeNativeJSONSchema)
		profile.OutputMode = peoplesweep.OutputModeJSONObject
		_, err := peoplesweep.NewGoogleGenerateContentDriver(server.Client()).Prepare(profile, structuredTestRequest())
		require.ErrorContains(t, err, "not supported by google_generate_content")
	})
	for _, field := range []string{"effort", "mode"} {
		t.Run("reasoning "+field, func(t *testing.T) {
			profile := googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON)
			if field == "effort" {
				profile.ReasoningEffort = "high"
			} else {
				profile.ReasoningMode = "enabled"
			}
			_, err := peoplesweep.NewGoogleGenerateContentDriver(server.Client()).Prepare(profile, structuredTestRequest())
			require.ErrorContains(t, err, "reasoning settings are not supported by google_generate_content")
		})
	}
	assert.Zero(t, calls.Load())
}

func TestGoogleGenerateContentRequiresGoogleAPIKeyBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON)
	profile.Auth = peoplesweep.AuthBearer
	_, err := peoplesweep.NewGoogleGenerateContentDriver(server.Client()).Prepare(profile, structuredTestRequest())
	require.ErrorContains(t, err, "google_api_key")
	assert.Zero(t, calls.Load())
}

func TestGoogleGenerateContentRejectsUnsafeEndpointJoinBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	for _, test := range []struct {
		name, endpoint, model string
	}{
		{name: "encoded endpoint path", endpoint: server.URL + "/v1beta/%2e%2e", model: "gemini-test"},
		{name: "endpoint traversal", endpoint: server.URL + "/v1beta/../escape", model: "gemini-test"},
		{name: "model traversal", endpoint: server.URL + "/v1beta", model: ".."},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := googleTestProfile(t, test.endpoint, test.model, peoplesweep.OutputModePromptJSON)
			_, err := peoplesweep.NewGoogleGenerateContentDriver(server.Client()).Prepare(profile, structuredTestRequest())
			require.ErrorContains(t, err, "unsafe")
		})
	}

	for _, endpoint := range []string{
		server.URL + "/v1beta?key=credential-secret-canary",
		server.URL + "/v1beta#credential-secret-canary",
		strings.Replace(server.URL, "https://", "https://user:credential-secret-canary@", 1),
	} {
		provider := peoplesweep.ProviderConfig{
			Protocol: peoplesweep.ProtocolGoogleGenerateContent, Endpoint: endpoint, Model: "gemini-test",
			Auth: peoplesweep.AuthGoogleAPIKey, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode:       peoplesweep.OutputModePromptJSON,
			RetentionPosture: "zero_retention", TrainingPosture: "no_training",
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
		}
		_, err := configWithProvider(provider).Profile()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "credential-secret-canary")
	}
	assert.Zero(t, calls.Load())
}

func TestGoogleGenerateContentRejectsUnicodeControlsBeforeIO(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	for _, test := range []struct {
		name     string
		endpoint string
		model    string
		canary   string
	}{
		{name: "endpoint C0", endpoint: server.URL + "/v1beta/\x00", model: "gemini-test"},
		{name: "endpoint DEL", endpoint: server.URL + "/v1beta/\x7f", model: "gemini-test"},
		{name: "endpoint C1 U+0085", endpoint: server.URL + "/v1beta/path-secret-canary\u0085", model: "gemini-test", canary: "path-secret-canary"},
		{name: "endpoint C1 U+009F", endpoint: server.URL + "/v1beta/path-secret-canary\u009f", model: "gemini-test", canary: "path-secret-canary"},
		{name: "model C0", endpoint: server.URL + "/v1beta", model: "model-secret-canary\x00", canary: "model-secret-canary"},
		{name: "model DEL", endpoint: server.URL + "/v1beta", model: "model-secret-canary\x7f", canary: "model-secret-canary"},
		{name: "model C1 U+0085", endpoint: server.URL + "/v1beta", model: "model-secret\u0085canary", canary: "model-secret"},
		{name: "model C1 U+009F", endpoint: server.URL + "/v1beta", model: "model-secret-canary\u009f", canary: "model-secret-canary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := peoplesweep.ProviderConfig{
				Protocol: peoplesweep.ProtocolGoogleGenerateContent, Endpoint: test.endpoint, Model: test.model,
				Auth: peoplesweep.AuthGoogleAPIKey, Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
				OutputMode:       peoplesweep.OutputModePromptJSON,
				RetentionPosture: "zero_retention", TrainingPosture: "no_training",
				AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2025-01-01",
			}
			profile, err := configWithProvider(provider).Profile()
			if err == nil {
				_, err = peoplesweep.NewGoogleGenerateContentDriver(server.Client()).Prepare(profile, structuredTestRequest())
			}
			require.Error(t, err)
			if test.canary != "" {
				assert.NotContains(t, err.Error(), test.canary)
			}
		})
	}
	assert.Zero(t, calls.Load())
}

func TestGoogleGenerateContentRejectsBlockedMissingAndAmbiguousCandidates(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{name: "prompt blocked", body: `{"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"HIGH"}]},"modelVersion":"gemini-test-001"}`},
		{name: "blocked candidate", body: `{"candidates":[{"content":{"role":"model","parts":[{"text":"provider-candidate-secret"}]},"finishReason":"SAFETY"}],"modelVersion":"gemini-test-001"}`},
		{name: "safety only", body: `{"promptFeedback":{"safetyRatings":[{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","probability":"LOW"}]},"modelVersion":"gemini-test-001"}`},
		{name: "missing candidates", body: `{"modelVersion":"gemini-test-001"}`},
		{name: "empty candidates", body: `{"candidates":[],"modelVersion":"gemini-test-001"}`},
		{name: "empty candidate", body: `{"candidates":[{}],"modelVersion":"gemini-test-001"}`},
		{name: "missing parts", body: `{"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"}`},
		{name: "empty parts", body: `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"}`},
		{name: "multiple good", body: `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"},{"content":{"role":"model","parts":[{"text":"{\"ok\":false}"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"}`},
		{name: "good before blocked", body: `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"},{"content":{"role":"model","parts":[{"text":"provider-candidate-secret"}]},"finishReason":"SAFETY"}],"modelVersion":"gemini-test-001"}`},
		{name: "blocked before good", body: `{"candidates":[{"content":{"role":"model","parts":[{"text":"provider-candidate-secret"}]},"finishReason":"SAFETY"},{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, test.body)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateGoogleContent(t, server,
				googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-candidate-secret")
			assert.NotContains(t, err.Error(), "test-key")
		})
	}
}

func TestGoogleGenerateContentRejectsMalformedAndMixedPartsInEitherOrder(t *testing.T) {
	tests := []struct {
		name, parts string
	}{
		{name: "empty text", parts: `{"text":""}`},
		{name: "nontext before text", parts: `{"inlineData":{"mimeType":"text/plain","data":"provider-candidate-secret"}},{"text":"{\"ok\":true}"}`},
		{name: "text before nontext", parts: `{"text":"{\"ok\":true}"},{"functionCall":{"name":"provider-candidate-secret"}}`},
		{name: "mixed before text", parts: `{"text":"{\"ok\":" ,"inlineData":{"data":"provider-candidate-secret"}},{"text":"true}"}`},
		{name: "text before mixed", parts: `{"text":"{\"ok\":"},{"text":"true}","thought":true}`},
		{name: "scalar part before text", parts: `false,{"text":"{\"ok\":true}"}`},
		{name: "text before scalar part", parts: `{"text":"{\"ok\":true}"},false`},
		{name: "invalid JSON", parts: `{"text":"provider-candidate-secret"}`},
		{name: "trailing JSON", parts: `{"text":"{\"ok\":true} provider-candidate-secret"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newAssert := assert.New
			assert := assert.New(t)
			require := require.New(t)
			body := `{"candidates":[{"content":{"role":"model","parts":[` + test.parts + `]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001","secret":"provider-body-secret"}`
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert := newAssert(t)
				_, err := io.WriteString(w, body)
				assert.NoError(err)
			}))
			defer server.Close()

			_, err := generateGoogleContent(t, server,
				googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
			require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(err.Error(), "provider-candidate-secret")
			assert.NotContains(err.Error(), "provider-body-secret")
			assert.NotContains(err.Error(), "test-key")
		})
	}
}

func TestGoogleGenerateContentDistinguishesCompleteUsagePresence(t *testing.T) {
	tests := []struct {
		name, usage string
		known       bool
		want        peoplesweep.TokenUsage
	}{
		{name: "missing"},
		{name: "empty metadata", usage: `,"usageMetadata":{}`},
		{name: "reported zero", usage: `,"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}`, known: true},
		{name: "reported values", usage: `,"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":4}`, known: true, want: peoplesweep.TokenUsage{InputTokens: 11, OutputTokens: 4}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"`+test.usage+`}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			response, err := generateGoogleContent(t, server,
				googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
			require.NoError(t, err)
			assert.Equal(t, test.known, response.UsageKnown)
			assert.Equal(t, test.want, response.Usage)
		})
	}
}

func TestGoogleGenerateContentRejectsPartialNegativeAndOverflowUsage(t *testing.T) {
	for _, test := range []struct {
		name, usage string
	}{
		{name: "prompt missing", usage: `"candidatesTokenCount":1`},
		{name: "candidate missing", usage: `"promptTokenCount":1`},
		{name: "prompt null", usage: `"promptTokenCount":null`},
		{name: "candidate null", usage: `"candidatesTokenCount":null`},
		{name: "prompt null with candidate", usage: `"promptTokenCount":null,"candidatesTokenCount":2`},
		{name: "candidate null with prompt", usage: `"promptTokenCount":2,"candidatesTokenCount":null`},
		{name: "both null", usage: `"promptTokenCount":null,"candidatesTokenCount":null`},
		{name: "prompt negative", usage: `"promptTokenCount":-1,"candidatesTokenCount":2`},
		{name: "candidate negative", usage: `"promptTokenCount":2,"candidatesTokenCount":-1`},
		{name: "prompt overflow", usage: `"promptTokenCount":9223372036854775808,"candidatesTokenCount":2`},
		{name: "candidate overflow", usage: `"promptTokenCount":2,"candidatesTokenCount":9223372036854775808`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001","usageMetadata":{`+test.usage+`},"secret":"provider-body-secret"}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			_, err := generateGoogleContent(t, server,
				googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
			require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(t, err.Error(), "provider-body-secret")
		})
	}
}

func TestGoogleGenerateContentRejectsMalformedEnvelopeAndUnsafeMetadata(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	for _, body := range []string{
		`{provider-body-secret`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"ok\":true}"}]},"finishReason":"STOP"}],"modelVersion":"gemini-test-001"} provider-body-secret`,
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			assert := newAssert(t)
			_, err := io.WriteString(w, body)
			assert.NoError(err)
		}))
		_, err := generateGoogleContent(t, server,
			googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
		server.Close()
		require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
		assert.NotContains(err.Error(), "provider-body-secret")
	}

	for _, field := range []string{"modelVersion", "responseId"} {
		body := map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": `{"ok":true}`}}},
				"finishReason": "STOP",
			}},
			"modelVersion": "gemini-test-001", "responseId": "response-safe",
		}
		body[field] = "unsafe metadata with provider-body-secret"
		encoded, err := json.Marshal(body)
		require.NoError(err)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			assert := newAssert(t)
			_, writeErr := w.Write(encoded)
			assert.NoError(writeErr)
		}))
		response, callErr := generateGoogleContent(t, server,
			googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
		server.Close()
		if field == "modelVersion" {
			require.ErrorIs(callErr, peoplesweep.ErrInvalidStructuredOutput)
			assert.NotContains(callErr.Error(), "provider-body-secret")
		} else {
			require.NoError(callErr)
			assert.Empty(response.ProviderRequestID)
		}
	}
}

func TestGoogleGenerateContentSanitizesHTTPFailureAndMakesOneAttempt(t *testing.T) {
	newAssert := assert.New
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert := newAssert(t)
		calls.Add(1)
		w.Header().Set("Request-Id", "req-safe")
		w.WriteHeader(http.StatusInternalServerError)
		_, err := io.WriteString(w, `{"error":{"message":"provider-body-secret"}}`)
		assert.NoError(err)
	}))
	defer server.Close()

	_, err := generateGoogleContent(t, server,
		googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
	require.Error(err)
	var providerErr *peoplesweep.ProviderError
	require.ErrorAs(err, &providerErr)
	assert.Equal(http.StatusInternalServerError, providerErr.StatusCode)
	assert.Equal("req-safe", providerErr.RequestID)
	assert.NotContains(err.Error(), "provider-body-secret")
	assert.NotContains(err.Error(), "test-key")
	assert.Equal(int64(1), calls.Load())
}

func TestGoogleGenerateContentRejectsOversizeResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.Copy(w, bytes.NewReader(bytes.Repeat([]byte("x"), (1<<20)+1)))
		assert.NoError(t, err)
	}))
	defer server.Close()

	_, err := generateGoogleContent(t, server,
		googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON))
	require.ErrorIs(t, err, peoplesweep.ErrInvalidStructuredOutput)
	require.ErrorContains(t, err, "too large")
}

func TestGoogleGenerateContentRejectsMismatchedCredentialAndForgedWireBeforeIO(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	profile := googleTestProfile(t, server.URL, "gemini-test", peoplesweep.OutputModePromptJSON)
	driver := peoplesweep.NewGoogleGenerateContentDriver(server.Client())
	prepared, err := driver.Prepare(profile, structuredTestRequest())
	require.NoError(err)

	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthBearer, "credential-secret-canary"), prepared)
	require.ErrorContains(err, "scheme does not match")
	assert.NotContains(err.Error(), "credential-secret-canary")
	assert.Zero(calls.Load())

	forged, err := peoplesweep.NewPreparedStructuredRequest(
		structuredTestRequest(), []byte(`{"different":"wire"}`))
	require.NoError(err)
	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthGoogleAPIKey, "test-key"), forged)
	require.ErrorContains(err, "does not match deterministic provider encoding")
	assert.Zero(calls.Load())
}
