package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/textutil"
)

// These interfaces are also understood by the grouped CLI error classifier.
// Keep comment refusals typed while that classifier is not yet on main.
type commentCLIError struct {
	command             *cobra.Command
	cause               error
	code, hint          string
	exit                int
	details             any
	requiredPermissions []string
	abuseWarning        bool
}

func (e *commentCLIError) Error() string        { return e.cause.Error() }
func (e *commentCLIError) Unwrap() error        { return e.cause }
func (e *commentCLIError) ExitCode() int        { return e.exit }
func (e *commentCLIError) Hint() string         { return e.hint }
func (e *commentCLIError) APIErrorCode() string { return e.code }

func newCommentCLIError(cmd *cobra.Command, code string, exit int, cause error) *commentCLIError {
	return &commentCLIError{command: cmd, cause: cause, code: code, exit: exit, hint: "run '" + cmd.CommandPath() + " --help' for flags and examples"}
}
func commentUsageError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	return newCommentCLIError(cmd, "usage", 2, err)
}
func isCommentCLICommand(cmd *cobra.Command) bool {
	for ; cmd != nil; cmd = cmd.Parent() {
		if cmd.Name() == "comment" {
			return true
		}
	}
	return false
}
func classifyCommentCLIError(cmd *cobra.Command, err error) *commentCLIError {
	if prior, ok := errors.AsType[*commentCLIError](err); ok {
		return prior
	}
	result := newCommentCLIError(cmd, "internal", 1, err)
	if failure, ok := errors.AsType[*meta.ActionError](err); ok {
		result.code, result.details = failure.Code, failure.Result
		result.requiredPermissions, result.abuseWarning = failure.RequiredPermissions, failure.AbuseWarning
		switch failure.Code {
		case "invalid_request", "invalid_body", "confirmation_mismatch", "not_a_comment", "unsupported_source":
			result.exit = 3
		case "message_not_found", "source_not_found":
			result.exit = 4
		case "action_not_enabled":
			result.exit = 5
			result.hint = "ask the archive owner to enable this action on the exact source profile"
		case "outcome_unknown", "sync_active", "stale_identity", "triage_conflict", "target_deleted", "private_reply_window_closed", "messaging_window_closed", "private_reply_not_allowed", "already_sent", "recipient_unavailable", "remote_accepted_local_failed":
			result.exit = 6
			result.hint = "inspect 'msgvault comment actions <message-id> --json' before retrying or resolving an uncertain outcome"
		case "cancelled_before_dispatch":
			result.exit = 130
		case "provider_rejected":
			result.exit = 3
		case "credential_unavailable", "token_invalid", "permission_missing", "app_access_insufficient", "thread_owned_by_other_app", "rate_limited", "provider_unavailable":
			result.exit = 7
			result.hint = "ask the archive owner to check this source's credentials, permissions and provider availability"
		}
	} else if _, ok := errors.AsType[*daemonclient.OperationInProgressError](err); ok {
		result.code, result.exit = "operation_in_progress", 6
		result.hint = "wait for the active daemon operation to finish, then retry this command"
	} else if failure, ok := errors.AsType[*daemonclient.APIError](err); ok {
		result.code = failure.Code
		switch failure.Status {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			result.exit = 3
		case http.StatusNotFound:
			result.exit = 4
		case http.StatusConflict:
			result.exit = 6
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			result.exit = 7
		}
	} else if _, ok := errors.AsType[net.Error](err); ok {
		result.code, result.exit = "unavailable", 7
		result.hint = "check 'msgvault daemon status' and the configured daemon connection"
	} else if errors.Is(err, context.Canceled) {
		result.code, result.exit = "interrupted", 130
	}
	return result
}

// ReportCLIError is called once, after exit logging, by the main command runner.
// The common grouped CLI renderer replaces this hook when it lands.
func (e *commentCLIError) ReportCLIError(rawArgs []string) {
	writer := e.command.ErrOrStderr()
	if commentJSONRequested(e.command, rawArgs) {
		record := struct {
			Code                string   `json:"code"`
			Message             string   `json:"message"`
			Hint                string   `json:"hint"`
			ExitCode            int      `json:"exit_code"`
			Details             any      `json:"details,omitempty"`
			RequiredPermissions []string `json:"required_permissions,omitempty"`
			AbuseWarning        bool     `json:"abuse_warning,omitempty"`
		}{e.code, e.Error(), e.hint, e.exit, e.details, e.requiredPermissions, e.abuseWarning}
		_ = json.MarshalWrite(writer, struct {
			Error any `json:"error"`
		}{record})
		_, _ = fmt.Fprintln(writer)
		return
	}
	_, _ = fmt.Fprintln(writer, "Error:", e.Error())
	if e.abuseWarning {
		_, _ = fmt.Fprintln(writer, "Warning: Meta flagged abusive traffic. Stop sending replies and review this source before sending again.")
	}
	if len(e.requiredPermissions) > 0 {
		_, _ = fmt.Fprintln(writer, "Required permissions:", textutil.SanitizeTerminal(strings.Join(e.requiredPermissions, ", ")))
	}
	_, _ = fmt.Fprintln(writer, "hint:", e.hint)
}

// Parse a permissive copy so a typo before --json does not hide the requested
// output mode. Known flags still consume their values, including a body that
// happens to be the literal text --json. The live command's values stay intact.
func commentJSONRequested(cmd *cobra.Command, args []string) bool {
	if len(args) == 0 {
		value, _ := cmd.Flags().GetBool("json")
		return value
	}
	scan := pflag.NewFlagSet("comment diagnostic", pflag.ContinueOnError)
	scan.SetOutput(io.Discard)
	scan.ParseErrorsAllowlist.UnknownFlags = true
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		scan.StringP(flag.Name, flag.Shorthand, flag.DefValue, "")
		scan.Lookup(flag.Name).NoOptDefVal = flag.NoOptDefVal
	})
	if scan.Lookup("json") == nil {
		scan.String("json", "false", "")
		scan.Lookup("json").NoOptDefVal = strconv.FormatBool(true)
	}
	_ = scan.Parse(args)
	raw, _ := scan.GetString("json")
	value, _ := strconv.ParseBool(raw)
	return value
}
