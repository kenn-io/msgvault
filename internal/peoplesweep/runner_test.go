package peoplesweep_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	active            bool
	err               error
	calls             atomic.Int64
	unverified        bool
	verificationErr   error
	verificationCalls atomic.Int64
	order             func(string)
}

// runnerTestCredential is deliberately local to this untagged test file:
// the credentialCanary constant lives in credential_store_test.go, which is
// compiled only on linux and darwin, so sharing it would break the Windows
// test build.
const runnerTestCredential = "runner-test-credential"

type credentialResolverFunc func(
	profileName string,
	profile peoplesweep.ProviderProfile,
) (peoplesweep.Credential, error)

func (f credentialResolverFunc) Resolve(
	profileName string,
	profile peoplesweep.ProviderProfile,
) (peoplesweep.Credential, error) {
	return f(profileName, profile)
}

func lookupCredentialResolver(lookup func(string) (string, bool)) peoplesweep.CredentialResolver {
	return peoplesweep.NewCredentialResolver(nil, lookup)
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

func (c *countingConsent) HasSuccessfulPersonInferenceCheck(
	_ context.Context,
	_ string,
) (bool, error) {
	c.verificationCalls.Add(1)
	if c.order != nil {
		c.order("verification")
	}
	return !c.unverified, c.verificationErr
}

type countingTransport struct {
	response         peoplesweep.DriverResponse
	err              error
	calls            atomic.Int64
	order            func(string)
	mu               sync.Mutex
	request          peoplesweep.StructuredRequest
	profile          peoplesweep.ProviderProfile
	credentialScheme peoplesweep.AuthScheme
	preserveVersions bool
}

type repairTransport struct {
	responses   []peoplesweep.DriverResponse
	requests    []peoplesweep.StructuredRequest
	profiles    []peoplesweep.ProviderProfile
	credentials []peoplesweep.Credential
	calls       int
}

func (t *repairTransport) Prepare(
	profile peoplesweep.ProviderProfile,
	request peoplesweep.StructuredRequest,
) (peoplesweep.PreparedStructuredRequest, error) {
	t.requests = append(t.requests, request)
	t.profiles = append(t.profiles, profile)
	wire, err := json.Marshal(request)
	if err != nil {
		return peoplesweep.PreparedStructuredRequest{}, err
	}
	return peoplesweep.NewPreparedStructuredRequest(request, wire)
}

func (t *repairTransport) GeneratePrepared(
	_ context.Context,
	_ peoplesweep.ProviderProfile,
	credential peoplesweep.Credential,
	_ peoplesweep.PreparedStructuredRequest,
) (peoplesweep.DriverResponse, error) {
	t.credentials = append(t.credentials, credential)
	if t.calls >= len(t.responses) {
		return peoplesweep.DriverResponse{}, errors.New("unexpected third provider call")
	}
	response := t.responses[t.calls]
	t.calls++
	return response, nil
}

type runnerExecutionFixture struct {
	runner    *peoplesweep.Runner
	transport *repairTransport
	request   peoplesweep.StructuredRequest
	primary   peoplesweep.PreparedStructuredRequest
}

func newRunnerExecutionFixture(
	t *testing.T,
	responses ...peoplesweep.DriverResponse,
) runnerExecutionFixture {
	t.Helper()
	transport := &repairTransport{responses: responses}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), &countingConsent{active: true}, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	require.NoError(t, err)
	request := structuredTestRequest()
	primary, err := runner.PrepareStructured(t.Context(), request)
	require.NoError(t, err)
	return runnerExecutionFixture{runner: runner, transport: transport,
		request: request, primary: primary}
}

func invalidRunnerExecution(
	t *testing.T,
) (runnerExecutionFixture, peoplesweep.StructuredExecutionSession,
	*peoplesweep.ValidationFailure, *int) {
	t.Helper()
	fixture := newRunnerExecutionFixture(t,
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":false}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"},
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"},
	)
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(t, err)
	call, err := session.PrimaryCall(fixture.primary)
	require.NoError(t, err)
	started := 0
	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	var failure *peoplesweep.ValidationFailure
	require.ErrorAs(t, err, &failure)
	return fixture, session, failure, &started
}

func TestRunnerExecutionSessionRejectsRepairFirst(t *testing.T) {
	fixture := newRunnerExecutionFixture(t)
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(t, err)

	_, err = session.PrepareRepair(peoplesweep.ValidationFailure{
		Candidate: json.RawMessage(`{"ok":false}`), Errors: []string{"invalid"},
	})
	require.ErrorContains(t, err, "primary")
	assert.Zero(t, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsDoublePrimary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRunnerExecutionFixture(t,
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"})
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(err)
	first, err := session.PrimaryCall(fixture.primary)
	require.NoError(err)

	_, err = session.PrimaryCall(fixture.primary)
	require.ErrorContains(err, "primary")
	started := 0
	_, err = first.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.NoError(err)
	assert.Equal(1, started)
	assert.Equal(1, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsCrossBatchPrimary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRunnerExecutionFixture(t,
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"})
	otherRequest := structuredTestRequest()
	otherRequest.InputText = `{"different":"batch"}`
	other, err := fixture.runner.PrepareStructured(t.Context(), otherRequest)
	require.NoError(err)
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(err)

	_, err = session.PrimaryCall(other)
	require.ErrorContains(err, "bound primary")
	call, err := session.PrimaryCall(fixture.primary)
	require.NoError(err)
	started := 0
	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.NoError(err)
	assert.Equal(1, started)
	assert.Equal(1, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsDoubleRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, session, failure, started := invalidRunnerExecution(t)
	repair, err := session.PrepareRepair(*failure)
	require.NoError(err)
	_, err = session.PrepareRepair(*failure)
	require.ErrorContains(err, "repair")
	call, err := session.RepairCall(repair)
	require.NoError(err)
	_, err = session.RepairCall(repair)
	require.ErrorContains(err, "repair")

	_, err = call.Execute(t.Context(), func(context.Context) error {
		(*started)++
		return nil
	})
	require.NoError(err)
	assert.Equal(2, *started)
	assert.Equal(2, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsForgedRepairPacket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, session, failure, started := invalidRunnerExecution(t)
	forged, err := fixture.runner.PrepareRepair(fixture.request, *failure)
	require.NoError(err)
	bound, err := session.PrepareRepair(*failure)
	require.NoError(err)

	_, err = session.RepairCall(forged)
	require.ErrorContains(err, "bound repair")
	call, err := session.RepairCall(bound)
	require.NoError(err)
	_, err = call.Execute(t.Context(), func(context.Context) error {
		(*started)++
		return nil
	})
	require.NoError(err)
	assert.Equal(2, *started)
	assert.Equal(2, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsMutatedPrimaryFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, session, failure, started := invalidRunnerExecution(t)
	mutated := *failure
	mutated.Candidate = json.RawMessage(`{"ok":"forged"}`)

	_, err := session.PrepareRepair(mutated)
	require.ErrorContains(err, "validation failure")
	repair, err := session.PrepareRepair(*failure)
	require.NoError(err)
	call, err := session.RepairCall(repair)
	require.NoError(err)
	_, err = call.Execute(t.Context(), func(context.Context) error {
		(*started)++
		return nil
	})
	require.NoError(err)
	assert.Equal(2, *started)
	assert.Equal(2, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsInPlaceMutatedPrimaryResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRunnerExecutionFixture(t,
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"})
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(err)
	call, err := session.PrimaryCall(fixture.primary)
	require.NoError(err)
	response, err := call.Execute(t.Context(), func(context.Context) error { return nil })
	require.NoError(err)
	pristine := response
	pristine.Output = slices.Clone(response.Output)

	copy(response.Output, json.RawMessage(`{"ok":null}`))
	_, err = session.SemanticValidationFailure(response)
	require.ErrorContains(err, "does not match")

	failure, err := session.SemanticValidationFailure(pristine)
	require.NoError(err)
	repair, err := session.PrepareRepair(failure)
	require.NoError(err)
	var instruction struct {
		InvalidCandidate string `json:"invalid_candidate"`
	}
	require.NoError(json.Unmarshal([]byte(repair.Request().InputText), &instruction))
	assert.JSONEq(`{"ok":true}`, instruction.InvalidCandidate)
	assert.Equal(1, fixture.transport.calls)
}

func TestRunnerExecutionSessionRejectsInPlaceMutatedFailureSlices(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, session, failure, started := invalidRunnerExecution(t)
	originalCandidate := slices.Clone(failure.Candidate)
	originalMessage := failure.Errors[0]

	failure.Candidate[2] = 'x'
	_, err := session.PrepareRepair(*failure)
	require.ErrorContains(err, "validation failure")
	copy(failure.Candidate, originalCandidate)
	failure.Errors[0] = "forged validation message"
	_, err = session.PrepareRepair(*failure)
	require.ErrorContains(err, "validation failure")
	failure.Errors[0] = originalMessage

	repair, err := session.PrepareRepair(*failure)
	require.NoError(err)
	call, err := session.RepairCall(repair)
	require.NoError(err)
	_, err = call.Execute(t.Context(), func(context.Context) error {
		(*started)++
		return nil
	})
	require.NoError(err)
	assert.Equal(2, *started)
	assert.Equal(2, fixture.transport.calls)
}

func TestRunnerExecutionRepairAccessorsDoNotAliasSessionLineage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture, session, failure, started := invalidRunnerExecution(t)
	repair, err := session.PrepareRepair(*failure)
	require.NoError(err)
	wire := repair.WireRequest()
	originalWire := slices.Clone(wire)
	request := repair.Request()
	originalSchema := slices.Clone(request.JSONSchema)
	originalSources := slices.Clone(request.Sources)

	wire[0] ^= 0xff
	request.JSONSchema[0] ^= 0xff
	request.Sources[0].ObservedOn = "1900-01-01"
	assert.Equal(originalWire, repair.WireRequest())
	assert.Equal(originalSchema, repair.Request().JSONSchema)
	assert.Equal(originalSources, repair.Request().Sources)

	call, err := session.RepairCall(repair)
	require.NoError(err)
	_, err = call.Execute(t.Context(), func(context.Context) error {
		(*started)++
		return nil
	})
	require.NoError(err)
	assert.Equal(2, *started)
	assert.Equal(2, fixture.transport.calls)
}

func TestRunnerExecutionCallReuseFailsBeforeStartedMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRunnerExecutionFixture(t,
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"})
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(err)
	call, err := session.PrimaryCall(fixture.primary)
	require.NoError(err)
	started := 0
	mark := func(context.Context) error {
		started++
		return nil
	}
	_, err = call.Execute(t.Context(), mark)
	require.NoError(err)
	_, err = call.Execute(t.Context(), mark)
	require.ErrorContains(err, "already claimed")
	assert.Equal(1, started)
	assert.Equal(1, fixture.transport.calls)
}

func TestRunnerExecutionSessionClaimsPrimaryAndHandleOnceConcurrently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRunnerExecutionFixture(t,
		peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`),
			ProviderVersion: "provider-v1", ModelVersion: "model-v1"})
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(err)

	const contenders = 16
	type preparedResult struct {
		call peoplesweep.PreparedStructuredCall
		err  error
	}
	preparedResults := make(chan preparedResult, contenders)
	var prepareGroup sync.WaitGroup
	for range contenders {
		prepareGroup.Go(func() {
			call, callErr := session.PrimaryCall(fixture.primary)
			preparedResults <- preparedResult{call: call, err: callErr}
		})
	}
	prepareGroup.Wait()
	close(preparedResults)
	var call peoplesweep.PreparedStructuredCall
	prepareErrors := 0
	for result := range preparedResults {
		if result.err != nil {
			prepareErrors++
			continue
		}
		call = result.call
	}
	require.NotNil(call)
	assert.Equal(contenders-1, prepareErrors)

	var started atomic.Int64
	executionErrors := make(chan error, contenders)
	var executeGroup sync.WaitGroup
	for range contenders {
		executeGroup.Go(func() {
			_, executeErr := call.Execute(t.Context(), func(context.Context) error {
				started.Add(1)
				return nil
			})
			executionErrors <- executeErr
		})
	}
	executeGroup.Wait()
	close(executionErrors)
	successes := 0
	for executeErr := range executionErrors {
		if executeErr == nil {
			successes++
		}
	}
	assert.Equal(1, successes)
	assert.Equal(int64(1), started.Load())
	assert.Equal(1, fixture.transport.calls)
}

func TestRunnerExecutionMarkerFailureTerminatesWithoutProviderIO(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newRunnerExecutionFixture(t)
	session, err := fixture.runner.BeginStructuredExecution(t.Context(), fixture.primary)
	require.NoError(err)
	call, err := session.PrimaryCall(fixture.primary)
	require.NoError(err)
	markErr := errors.New("mark fixture")
	started := 0
	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return markErr
	})
	require.ErrorIs(err, markErr)
	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.ErrorContains(err, "already claimed")
	_, err = session.PrimaryCall(fixture.primary)
	require.ErrorContains(err, "primary")
	assert.Equal(1, started)
	assert.Zero(fixture.transport.calls)
}

func TestRunnerExecutionConsentFailureOccursBeforeStartedMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	consent := &countingConsent{active: true}
	transport := &repairTransport{responses: []peoplesweep.DriverResponse{{
		CandidateJSON:   json.RawMessage(`{"ok":true}`),
		ProviderVersion: "provider-v1", ModelVersion: "model-v1",
	}}}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	require.NoError(err)
	primary, err := runner.PrepareStructured(t.Context(), structuredTestRequest())
	require.NoError(err)
	session, err := runner.BeginStructuredExecution(t.Context(), primary)
	require.NoError(err)
	call, err := session.PrimaryCall(primary)
	require.NoError(err)
	consent.active = false
	started := 0

	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.ErrorIs(err, peoplesweep.ErrPersonSweepConsentRevoked)
	assert.Zero(started)
	assert.Zero(transport.calls)
	_, err = call.Execute(t.Context(), func(context.Context) error {
		started++
		return nil
	})
	require.ErrorContains(err, "already claimed")
	assert.Zero(started)
}

func (t *countingTransport) Prepare(
	profile peoplesweep.ProviderProfile,
	request peoplesweep.StructuredRequest,
) (peoplesweep.PreparedStructuredRequest, error) {
	return peoplesweep.NewPreparedStructuredRequest(request, []byte(`{"prepared":true}`))
}

func (t *countingTransport) GeneratePrepared(
	_ context.Context,
	profile peoplesweep.ProviderProfile,
	credential peoplesweep.Credential,
	prepared peoplesweep.PreparedStructuredRequest,
) (peoplesweep.DriverResponse, error) {
	t.calls.Add(1)
	if t.order != nil {
		t.order("transport")
	}
	t.mu.Lock()
	t.request = prepared.Request()
	t.profile = profile
	t.credentialScheme = credential.Scheme
	t.mu.Unlock()
	response := t.response
	if !t.preserveVersions && response.ProviderVersion == "" {
		response.ProviderVersion = "test-provider-v1"
	}
	if !t.preserveVersions && response.ModelVersion == "" {
		response.ModelVersion = "test-model-v1"
	}
	return response, t.err
}

func testDriverRegistry(driver peoplesweep.StructuredDriver) *peoplesweep.DriverRegistry {
	return peoplesweep.NewTestDriverRegistry(peoplesweep.ProtocolOpenAIChat, driver)
}

func runnerTestConfig() peoplesweep.Config {
	config := validConfig()
	mutateActiveProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.AllowedSources = []peoplesweep.SourceClass{
			peoplesweep.SourceConversationText,
			peoplesweep.SourceMeetingText,
		}
	})
	return config
}

func TestRunnerCallsVerificationConsentCredentialAndTransportInOrder(t *testing.T) {
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
		response: peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`)},
		order:    record,
	}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, testDriverRegistry(transport),
		credentialResolverFunc(func(name string, profile peoplesweep.ProviderProfile) (peoplesweep.Credential, error) {
			record("credential")
			assert.Equal("default", name)
			assert.Equal("TEST_KEY", profile.CredentialRef)
			return peoplesweep.NewCredential(peoplesweep.AuthBearer, runnerTestCredential), nil
		}),
	)
	require.NoError(t, err)

	got, err := runner.RunStructured(t.Context(), structuredTestRequest())
	require.NoError(t, err)
	assert.JSONEq(`{"ok":true}`, string(got.Output))
	assert.Equal([]string{"verification", "consent", "credential", "transport"}, order)
	assert.Equal(int64(1), consent.verificationCalls.Load())
	assert.Equal(int64(1), consent.calls.Load())
	assert.Equal(int64(1), transport.calls.Load())
	assert.Equal(peoplesweep.AuthBearer, transport.credentialScheme)
}

func TestRunnerFailsClosedBeforeCredentialOrTransport(t *testing.T) {
	baseRequest := structuredTestRequest()
	tests := []struct {
		name           string
		config         peoplesweep.Config
		request        peoplesweep.StructuredRequest
		consented      bool
		unverified     bool
		wantVerified   int64
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
			name: "missing verification", config: runnerTestConfig(), request: baseRequest,
			unverified: true, wantVerified: 1, want: "successful check",
		},
		{
			name: "missing consent", config: runnerTestConfig(), request: baseRequest,
			wantVerified: 1, wantConsent: 1, want: "exact consent",
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
			wantConsent: 0, want: "source class",
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
			wantConsent: 0, want: "date range",
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
			wantConsent: 0, want: "date range",
		},
		{
			name: "sensitive", config: runnerTestConfig(), consented: true,
			request: func() peoplesweep.StructuredRequest {
				request := baseRequest
				request.ContainsSensitive = true
				return request
			}(),
			wantConsent: 0, want: "sensitive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			consent := &countingConsent{active: test.consented, unverified: test.unverified}
			transport := &countingTransport{}
			var credentialCalls atomic.Int64
			runner, err := peoplesweep.NewRunner(
				test.config, consent, testDriverRegistry(transport),
				lookupCredentialResolver(func(string) (string, bool) {
					credentialCalls.Add(1)
					return "test-key", true
				}),
			)
			require.NoError(t, err)

			_, err = runner.RunStructured(t.Context(), test.request)
			require.ErrorContains(t, err, test.want)
			assert.Equal(test.wantVerified, consent.verificationCalls.Load())
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
		runnerTestConfig(), consent, testDriverRegistry(transport),
		lookupCredentialResolver(func(name string) (string, bool) {
			credentialCalls.Add(1)
			assert.Equal("TEST_KEY", name)
			return "", false
		}),
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
			transport := &countingTransport{response: peoplesweep.DriverResponse{
				CandidateJSON: json.RawMessage(test.output),
			}}
			runner, err := peoplesweep.NewRunner(
				runnerTestConfig(), consent, testDriverRegistry(transport),
				lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
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

func TestRunnerPreparesOneBoundedSameProfileRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	request := structuredTestRequest()
	transport := &repairTransport{responses: []peoplesweep.DriverResponse{
		{CandidateJSON: json.RawMessage(`{"ok":false}`), ProviderRequestID: "request-primary",
			ProviderVersion: "provider-v1", ModelVersion: "model-v1",
			Usage: peoplesweep.TokenUsage{InputTokens: 17, OutputTokens: 3}, UsageKnown: true},
		{CandidateJSON: json.RawMessage(`{"ok":true}`), ProviderRequestID: "request-repair",
			ProviderVersion: "provider-v1", ModelVersion: "model-v1",
			Usage: peoplesweep.TokenUsage{InputTokens: 23, OutputTokens: 2}, UsageKnown: true},
	}}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), &countingConsent{active: true}, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	require.NoError(err)

	prepared, err := runner.PrepareStructured(t.Context(), request)
	require.NoError(err)
	execution, err := runner.BeginStructuredExecution(t.Context(), prepared)
	require.NoError(err)
	primaryCall, err := execution.PrimaryCall(prepared)
	require.NoError(err)
	primary, err := primaryCall.Execute(t.Context(), func(context.Context) error { return nil })
	var failure *peoplesweep.ValidationFailure
	require.ErrorAs(err, &failure)
	require.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	assert.NotContains(err.Error(), `{"ok":false}`)
	assert.JSONEq(`{"ok":false}`, string(failure.Candidate))
	require.Len(failure.Errors, 1)
	assert.LessOrEqual(len(failure.Errors[0]), 256)
	assert.Equal("request-primary", primary.ProviderRequestID)
	assert.Equal(peoplesweep.TokenUsage{InputTokens: 17, OutputTokens: 3}, primary.Usage)
	assert.True(primary.UsageKnown)

	repair, err := execution.PrepareRepair(*failure)
	require.NoError(err)
	repairRequest := repair.Request()
	assert.Equal(request.Sources, repairRequest.Sources)
	var instruction struct {
		OriginalRequest  peoplesweep.StructuredRequest `json:"original_request"`
		InvalidCandidate string                        `json:"invalid_candidate"`
		ValidationErrors []string                      `json:"validation_errors"`
	}
	require.NoError(json.Unmarshal([]byte(repairRequest.InputText), &instruction))
	assert.Equal(request.InputText, instruction.OriginalRequest.InputText)
	assert.JSONEq(`{"ok":false}`, instruction.InvalidCandidate)
	assert.Equal(failure.Errors, instruction.ValidationErrors)

	_, err = runner.RunPreparedStructured(t.Context(), repair)
	require.ErrorContains(err, "execution session")
	assert.Equal(1, transport.calls)
	repairCall, err := execution.RepairCall(repair)
	require.NoError(err)
	repaired, err := repairCall.Execute(t.Context(), func(context.Context) error { return nil })
	require.NoError(err)
	assert.JSONEq(`{"ok":true}`, string(repaired.Output))
	assert.Equal(2, transport.calls)
	require.NotEmpty(transport.profiles)
	for _, profile := range transport.profiles {
		assert.Equal(transport.profiles[0], profile)
	}

	_, err = runner.PrepareRepair(repairRequest, peoplesweep.ValidationFailure{
		Candidate: json.RawMessage(`{"ok":false}`), Errors: []string{"still invalid"},
	})
	require.ErrorContains(err, "already a repair")
	assert.NotContains(err.Error(), `{"ok":false}`)
}

func TestRunnerRejectsUnsafeRepairFailureBoundsWithoutLeakingContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	transport := &repairTransport{}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), &countingConsent{active: true}, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	require.NoError(err)
	secret := "provider-body-canary"

	tests := []peoplesweep.ValidationFailure{
		{Candidate: json.RawMessage(strings.Repeat(secret, (1<<20)/len(secret)+2)), Errors: []string{"invalid"}},
		{Candidate: json.RawMessage(`{"ok":false}`), Errors: []string{strings.Repeat(secret, 20)}},
		{Candidate: json.RawMessage(`{"ok":false}`), Errors: slices.Repeat([]string{"invalid"}, 33)},
	}
	for _, failure := range tests {
		_, err := runner.PrepareRepair(structuredTestRequest(), failure)
		require.Error(err)
		assert.NotContains(err.Error(), secret)
	}
	assert.Empty(transport.requests)
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
				runnerTestConfig(), consent, testDriverRegistry(transport),
				lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
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
	consent := &countingConsent{unverified: true}
	transport := &countingTransport{response: peoplesweep.DriverResponse{
		CandidateJSON: json.RawMessage(`{"ok":true}`),
	}}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
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
	assert.Zero(consent.verificationCalls.Load())
	assert.Zero(consent.calls.Load())
}

func TestRunnerCheckRejectsSchemaInvalidProviderOutput(t *testing.T) {
	transport := &countingTransport{response: peoplesweep.DriverResponse{
		CandidateJSON: json.RawMessage(`{"ok":false}`),
	}}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), &countingConsent{active: true}, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	require.NoError(t, err)

	_, err = runner.Check(t.Context())
	require.ErrorContains(t, err, "does not match")
}

type blockingTransport struct{}

func (blockingTransport) Prepare(
	_ peoplesweep.ProviderProfile,
	request peoplesweep.StructuredRequest,
) (peoplesweep.PreparedStructuredRequest, error) {
	return peoplesweep.NewPreparedStructuredRequest(request, []byte(`{"prepared":true}`))
}

func (blockingTransport) GeneratePrepared(
	ctx context.Context,
	_ peoplesweep.ProviderProfile,
	_ peoplesweep.Credential,
	_ peoplesweep.PreparedStructuredRequest,
) (peoplesweep.DriverResponse, error) {
	<-ctx.Done()
	return peoplesweep.DriverResponse{}, ctx.Err()
}

func TestStructuredResponseCarriesAuthoritativeVersions(t *testing.T) {
	for _, test := range []struct {
		name     string
		response peoplesweep.DriverResponse
		want     string
	}{
		{"preserves versions", peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`), ProviderVersion: "provider-v1", ModelVersion: "model-v1"}, ""},
		{"missing provider version", peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`), ModelVersion: "model-v1"}, "version metadata"},
		{"missing model version", peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`), ProviderVersion: "provider-v1"}, "version metadata"},
		{"unsafe model version", peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`), ProviderVersion: "provider-v1", ModelVersion: "model\nversion"}, "version metadata"},
		{"provider version with spaces", peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`), ProviderVersion: "provider claim", ModelVersion: "model-v1"}, "version metadata"},
		{"oversized model version", peoplesweep.DriverResponse{CandidateJSON: json.RawMessage(`{"ok":true}`), ProviderVersion: "provider-v1", ModelVersion: strings.Repeat("m", 129)}, "version metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			transport := &countingTransport{response: test.response, preserveVersions: true}
			runner, err := peoplesweep.NewRunner(
				runnerTestConfig(), &countingConsent{active: true}, testDriverRegistry(transport),
				lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
			)
			requirements.NoError(err)

			got, runErr := runner.RunStructured(t.Context(), structuredTestRequest())
			if test.want != "" {
				requirements.ErrorContains(runErr, test.want)
				return
			}
			requirements.NoError(runErr)
			checks.Equal("provider-v1", got.ProviderVersion)
			checks.Equal("model-v1", got.ModelVersion)
		})
	}
}

func TestPreparedStructuredRequestUsesExactSentBytes(t *testing.T) {
	checks := assert.New(t)
	request := structuredTestRequest()
	prepared, err := peoplesweep.NewPreparedStructuredRequest(request, []byte(`{"request":"exact"}`))
	require.NoError(t, err)

	wire := prepared.WireRequest()
	wire[0] = '['
	copyRequest := prepared.Request()
	copyRequest.InputText = "mutated"

	checks.JSONEq(`{"request":"exact"}`, string(prepared.WireRequest()))
	checks.Equal("Synthetic input only.", prepared.Request().InputText)
	checks.NotEmpty(prepared.WireSHA256())
}

func TestRunnerRejectsForgedPreparedRequestWire(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	transport := &countingTransport{response: peoplesweep.DriverResponse{
		CandidateJSON: json.RawMessage(`{"ok":true}`),
	}}
	consent := &countingConsent{active: true}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	requirements.NoError(err)

	forged, err := peoplesweep.NewPreparedStructuredRequest(
		structuredTestRequest(), []byte(`{"different":"wire"}`))
	requirements.NoError(err)

	_, err = runner.RunPreparedStructured(t.Context(), forged)
	requirements.ErrorContains(err, "does not match deterministic provider encoding")
	checks.Zero(consent.calls.Load())
	checks.Zero(transport.calls.Load())
}

func TestRunnerDoesNotTreatCallerProviderCheckAsSynthetic(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	transport := &countingTransport{response: peoplesweep.DriverResponse{
		CandidateJSON: json.RawMessage(`{"ok":true}`),
	}}
	consent := &countingConsent{active: true}
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, testDriverRegistry(transport),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	requirements.NoError(err)

	request := structuredTestRequest()
	request.ProgramID = "provider-check"
	request.Sources[0].Class = peoplesweep.SourceDocumentText
	prepared, err := peoplesweep.NewPreparedStructuredRequest(request, []byte(`{"prepared":true}`))
	requirements.NoError(err)

	_, err = runner.RunPreparedStructured(t.Context(), prepared)
	requirements.ErrorContains(err, "source class")
	checks.Zero(consent.verificationCalls.Load())
	checks.Zero(consent.calls.Load())
	checks.Zero(transport.calls.Load())
}

func TestRunnerAppliesConfiguredRequestTimeout(t *testing.T) {
	config := runnerTestConfig()
	mutateActiveProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.RequestTimeout = 20 * time.Millisecond
	})
	runner, err := peoplesweep.NewRunner(
		config, &countingConsent{active: true}, testDriverRegistry(blockingTransport{}),
		lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
	)
	require.NoError(t, err)

	_, err = runner.RunStructured(t.Context(), structuredTestRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunnerPreservesOnlyCompletedInvalidOutputMetadata(t *testing.T) {
	tests := []struct {
		name         string
		transportErr error
		wantUsage    peoplesweep.TokenUsage
	}{
		{
			name: "completed invalid output",
			transportErr: errors.Join(peoplesweep.ErrInvalidStructuredOutput,
				errors.New("synthetic adapter validation")),
			wantUsage: peoplesweep.TokenUsage{InputTokens: 321, OutputTokens: 45},
		},
		{name: "ordinary transport failure", transportErr: errors.New("synthetic network failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &countingTransport{err: test.transportErr,
				response: peoplesweep.DriverResponse{Usage: peoplesweep.TokenUsage{
					InputTokens: 321, OutputTokens: 45,
				}}}
			runner, err := peoplesweep.NewRunner(
				runnerTestConfig(), &countingConsent{active: true}, testDriverRegistry(transport),
				lookupCredentialResolver(func(string) (string, bool) { return "test-key", true }),
			)
			require.NoError(t, err)

			response, err := runner.RunStructured(t.Context(), structuredTestRequest())
			require.ErrorIs(t, err, test.transportErr)
			assert.Equal(t, test.wantUsage, response.Usage)
		})
	}
}

func TestRunnerReturnsConsentCheckFailureWithoutCredentialLookup(t *testing.T) {
	consentErr := errors.New("consent store unavailable")
	consent := &countingConsent{err: consentErr}
	var credentialCalls atomic.Int64
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), consent, testDriverRegistry(&countingTransport{}),
		lookupCredentialResolver(func(string) (string, bool) {
			credentialCalls.Add(1)
			return "test-key", true
		}),
	)
	require.NoError(t, err)

	_, err = runner.RunStructured(t.Context(), structuredTestRequest())
	require.ErrorIs(t, err, consentErr)
	assert.Zero(t, credentialCalls.Load())
}

func TestRunnerReturnsVerificationCheckFailureBeforeConsentOrCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	verificationErr := errors.New("verification store unavailable")
	authority := &countingConsent{verificationErr: verificationErr}
	var credentialCalls atomic.Int64
	runner, err := peoplesweep.NewRunner(
		runnerTestConfig(), authority, testDriverRegistry(&countingTransport{}),
		lookupCredentialResolver(func(string) (string, bool) {
			credentialCalls.Add(1)
			return "test-key", true
		}),
	)
	require.NoError(err)

	_, err = runner.RunStructured(t.Context(), structuredTestRequest())
	require.ErrorIs(err, verificationErr)
	assert.Equal(int64(1), authority.verificationCalls.Load())
	assert.Zero(authority.calls.Load())
	assert.Zero(credentialCalls.Load())
}
