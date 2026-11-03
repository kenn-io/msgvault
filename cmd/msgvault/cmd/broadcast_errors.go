package cmd

import (
	"database/sql"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

// These interfaces are consumed by the shared CLI diagnostic renderer and the
// daemon's exit metadata transport. Keep rendering in the shared CLI layer.
type broadcastCLIError struct {
	err  error
	exit int
	hint string
}

func (e *broadcastCLIError) Error() string { return e.err.Error() }
func (e *broadcastCLIError) Unwrap() error { return e.err }
func (e *broadcastCLIError) ExitCode() int { return e.exit }
func (e *broadcastCLIError) Hint() string  { return e.hint }

const broadcastHelpHint = "run 'msgvault broadcast --help' for commands and flags"

func broadcastArgumentError(err error) error {
	exit := 2
	if opserr.KindOf(err) == opserr.KindInvalid {
		exit = 3
	}
	return &broadcastCLIError{err: err, exit: exit, hint: broadcastHelpHint}
}

func classifyBroadcastStreamError(err error) error {
	if classified, ok := errors.AsType[interface {
		error
		HasExitCode() bool
	}](err); ok && classified.HasExitCode() {
		return err
	}
	// Preserve transport errors so the shared classifier can recognize network
	// and HTTP failures. Only daemon operation errors use broadcast categories.
	if _, ok := errors.AsType[*daemonclient.CLIRunError](err); ok {
		return classifyBroadcastError(err)
	}
	return err
}

func classifyBroadcastError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*broadcastCLIError](err); ok {
		return err
	}
	exit, hint := 1, broadcastHelpHint
	code, _, _ := strings.Cut(err.Error(), ":")
	switch {
	case opserr.KindOf(err) == opserr.KindInvalid:
		exit = 3
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, store.ErrSourceNotFound), err.Error() == sql.ErrNoRows.Error():
		exit = 4
	case errors.Is(err, store.ErrBroadcastFrozen), err.Error() == store.ErrBroadcastFrozen.Error():
		exit = 6
	default:
		switch code {
		case "invalid_args":
			exit = 2
		case "budget_over_cap", "campaign_foreign", "invalid_limit", "too_many_recipients":
			exit = 3
		case "broadcast_profile_not_found", "subscription_not_found":
			exit = 4
		case "broadcast_source_not_initialized", "configuration is unavailable":
			exit = 5
			hint = "configure the profile, then run 'msgvault source facebook-messages add <identifier>'"
		case "plan_changed", "campaign_unknown", "campaign_warming", "campaign_expired", "broadcast_cancelled", "broadcast_source_inactive", "conversation_gap", "frequency_cap", "pacing", "spend_limit":
			exit = 6
			hint = "inspect 'msgvault broadcast show <broadcast-id> --json' before planning or retrying a send"
		case "broadcast_sends_disabled", "broadcast_profile_disabled", "broadcast_credentials_missing", "not_permitted", "permission_missing", "token_invalid", "rate_limited", "recipient_unavailable", "campaign_rejected", "send_rejected", "provider_unsubscribe_failed", "marketing subscription list failed":
			exit = 7
			hint = "check the profile, daemon credentials and provider access; use 'msgvault broadcast show <broadcast-id> --json' before retrying a send"
		}
	}
	return &broadcastCLIError{err: err, exit: exit, hint: hint}
}
