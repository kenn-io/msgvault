package cmd

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/cligrammar"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string { return e.err.Error() }
func (e *hintedError) Unwrap() error { return e.err }
func (e *hintedError) Hint() string  { return e.hint }
func withHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &hintedError{err: err, hint: hint}
}

func validationErr(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	return &cliError{Kind: "validation", Code: "validation", Message: err.Error(), Hint: commandHelpHint(cmd), ExitCode: 3, Err: err}
}

func commandHelpHint(cmd *cobra.Command) string {
	path := "msgvault"
	if cmd != nil {
		path = "msgvault"
		if suffix := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), "msgvault")); suffix != "" {
			path += " " + strings.Join(cligrammar.Canonical(strings.Fields(suffix)), " ")
		}
	}
	return "run '" + path + " --help' for flags and examples"
}

// ExitCode and the diagnostic renderer use the same classification precedence.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return classifyCLIError(nil, err).ExitCode
}

func classifyCLIError(cmd *cobra.Command, err error) *cliError {
	result := &cliError{Kind: "internal", Code: "internal", Message: err.Error(), ExitCode: 1, Err: err}
	if explicit, ok := errors.AsType[*cliError](err); ok {
		*result = *explicit
		// Keep contextual wrappers and every errors.Join branch reachable.
		result.Err = err
		if result.Message == "" {
			result.Message = err.Error()
		}
	} else if errors.Is(err, ErrSyncPaused) {
		result.Kind = "sync_paused"
		result.Code = "sync_paused"
		result.ExitCode = ExitCodeSyncPaused
		result.Hint = "retry the same sync command later; inspect sources with 'msgvault account list'"
	} else if code, ok := explicitChildExitCode(err); ok {
		result.ExitCode = code
		result.Kind = kindForExit(code)
		result.Code = result.Kind
	} else {
		code := ""
		if coded, ok := errors.AsType[interface {
			error
			APIErrorCode() string
		}](err); ok {
			code = coded.APIErrorCode()
		}
		if exit, known := apiExitCodes[code]; known {
			result.ExitCode = exit
			result.Kind = kindForExit(exit)
			result.Code = code
		} else if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok {
			result.ExitCode = exitForHTTPStatus(apiErr.Status)
			result.Kind = kindForExit(result.ExitCode)
			result.Code = apiErr.Code
		} else if code != "" {
			result.Code = code
		} else if errors.Is(err, store.ErrMessageNotFound) {
			result.Kind = "not_found"
			result.Code = "message_not_found"
			result.ExitCode = 4
			result.Hint = "find message IDs with 'msgvault search <query> --json'"
		} else if kind := opserr.KindOf(err); kind == opserr.KindInvalid || kind == opserr.KindNotFound {
			result.ExitCode = 3
			if kind == opserr.KindNotFound {
				result.ExitCode = 4
			}
			result.Kind = kindForExit(result.ExitCode)
			result.Code = result.Kind
		} else if _, ok := errors.AsType[net.Error](err); ok {
			result.Kind = "unavailable"
			result.Code = "unavailable"
			result.ExitCode = 7
			result.Hint = "check 'msgvault daemon status'; start a local daemon with 'msgvault daemon start', or check [remote].url in config.toml"
		}
		if result.Hint == "" {
			result.Hint = hintForAPICode(code)
		}
	}
	if hint := recoveryHint(err); hint != "" {
		result.Hint = hint
	}
	if result.Hint == "" {
		result.Hint = readAPIRecoveryHint(cmd, err)
	}
	if result.Hint == "" {
		result.Hint = commandHelpHint(cmd)
	}
	if result.ExitCode <= 0 {
		result.ExitCode = 1
	}
	if result.Kind == "" {
		result.Kind = kindForExit(result.ExitCode)
	}
	if result.Code == "" {
		result.Code = result.Kind
	}
	return result
}

// A missing child status is not an explicit exit 1: HTTP metadata may still
// classify a daemon refusal. Old subprocess sentinels otherwise fall back to 1.
func explicitChildExitCode(err error) (int, bool) {
	if stream, ok := errors.AsType[*daemonclient.CLIRunError](err); ok && !stream.HasExitCode() {
		return 0, false
	}
	if exit, ok := errors.AsType[interface {
		error
		ExitCode() int
	}](err); ok {
		code := exit.ExitCode()
		return code, code > 0 && code <= 255
	}
	return 0, false
}

// Walk in outer-to-inner order so an explicit CLI hint wins over an API hint,
// while withHint can deliberately replace the default command-help guidance.
func recoveryHint(err error) string {
	if err == nil {
		return ""
	}
	if hinted, ok := errors.AsType[interface {
		error
		Hint() string
	}](err); ok && hinted.Hint() != "" {
		return hinted.Hint()
	}
	if cli, ok := errors.AsType[*cliError](err); ok && cli.Hint != "" {
		return cli.Hint
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if hint := recoveryHint(cause); hint != "" {
				return hint
			}
		}
		return ""
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return recoveryHint(single.Unwrap())
	}
	return ""
}

var apiExitCodes = map[string]int{
	"invalid_args": 2, "from_ambiguous": 2,
	"invalid_from": 3, "invalid_source": 3, "invalid_parent": 3, "invalid_mailbox": 3, "index_scope_mismatch": 3,
	"message_not_found": 4, "draft_not_found": 4, "source_not_found": 4, "collection_not_found": 4,
	"vector_not_enabled": 5, "draft_disabled": 5,
	"index_stale": 6, "index_building": 6, "revision_mismatch": 6, "sync_active": 6, "agent_read_busy": 6,
	"not_permitted": 7, "permission_denied": 7, "embedding_timeout": 7, "engine_unavailable": 7, "source_scope_unavailable": 7,
}

func exitForHTTPStatus(status int) int {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return 3
	case http.StatusNotFound:
		return 4
	case http.StatusConflict:
		return 6
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return 7
	}
	return 1
}

// usageErr marks invocation mistakes at their Cobra/command validation boundary.
func usageErr(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	if cmd != nil {
		cmd.SilenceUsage = true
	}
	if _, ok := errors.AsType[*cliError](err); ok {
		return err
	}
	hint := recoveryHint(err)
	if hint == "" {
		hint = commandHelpHint(cmd)
	}
	return &cliError{Kind: "usage", Code: "usage", Message: err.Error(), Hint: hint, ExitCode: 2, Err: err}
}

func hintForAPICode(code string) string {
	switch code {
	case "message_not_found", "raw_message_not_found":
		return "find message IDs with 'msgvault search <query> --json'; read available content with 'msgvault show <id> --json'"
	case "vector_not_enabled":
		return "run 'msgvault setup providers' and check 'msgvault embeddings status'"
	case "index_stale", "index_building", "index_scope_mismatch":
		return "check 'msgvault embeddings status'; build the search index with 'msgvault embeddings build'"
	case "draft_disabled", "invalid_mailbox":
		return "ask the archive owner to check the source's draft grant in config.toml, then run 'msgvault daemon restart'"
	case "revision_mismatch":
		return "reload the current draft and its revision with 'msgvault draft get <draft-id> --json' before retrying"
	case "invalid_from", "from_ambiguous":
		return "list confirmed senders with 'msgvault identity list --json' and select one with --from"
	case "not_permitted":
		return "ask the archive owner to check the source and sender permissions with 'msgvault agent-token list'"
	case "remote_delete_disabled":
		return "ask the archive owner to enable remote deletion; agent grants cannot enable it"
	}
	return ""
}

func cliFlagError(cmd *cobra.Command, err error) error {
	if strings.HasPrefix(err.Error(), "invalid argument ") || strings.HasPrefix(err.Error(), "invalid boolean value ") {
		return validationErr(cmd, err)
	}
	return usageErr(cmd, err)
}

func kindForExit(code int) string {
	switch code {
	case 2:
		return "usage"
	case 3:
		return "validation"
	case 4:
		return "not_found"
	case 5:
		return "not_initialized"
	case 6:
		return "conflict"
	case 7:
		return "unavailable"
	case 75:
		return "sync_paused"
	case 130:
		return "interrupted"
	}
	return "internal"
}
