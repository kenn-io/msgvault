package daemonclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpCLIWireLimit = 1 << 20

// MCPCLIResult retains bounded command output for the owning MCP adapter to
// validate against its structured result/receipt contract. Stderr is private
// until that adapter recognizes a documented receipt. Failed represents the
// daemon's error terminal event, not an invented subprocess exit status.
type MCPCLIResult struct {
	Stdout                    string
	Stderr                    string
	Failed                    bool
	ErrorCode                 string
	OperationMayHaveCompleted bool
}

// MCPCLIError has a fixed public code and a private underlying diagnostic.
type MCPCLIError struct {
	Code  string
	cause error
}

func (e *MCPCLIError) Error() string        { return e.Code }
func (e *MCPCLIError) APIErrorCode() string { return e.Code }
func (e *MCPCLIError) Unwrap() error        { return e.cause }

// RunMCPCLICommand executes a fixed command chosen by an MCP adapter. It does
// not accept an environment or retry requests. Cwd is reserved for an operator's
// same-host document manifest and never comes from tool arguments.
func (c *Client) RunMCPCLICommand(ctx context.Context, args []string, cwd string) (*MCPCLIResult, error) {
	body := generated.RunCLIBody{Args: args, Cwd: optionalString(cwd)}
	resp, err := c.DoGeneratedStreamingRequestWithContext(ctx, http.MethodPost,
		"/api/v1/cli/run", &generated.RunCLIRequestOptions{Body: &body})
	if err != nil {
		return &MCPCLIResult{Failed: true, ErrorCode: "cli_execution_failed", OperationMayHaveCompleted: true},
			&MCPCLIError{Code: "cli_execution_failed", cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		limited := io.LimitReader(resp.Body, mcpCLIWireLimit+1)
		data, readErr := io.ReadAll(limited)
		if readErr != nil {
			return nil, &MCPCLIError{Code: "cli_execution_failed", cause: readErr}
		}
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		return nil, &MCPCLIError{Code: safeMCPCLIErrorCode(failure.Error)}
	}
	return decodeMCPCLIStream(resp.Body)
}

func decodeMCPCLIStream(body io.Reader) (*MCPCLIResult, error) {
	result := &MCPCLIResult{}
	var stdout, stderr strings.Builder
	defer func() {
		result.Stdout, result.Stderr = stdout.String(), stderr.String()
	}()
	limited := &io.LimitedReader{R: body, N: mcpCLIWireLimit + 1}
	decoder := jsontext.NewDecoder(limited)
	terminal := false
	fail := func(code string, cause error) (*MCPCLIResult, error) {
		result.ErrorCode = code
		result.Failed = true
		result.OperationMayHaveCompleted = true
		return result, &MCPCLIError{Code: code, cause: cause}
	}
	for {
		var event cliStreamEvent
		err := json.UnmarshalDecode(decoder, &event, json.RejectUnknownMembers(true))
		// The raw wire limit applies before an escaped event is allocated or
		// a callback sees output. Check it before interpreting decoder errors.
		if limited.N == 0 {
			return fail("output_limit_exceeded", err)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || terminal {
			return fail("invalid_cli_stream", err)
		}
		switch event.Type {
		case "stdout", "stderr":
			if event.Error != "" {
				return fail("invalid_cli_stream", nil)
			}
			if event.Type == "stdout" {
				stdout.WriteString(event.Data)
			} else {
				stderr.WriteString(event.Data)
			}
		case "complete", "error":
			if event.Data != "" || (event.Type == "complete" && event.Error != "") {
				return fail("invalid_cli_stream", nil)
			}
			terminal = true
			if event.Type == "error" {
				result.Failed = true
				result.ErrorCode = safeMCPCLIErrorCode(event.Error)
			}
		default:
			return fail("invalid_cli_stream", nil)
		}
	}
	if !terminal {
		return fail("invalid_cli_stream", nil)
	}
	if result.Failed {
		return fail(result.ErrorCode, nil)
	}
	return result, nil
}

// Accept only known protocol codes; arbitrary provider/host error text never
// becomes public merely because it resembles an identifier.
func safeMCPCLIErrorCode(code string) string {
	switch code {
	case "invalid_args", "invalid_request", "command_not_allowed", "not_permitted",
		"unauthorized", "operation_in_progress", "draft_disabled", "invalid_mailbox",
		"invalid_from", "from_ambiguous", "invalid_parent", "invalid_source",
		"invalid_reply_metadata", "invalid_compose_metadata", "sync_active",
		"sync_lock_failed", "output_failed", "draft_not_found", "draft_read_failed",
		"draft_discarded", "revision_conflict", "remote_unknown", "accepted_local_failed",
		"cleanup_local_failed", "cancelled", "draft_conflict", "draft_pending",
		"revision_mismatch", "pending_operation", "provider_absent", "provider_refused",
		"insufficient_scope", "changed_externally", "not_supported", "local_persistence_failed",
		"claim_failed", "unknown_replacement", "invalid_draft", "invalid_state", "append_failed",
		"invalid_message", "uidplus_required", "append_rejected", "accepted_unidentified",
		"connection_failed", "select_failed", "uidvalidity_mismatch", "modseq_unusable",
		"fetch_failed", "not_draft", "absent", "not_found", "already_deleted", "store_failed",
		"store_conflict", "expunge_failed", "confirmation_failed", "survivor",
		"attachment_preflight_failed", "chat_not_found", "draft_exists", "invalid_destination", "invalid_forward_metadata", "local_store_failed", "provider_identity_mismatch", "provider_rejected", "provider_unavailable", "unsupported_source":
		return code
	default:
		return "cli_execution_failed"
	}
}
