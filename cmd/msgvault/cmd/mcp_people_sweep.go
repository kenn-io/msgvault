package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var sweepUntrackedDiagnostic = regexp.MustCompile(`^Error: person [0-9]+ is not tracked$`)

type mcpSweepArgs struct {
	PersonID *int64 `json:"person_id,omitempty"`
	Limit    *int   `json:"limit,omitempty"`
	Backstop bool   `json:"backstop,omitempty"`
	Provider string `json:"provider,omitempty"`
}

func sweepCommand(name string, args map[string]any) (*cobra.Command, []string, mcpSweepArgs, error) {
	input, err := decodeMCPOperationArguments[mcpSweepArgs](args)
	if err != nil {
		return nil, nil, input, err
	}
	if _, provided := args["provider"]; provided && input.Provider == "" {
		return nil, nil, input, errors.New("invalid provider")
	}
	var cmd *cobra.Command
	var positional []string
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	root.AddCommand(person)
	switch name {
	case "get_people_sweep_status":
		cmd = newPersonSweepStatusCommand(defaultPersonSweepCommandDeps())
		sweep := &cobra.Command{Use: "sweep"}
		person.AddCommand(sweep)
		sweep.AddCommand(cmd)
	case "list_people_sweep_history":
		if input.Provider != "" {
			provider := &cobra.Command{Use: "provider"}
			person.AddCommand(provider)
			cmd = newPersonProviderHistoryCommand(defaultPersonProviderCommandDeps())
			provider.AddCommand(cmd)
			positional = []string{input.Provider}
		} else {
			sweep := &cobra.Command{Use: "sweep"}
			person.AddCommand(sweep)
			cmd = newPersonSweepHistoryCommand(defaultPersonSweepCommandDeps())
			sweep.AddCommand(cmd)
		}
	case "run_people_sweep":
		sweep := &cobra.Command{Use: "sweep"}
		person.AddCommand(sweep)
		cmd = newPersonSweepRunCommand(defaultPersonSweepCommandDeps())
		sweep.AddCommand(cmd)
	default:
		return nil, nil, input, errors.New("unsupported operation")
	}
	if input.PersonID != nil {
		if err := cmd.Flags().Set("person", strconv.FormatInt(*input.PersonID, 10)); err != nil {
			return nil, nil, input, err
		}
	}
	if input.Limit != nil {
		if err := cmd.Flags().Set("limit", strconv.Itoa(*input.Limit)); err != nil {
			return nil, nil, input, err
		}
	}
	if input.Backstop {
		if err := cmd.Flags().Set("backstop", "true"); err != nil {
			return nil, nil, input, err
		}
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		return nil, nil, input, err
	}
	if err := cmd.ValidateArgs(positional); err != nil {
		return nil, nil, input, err
	}
	return cmd, positional, input, nil
}

func (b *daemonMCPOperations) peopleSweepDisclosure(name string, args map[string]any) (string, bool, error) {
	if name != "run_people_sweep" {
		return "", false, nil
	}
	_, _, input, err := sweepCommand(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	disclosure := struct {
		Operation string `json:"operation"`
		PersonID  *int64 `json:"person_id"`
		Limit     *int   `json:"limit"`
		Backstop  bool   `json:"backstop"`
		Scope     string `json:"scope"`
	}{name, input.PersonID, input.Limit, input.Backstop, "Run people maintenance with the configured provider and existing consent; omitted limit uses the daemon default."}
	data, err := json.Marshal(disclosure, json.Deterministic(true))
	return string(data), true, err
}

func (b *daemonMCPOperations) executePeopleSweep(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name != "get_people_sweep_status" && name != "list_people_sweep_history" && name != "run_people_sweep" {
		return nil, false, nil
	}
	writes := name == "run_people_sweep"
	cmd, positional, _, err := sweepCommand(name, args)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid input is a structured refusal.
	}
	argv, err := daemonCLIArgsFromCobra(cmd, positional)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid input is a structured refusal.
	}
	body := generated.RunCLIBody{Args: argv}
	response, err := b.client.DoGeneratedStreamingRequestWithContext(ctx, http.MethodPost, "/api/v1/cli/run", &generated.RunCLIRequestOptions{Body: &body})
	if err != nil {
		return operationFailure("operation_failed", writes), true, nil //nolint:nilerr // Transport failure becomes a safe structured refusal.
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
		if readErr != nil || len(data) > 65536 {
			return operationFailure("operation_response_failed", writes), true, nil //nolint:nilerr // A failed or oversized error body is a safe refusal.
		}
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		switch failure.Error {
		case "operation_in_progress", "command_not_allowed", "unauthorized", "forbidden":
			return operationFailure(failure.Error, false), true, nil
		}
		return operationFailure("operation_refused", writes), true, nil
	}
	var stdout, stderr strings.Builder
	var failure *mcpserver.OperationResult
	err = daemonclient.DecodeCLIStream(io.LimitReader(response.Body, (1<<20)+1), "people sweep", func(event api.CLIRunEvent) (bool, error) {
		switch event.Type {
		case "stdout":
			if stdout.Len()+len(event.Data) > 1<<20 {
				failure = operationFailure("operation_response_limit_exceeded", writes)
				return false, errors.New("response limit")
			}
			stdout.WriteString(event.Data)
		case "stderr":
			if stderr.Len()+len(event.Data) > 65536 {
				failure = operationFailure("operation_response_limit_exceeded", writes)
				return false, errors.New("response limit")
			}
			stderr.WriteString(event.Data)
		case "error":
			code, uncertain := "operation_refused", writes
			diagnostic := strings.TrimSpace(stderr.String())
			if event.Error == cliSubprocessExitSentinel {
				switch {
				case diagnostic == "Error: people sweep is disabled" || diagnostic == "people sweep is disabled":
					code, uncertain = "people_sweep_disabled", false
				case writes && strings.HasPrefix(diagnostic, "Error: --limit must be between 1 and "):
					code, uncertain = "invalid_limit", false
				case writes && sweepUntrackedDiagnostic.MatchString(diagnostic):
					code, uncertain = "person_not_tracked", false
				}
			}
			failure = operationFailure(code, uncertain)
			return false, errors.New("CLI refused")
		case "complete":
			return true, nil
		}
		return false, nil
	})
	if failure != nil {
		return failure, true, nil
	}
	if err != nil {
		return operationFailure("operation_response_failed", writes), true, nil //nolint:nilerr // Stream failure becomes a safe structured refusal.
	}
	var output any
	switch name {
	case "get_people_sweep_status":
		output = &mcpserver.SweepStatus{}
	case "list_people_sweep_history":
		output = &mcpserver.SweepHistory{}
	default:
		output = &mcpserver.SweepRun{}
	}
	if err := json.Unmarshal(bytes.TrimSpace([]byte(stdout.String())), output); err != nil {
		return operationFailure("invalid_operation_response", writes), true, nil //nolint:nilerr // Invalid output becomes a safe structured refusal.
	}
	return &mcpserver.OperationResult{Output: output}, true, nil
}
