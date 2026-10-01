package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

const mcpSweepCommandName = "sweep"

func sweepMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	hasCommand := func(name string, flags ...string) bool {
		for _, command := range capabilities.Commands {
			if command.Name != name || command.Delegated {
				continue
			}
			if !slices.Contains(command.Flags, flagJSON) {
				return false
			}
			for _, flag := range flags {
				if !slices.Contains(command.Flags, flag) {
					return false
				}
			}
			return true
		}
		return false
	}
	names := []string{}
	if hasCommand("person sweep status") {
		names = append(names, "get_people_sweep_status")
		if hasCommand("person sweep run", "person", "limit", "backstop") && hasCommand("person provider status") && hasRoute("getSettingsPeopleInference", http.MethodGet, peopleProviderSettingsPath) {
			names = append(names, "run_people_sweep")
		}
	}
	if hasCommand("person sweep history", "person", "limit") {
		names = append(names, "list_people_sweep_history")
	}
	return names
}

type mcpSweepInput struct {
	PersonID *int64 `json:"person_id,omitzero"`
	Limit    *int64 `json:"limit,omitzero"`
	Backstop *bool  `json:"backstop,omitzero"`
}

func mcpSweepArguments(name string, args map[string]any) (mcpSweepInput, error) {
	allowed := []string{}
	if name == "list_people_sweep_history" || name == "run_people_sweep" {
		allowed = append(allowed, "person_id", "limit")
	}
	if name == "run_people_sweep" {
		allowed = append(allowed, "backstop")
	}
	for key, value := range args {
		if !slices.Contains(allowed, key) || value == nil {
			return mcpSweepInput{}, errors.New("invalid sweep argument")
		}
	}
	input, err := decodeMCPOperationArguments[mcpSweepInput](args)
	if err != nil || input.PersonID != nil && !mcpPositiveSafeID(*input.PersonID) || input.Limit != nil && (*input.Limit < 1 || *input.Limit > 9007199254740991 || name == "list_people_sweep_history" && *input.Limit > 200) {
		return mcpSweepInput{}, errors.New("invalid sweep arguments")
	}
	return input, nil
}

func readMCPSweepJSON[T any](ctx context.Context, b *daemonMCPOperations, argv []string, writes bool) (*mcpserver.OperationResult, error) {
	stream, err := b.client.RunMCPCLICommand(ctx, argv, "")
	if err != nil || stream == nil || stream.Failed {
		return operationFailure("people_sweep_unavailable", writes), err
	}
	var output T
	if err := json.Unmarshal([]byte(stream.Stdout), &output, json.RejectUnknownMembers(true)); err != nil {
		return operationFailure("invalid_operation_response", writes), err
	}
	return &mcpserver.OperationResult{Output: output}, nil
}

func (b *daemonMCPOperations) resolveSweepLimit(ctx context.Context, input mcpSweepInput) (peoplesweep.StatusOutput, int64, error) {
	result, err := readMCPSweepJSON[peoplesweep.StatusOutput](ctx, b, []string{personValue, mcpSweepCommandName, multimodalStatusSubcommand, "--json"}, false)
	if err != nil {
		return peoplesweep.StatusOutput{}, 0, err
	}
	if result == nil || result.IsError {
		return peoplesweep.StatusOutput{}, 0, &mcpserver.OperationRefusalError{Code: "people_sweep_unavailable"}
	}
	status, ok := result.Output.(peoplesweep.StatusOutput)
	if !ok || !status.Enabled || status.WorkBatchSize < 1 || status.ProviderFingerprint == "" {
		return peoplesweep.StatusOutput{}, 0, &mcpserver.OperationRefusalError{Code: "people_sweep_unavailable"}
	}
	limit := min(int64(25), int64(status.WorkBatchSize))
	if input.Limit != nil {
		limit = *input.Limit
	}
	if limit > int64(status.WorkBatchSize) {
		return peoplesweep.StatusOutput{}, 0, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
	}
	return status, limit, nil
}

func (b *daemonMCPOperations) executeSweepOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !slices.Contains([]string{"get_people_sweep_status", "list_people_sweep_history", "run_people_sweep"}, name) {
		return nil, false, nil
	}
	input, err := mcpSweepArguments(name, args)
	if err != nil {
		return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
	}
	subcommand := multimodalStatusSubcommand
	if name == "list_people_sweep_history" {
		subcommand = "history"
	}
	if name == "run_people_sweep" {
		subcommand = "run"
		_, limit, err := b.resolveSweepLimit(ctx, input)
		if err != nil {
			if refusal, ok := errors.AsType[*mcpserver.OperationRefusalError](err); ok {
				return operationFailure(refusal.Code, false), true, nil
			}
			return nil, true, err
		}
		input.Limit = &limit
	}
	argv := []string{personValue, mcpSweepCommandName, subcommand, "--json"}
	if input.PersonID != nil {
		argv = append(argv, "--person="+strconv.FormatInt(*input.PersonID, 10))
	}
	if input.Limit != nil {
		argv = append(argv, "--limit="+strconv.FormatInt(*input.Limit, 10))
	}
	if input.Backstop != nil {
		argv = append(argv, "--backstop="+strconv.FormatBool(*input.Backstop))
	}
	switch name {
	case "get_people_sweep_status":
		result, err := readMCPSweepJSON[peoplesweep.StatusOutput](ctx, b, argv, false)
		return result, true, err
	case "list_people_sweep_history":
		result, err := readMCPSweepJSON[peoplesweep.HistoryOutput](ctx, b, argv, false)
		return result, true, err
	default:
		result, err := readMCPSweepJSON[peoplesweep.RunOutput](ctx, b, argv, true)
		return result, true, err
	}
}

func (b *daemonMCPOperations) sweepOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "run_people_sweep" {
		return "", false, nil
	}
	input, err := mcpSweepArguments(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
	}
	status, limit, err := b.resolveSweepLimit(ctx, input)
	if err != nil {
		return "", true, err
	}
	input.Limit = &limit
	provider, err := b.readProviderCLI(ctx, "get_people_provider_status", mcpProviderInput{})
	if err != nil {
		return "", true, err
	}
	if provider == nil || provider.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "people_inference_unavailable"}
	}
	policy, ok := provider.Output.(mcpserver.PeopleProviderStatus)
	if !ok || policy.Policy.Fingerprint != status.ProviderFingerprint || !policy.Policy.Checked || !policy.Policy.ConsentActive {
		return "", true, &mcpserver.OperationRefusalError{Code: "consent_required"}
	}
	data, err := json.Marshal(struct {
		Operation string                         `json:"operation"`
		Request   mcpSweepInput                  `json:"request"`
		Status    peoplesweep.StatusOutput       `json:"status"`
		Provider  mcpserver.PeopleProviderStatus `json:"configured_provider"`
		Effect    string                         `json:"effect"`
	}{name, input, status, policy, "Run the fixed owning command against eligible tracked people. Current configured provider policy and native consent, lease, per-person/run/day budgets are rechecked by the worker. Zero monetary caps mean no monetary cap; zero configured token prices do not mean the provider is free. May incur cost; inspect history for partial failures."}, json.Deterministic(true))
	return string(data), true, err
}
