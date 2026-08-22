package peoplesweep_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

type countingConsent struct {
	active bool
	err    error
	calls  atomic.Int64
	order  func(string)
}

func (c *countingConsent) HasActivePersonInferenceConsent(
	_ context.Context,
	_ string,
) (bool, error) {
	c.calls.Add(1)
	if c.order != nil {
		c.order("consent")
	}
	return c.active, c.err
}

type countingTransport struct {
	response peoplesweep.StructuredResponse
	err      error
	calls    atomic.Int64
	order    func(string)
	mu       sync.Mutex
	request  peoplesweep.StructuredRequest
	profile  peoplesweep.ProviderProfile
	key      string
}

func (t *countingTransport) GenerateJSON(
	_ context.Context,
	profile peoplesweep.ProviderProfile,
	key string,
	request peoplesweep.StructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	t.calls.Add(1)
	if t.order != nil {
		t.order("transport")
	}
	t.mu.Lock()
	t.request = request
	t.profile = profile
	t.key = key
	t.mu.Unlock()
	return t.response, t.err
}

func runnerTestConfig() peoplesweep.Config {
	config := validConfig()
	config.Provider.AllowedSources = []peoplesweep.SourceClass{
		peoplesweep.SourceConversationText,
		peoplesweep.SourceMeetingText,
	}
	return config
}

func TestRunnerCallsConsentCredentialAndTransportInOrder(t *testing.T) {
	assert := assert.New(t)
	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}
	consent := &countingConsent{active: true, order: record}
	transport := &countingTransport{
		response: peoplesweep.StructuredResponse{Output: json.RawMessage(`{"ok":true}`)},
		order:    record,
	}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, transport,
		func(name string) (string, bool) {
			record("credential")
			assert.Equal("TEST_KEY", name)
			return "test-key", true
		},
	)
	require.NoError(t, err)

	got, err := runner.RunStructured(t.Context(), structuredTestRequest())
	require.NoError(t, err)
	assert.JSONEq(`{"ok":true}`, string(got.Output))
	assert.Equal([]string{"consent", "credential", "transport"}, order)
	assert.Equal(int64(1), consent.calls.Load())
	assert.Equal(int64(1), transport.calls.Load())
	assert.Equal("test-key", transport.key)
}

func TestRunnerFailsClosedBeforeCredentialOrTransport(t *testing.T) {
	baseRequest := structuredTestRequest()
	tests := []struct {
		name           string
		config         peoplesweep.Config
		request        peoplesweep.StructuredRequest
		consented      bool
		wantConsent    int64
		wantCredential int64
		want           string
	}{
		{
			name: "invalid request", config: runnerTestConfig(), consented: true,
			request: func() peoplesweep.StructuredRequest {
				request := baseRequest
				request.ProgramID = ""
				return request
			}(),
			want: "program_id",
		},
		{
			name: "disabled", config: func() peoplesweep.Config {
				config := runnerTestConfig()
				config.Enabled = false
				return config
			}(),
			request: baseRequest, consented: true, want: "disabled",
		},
		{
			name: "missing consent", config: runnerTestConfig(), request: baseRequest,
			wantConsent: 1, want: "exact consent",
		},
		{
			name: "source class", config: runnerTestConfig(), consented: true,
			request: func() peoplesweep.StructuredRequest {
				request := baseRequest
				request.Sources = []peoplesweep.SourceDescriptor{{
					Class: peoplesweep.SourceDocumentText, ObservedOn: "2025-08-20",
				}}
				return request
			}(),
			wantConsent: 1, want: "source class",
		},
		{
			name: "before date", config: runnerTestConfig(), consented: true,
			request: func() peoplesweep.StructuredRequest {
				request := baseRequest
				request.Sources = []peoplesweep.SourceDescriptor{{
					Class: peoplesweep.SourceConversationText, ObservedOn: "2024-12-31",
				}}
				return request
			}(),
			wantConsent: 1, want: "date range",
		},
		{
			name: "after date", config: runnerTestConfig(), consented: true,
			request: func() peoplesweep.StructuredRequest {
				request := baseRequest
				request.Sources = []peoplesweep.SourceDescriptor{{
					Class: peoplesweep.SourceConversationText, ObservedOn: "2026-01-01",
				}}
				return request
			}(),
			wantConsent: 1, want: "date range",
		},
		{
			name: "sensitive", config: runnerTestConfig(), consented: true,
			request: func() peoplesweep.StructuredRequest {
				request := baseRequest
				request.ContainsSensitive = true
				return request
			}(),
			wantConsent: 1, want: "sensitive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			consent := &countingConsent{active: test.consented}
			transport := &countingTransport{}
			var credentialCalls atomic.Int64
			runner, err := peoplesweep.NewRunner(
				test.config, consent, transport,
				func(string) (string, bool) {
					credentialCalls.Add(1)
					return "test-key", true
				},
			)
			require.NoError(t, err)

			_, err = runner.RunStructured(t.Context(), test.request)
			require.ErrorContains(t, err, test.want)
			assert.Equal(test.wantConsent, consent.calls.Load())
			assert.Equal(test.wantCredential, credentialCalls.Load())
			assert.Zero(transport.calls.Load())
		})
	}
}

func TestRunnerMissingCredentialDoesNotCallTransport(t *testing.T) {
	assert := assert.New(t)
	consent := &countingConsent{active: true}
	transport := &countingTransport{}
	var credentialCalls atomic.Int64
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, transport,
		func(name string) (string, bool) {
			credentialCalls.Add(1)
			assert.Equal("TEST_KEY", name)
			return "", false
		},
	)
	require.NoError(t, err)

	_, err = runner.RunStructured(t.Context(), structuredTestRequest())
	require.ErrorContains(t, err, "TEST_KEY")
	assert.Equal(int64(1), consent.calls.Load())
	assert.Equal(int64(1), credentialCalls.Load())
	assert.Zero(transport.calls.Load())
}

func TestRunnerValidatesRequestAndOutputSchema(t *testing.T) {
	tests := []struct {
		name     string
		request  func() peoplesweep.StructuredRequest
		output   string
		wantCall bool
		want     string
	}{
		{
			name: "invalid schema", request: func() peoplesweep.StructuredRequest {
				request := structuredTestRequest()
				request.JSONSchema = json.RawMessage(`{"type":`)
				return request
			},
			output: `{"ok":true}`, want: "JSON Schema",
		},
		{
			name: "schema mismatch", request: structuredTestRequest,
			output: `{"ok":false}`, wantCall: true, want: "does not match",
		},
		{
			name: "trailing output", request: structuredTestRequest,
			output:   `{"ok":true} {"secret":"provider-output"}`,
			wantCall: true, want: "invalid structured JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			consent := &countingConsent{active: true}
			transport := &countingTransport{response: peoplesweep.StructuredResponse{
				Output: json.RawMessage(test.output),
			}}
			runner, err := peoplesweep.NewRunner(
				runnerTestConfig(), consent, transport,
				func(string) (string, bool) { return "test-key", true },
			)
			require.NoError(t, err)

			_, err = runner.RunStructured(t.Context(), test.request())
			require.ErrorContains(t, err, test.want)
			if test.wantCall {
				assert.Equal(int64(1), transport.calls.Load())
			} else {
				assert.Zero(consent.calls.Load())
				assert.Zero(transport.calls.Load())
			}
			assert.NotContains(err.Error(), "provider-output")
		})
	}
}

func TestRunnerRejectsStructuredRequestBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*peoplesweep.StructuredRequest)
		want   string
	}{
		{"program id syntax", func(r *peoplesweep.StructuredRequest) { r.ProgramID = "bad id" }, "program_id"},
		{"program version syntax", func(r *peoplesweep.StructuredRequest) { r.ProgramVersion = "v 1" }, "program_version"},
		{"schema name syntax", func(r *peoplesweep.StructuredRequest) { r.SchemaName = "bad.name" }, "schema_name"},
		{"empty input", func(r *peoplesweep.StructuredRequest) { r.InputText = "" }, "input_text"},
		{"large input", func(r *peoplesweep.StructuredRequest) { r.InputText = strings.Repeat("x", (128<<10)+1) }, "input_text"},
		{"large schema", func(r *peoplesweep.StructuredRequest) {
			r.JSONSchema = json.RawMessage(`"` + strings.Repeat("x", 64<<10) + `"`)
		}, "JSON Schema"},
		{"zero output cap", func(r *peoplesweep.StructuredRequest) { r.MaxOutputTokens = 0 }, "max_output_tokens"},
		{"large output cap", func(r *peoplesweep.StructuredRequest) { r.MaxOutputTokens = 32_769 }, "max_output_tokens"},
		{"missing sources", func(r *peoplesweep.StructuredRequest) { r.Sources = nil }, "source"},
		{"invalid source date", func(r *peoplesweep.StructuredRequest) { r.Sources[0].ObservedOn = "2025-02-30" }, "observed_on"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			request := structuredTestRequest()
			test.mutate(&request)
			consent := &countingConsent{active: true}
			transport := &countingTransport{}
			runner, err := peoplesweep.NewRunner(
				runnerTestConfig(), consent, transport,
				func(string) (string, bool) { return "test-key", true },
			)
			require.NoError(t, err)

			_, err = runner.RunStructured(t.Context(), request)
			require.ErrorContains(t, err, test.want)
			assert.Zero(consent.calls.Load())
			assert.Zero(transport.calls.Load())
		})
	}
}

func TestRunnerCheckUsesOnlyFixedSyntheticInput(t *testing.T) {
	assert := assert.New(t)
	consent := &countingConsent{active: true}
	transport := &countingTransport{response: peoplesweep.StructuredResponse{
		Output: json.RawMessage(`{"ok":true}`),
	}}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, transport,
		func(string) (string, bool) { return "test-key", true },
	)
	require.NoError(t, err)

	_, err = runner.Check(t.Context())
	require.NoError(t, err)
	assert.Equal("provider-check", transport.request.ProgramID)
	assert.Equal("1", transport.request.ProgramVersion)
	assert.Empty(transport.request.Sources)
	assert.False(transport.request.ContainsSensitive)
	assert.Equal("Return an object with ok set to true.", transport.request.InputText)
	assert.Equal("provider_check", transport.request.SchemaName)
	assert.JSONEq(`{
		"type":"object",
		"properties":{"ok":{"type":"boolean","const":true}},
		"required":["ok"],
		"additionalProperties":false
	}`, string(transport.request.JSONSchema))
}

func TestRunnerCheckRejectsSchemaInvalidProviderOutput(t *testing.T) {
	transport := &countingTransport{response: peoplesweep.StructuredResponse{
		Output: json.RawMessage(`{"ok":false}`),
	}}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), &countingConsent{active: true}, transport,
		func(string) (string, bool) { return "test-key", true },
	)
	require.NoError(t, err)

	_, err = runner.Check(t.Context())
	assert.ErrorContains(t, err, "does not match")
}

type blockingTransport struct{}

func (blockingTransport) GenerateJSON(
	ctx context.Context,
	_ peoplesweep.ProviderProfile,
	_ string,
	_ peoplesweep.StructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	<-ctx.Done()
	return peoplesweep.StructuredResponse{}, ctx.Err()
}

func TestRunnerAppliesConfiguredRequestTimeout(t *testing.T) {
	config := runnerTestConfig()
	config.Provider.RequestTimeout = 20 * time.Millisecond
	runner, err := peoplesweep.NewRunner(
		config, &countingConsent{active: true}, blockingTransport{},
		func(string) (string, bool) { return "test-key", true },
	)
	require.NoError(t, err)

	_, err = runner.RunStructured(t.Context(), structuredTestRequest())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunnerReturnsConsentCheckFailureWithoutCredentialLookup(t *testing.T) {
	consentErr := errors.New("consent store unavailable")
	consent := &countingConsent{err: consentErr}
	var credentialCalls atomic.Int64
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, &countingTransport{},
		func(string) (string, bool) {
			credentialCalls.Add(1)
			return "test-key", true
		},
	)
	require.NoError(t, err)

	_, err = runner.RunStructured(t.Context(), structuredTestRequest())
	require.ErrorIs(t, err, consentErr)
	assert.Zero(t, credentialCalls.Load())
}
