package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	root.AddCommand(person)
	person.AddCommand(newPersonSweepCommand(defaultPersonSweepCommandDeps()), newPersonProviderCommand(defaultPersonProviderCommandDeps()))
	var path, positional []string
	switch name {
	case "get_people_sweep_status":
		path = []string{"sweep", "status"}
	case "list_people_sweep_history":
		path = []string{"provider", "history"}
		if input.Provider != "" {
			positional = []string{input.Provider}
		}
	case "run_people_sweep":
		path = []string{"sweep", "run"}
	default:
		return nil, nil, input, errors.New("unsupported operation")
	}
	cmd, _, err := person.Find(path)
	if err != nil {
		return nil, nil, input, fmt.Errorf("find sweep command: %w", err)
	}
	if input.PersonID != nil {
		if err := cmd.Flags().Set("person", strconv.FormatInt(*input.PersonID, 10)); err != nil {
			return nil, nil, input, fmt.Errorf("set person flag: %w", err)
		}
	}
	if input.Limit != nil {
		if err := cmd.Flags().Set("limit", strconv.Itoa(*input.Limit)); err != nil {
			return nil, nil, input, fmt.Errorf("set limit flag: %w", err)
		}
	}
	if input.Backstop {
		if err := cmd.Flags().Set("backstop", "true"); err != nil {
			return nil, nil, input, fmt.Errorf("set backstop flag: %w", err)
		}
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		return nil, nil, input, fmt.Errorf("set json flag: %w", err)
	}
	if err := cmd.ValidateArgs(positional); err != nil {
		return nil, nil, input, fmt.Errorf("validate sweep arguments: %w", err)
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
		return operationFailure("invalid_arguments", false), true, nil
	}
	argv, err := daemonCLIArgsFromCobra(cmd, positional)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil
	}
	body := generated.RunCLIBody{Args: argv}
	response, err := b.client.DoGeneratedStreamingRequestWithContext(ctx, http.MethodPost, "/api/v1/cli/run", &generated.RunCLIRequestOptions{Body: &body})
	if err != nil {
		return operationFailure("operation_failed", writes), true, nil
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
		if readErr != nil || len(data) > 65536 {
			return operationFailure("operation_response_failed", writes), true, nil
		}
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		switch failure.Error {
		case "operation_in_progress", "command_not_allowed", "unauthorized", "forbidden",
			"server_busy", "rate_limit_exceeded", "cli_store_unavailable", "invalid_request":
			return operationFailure(failure.Error, false), true, nil
		}
		return operationFailure("operation_refused", writes), true, nil
	}
	var stdout, stderr strings.Builder
	var failure *mcpserver.OperationResult
	err = daemonclient.DecodeCLIStream(io.LimitReader(response.Body, (1<<20)+1), "people sweep", func(event api.CLIRunEvent) (bool, error) {
		switch event.Type {
		case "stdout":
			stdout.WriteString(event.Data)
		case "stderr":
			stderr.WriteString(event.Data)
		case "error":
			code, uncertain := "operation_refused", writes
			diagnostic := ""
			for line := range strings.SplitSeq(stderr.String(), "\n") {
				if strings.HasPrefix(line, "Error: ") {
					diagnostic = strings.TrimSuffix(line, "\r")
				}
			}
			if event.Error == cliSubprocessExitSentinel {
				switch {
				case diagnostic == "Error: people sweep is disabled":
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
		return operationFailure("operation_response_failed", writes), true, nil
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
		return operationFailure("invalid_operation_response", writes), true, nil
	}
	return &mcpserver.OperationResult{Output: output}, true, nil
}
