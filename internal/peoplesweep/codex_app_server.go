package peoplesweep

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	codexPacketFilename           = "packet.json"
	codexPreparedComponentCount   = 5
	codexFixedUserInput           = "Read packet.json and return only JSON matching the supplied output schema."
	codexModelListLimit           = 100
	codexThreadIDReservationBytes = 128
	codexDisableFlag              = "--disable"
	codexProcessExitGrace         = 100 * time.Millisecond
)

var codexReservedThreadID = strings.Repeat("t", codexThreadIDReservationBytes)

var codexAppServerArgs = []string{
	"app-server", "--stdio", "--strict-config",
	codexDisableFlag, "plugins", codexDisableFlag, "apps", codexDisableFlag, "enable_mcp_apps",
	codexDisableFlag, "browser_use", codexDisableFlag, "computer_use", codexDisableFlag, "image_generation",
	codexDisableFlag, "skill_search", codexDisableFlag, "hooks", codexDisableFlag, "memories",
	codexDisableFlag, "multi_agent", "-c", "mcp_servers={}", "-c", "analytics.enabled=false",
}

// DeviceLogin is the bounded user-visible portion of a device-code login.
type DeviceLogin struct {
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// CodexModel is the bounded model-catalog surface exposed by msgvault.
type CodexModel struct {
	ID                     string   `json:"id"`
	DisplayName            string   `json:"display_name"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort"`
	SupportedEfforts       []string `json:"supported_efforts"`
}

// CodexAppServerDriver runs the attested Codex executable through the
// bounded App Server v2 stdio protocol.
type CodexAppServerDriver struct {
	Config    ProviderConfig
	Commands  CommandStarter
	Isolation CodexIsolationGate
}

// NewCodexAppServerDriver validates the immutable launch dependencies. The
// release gate is checked again for each launch; constructing a transport does
// not retain an attestation for later use.
func NewCodexAppServerDriver(
	cfg ProviderConfig,
	commands CommandStarter,
	isolation CodexIsolationGate,
) (*CodexAppServerDriver, error) {
	validation := Config{
		Enabled: true, Provider: ProviderSelection{Name: "runtime"},
		Providers: map[string]ProviderConfig{"runtime": cfg},
	}
	validation.ApplyDefaults()
	if err := validation.Validate(); err != nil {
		return nil, err
	}
	_, provider, err := validation.ActiveProviderConfig()
	if err != nil {
		return nil, err
	}
	if provider.Protocol != ProtocolCodexAppServer {
		return nil, errors.New("codex app-server transport requires codex_app_server configuration")
	}
	if commands == nil {
		return nil, errors.New("codex app-server command starter is required")
	}
	if isolation == nil {
		return nil, errors.New("codex app-server isolation gate is required")
	}
	return &CodexAppServerDriver{
		Config: provider, Commands: commands, Isolation: isolation,
	}, nil
}

type codexInitializeParams struct {
	ClientInfo struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"clientInfo"`
	Capabilities struct {
		ExperimentalAPI bool `json:"experimentalApi"`
	} `json:"capabilities"`
}

type codexModelListParams struct {
	Limit         int  `json:"limit"`
	IncludeHidden bool `json:"includeHidden"`
}

type codexReadOnlySandboxPolicy struct {
	Type          string `json:"type"`
	NetworkAccess bool   `json:"networkAccess"`
}

type codexThreadStartParams struct {
	Model                   string                     `json:"model"`
	Effort                  string                     `json:"effort"`
	Ephemeral               bool                       `json:"ephemeral"`
	CWD                     string                     `json:"cwd"`
	RuntimeWorkspaceRoots   []string                   `json:"runtimeWorkspaceRoots"`
	SelectedCapabilityRoots []string                   `json:"selectedCapabilityRoots"`
	DynamicTools            []any                      `json:"dynamicTools"`
	Environments            []any                      `json:"environments"`
	ApprovalPolicy          string                     `json:"approvalPolicy"`
	Sandbox                 string                     `json:"sandbox"`
	SandboxPolicy           codexReadOnlySandboxPolicy `json:"sandboxPolicy"`
}

type codexTurnStartParams struct {
	ThreadID       string                     `json:"threadId"`
	Input          []codexTextInput           `json:"input"`
	Model          string                     `json:"model"`
	Effort         string                     `json:"effort"`
	CWD            string                     `json:"cwd"`
	ApprovalPolicy string                     `json:"approvalPolicy"`
	Sandbox        string                     `json:"sandbox"`
	SandboxPolicy  codexReadOnlySandboxPolicy `json:"sandboxPolicy"`
	OutputSchema   json.RawMessage            `json:"outputSchema"`
}

type codexTextInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func codexInitializeRequest(id int64) codexRPCRequest {
	params := codexInitializeParams{}
	params.ClientInfo.Name = "msgvault"
	params.ClientInfo.Title = "msgvault"
	params.ClientInfo.Version = "1"
	params.Capabilities.ExperimentalAPI = true
	return codexRPCRequest{Method: "initialize", ID: id, Params: params}
}

func codexModelListRequest(id int64) codexRPCRequest {
	return codexRPCRequest{Method: "model/list", ID: id, Params: codexModelListParams{
		Limit: codexModelListLimit, IncludeHidden: true,
	}}
}

func codexThreadStartRequest(id int64, profile ProviderProfile) codexRPCRequest {
	return codexRPCRequest{Method: "thread/start", ID: id, Params: codexThreadStartParams{
		Model: profile.Model, Effort: profile.ReasoningEffort, Ephemeral: true, CWD: ".",
		RuntimeWorkspaceRoots: []string{"."}, SelectedCapabilityRoots: []string{},
		DynamicTools: []any{}, Environments: []any{}, ApprovalPolicy: "never", Sandbox: "read-only",
		SandboxPolicy: codexReadOnlySandboxPolicy{Type: "readOnly", NetworkAccess: false},
	}}
}

func codexTurnStartRequest(
	id int64,
	profile ProviderProfile,
	request StructuredRequest,
	threadID string,
) codexRPCRequest {
	return codexRPCRequest{Method: "turn/start", ID: id, Params: codexTurnStartParams{
		ThreadID: threadID,
		Input:    []codexTextInput{{Type: "text", Text: codexFixedUserInput}},
		Model:    profile.Model, Effort: profile.ReasoningEffort, CWD: ".",
		ApprovalPolicy: "never", Sandbox: "read-only",
		SandboxPolicy: codexReadOnlySandboxPolicy{Type: "readOnly", NetworkAccess: false},
		OutputSchema:  slices.Clone(request.JSONSchema),
	}}
}

// Prepare constructs the packet and four exact outbound request frames.
// Each component is independently length-prefixed for unambiguous reservation.
func (t *CodexAppServerDriver) Prepare(
	profile ProviderProfile,
	request StructuredRequest,
) (PreparedStructuredRequest, error) {
	if err := t.validateProfile(profile); err != nil {
		return PreparedStructuredRequest{}, err
	}
	frames := []codexRPCRequest{
		codexInitializeRequest(1),
		codexModelListRequest(2),
		codexThreadStartRequest(3, profile),
		codexTurnStartRequest(4, profile, request, codexReservedThreadID),
	}
	components := make([][]byte, 0, codexPreparedComponentCount)
	components = append(components, []byte(request.InputText))
	for _, frame := range frames {
		encoded, err := json.Marshal(frame)
		if err != nil {
			return PreparedStructuredRequest{}, errors.New("encode codex app-server request")
		}
		components = append(components, append(encoded, '\n'))
	}
	wire, err := encodeCodexPreparedComponents(components)
	if err != nil {
		return PreparedStructuredRequest{}, err
	}
	return NewPreparedStructuredRequest(request, wire)
}

func (t *CodexAppServerDriver) validateProfile(profile ProviderProfile) error {
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.Protocol != ProtocolCodexAppServer ||
		profile.Model != strings.TrimSpace(t.Config.Model) ||
		profile.ReasoningEffort != strings.TrimSpace(t.Config.ReasoningEffort) ||
		profile.ExecutionBoundary != t.Config.ExecutionBoundary {
		return errors.New("codex app-server profile does not match transport configuration")
	}
	return nil
}

func encodeCodexPreparedComponents(components [][]byte) ([]byte, error) {
	var wire bytes.Buffer
	for _, component := range components {
		if len(component) == 0 {
			return nil, errors.New("codex prepared component is empty")
		}
		if err := binary.Write(&wire, binary.BigEndian, uint64(len(component))); err != nil {
			return nil, errors.New("encode codex prepared component length")
		}
		if _, err := wire.Write(component); err != nil {
			return nil, errors.New("encode codex prepared component")
		}
	}
	return wire.Bytes(), nil
}

func decodeCodexPreparedComponents(wire []byte) ([][]byte, error) {
	components := make([][]byte, 0, codexPreparedComponentCount)
	for len(wire) > 0 {
		if len(wire) < 8 {
			return nil, errors.New("prepared codex app-server wire is truncated")
		}
		length := binary.BigEndian.Uint64(wire[:8])
		wire = wire[8:]
		if length == 0 || length > uint64(len(wire)) || length > defaultCodexMaxFrameBytes {
			return nil, errors.New("prepared codex app-server component length is invalid")
		}
		components = append(components, append([]byte(nil), wire[:length]...))
		wire = wire[length:]
	}
	if len(components) != codexPreparedComponentCount {
		return nil, errors.New("prepared codex app-server wire has an invalid component count")
	}
	return components, nil
}

// GeneratePrepared launches the attested process and sends only the exact
// prepared packet and frames covered by the caller's reservation.
func (t *CodexAppServerDriver) GeneratePrepared(
	ctx context.Context,
	profile ProviderProfile,
	credential Credential,
	prepared PreparedStructuredRequest,
) (response DriverResponse, retErr error) {
	if credential.hasValue() {
		return DriverResponse{}, errors.New("codex app-server does not accept a forwarded credential")
	}
	if err := t.validateProfile(profile); err != nil {
		return DriverResponse{}, err
	}
	if err := prepared.validateWireHash(); err != nil {
		return DriverResponse{}, err
	}
	expected, err := t.Prepare(profile, prepared.Request())
	if err != nil {
		return DriverResponse{}, fmt.Errorf("re-encode prepared codex app-server request: %w", err)
	}
	if !bytes.Equal(expected.WireRequest(), prepared.WireRequest()) {
		return DriverResponse{}, errors.New("prepared codex app-server request does not match deterministic encoding")
	}
	components, err := decodeCodexPreparedComponents(prepared.WireRequest())
	if err != nil {
		return DriverResponse{}, err
	}
	if !bytes.Equal(components[0], []byte(prepared.Request().InputText)) {
		return DriverResponse{}, errors.New("prepared codex app-server packet does not match request")
	}

	attestation, err := t.attest(ctx)
	if err != nil {
		return DriverResponse{}, err
	}
	defer func() {
		if closeErr := attestation.Close(); closeErr != nil && retErr == nil {
			response = DriverResponse{}
			retErr = closeErr
		}
	}()
	packetRoot, err := os.MkdirTemp("", "msgvault-codex-packet-")
	if err != nil {
		return DriverResponse{}, errors.New("create codex packet root")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(packetRoot); cleanupErr != nil && retErr == nil {
			response = DriverResponse{}
			retErr = errors.New("remove codex packet root")
		}
	}()
	packetPath := filepath.Join(packetRoot, codexPacketFilename)
	if err := os.WriteFile(packetPath, components[0], 0o400); err != nil {
		return DriverResponse{}, errors.New("write codex packet")
	}
	if err := os.Chmod(packetPath, 0o400); err != nil {
		return DriverResponse{}, errors.New("protect codex packet")
	}

	process, err := t.startAttested(ctx, attestation, packetRoot)
	if err != nil {
		return DriverResponse{}, err
	}
	client := &CodexRPCClient{Process: process}
	defer func() {
		cleanupErr := finishCodexProcess(ctx, process, client, retErr != nil)
		if cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()

	var initialized map[string]any
	if err := client.callPrepared(ctx, components[1], &initialized); err != nil {
		return DriverResponse{}, err
	}
	var catalog codexModelListResult
	if err := client.callPrepared(ctx, components[2], &catalog); err != nil {
		return DriverResponse{}, err
	}
	if catalog.NextCursor != nil || len(catalog.Data) > codexModelListLimit {
		return DriverResponse{}, fmt.Errorf("%w: Codex model catalog exceeds the bounded page", ErrInvalidStructuredOutput)
	}
	selected, err := selectCodexModel(catalog.Data, profile.Model, profile.ReasoningEffort)
	if err != nil {
		return DriverResponse{}, fmt.Errorf("%w: configured Codex model or effort is unavailable", ErrInvalidStructuredOutput)
	}

	var threadResult codexThreadStartResult
	if err := client.callPrepared(ctx, components[3], &threadResult); err != nil {
		return DriverResponse{}, err
	}
	if !safeProviderMetadata(threadResult.Thread.ID) || !threadResult.Thread.Ephemeral {
		return DriverResponse{}, fmt.Errorf("%w: Codex returned an invalid ephemeral thread", ErrInvalidStructuredOutput)
	}
	turnFrame, err := rewritePreparedTurnThreadID(components[4], threadResult.Thread.ID)
	if err != nil {
		return DriverResponse{}, err
	}
	var turnResult codexTurnStartResult
	if err := client.callPrepared(ctx, turnFrame, &turnResult); err != nil {
		return DriverResponse{}, err
	}
	if !safeProviderMetadata(turnResult.Turn.ID) {
		return DriverResponse{}, fmt.Errorf("%w: Codex returned an invalid turn", ErrInvalidStructuredOutput)
	}

	providerVersion, err := CanonicalCodexProviderVersion(attestation)
	if err != nil {
		return DriverResponse{}, fmt.Errorf("%w: invalid Codex attestation identity", ErrInvalidStructuredOutput)
	}
	response = DriverResponse{
		ProviderRequestID: turnResult.Turn.ID,
		ProviderVersion:   providerVersion,
		ModelVersion:      selected.ID,
	}
	final, usage, usageKnown, err := readCodexFinal(ctx, client, threadResult.Thread.ID, turnResult.Turn.ID)
	response.Usage = usage
	response.UsageKnown = usageKnown
	if err != nil {
		return response, err
	}
	request := prepared.Request()
	if err := validateCodexFinal(request, final); err != nil {
		return response, err
	}
	response.CandidateJSON = append(json.RawMessage(nil), final...)
	return response, nil
}

var _ StructuredDriver = (*CodexAppServerDriver)(nil)

func rewritePreparedTurnThreadID(frame []byte, actual string) ([]byte, error) {
	if !safeProviderMetadata(actual) || len(actual) > codexThreadIDReservationBytes {
		return nil, fmt.Errorf("%w: Codex returned an invalid thread ID", ErrInvalidStructuredOutput)
	}
	var request struct {
		Method string          `json:"method"`
		ID     int64           `json:"id"`
		Params json.RawMessage `json:"params"`
	}
	if err := decodeSingleJSON(frame, &request); err != nil || request.Method != "turn/start" || request.ID != 4 {
		return nil, errors.New("prepared codex turn frame is invalid")
	}
	var params codexTurnStartParams
	if decodeSingleJSON(request.Params, &params) != nil {
		return nil, errors.New("prepared codex turn parameters are invalid")
	}
	if params.ThreadID != codexReservedThreadID {
		return nil, errors.New("prepared codex turn thread-ID slot is invalid")
	}
	reserved := []byte(codexReservedThreadID)
	if bytes.Count(frame, reserved) != 1 {
		return nil, errors.New("prepared codex turn thread-ID slot is ambiguous")
	}
	actualFrame := bytes.Replace(frame, reserved, []byte(actual), 1)
	if len(actualFrame) > len(frame) {
		return nil, errors.New("codex turn thread-ID substitution exceeds its reservation")
	}
	return actualFrame, nil
}

type codexReasoningEffort struct {
	ReasoningEffort string `json:"reasoningEffort"`
}

type codexModelEntry struct {
	ID                     string                 `json:"id"`
	Model                  string                 `json:"model"`
	DisplayName            string                 `json:"displayName"`
	DefaultReasoningEffort string                 `json:"defaultReasoningEffort"`
	SupportedEfforts       []codexReasoningEffort `json:"supportedReasoningEfforts"`
}

type codexModelListResult struct {
	Data       []codexModelEntry `json:"data"`
	NextCursor *string           `json:"nextCursor"`
}

type codexThreadStartResult struct {
	Thread struct {
		ID        string `json:"id"`
		Ephemeral bool   `json:"ephemeral"`
	} `json:"thread"`
}

type codexTurnStartResult struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

func selectCodexModel(entries []codexModelEntry, model, effort string) (CodexModel, error) {
	for _, entry := range entries {
		if entry.ID != model {
			continue
		}
		converted, err := convertCodexModel(entry)
		if err != nil || !slices.Contains(converted.SupportedEfforts, effort) {
			return CodexModel{}, errors.New("configured Codex effort is unsupported")
		}
		return converted, nil
	}
	return CodexModel{}, errors.New("configured Codex model is unavailable")
}

func convertCodexModel(entry codexModelEntry) (CodexModel, error) {
	if !safeProviderMetadata(entry.ID) || entry.Model != entry.ID ||
		!safeCodexDisplay(entry.DisplayName) || !safeProviderMetadata(entry.DefaultReasoningEffort) {
		return CodexModel{}, errors.New("codex returned invalid model metadata")
	}
	efforts := make([]string, 0, len(entry.SupportedEfforts))
	for _, effort := range entry.SupportedEfforts {
		if !safeProviderMetadata(effort.ReasoningEffort) || slices.Contains(efforts, effort.ReasoningEffort) {
			return CodexModel{}, errors.New("codex returned invalid reasoning effort metadata")
		}
		efforts = append(efforts, effort.ReasoningEffort)
	}
	if len(efforts) == 0 || !slices.Contains(efforts, entry.DefaultReasoningEffort) {
		return CodexModel{}, errors.New("codex returned an invalid default reasoning effort")
	}
	return CodexModel{
		ID: entry.ID, DisplayName: entry.DisplayName,
		DefaultReasoningEffort: entry.DefaultReasoningEffort,
		SupportedEfforts:       efforts,
	}, nil
}

func safeCodexDisplay(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) &&
		strings.IndexFunc(value, unicode.IsControl) == -1
}

func readCodexFinal(
	ctx context.Context,
	client *CodexRPCClient,
	threadID string,
	turnID string,
) (json.RawMessage, TokenUsage, bool, error) {
	var final json.RawMessage
	usage := TokenUsage{}
	usageKnown := false
	for {
		method, params, err := client.nextNotification(ctx)
		if err != nil {
			return nil, usage, usageKnown, err
		}
		switch method {
		case "item/completed":
			var event struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Item     struct {
					Type  string `json:"type"`
					Phase string `json:"phase"`
					Text  string `json:"text"`
				} `json:"item"`
			}
			if decodeSingleJSON(params, &event) != nil {
				return nil, usage, usageKnown, fmt.Errorf("%w: malformed Codex item event", ErrInvalidStructuredOutput)
			}
			if event.ThreadID == threadID && event.TurnID == turnID &&
				event.Item.Type == "agentMessage" && event.Item.Phase == "final_answer" {
				if len(final) != 0 || len(event.Item.Text) == 0 || len(event.Item.Text) > defaultCodexMaxFrameBytes {
					return nil, usage, usageKnown, fmt.Errorf("%w: invalid Codex final assistant output", ErrInvalidStructuredOutput)
				}
				final = json.RawMessage(append([]byte(nil), event.Item.Text...))
			}
		case "thread/tokenUsage/updated":
			var event struct {
				ThreadID   string `json:"threadId"`
				TurnID     string `json:"turnId"`
				TokenUsage *struct {
					Total *struct {
						InputTokens  *int64 `json:"inputTokens"`
						OutputTokens *int64 `json:"outputTokens"`
					} `json:"totalTokenUsage"`
				} `json:"tokenUsage"`
			}
			if decodeSingleJSON(params, &event) != nil || event.ThreadID != threadID ||
				event.TurnID != turnID || event.TokenUsage == nil || event.TokenUsage.Total == nil ||
				event.TokenUsage.Total.InputTokens == nil || event.TokenUsage.Total.OutputTokens == nil ||
				*event.TokenUsage.Total.InputTokens < 0 || *event.TokenUsage.Total.OutputTokens < 0 ||
				*event.TokenUsage.Total.InputTokens < usage.InputTokens ||
				*event.TokenUsage.Total.OutputTokens < usage.OutputTokens {
				return nil, usage, usageKnown, fmt.Errorf("%w: invalid Codex usage metadata", ErrInvalidStructuredOutput)
			}
			usage = TokenUsage{
				InputTokens:  *event.TokenUsage.Total.InputTokens,
				OutputTokens: *event.TokenUsage.Total.OutputTokens,
			}
			usageKnown = true
		case "turn/completed":
			var event struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if decodeSingleJSON(params, &event) != nil || event.ThreadID != threadID || event.Turn.ID != turnID {
				return nil, usage, usageKnown, fmt.Errorf("%w: malformed Codex turn completion", ErrInvalidStructuredOutput)
			}
			if event.Turn.Status != "completed" || len(final) == 0 {
				return nil, usage, usageKnown, fmt.Errorf("%w: Codex turn did not return final structured output", ErrInvalidStructuredOutput)
			}
			return final, usage, usageKnown, nil
		}
	}
}

func validateCodexFinal(request StructuredRequest, final json.RawMessage) error {
	resolvedSchema, err := validateStructuredRequest(request, request.ProgramID == "provider-check")
	if err != nil {
		return err
	}
	var decoded any
	if err := decodeJSONSchemaInstance(final, &decoded); err != nil {
		return fmt.Errorf("%w: Codex returned invalid structured JSON", ErrInvalidStructuredOutput)
	}
	if err := resolvedSchema.Validate(decoded); err != nil {
		return fmt.Errorf("%w: Codex output does not match requested schema", ErrInvalidStructuredOutput)
	}
	return nil
}

// StartDeviceLogin keeps the app-server session alive until the device-code
// flow completes. present receives only the bounded public ceremony fields.
func (t *CodexAppServerDriver) StartDeviceLogin(
	ctx context.Context, present func(DeviceLogin) error,
) (retErr error) {
	operationCtx, cancel := context.WithTimeout(ctx, t.Config.RequestTimeout)
	defer cancel()
	return t.withProcessDeviceLogin(operationCtx, present)
}

func (t *CodexAppServerDriver) withProcessDeviceLogin(
	ctx context.Context, present func(DeviceLogin) error,
) (retErr error) {
	if present == nil {
		return errors.New("codex device login presenter is required")
	}
	process, cleanup, err := t.launchEmpty(ctx)
	if err != nil {
		return err
	}
	client := &CodexRPCClient{Process: process}
	defer func() { retErr = cleanup(client, retErr) }()
	var initialized map[string]any
	if err := client.Call(ctx, "initialize", codexInitializeRequest(1).Params, &initialized); err != nil {
		return err
	}
	var result struct {
		Type            string `json:"type"`
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
		ExpiresAt       string `json:"expiresAt"`
		ExpiresIn       int64  `json:"expiresIn"`
	}
	if err := client.Call(ctx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"}, &result); err != nil {
		return err
	}
	if result.Type != "chatgptDeviceCode" || !safeCodexDeviceURL(result.VerificationURL) ||
		!safeProviderMetadata(result.UserCode) || !safeProviderMetadata(result.LoginID) {
		return errors.New("codex app-server returned invalid device login metadata")
	}
	fallbackExpiry, _ := ctx.Deadline()
	expiresAt, err := parseCodexDeviceExpiry(result.ExpiresAt, result.ExpiresIn, fallbackExpiry)
	if err != nil {
		return err
	}
	if err := present(DeviceLogin{
		VerificationURL: result.VerificationURL, UserCode: result.UserCode, ExpiresAt: expiresAt,
	}); err != nil {
		return err
	}
	for {
		method, params, err := client.nextNotification(ctx)
		if err != nil {
			return err
		}
		if method != "account/login/completed" {
			continue
		}
		var event struct {
			Success bool    `json:"success"`
			LoginID *string `json:"loginId"`
			Error   *string `json:"error"`
		}
		if decodeSingleJSON(params, &event) != nil || event.LoginID == nil ||
			*event.LoginID != result.LoginID || !event.Success || event.Error != nil {
			return errors.New("codex app-server device login did not complete successfully")
		}
		return nil
	}
}

func safeCodexDeviceURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func parseCodexDeviceExpiry(raw string, expiresIn int64, fallback time.Time) (time.Time, error) {
	if raw != "" {
		expiresAt, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, errors.New("codex app-server returned invalid device login expiry")
		}
		return expiresAt.UTC(), nil
	}
	if expiresIn == 0 && !fallback.IsZero() {
		return fallback.UTC(), nil
	}
	if expiresIn <= 0 || expiresIn > int64((24*time.Hour)/time.Second) {
		return time.Time{}, errors.New("codex app-server did not return a bounded device login expiry")
	}
	return time.Now().UTC().Add(time.Duration(expiresIn) * time.Second), nil
}

// ListModels returns the bounded model IDs and effort choices supplied by the
// attested App Server process.
func (t *CodexAppServerDriver) ListModels(ctx context.Context) (models []CodexModel, retErr error) {
	operationCtx, cancel := context.WithTimeout(ctx, t.Config.RequestTimeout)
	defer cancel()
	ctx = operationCtx

	process, cleanup, err := t.launchEmpty(ctx)
	if err != nil {
		return nil, err
	}
	client := &CodexRPCClient{Process: process}
	defer func() { retErr = cleanup(client, retErr) }()
	var initialized map[string]any
	if err := client.Call(ctx, "initialize", codexInitializeRequest(1).Params, &initialized); err != nil {
		return nil, err
	}
	var result codexModelListResult
	if err := client.Call(ctx, "model/list", codexModelListRequest(2).Params, &result); err != nil {
		return nil, err
	}
	if result.NextCursor != nil || len(result.Data) > codexModelListLimit {
		return nil, errors.New("codex model catalog exceeds the bounded page")
	}
	models = make([]CodexModel, 0, len(result.Data))
	for _, entry := range result.Data {
		model, err := convertCodexModel(entry)
		if err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, nil
}

func (t *CodexAppServerDriver) attest(ctx context.Context) (CodexAttestation, error) {
	attestation, err := t.Isolation.Verify(ctx, t.Config.Executable, t.Config.ExecutionBoundary)
	if err != nil {
		_ = attestation.Close()
		return CodexAttestation{}, fmt.Errorf("verify codex app-server isolation: %w", err)
	}
	if attestation.ExecutablePath == "" || !filepath.IsAbs(attestation.ExecutablePath) ||
		attestation.ExecutionBoundary != t.Config.ExecutionBoundary {
		_ = attestation.Close()
		return CodexAttestation{}, errors.New("codex app-server attestation is invalid")
	}
	if _, err := CanonicalCodexProviderVersion(attestation); err != nil {
		_ = attestation.Close()
		return CodexAttestation{}, errors.New("codex app-server attestation identity is invalid")
	}
	return attestation, nil
}

func (t *CodexAppServerDriver) startAttested(
	ctx context.Context,
	attestation CodexAttestation,
	dir string,
) (RPCProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.Isolation.ReverifyForLaunch(attestation); err != nil {
		return nil, fmt.Errorf("reverify codex app-server isolation: %w", err)
	}
	process, err := t.Commands.Start(
		ctx, attestation.VerifiedExecutable(), slices.Clone(codexAppServerArgs), scrubCodexEnvironment(os.Environ()), dir,
	)
	if err != nil {
		return nil, errors.New("start codex app-server process")
	}
	return process, nil
}

func (t *CodexAppServerDriver) launchEmpty(
	ctx context.Context,
) (RPCProcess, func(*CodexRPCClient, error) error, error) {
	attestation, err := t.attest(ctx)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "msgvault-codex-operation-")
	if err != nil {
		_ = attestation.Close()
		return nil, nil, errors.New("create codex operation root")
	}
	process, err := t.startAttested(ctx, attestation, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		_ = attestation.Close()
		return nil, nil, err
	}
	cleanup := func(client *CodexRPCClient, operationErr error) error {
		processErr := finishCodexProcess(ctx, process, client, operationErr != nil)
		removeErr := os.RemoveAll(dir)
		attestationErr := attestation.Close()
		if operationErr != nil {
			return operationErr
		}
		if processErr != nil {
			return processErr
		}
		if removeErr != nil {
			return errors.New("remove codex operation root")
		}
		if attestationErr != nil {
			return attestationErr
		}
		return nil
	}
	return process, cleanup, nil
}

func finishCodexProcess(
	ctx context.Context,
	process RPCProcess,
	client *CodexRPCClient,
	forceKill bool,
) error {
	if process == nil {
		return nil
	}
	var closeErr error
	if stdin := process.Stdin(); stdin != nil {
		closeErr = stdin.Close()
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- process.Wait() }()
	var waitErr error
	var cleanupContextErr error
	waited := false
	killAttempted := false
	killSucceeded := false
	killAlreadyFinished := false
	killFailed := false
	kill := func() {
		if killAttempted {
			return
		}
		killAttempted = true
		killErr := process.Kill()
		killSucceeded = killErr == nil
		killAlreadyFinished = errors.Is(killErr, os.ErrProcessDone)
		killFailed = killErr != nil && !killAlreadyFinished
	}
	if forceKill {
		kill()
	} else {
		timer := time.NewTimer(codexProcessExitGrace)
		select {
		case waitErr = <-waitDone:
			waited = true
		case <-ctx.Done():
			cleanupContextErr = ctx.Err()
			kill()
		case <-timer.C:
			kill()
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	waitAbandoned := false
	if !waited {
		if killAttempted && (killAlreadyFinished || killFailed) {
			timer := time.NewTimer(codexProcessExitGrace)
			select {
			case waitErr = <-waitDone:
			case <-timer.C:
				waitAbandoned = true
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		} else {
			waitErr = <-waitDone
		}
	}
	stderrJoined := false
	var stderrErr error
	if !waitAbandoned {
		stderrJoined, stderrErr = client.waitForStderr(ctx, codexProcessExitGrace)
	}
	if stdout := process.Stdout(); stdout != nil {
		if err := stdout.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	if stderr := process.Stderr(); stderr != nil {
		if err := stderr.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	if !stderrJoined && client != nil {
		var joined bool
		joined, stderrErr = client.waitForStderr(context.Background(), codexProcessExitGrace)
		if !joined {
			stderrErr = errors.New("codex app-server stderr drain did not finish")
		}
	}
	if killSucceeded && errors.Is(stderrErr, errCodexStderrRead) {
		stderrErr = nil
	}
	if forceKill {
		return nil
	}
	if cleanupContextErr != nil {
		return cleanupContextErr
	}
	if waitAbandoned {
		return errors.New("codex app-server process termination failed")
	}
	if waitErr != nil && !killSucceeded {
		return errors.New("codex app-server process failed")
	}
	if stderrErr != nil {
		return stderrErr
	}
	if closeErr != nil {
		return errors.New("close codex app-server process streams")
	}
	return nil
}

func scrubCodexEnvironment(environment []string) []string {
	allowed := map[string]struct{}{
		"PATH": {}, "HOME": {}, "USERPROFILE": {}, "CODEX_HOME": {}, "XDG_CONFIG_HOME": {},
		"TMPDIR": {}, "TMP": {}, "TEMP": {}, "SystemRoot": {}, "SYSTEMROOT": {},
		"WINDIR": {}, "COMSPEC": {}, "PATHEXT": {},
	}
	result := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, item := range environment {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, ok := allowed[name]; !ok {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, item)
	}
	slices.Sort(result)
	return result
}

type execCommandStarter struct{}

// NewCodexCommandStarter returns the production os/exec-backed process
// boundary. The isolation gate still controls which absolute executable may be
// passed to it.
func NewCodexCommandStarter() CommandStarter { return execCommandStarter{} }

func (execCommandStarter) Start(
	ctx context.Context,
	executable CodexExecutable,
	args []string,
	env []string,
	dir string,
) (RPCProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if executable.verifiedPath == "" || !filepath.IsAbs(executable.verifiedPath) {
		return nil, errors.New("codex app-server verified executable is required")
	}
	command := exec.Command(executable.verifiedPath, args...) //nolint:gosec // The gate owns this absolute private snapshot.
	command.Env = slices.Clone(env)
	command.Dir = dir
	processTree, err := newCodexAppServerProcessTree(command)
	if err != nil {
		return nil, err
	}
	processTreeOwned := true
	defer func() {
		if processTreeOwned {
			_ = processTree.close()
		}
	}()
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("create codex app-server stdin")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.New("create codex app-server stdout")
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, errors.New("create codex app-server stderr")
	}
	if err := command.Start(); err != nil {
		return nil, errors.New("start codex app-server executable")
	}
	if err := processTree.attach(command); err != nil {
		_ = processTree.terminate(command)
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	processTreeOwned = false
	return &execRPCProcess{
		command: command, stdin: stdin, stdout: stdout, stderr: stderr, processTree: processTree,
	}, nil
}

type execRPCProcess struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	processTree *codexAppServerProcessTree
	waitOnce    sync.Once
	waitErr     error
}

func (p *execRPCProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execRPCProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execRPCProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *execRPCProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.command.Wait()
		if p.processTree != nil {
			terminateErr := p.processTree.terminate(p.command)
			closeErr := p.processTree.close()
			if p.waitErr == nil && terminateErr != nil && !errors.Is(terminateErr, os.ErrProcessDone) {
				p.waitErr = terminateErr
			}
			if p.waitErr == nil && closeErr != nil {
				p.waitErr = closeErr
			}
		}
	})
	return p.waitErr
}
func (p *execRPCProcess) Kill() error {
	if p.command.Process == nil {
		return nil
	}
	if p.processTree != nil {
		return p.processTree.terminate(p.command)
	}
	return p.command.Process.Kill()
}
