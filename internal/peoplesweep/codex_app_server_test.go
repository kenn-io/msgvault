package peoplesweep_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

const codexTestDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func codexTestAbsolutePath() string {
	if runtime.GOOS == "windows" {
		return `C:\attested\codex.exe`
	}
	return "/attested/codex"
}

type recordingCodexGate struct {
	attestation   peoplesweep.CodexAttestation
	verifyErr     error
	reverifyErr   error
	verifyCalls   atomic.Int64
	reverifyCalls atomic.Int64
}

func (g *recordingCodexGate) Verify(_ context.Context, executable, boundary string) (peoplesweep.CodexAttestation, error) {
	g.verifyCalls.Add(1)
	if g.verifyErr != nil {
		return peoplesweep.CodexAttestation{}, g.verifyErr
	}
	attestation := g.attestation
	if attestation.ExecutablePath == "" {
		attestation = peoplesweep.CodexAttestation{
			ExecutablePath: codexTestAbsolutePath(), Version: "codex-cli 0.149.0",
			ExecutableSHA256: codexTestDigest, ExecutionBoundary: boundary,
			LaunchArtifact: peoplesweep.CodexLaunchArtifactNativeStandaloneV1,
		}
	}
	_ = executable
	return attestation, nil
}

func (g *recordingCodexGate) ReverifyForLaunch(attestation peoplesweep.CodexAttestation) error {
	g.reverifyCalls.Add(1)
	if g.reverifyErr != nil {
		return g.reverifyErr
	}
	if attestation.ExecutablePath == "" || attestation.ExecutableSHA256 == "" || attestation.Version == "" || attestation.ExecutionBoundary == "" {
		return errors.New("incomplete attestation")
	}
	return nil
}

type codexStartRecord struct {
	executable string
	args       []string
	env        []string
	dir        string
	process    *pipeRPCProcess
}

type recordingCodexStarter struct {
	t                *testing.T
	mu               sync.Mutex
	scripts          []func(*bufio.Reader, io.Writer, io.Writer) error
	records          []codexStartRecord
	starts           atomic.Int64
	proxyStarts      atomic.Int64
	inspect          func(string)
	configureProcess func(*pipeRPCProcess)
}

func (s *recordingCodexStarter) StartWithProxy(
	ctx context.Context, executable peoplesweep.CodexExecutable, args, env []string, dir, socketPath string,
) (peoplesweep.RPCProcess, error) {
	s.proxyStarts.Add(1)
	info, err := os.Lstat(socketPath)
	require.NoError(s.t, err)
	assert.NotZero(s.t, info.Mode()&os.ModeSocket)
	assert.Equal(s.t, filepath.Join(dir, ".proxy.sock"), socketPath)
	return s.Start(ctx, executable, args, env, dir)
}

func (s *recordingCodexStarter) Start(
	_ context.Context,
	executable peoplesweep.CodexExecutable,
	args []string,
	env []string,
	dir string,
) (peoplesweep.RPCProcess, error) {
	s.starts.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.scripts) == 0 {
		return nil, errors.New("unexpected process start")
	}
	if s.inspect != nil {
		s.inspect(dir)
	}
	script := s.scripts[0]
	s.scripts = s.scripts[1:]
	process := newPipeRPCProcess(s.t, script)
	if s.configureProcess != nil {
		s.configureProcess(process)
	}
	s.records = append(s.records, codexStartRecord{
		executable: executable.Path(), args: slices.Clone(args), env: slices.Clone(env), dir: dir, process: process,
	})
	return process, nil
}

func codexTestConfig() peoplesweep.ProviderConfig {
	return peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Model: "gpt-test", ReasoningEffort: "high",
		Auth: peoplesweep.AuthNone, Credential: peoplesweep.CredentialNone,
		OutputMode:       peoplesweep.OutputModeNativeJSONSchema,
		RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2025-01-01", Executable: "codex",
		ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1, RequestTimeout: time.Second,
	}
}

func codexTestProfile(t *testing.T) peoplesweep.ProviderProfile {
	t.Helper()
	config := configWithProvider(codexTestConfig())
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}

func codexTestRequest() peoplesweep.StructuredRequest {
	return peoplesweep.StructuredRequest{
		ProgramID: "person-facts", ProgramVersion: "1",
		Sources:    []peoplesweep.SourceDescriptor{{Class: peoplesweep.SourceConversationText, ObservedOn: "2026-08-22"}},
		InputText:  `{"person_id":7,"evidence":[{"text":"private packet marker"}]}`,
		SchemaName: "claims", JSONSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"claims":{"type":"array"}},
			"required":["claims"],
			"additionalProperties":false
		}`),
		MaxOutputTokens: 128,
	}
}

type codexTranscript struct {
	mu          sync.Mutex
	methods     []string
	frames      [][]byte
	rootEntries []string
}

func (t *codexTranscript) record(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.frames = append(t.frames, append([]byte(nil), line...))
	var envelope struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(line, &envelope)
	t.methods = append(t.methods, envelope.Method)
}

func successfulCodexScript(
	t *testing.T,
	transcript *codexTranscript,
	modelID string,
	efforts []string,
	nextCursor *string,
	finalJSON string,
) func(*bufio.Reader, io.Writer, io.Writer) error {
	t.Helper()
	return func(reader *bufio.Reader, stdout, _ io.Writer) error {
		requestID := int64(0)
		for _, wantMethod := range []string{"initialize", "initialized", "model/list", "thread/start", "turn/start"} {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read codex request frame: %w", err)
			}
			transcript.record(line)
			var envelope struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(line, &envelope); err != nil {
				return err
			}
			if wantMethod != "initialized" {
				requestID++
			}
			if envelope.Method != wantMethod ||
				(wantMethod == "initialized" && envelope.ID != 0) ||
				(wantMethod != "initialized" && envelope.ID != requestID) {
				return errors.New("unexpected prepared request order")
			}
			switch wantMethod {
			case "initialized":
				continue
			case "initialize":
				err = writeRPCFrame(stdout, map[string]any{"id": envelope.ID, "result": map[string]any{}})
			case "model/list":
				supported := make([]map[string]string, len(efforts))
				for index, effort := range efforts {
					supported[index] = map[string]string{"reasoningEffort": effort, "description": "safe"}
				}
				err = writeRPCFrame(stdout, map[string]any{"id": envelope.ID, "result": map[string]any{
					"data": []any{map[string]any{
						"id": modelID, "model": modelID, "displayName": "Test Model",
						"defaultReasoningEffort": "high", "supportedReasoningEfforts": supported,
					}}, "nextCursor": nextCursor,
				}})
			case "thread/start":
				err = writeRPCFrame(stdout, map[string]any{"id": envelope.ID, "result": map[string]any{
					"thread": map[string]any{"id": "thr_test", "ephemeral": true},
				}})
			case "turn/start":
				err = writeRPCFrame(stdout, map[string]any{"id": envelope.ID, "result": map[string]any{
					"turn": map[string]any{"id": "turn_test", "status": "inProgress", "items": []any{}},
				}})
			}
			if err != nil {
				return err
			}
		}
		if err := writeRPCFrame(stdout, map[string]any{"method": "item/completed", "params": map[string]any{
			"threadId": "thr_test", "turnId": "turn_test",
			"item": map[string]any{"type": "agentMessage", "id": "item_final", "phase": "final_answer", "text": finalJSON},
		}}); err != nil {
			return err
		}
		if err := writeCodexUsageEvent(stdout, 21, 4); err != nil {
			return err
		}
		return writeRPCFrame(stdout, map[string]any{"method": "turn/completed", "params": map[string]any{
			"threadId": "thr_test", "turn": map[string]any{"id": "turn_test", "status": "completed", "items": []any{}},
		}})
	}
}

func codexTurnEventScript(
	t *testing.T,
	transcript *codexTranscript,
	beforeTurnResponse func(io.Writer) error,
	afterTurnResponse func(io.Writer) error,
) func(*bufio.Reader, io.Writer, io.Writer) error {
	t.Helper()
	return func(reader *bufio.Reader, stdout, _ io.Writer) error {
		responses := []any{
			map[string]any{},
			map[string]any{"data": []any{map[string]any{
				"id": "gpt-test", "model": "gpt-test", "displayName": "Test Model",
				"defaultReasoningEffort":    "high",
				"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
			}}, "nextCursor": nil},
			map[string]any{"thread": map[string]any{"id": "thr_test", "ephemeral": true}},
		}
		for id, result := range responses {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read codex setup request: %w", err)
			}
			transcript.record(line)
			if err := writeRPCFrame(stdout, map[string]any{"id": id + 1, "result": result}); err != nil {
				return err
			}
			if id == 0 {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					return fmt.Errorf("read codex initialized notification: %w", err)
				}
				transcript.record(line)
				if !bytes.Contains(line, []byte(`"method":"initialized"`)) {
					return errors.New("missing codex initialized notification")
				}
			}
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read codex turn request: %w", err)
		}
		transcript.record(line)
		if beforeTurnResponse != nil {
			if err := beforeTurnResponse(stdout); err != nil {
				return err
			}
		}
		if err := writeRPCFrame(stdout, map[string]any{"id": 4, "result": map[string]any{
			"turn": map[string]any{"id": "turn_test", "status": "inProgress", "items": []any{}},
		}}); err != nil {
			return err
		}
		if afterTurnResponse != nil {
			return afterTurnResponse(stdout)
		}
		return nil
	}
}

func writeCodexUsageEvent(w io.Writer, inputTokens, outputTokens int64) error {
	breakdown := map[string]any{
		"inputTokens": inputTokens, "outputTokens": outputTokens, "cachedInputTokens": 0,
		"reasoningOutputTokens": 0, "totalTokens": inputTokens + outputTokens,
	}
	return writeRPCFrame(w, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{
		"threadId": "thr_test", "turnId": "turn_test",
		"tokenUsage": map[string]any{"total": breakdown, "last": breakdown},
	}})
}

func writeCodexFinalEvent(w io.Writer, finalJSON string) error {
	return writeRPCFrame(w, map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": "thr_test", "turnId": "turn_test",
		"item": map[string]any{"type": "agentMessage", "id": "item_final", "phase": "final_answer", "text": finalJSON},
	}})
}

func writeCodexCompletedEvent(w io.Writer) error {
	return writeRPCFrame(w, map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": "thr_test", "turn": map[string]any{"id": "turn_test", "status": "completed", "items": []any{}},
	}})
}

func newSuccessfulCodexTransport(
	t *testing.T,
	finalJSON string,
) (*peoplesweep.CodexAppServerDriver, *recordingCodexStarter, *recordingCodexGate, *codexTranscript) {
	t.Helper()
	transcript := &codexTranscript{}
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		successfulCodexScript(t, transcript, "gpt-test", []string{"low", "high"}, nil, finalJSON),
	}}
	starter.inspect = func(dir string) {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, entry := range entries {
			transcript.rootEntries = append(transcript.rootEntries, entry.Name())
		}
	}
	gate := &recordingCodexGate{}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, gate)
	require.NoError(t, err)
	return transport, starter, gate, transcript
}

func TestCodexRegistryUsesOnlyExplicitAuthHome(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("codex auth home permission gates require Unix permission bits")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), []byte(`{"synthetic":true}`), 0o600))
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "unrelated.txt"), []byte("SYNTHETIC_UNRELATED"), 0o600))
	ambientHome := t.TempDir()
	requireChecks.NoError(os.WriteFile(filepath.Join(ambientHome, "auth.json"), []byte("SYNTHETIC_AMBIENT"), 0o600))
	t.Setenv("CODEX_HOME", ambientHome)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		successfulCodexScript(t, &codexTranscript{}, "gpt-test", []string{"low", "high"}, nil, `{"claims":[]}`),
	}}
	starter.inspect = func(workRoot string) {
		contents, err := os.ReadFile(filepath.Join(workRoot, ".codex", "auth.json"))
		require.NoError(t, err)
		assert.Equal(t, `{"synthetic":true}`, string(contents))
		assert.NoFileExists(t, filepath.Join(workRoot, "unrelated.txt"))
		assert.NoFileExists(t, filepath.Join(workRoot, ".codex", "unrelated.txt"))
	}
	registry, err := peoplesweep.NewDriverRegistryWithCodexAuthHome(nil, starter, &recordingCodexGate{}, authHome)
	requireChecks.NoError(err)
	driver, err := registry.Driver(peoplesweep.ProtocolCodexAppServer, codexTestConfig())
	requireChecks.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := driver.Prepare(profile, codexTestRequest())
	requireChecks.NoError(err)
	_, err = driver.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	requireChecks.NoError(err)
	requireChecks.Len(starter.records, 1)
	assertChecks.Equal(int64(1), starter.proxyStarts.Load())
	assertChecks.Empty(starter.records[0].env)
	assertChecks.NoDirExists(starter.records[0].dir)
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(`{"synthetic":true}`, string(contents))
}

func syntheticCodexAuth(user, workspace, refresh string) []byte {
	claims := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_user_id":%q,"chatgpt_account_id":%q}}`, user, workspace)
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	return []byte(fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":"synthetic-access","refresh_token":%q,"account_id":%q}}`, idToken, refresh, workspace))
}

func TestCodexInferenceCopiesBackRefreshForSameAccount(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("codex auth home permission gates require Unix permission bits")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	initial := syntheticCodexAuth("user-one", "workspace-one", "old-refresh")
	refreshed := syntheticCodexAuth("user-one", "workspace-one", "new-refresh")
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), initial, 0o600))
	var workRoot string
	base := successfulCodexScript(t, &codexTranscript{}, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, inspect: func(dir string) { workRoot = dir }, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := base(reader, stdout, stderr); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), refreshed, 0o600)
		},
	}}
	driver, err := peoplesweep.NewCodexAppServerDriverWithAuthHome(codexTestConfig(), starter, &recordingCodexGate{}, authHome)
	requireChecks.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := driver.Prepare(profile, codexTestRequest())
	requireChecks.NoError(err)
	response, err := driver.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	requireChecks.NoError(err)
	assertChecks.JSONEq(`{"claims":[]}`, string(response.CandidateJSON))
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(sha256.Sum256(refreshed), sha256.Sum256(contents))
	info, err := os.Lstat(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(os.FileMode(0o600), info.Mode().Perm())
	assertChecks.NoDirExists(workRoot)
}

func TestCodexInferenceRejectsChangedAccountDuringRefresh(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("codex auth home permission gates require Unix permission bits")
	}
	authHome := t.TempDir()
	requireChecks.NoError(os.Chmod(authHome, 0o700))
	initial := syntheticCodexAuth("user-one", "workspace-one", "old-refresh")
	changed := syntheticCodexAuth("user-two", "workspace-one", "new-refresh")
	requireChecks.NoError(os.WriteFile(filepath.Join(authHome, "auth.json"), initial, 0o600))
	var workRoot string
	base := successfulCodexScript(t, &codexTranscript{}, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, inspect: func(dir string) { workRoot = dir }, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := base(reader, stdout, stderr); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), changed, 0o600)
		},
	}}
	driver, err := peoplesweep.NewCodexAppServerDriverWithAuthHome(codexTestConfig(), starter, &recordingCodexGate{}, authHome)
	requireChecks.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := driver.Prepare(profile, codexTestRequest())
	requireChecks.NoError(err)
	response, err := driver.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	requireChecks.ErrorContains(err, "account identity changed")
	assertChecks.Empty(response.CandidateJSON)
	contents, err := os.ReadFile(filepath.Join(authHome, "auth.json"))
	requireChecks.NoError(err)
	assertChecks.Equal(sha256.Sum256(initial), sha256.Sum256(contents))
	assertChecks.NoDirExists(workRoot)
}

func decodeLengthPrefixedComponents(t *testing.T, wire []byte) [][]byte {
	t.Helper()
	var components [][]byte
	for len(wire) > 0 {
		require.GreaterOrEqual(t, len(wire), 8)
		length := binary.BigEndian.Uint64(wire[:8])
		wire = wire[8:]
		require.LessOrEqual(t, length, uint64(len(wire)))
		components = append(components, append([]byte(nil), wire[:length]...))
		wire = wire[length:]
	}
	return components
}

func TestCodexTransportUsesEphemeralSchemaConstrainedTurn(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	checks := assert.New(t)
	must := require.New(t)
	transport, starter, gate, transcript := newSuccessfulCodexTransport(t, `{"claims":[]}`)
	profile := codexTestProfile(t)
	request := codexTestRequest()
	prepared, err := transport.Prepare(profile, request)
	must.NoError(err)
	response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.NoError(err)
	checks.JSONEq(`{"claims":[]}`, string(response.CandidateJSON))
	assertChecks.Equal([]string{"initialize", "initialized", "model/list", "thread/start", "turn/start"}, transcript.methods)
	must.Len(transcript.frames, 5)

	var threadStart struct {
		Params struct {
			Model          string `json:"model"`
			Ephemeral      bool   `json:"ephemeral"`
			CWD            string `json:"cwd"`
			ApprovalPolicy string `json:"approvalPolicy"`
			Sandbox        string `json:"sandbox"`
		} `json:"params"`
	}
	must.NoError(json.Unmarshal(transcript.frames[3], &threadStart))
	var threadFields struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	requireChecks.NoError(json.Unmarshal(transcript.frames[3], &threadFields))
	assertChecks.NotContains(threadFields.Params, "effort")
	assertChecks.NotContains(threadFields.Params, "sandboxPolicy")
	checks.Equal("gpt-test", threadStart.Params.Model)
	checks.True(threadStart.Params.Ephemeral)
	checks.Equal("/work", threadStart.Params.CWD)
	assertChecks.NotContains(threadFields.Params, "runtimeWorkspaceRoots")
	assertChecks.NotContains(threadFields.Params, "selectedCapabilityRoots")
	assertChecks.NotContains(threadFields.Params, "dynamicTools")
	assertChecks.NotContains(threadFields.Params, "environments")
	checks.Equal("never", threadStart.Params.ApprovalPolicy)
	checks.Equal("read-only", threadStart.Params.Sandbox)

	var turnStart struct {
		Params struct {
			ThreadID string `json:"threadId"`
			Input    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
			Model         string          `json:"model"`
			Effort        string          `json:"effort"`
			OutputSchema  json.RawMessage `json:"outputSchema"`
			SandboxPolicy struct {
				Type          string `json:"type"`
				NetworkAccess bool   `json:"networkAccess"`
			} `json:"sandboxPolicy"`
		} `json:"params"`
	}
	must.NoError(json.Unmarshal(transcript.frames[4], &turnStart))
	var turnFields struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	requireChecks.NoError(json.Unmarshal(transcript.frames[4], &turnFields))
	assertChecks.NotContains(turnFields.Params, "sandbox")
	checks.Equal("thr_test", turnStart.Params.ThreadID)
	checks.Equal("gpt-test", turnStart.Params.Model)
	checks.Equal("high", turnStart.Params.Effort)
	assertChecks.Equal("readOnly", turnStart.Params.SandboxPolicy.Type)
	assertChecks.False(turnStart.Params.SandboxPolicy.NetworkAccess)
	must.Len(turnStart.Params.Input, 1)
	checks.Equal("text", turnStart.Params.Input[0].Type)
	assertChecks.NotContains(turnStart.Params.Input[0].Text, "packet.json")
	assertChecks.Contains(turnStart.Params.Input[0].Text, request.InputText)
	checks.JSONEq(string(request.JSONSchema), string(turnStart.Params.OutputSchema))

	must.Len(starter.records, 1)
	record := starter.records[0]
	checks.Equal(codexTestAbsolutePath(), record.executable)
	checks.Equal(int64(1), gate.verifyCalls.Load(), "launch attests immediately before execution")
	checks.Equal(int64(1), gate.reverifyCalls.Load())
	checks.NoDirExists(record.dir, "packet root must be removed after process join")
}

func TestCodexTurnPreservesPacketContainingReservedThreadText(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	transport, _, _, transcript := newSuccessfulCodexTransport(t, `{"claims":[]}`)
	profile := codexTestProfile(t)
	request := codexTestRequest()
	marker := strings.Repeat("t", 128)
	request.InputText += marker
	prepared, err := transport.Prepare(profile, request)
	requireChecks.NoError(err)
	response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	requireChecks.NoError(err)
	assertChecks.JSONEq(`{"claims":[]}`, string(response.CandidateJSON))
	requireChecks.Len(transcript.frames, 5)
	var turn struct {
		Params struct {
			ThreadID string `json:"threadId"`
			Input    []struct {
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	requireChecks.NoError(json.Unmarshal(transcript.frames[4], &turn))
	assertChecks.Equal("thr_test", turn.Params.ThreadID)
	requireChecks.Len(turn.Params.Input, 1)
	assertChecks.Equal("Return only JSON matching the supplied output schema.\n\n"+request.InputText, turn.Params.Input[0].Text)
}

func TestCodexPreparedWireCoversPacketAndEveryOutboundFrame(t *testing.T) {
	assertChecks := assert.New(t)
	checks := assert.New(t)
	must := require.New(t)
	transport, starter, _, transcript := newSuccessfulCodexTransport(t, `{"claims":[]}`)
	profile := codexTestProfile(t)
	request := codexTestRequest()
	prepared, err := transport.Prepare(profile, request)
	must.NoError(err)
	components := decodeLengthPrefixedComponents(t, prepared.WireRequest())
	require.Len(t, components, 6)
	checks.Equal([]byte(request.InputText), components[0])
	for index := 1; index < len(components); index++ {
		checks.True(bytes.HasSuffix(components[index], []byte("\n")), "JSONL frame %d", index)
	}

	_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.NoError(err)
	assertChecks.Equal(components[1:5], transcript.frames[:4])
	var reservedTurn map[string]any
	var launchedTurn map[string]any
	must.NoError(json.Unmarshal(components[5], &reservedTurn))
	must.NoError(json.Unmarshal(transcript.frames[4], &launchedTurn))
	reservedParams, ok := reservedTurn["params"].(map[string]any)
	must.True(ok)
	launchedParams, ok := launchedTurn["params"].(map[string]any)
	must.True(ok)
	reservedID, ok := reservedParams["threadId"].(string)
	must.True(ok)
	checks.Len(reservedID, 128)
	checks.Equal("thr_test", launchedParams["threadId"])
	wantLaunchedTurn := bytes.Replace(components[5], []byte(reservedID), []byte("thr_test"), 1)
	checks.Equal(wantLaunchedTurn, transcript.frames[4],
		"the server thread-ID slot must be the only changed wire bytes")
	reservedParams["threadId"] = launchedParams["threadId"]
	checks.Equal(reservedTurn, launchedTurn, "only the bounded server thread-ID slot may change")
	actualWireBytes := len(prepared.WireRequest()) - len(components[5]) + len(transcript.frames[4])
	checks.GreaterOrEqual(len(prepared.WireRequest()), actualWireBytes,
		"reservation must cover the response-dependent turn frame")
	must.Len(starter.records, 1)
	record := starter.records[0]
	assertChecks.Contains(string(transcript.frames[4]), "private packet marker")
	assertChecks.Empty(transcript.rootEntries)
	checks.NoDirExists(record.dir)

	wireCopy := prepared.WireRequest()
	wireCopy[len(wireCopy)-1] ^= 0xff
	checks.NotEqual(wireCopy, prepared.WireRequest(), "wire accessor must return a copy")

	changed := request
	changed.InputText = strings.Replace(request.InputText, "private", "Private", 1)
	changedPrepared, err := transport.Prepare(profile, changed)
	must.NoError(err)
	assertChecks.NotEqual(prepared.WireSHA256(), changedPrepared.WireSHA256())
	changedComponents := decodeLengthPrefixedComponents(t, changedPrepared.WireRequest())
	assertChecks.NotEqual(components[5], changedComponents[5], "the disclosed turn must change with the packet")
}

func TestCodexTransportRejectsUnsupportedModelAndEffort(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	tests := []struct {
		name    string
		modelID string
		efforts []string
	}{
		{name: "model", modelID: "other-model", efforts: []string{"high"}},
		{name: "effort", modelID: "gpt-test", efforts: []string{"low"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transcript := &codexTranscript{}
			starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
				successfulCodexScript(t, transcript, test.modelID, test.efforts, nil, `{"claims":[]}`),
			}}
			transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
			must.NoError(err)
			profile := codexTestProfile(t)
			prepared, err := transport.Prepare(profile, codexTestRequest())
			must.NoError(err)
			_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
			must.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
			checks.NotContains(transcript.methods, "turn/start")
		})
	}
}

func TestCodexTransportKillsProcessOnTimeout(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	started := make(chan struct{})
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, _, _ io.Writer) error {
			close(started)
			_, err := reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read first codex request frame: %w", err)
			}
			_, err = reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read blocked codex request frame: %w", err)
			}
			return errors.New("unexpected second codex request frame")
		},
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, generateErr := transport.GeneratePrepared(ctx, profile, peoplesweep.Credential{}, prepared)
		done <- generateErr
	}()
	<-started
	cancel()
	err = <-done
	must.ErrorIs(err, context.Canceled)
	must.Len(starter.records, 1)
	process := starter.records[0].process
	checks.Equal(int64(1), process.kills.Load())
	checks.Equal(int64(1), process.waits.Load(), "canceled process must be joined")
	checks.False(process.killedBeforeStdinClose.Load(), "stdin must close before process kill")
	checks.NoDirExists(starter.records[0].dir)
}

func TestCodexCleanupKillsEOFIgnoringProcessAndClosesStreams(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	killRelease := make(chan struct{})
	var releaseOnce sync.Once
	transcript := &codexTranscript{}
	baseScript := successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := baseScript(reader, stdout, stderr); err != nil {
				return err
			}
			<-killRelease
			return errors.New("induced process exit after cleanup kill")
		},
	}}
	processReady := make(chan *pipeRPCProcess, 1)
	starter.configureProcess = func(process *pipeRPCProcess) {
		process.onKill = func() { releaseOnce.Do(func() { close(killRelease) }) }
		processReady <- process
	}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	done := make(chan error, 1)
	go func() {
		_, generateErr := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
		done <- generateErr
	}()
	process := <-processReady
	select {
	case err = <-done:
		must.NoError(err)
	case <-time.After(750 * time.Millisecond):
		checks.Fail("cleanup did not terminate EOF-ignoring process")
		_ = process.Kill()
		<-done
	}
	checks.Equal(int64(1), process.kills.Load())
	checks.Equal(int64(1), process.waits.Load())
	checks.True(process.stdinClosed.Load())
	checks.True(process.stdoutClosed.Load())
	checks.True(process.stderrClosed.Load())
	select {
	case <-process.done:
	default:
		checks.Fail("process server goroutine was not joined")
	}
	must.Len(starter.records, 1)
	checks.NoDirExists(starter.records[0].dir)
}

func TestCodexCleanupPreservesNaturalNonzeroExitWhenKillReportsAlreadyFinished(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const secret = "natural-exit-race-secret"
	naturalExit := make(chan struct{})
	var releaseOnce sync.Once
	transcript := &codexTranscript{}
	baseScript := successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := baseScript(reader, stdout, stderr); err != nil {
				return err
			}
			<-naturalExit
			return errors.New(secret)
		},
	}}
	starter.configureProcess = func(process *pipeRPCProcess) {
		process.killErr = os.ErrProcessDone
		process.onKill = func() { releaseOnce.Do(func() { close(naturalExit) }) }
	}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	startedAt := time.Now()
	_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.Error(err)
	checks.Less(time.Since(startedAt), 500*time.Millisecond)
	checks.NotContains(err.Error(), secret)
	must.Len(starter.records, 1)
	process := starter.records[0].process
	checks.Equal(int64(1), process.kills.Load())
	checks.Equal(int64(1), process.waits.Load())
	checks.True(process.stdinClosed.Load())
	checks.True(process.stdoutClosed.Load())
	checks.True(process.stderrClosed.Load())
	select {
	case <-process.done:
	default:
		checks.Fail("naturally exited process was not joined")
	}
	checks.NoDirExists(starter.records[0].dir)
}

func TestCodexCleanupReturnsBoundedSafeErrorWhenKillFailsAndWaitBlocks(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const secret = "failed-kill-secret"
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	transcript := &codexTranscript{}
	baseScript := successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := baseScript(reader, stdout, stderr); err != nil {
				return err
			}
			<-release
			return nil
		},
	}}
	processReady := make(chan *pipeRPCProcess, 1)
	starter.configureProcess = func(process *pipeRPCProcess) {
		process.killErr = errors.New(secret)
		processReady <- process
	}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	done := make(chan error, 1)
	go func() {
		_, generateErr := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
		done <- generateErr
	}()
	process := <-processReady
	// Cleanup must return while the process is still blocked. The timeout
	// only catches a hang; runner throughput is not part of this contract.
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		must.FailNow("failed process kill left cleanup blocked in Wait")
	}
	must.Error(err)
	checks.NotContains(err.Error(), secret)
	checks.Equal(int64(1), process.kills.Load())
	checks.Equal(int64(1), process.waits.Load())
	checks.True(process.stdinClosed.Load())
	checks.True(process.stdoutClosed.Load())
	checks.True(process.stderrClosed.Load())
	select {
	case <-process.done:
		checks.Fail("failed kill unexpectedly joined a still-running process")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-process.done:
	case <-time.After(30 * time.Second):
		checks.Fail("released process did not finish after bounded cleanup returned")
	}
	checks.NoDirExists(starter.records[0].dir)
}

func TestCodexCleanupCancellationKillsAndJoinsOnce(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	killRelease := make(chan struct{})
	var releaseOnce sync.Once
	transcript := &codexTranscript{}
	baseScript := successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := baseScript(reader, stdout, stderr); err != nil {
				return err
			}
			<-killRelease
			return nil
		},
	}}
	processReady := make(chan *pipeRPCProcess, 1)
	starter.configureProcess = func(process *pipeRPCProcess) {
		process.onKill = func() { releaseOnce.Do(func() { close(killRelease) }) }
		processReady <- process
	}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, generateErr := transport.GeneratePrepared(ctx, profile, peoplesweep.Credential{}, prepared)
		done <- generateErr
	}()
	process := <-processReady
	<-process.waitStarted
	cancel()
	select {
	case err = <-done:
		must.ErrorIs(err, context.Canceled)
	case <-time.After(500 * time.Millisecond):
		checks.Fail("cleanup ignored context cancellation")
		_ = process.Kill()
		<-done
	}
	checks.Equal(int64(1), process.kills.Load())
	checks.Equal(int64(1), process.waits.Load())
	checks.False(process.killedBeforeStdinClose.Load())
	checks.True(process.stdoutClosed.Load())
	checks.True(process.stderrClosed.Load())
	select {
	case <-process.done:
	default:
		checks.Fail("canceled cleanup did not join server goroutine")
	}
}

func TestCodexCleanupClosesStreamsOnNonzeroExit(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const secret = "nonzero-process-secret"
	transcript := &codexTranscript{}
	baseScript := successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := baseScript(reader, stdout, stderr); err != nil {
				return err
			}
			return errors.New(secret)
		},
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.Error(err)
	checks.NotContains(err.Error(), secret)
	must.Len(starter.records, 1)
	process := starter.records[0].process
	checks.Equal(int64(1), process.waits.Load())
	checks.Zero(process.kills.Load())
	checks.True(process.stdinClosed.Load())
	checks.True(process.stdoutClosed.Load())
	checks.True(process.stderrClosed.Load())
	checks.NoDirExists(starter.records[0].dir)
}

func TestCodexTransportRejectsUnboundedModelCatalog(t *testing.T) {
	must := require.New(t)
	cursor := "more-models"
	transcript := &codexTranscript{}
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, &cursor, `{"claims":[]}`),
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	assert.Equal(t, []string{"initialize", "initialized", "model/list"}, transcript.methods)
}

func TestCodexTransportRejectsMalformedOrOversizedThreadIDBeforeTurn(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	tests := []struct {
		name     string
		threadID string
	}{
		{name: "malformed", threadID: "thread\nunsafe"},
		{name: "oversized", threadID: strings.Repeat("x", 129)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transcript := &codexTranscript{}
			starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
				func(reader *bufio.Reader, stdout, _ io.Writer) error {
					for id, result := range []any{
						map[string]any{},
						map[string]any{"data": []any{map[string]any{
							"id": "gpt-test", "model": "gpt-test", "displayName": "Test Model",
							"defaultReasoningEffort":    "high",
							"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}},
						}}, "nextCursor": nil},
						map[string]any{"thread": map[string]any{"id": test.threadID, "ephemeral": true}},
					} {
						line, err := reader.ReadBytes('\n')
						if err != nil {
							return fmt.Errorf("read codex request frame: %w", err)
						}
						transcript.record(line)
						if err := writeRPCFrame(stdout, map[string]any{"id": id + 1, "result": result}); err != nil {
							return err
						}
						if id == 0 {
							line, err := reader.ReadBytes('\n')
							if err != nil {
								return fmt.Errorf("read codex initialized notification: %w", err)
							}
							transcript.record(line)
						}
					}
					_, err := reader.ReadBytes('\n')
					if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
						return nil
					}
					if err != nil {
						return fmt.Errorf("read unexpected codex turn frame: %w", err)
					}
					return errors.New("unexpected codex turn frame")
				},
			}}
			transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
			must.NoError(err)
			profile := codexTestProfile(t)
			prepared, err := transport.Prepare(profile, codexTestRequest())
			must.NoError(err)
			_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
			must.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
			checks.NotContains(err.Error(), test.threadID)
			assert.Equal(t, []string{"initialize", "initialized", "model/list", "thread/start"}, transcript.methods)
		})
	}
}

func TestCodexLaunchReverifiesBeforeStartingProcess(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	gate := &recordingCodexGate{reverifyErr: peoplesweep.ErrCodexIsolationUnreleased}
	starter := &recordingCodexStarter{t: t}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, gate)
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
	checks.Equal(int64(1), gate.verifyCalls.Load())
	checks.Equal(int64(1), gate.reverifyCalls.Load())
	checks.Zero(starter.starts.Load(), "failed reverification must start no process")
}

func TestCodexTransportRejectsInvalidFinalSchema(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	transport, starter, gate, transcript := newSuccessfulCodexTransport(t, `{"not_claims":[]}`)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	checks.Empty(response.CandidateJSON)
	checks.Equal(peoplesweep.TokenUsage{InputTokens: 21, OutputTokens: 4}, response.Usage)
	checks.NotEmpty(response.ProviderVersion)
	checks.Equal("gpt-test", response.ModelVersion)
	checks.Equal(int64(1), starter.starts.Load())
	checks.Equal(int64(1), gate.verifyCalls.Load())
	assert.Equal(t, []string{"initialize", "initialized", "model/list", "thread/start", "turn/start"}, transcript.methods)
}

func TestCodexTransportPreservesUsageWhenCumulativeTotalsAreInvalid(t *testing.T) {
	tests := []struct {
		name     string
		writeBad func(io.Writer) error
	}{
		{name: "missing total", writeBad: func(w io.Writer) error {
			return writeRPCFrame(w, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{
				"threadId": "thr_test", "turnId": "turn_test", "tokenUsage": map[string]any{},
			}})
		}},
		{name: "missing output", writeBad: func(w io.Writer) error {
			return writeRPCFrame(w, map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{
				"threadId": "thr_test", "turnId": "turn_test",
				"tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 22}},
			}})
		}},
		{name: "decreasing", writeBad: func(w io.Writer) error { return writeCodexUsageEvent(w, 20, 3) }},
		{name: "overflow", writeBad: func(w io.Writer) error {
			_, err := io.WriteString(w, `{"method":"thread/tokenUsage/updated","params":{"threadId":"thr_test","turnId":"turn_test","tokenUsage":{"total":{"inputTokens":9223372036854775808,"outputTokens":5}}}}`+"\n")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			transcript := &codexTranscript{}
			starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
				codexTurnEventScript(t, transcript, nil, func(stdout io.Writer) error {
					if err := writeCodexUsageEvent(stdout, 21, 4); err != nil {
						return err
					}
					if err := writeCodexFinalEvent(stdout, `{"not_claims":[]}`); err != nil {
						return err
					}
					if err := test.writeBad(stdout); err != nil {
						return err
					}
					return writeCodexCompletedEvent(stdout)
				}),
			}}
			transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
			must.NoError(err)
			profile := codexTestProfile(t)
			prepared, err := transport.Prepare(profile, codexTestRequest())
			must.NoError(err)
			response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
			must.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
			checks.Empty(response.CandidateJSON)
			checks.Equal(peoplesweep.TokenUsage{InputTokens: 21, OutputTokens: 4}, response.Usage)
			checks.NotEmpty(response.ProviderVersion)
			checks.Equal("gpt-test", response.ModelVersion)
		})
	}
}

func TestCodexTransportConsumesNotificationsQueuedBeforeTurnResponseOnce(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	transcript := &codexTranscript{}
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		codexTurnEventScript(t, transcript, func(stdout io.Writer) error {
			if err := writeCodexUsageEvent(stdout, 21, 4); err != nil {
				return err
			}
			if err := writeCodexFinalEvent(stdout, `{"claims":[]}`); err != nil {
				return err
			}
			if err := writeCodexUsageEvent(stdout, 22, 5); err != nil {
				return err
			}
			return writeCodexCompletedEvent(stdout)
		}, nil),
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.NoError(err)
	checks.JSONEq(`{"claims":[]}`, string(response.CandidateJSON))
	checks.Equal(peoplesweep.TokenUsage{InputTokens: 22, OutputTokens: 5}, response.Usage)
	assert.Equal(t, []string{"initialize", "initialized", "model/list", "thread/start", "turn/start"}, transcript.methods)
}

func TestCodexDriverMarksReportedZeroUsageKnown(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	transcript := &codexTranscript{}
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		codexTurnEventScript(t, transcript, nil, func(stdout io.Writer) error {
			if err := writeCodexUsageEvent(stdout, 0, 0); err != nil {
				return err
			}
			if err := writeCodexFinalEvent(stdout, `{"claims":[]}`); err != nil {
				return err
			}
			return writeCodexCompletedEvent(stdout)
		}),
	}}
	driver, err := peoplesweep.NewCodexAppServerDriver(
		codexTestConfig(), starter, &recordingCodexGate{})
	requireChecks.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := driver.Prepare(profile, codexTestRequest())
	requireChecks.NoError(err)

	response, err := driver.GeneratePrepared(
		t.Context(), profile, peoplesweep.Credential{}, prepared)
	requireChecks.NoError(err)
	assertChecks.True(response.UsageKnown)
	assertChecks.Equal(peoplesweep.TokenUsage{}, response.Usage)
}

func TestCodexDriverRejectsNonEmptyCredentialBeforeAttestation(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	gate := &recordingCodexGate{}
	starter := &recordingCodexStarter{t: t}
	driver, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, gate)
	requireChecks.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := driver.Prepare(profile, codexTestRequest())
	requireChecks.NoError(err)

	_, err = driver.GeneratePrepared(t.Context(), profile,
		peoplesweep.NewCredential(peoplesweep.AuthBearer, "codex-secret-canary"), prepared)
	requireChecks.ErrorContains(err, "does not accept")
	assertChecks.NotContains(err.Error(), "codex-secret-canary")
	assertChecks.Zero(gate.verifyCalls.Load())
	assertChecks.Zero(starter.starts.Load())
}

func TestCodexTransportRejectsLateStderrOverflowAfterFinalFrame(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const secret = "late-stderr-secret"
	transcript := &codexTranscript{}
	baseScript := successfulCodexScript(t, transcript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`)
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, stderr io.Writer) error {
			if err := baseScript(reader, stdout, stderr); err != nil {
				return err
			}
			_, err := io.WriteString(stderr, strings.Repeat(secret, (64<<10)/len(secret)+2))
			return err
		},
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.Error(err)
	checks.NotContains(err.Error(), secret)
	checks.Equal(peoplesweep.TokenUsage{InputTokens: 21, OutputTokens: 4}, response.Usage)
	checks.NotEmpty(response.ProviderVersion)
}

func TestCodexLoginAndModelsApplyConfiguredTimeout(t *testing.T) {
	for _, operation := range []string{"login", "models"} {
		t.Run(operation, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
				func(reader *bufio.Reader, _, _ io.Writer) error {
					if _, err := reader.ReadBytes('\n'); err != nil {
						return fmt.Errorf("read silent Codex request: %w", err)
					}
					_, err := reader.ReadBytes('\n')
					if err != nil {
						return fmt.Errorf("wait for silent Codex request: %w", err)
					}
					return nil
				},
			}}
			config := codexTestConfig()
			config.RequestTimeout = 30 * time.Millisecond
			transport, err := peoplesweep.NewCodexAppServerDriver(config, starter, &recordingCodexGate{})
			must.NoError(err)
			switch operation {
			case "login":
				err = transport.StartDeviceLogin(t.Context(), func(peoplesweep.DeviceLogin) error { return nil })
			case "models":
				_, err = transport.ListModels(t.Context())
			}
			must.ErrorIs(err, context.DeadlineExceeded)
			must.NoError(t.Context().Err(), "the configured provider deadline must expire before the test context")
			must.Len(starter.records, 1)
			process := starter.records[0].process
			checks.Equal(int64(1), process.kills.Load())
			checks.Equal(int64(1), process.waits.Load())
			checks.True(process.stdinClosed.Load())
			checks.True(process.stdoutClosed.Load())
			checks.True(process.stderrClosed.Load())
			checks.NoDirExists(starter.records[0].dir)
		})
	}
}

func TestCodexDeviceLoginUsesDeviceCodeMethod(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	var frames [][]byte
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, _ io.Writer) error {
			for id, wantMethod := range []string{"initialize", "initialized", "account/login/start"} {
				line, err := reader.ReadBytes('\n')
				if err != nil {
					return fmt.Errorf("read codex login frame: %w", err)
				}
				frames = append(frames, append([]byte(nil), line...))
				if id == 0 {
					if err := writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}}); err != nil {
						return err
					}
					continue
				}
				if id == 1 {
					var got struct {
						Method string `json:"method"`
					}
					if err := json.Unmarshal(line, &got); err != nil || got.Method != "initialized" {
						return errors.New("missing codex initialized notification")
					}
					continue
				}
				var got struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if err := json.Unmarshal(line, &got); err != nil {
					return err
				}
				if got.Method != wantMethod {
					return errors.New("unexpected login method")
				}
				if got.Params["type"] != "chatgptDeviceCode" {
					return errors.New("unexpected login type")
				}
				if err := writeRPCFrame(stdout, map[string]any{"id": 2, "result": map[string]any{
					"type": "chatgptDeviceCode", "loginId": "login-safe",
					"verificationUrl": "https://auth.example.test/device", "userCode": "ABCD-1234",
				}}); err != nil {
					return err
				}
				return writeRPCFrame(stdout, map[string]any{"method": "account/login/completed", "params": map[string]any{
					"success": true, "loginId": "login-safe",
				}})
			}
			return nil
		},
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	var login peoplesweep.DeviceLogin
	started := time.Now()
	err = transport.StartDeviceLogin(t.Context(), func(value peoplesweep.DeviceLogin) error {
		login = value
		return nil
	})
	must.NoError(err)
	checks.Equal("https://auth.example.test/device", login.VerificationURL)
	checks.Equal("ABCD-1234", login.UserCode)
	assert.WithinRange(t, login.ExpiresAt, started, started.Add(2*time.Second))
	require.Len(t, frames, 3)
}

func TestCodexModelListReturnsSupportedEfforts(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	transcript := &codexTranscript{}
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		func(reader *bufio.Reader, stdout, _ io.Writer) error {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read codex initialize frame: %w", err)
			}
			transcript.record(line)
			if err := writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}}); err != nil {
				return err
			}
			line, err = reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read codex initialized notification: %w", err)
			}
			transcript.record(line)
			var notification map[string]any
			if err := json.Unmarshal(line, &notification); err != nil {
				return err
			}
			if notification["method"] != "initialized" || len(notification) != 1 {
				return errors.New("initialize was not followed by the initialized notification")
			}
			line, err = reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read codex model-list frame: %w", err)
			}
			transcript.record(line)
			return writeRPCFrame(stdout, map[string]any{"id": 2, "result": map[string]any{
				"data": []any{map[string]any{
					"id": "gpt-test", "model": "gpt-test", "displayName": "Test Model",
					"defaultReasoningEffort": "medium",
					"supportedReasoningEfforts": []any{
						map[string]any{"reasoningEffort": "low", "description": "Fast"},
						map[string]any{"reasoningEffort": "medium", "description": "Balanced"},
					},
				}}, "nextCursor": nil,
			}})
		},
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	models, err := transport.ListModels(t.Context())
	must.NoError(err)
	checks.Equal([]peoplesweep.CodexModel{{
		ID: "gpt-test", DisplayName: "Test Model", DefaultReasoningEffort: "medium",
		SupportedEfforts: []string{"low", "medium"},
	}}, models)
	assert.Equal(t, []string{"initialize", "initialized", "model/list"}, transcript.methods)
}

func TestCodexEveryProcessRequiresIsolationGate(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	denied := errors.New("deny marker: " + peoplesweep.ErrCodexIsolationUnreleased.Error())
	gate := &recordingCodexGate{verifyErr: errors.Join(peoplesweep.ErrCodexIsolationUnreleased, denied)}
	starter := &recordingCodexStarter{t: t}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, gate)
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)

	operations := []struct {
		name string
		run  func() error
	}{
		{name: "generation", run: func() error {
			_, callErr := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
			return callErr
		}},
		{name: "login", run: func() error {
			return transport.StartDeviceLogin(t.Context(), func(peoplesweep.DeviceLogin) error { return nil })
		}},
		{name: "models", run: func() error { _, callErr := transport.ListModels(t.Context()); return callErr }},
		{name: "provider status", run: func() error {
			registry, callErr := peoplesweep.NewDriverRegistry(nil, starter, gate)
			if callErr == nil {
				_, callErr = registry.Driver(peoplesweep.ProtocolCodexAppServer, codexTestConfig())
			}
			return callErr
		}},
		{name: "provider check", run: func() error {
			registry, callErr := peoplesweep.NewDriverRegistry(nil, starter, gate)
			if callErr == nil {
				_, callErr = registry.Driver(peoplesweep.ProtocolCodexAppServer, codexTestConfig())
			}
			return callErr
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			must.ErrorIs(err, peoplesweep.ErrCodexIsolationUnreleased)
		})
	}
	// Consent and revoke are durable host operations and do not touch the transport.
	checks.Zero(starter.starts.Load())
}

func TestCodexTransportReturnsAttestedVersions(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	transport, _, gate, _ := newSuccessfulCodexTransport(t, `{"claims":[]}`)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	response, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.NoError(err)
	wantProviderVersion, err := peoplesweep.CanonicalCodexProviderVersion(peoplesweep.CodexAttestation{
		ExecutablePath: codexTestAbsolutePath(), Version: "codex-cli 0.149.0",
		ExecutableSHA256: codexTestDigest, ExecutionBoundary: peoplesweep.CodexExecutionBoundaryV1,
		LaunchArtifact: peoplesweep.CodexLaunchArtifactNativeStandaloneV1,
	})
	must.NoError(err)
	checks.Equal(wantProviderVersion, response.ProviderVersion)
	checks.Equal("gpt-test", response.ModelVersion)
	checks.Equal(int64(1), gate.verifyCalls.Load())
}

func TestCodexTransportRejectsModelVersionChangeAcrossBatches(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	// A catalog drift that removes the exact configured ID must fail before a
	// second turn, so no output can authorize cursor movement under another model.
	firstTranscript := &codexTranscript{}
	secondTranscript := &codexTranscript{}
	starter := &recordingCodexStarter{t: t, scripts: []func(*bufio.Reader, io.Writer, io.Writer) error{
		successfulCodexScript(t, firstTranscript, "gpt-test", []string{"high"}, nil, `{"claims":[]}`),
		successfulCodexScript(t, secondTranscript, "gpt-test-drifted", []string{"high"}, nil, `{"claims":[{"unsafe":true}]}`),
	}}
	transport, err := peoplesweep.NewCodexAppServerDriver(codexTestConfig(), starter, &recordingCodexGate{})
	must.NoError(err)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	must.NoError(err)
	first, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.NoError(err)
	checks.Equal("gpt-test", first.ModelVersion)
	second, err := transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	must.ErrorIs(err, peoplesweep.ErrInvalidStructuredOutput)
	checks.Empty(second.CandidateJSON)
	checks.NotContains(secondTranscript.methods, "turn/start")
}

func TestCodexLaunchScrubsEnvironmentAndDisablesExtensions(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	t.Setenv("PACKET_SECRET", "must-not-forward")
	t.Setenv("OPENAI_API_KEY", "must-not-forward")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "auth-store"))
	transport, starter, _, transcript := newSuccessfulCodexTransport(t, `{"claims":[]}`)
	profile := codexTestProfile(t)
	prepared, err := transport.Prepare(profile, codexTestRequest())
	requireChecks.NoError(err)
	_, err = transport.GeneratePrepared(t.Context(), profile, peoplesweep.Credential{}, prepared)
	requireChecks.NoError(err)
	requireChecks.Len(starter.records, 1)
	record := starter.records[0]
	joinedEnv := strings.Join(record.env, "\n")
	assertChecks.NotContains(joinedEnv, "CODEX_HOME=")
	assertChecks.NotContains(joinedEnv, "PACKET_SECRET")
	assertChecks.NotContains(joinedEnv, "OPENAI_API_KEY")
	assertChecks.Equal([]string{
		"app-server", "--stdio", "--strict-config",
		"--disable", "plugins", "--disable", "apps", "--disable", "enable_mcp_apps",
		"--disable", "browser_use", "--disable", "computer_use", "--disable", "image_generation",
		"--disable", "skill_search", "--disable", "hooks", "--disable", "memories",
		"--disable", "multi_agent", "-c", "mcp_servers={}", "-c", "analytics.enabled=false",
	}, record.args)
	for index, frame := range transcript.frames {
		if index == len(transcript.frames)-1 {
			assertChecks.Contains(string(frame), "private packet marker")
		} else {
			assertChecks.NotContains(string(frame), "private packet marker")
		}
		assertChecks.NotContains(string(frame), "projectId")
		assertChecks.NotContains(string(frame), "developerInstructions")
	}
}
