package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	imaplib "go.kenn.io/msgvault/internal/imap"
)

type draftComposeIntent struct {
	From        string
	Account     string
	SourceID    int64
	SourceIDSet bool
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Body        string
	JSON        bool
}

func invalidDraftComposeArgs(format string, args ...any) (draftComposeIntent, error) {
	return draftComposeIntent{}, draftReplyError("invalid_args", fmt.Errorf(format, args...))
}

func parseDraftComposeArgs(args []string) (draftComposeIntent, error) {
	if !api.IsCLIRunDraftCompose(args) {
		return invalidDraftComposeArgs("expected %s as the first argument", api.CLIRunDraftComposeCommand)
	}
	var intent draftComposeIntent
	var fromSet, accountSet, sourceIDSet, subjectSet, bodySet, jsonSet bool
	rest := args[1:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		nameValue, ok := strings.CutPrefix(arg, "--")
		if !ok {
			return invalidDraftComposeArgs("draft-compose accepts flags only")
		}
		name, value, hasValue := strings.Cut(nameValue, "=")
		switch name {
		case "from", "account", "source-id", "subject", "body", "to", "cc", "bcc":
			if !hasValue {
				if len(rest) == 0 {
					return invalidDraftComposeArgs("--%s requires a value", name)
				}
				value, rest = rest[0], rest[1:]
			}
			switch name {
			case "from":
				if fromSet {
					return invalidDraftComposeArgs("--from given more than once")
				}
				if strings.TrimSpace(value) == "" {
					return invalidDraftComposeArgs("--from must not be empty")
				}
				intent.From, fromSet = value, true
			case "account":
				if accountSet || strings.TrimSpace(value) == "" {
					return invalidDraftComposeArgs("--account must be given once with a value")
				}
				intent.Account, accountSet = strings.TrimSpace(value), true
			case "source-id":
				if sourceIDSet {
					return invalidDraftComposeArgs("--source-id given more than once")
				}
				id, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				if parseErr != nil || id <= 0 {
					return invalidDraftComposeArgs("source ID must be a positive integer")
				}
				intent.SourceID, intent.SourceIDSet, sourceIDSet = id, true, true
			case "subject":
				if subjectSet {
					return invalidDraftComposeArgs("--subject given more than once")
				}
				intent.Subject, subjectSet = value, true
			case "body":
				if bodySet {
					return invalidDraftComposeArgs("--body given more than once")
				}
				intent.Body, bodySet = value, true
			case "to":
				if strings.TrimSpace(value) == "" {
					return invalidDraftComposeArgs("--to must not be empty")
				}
				intent.To = append(intent.To, value)
			case "cc":
				if strings.TrimSpace(value) == "" {
					return invalidDraftComposeArgs("--cc must not be empty")
				}
				intent.Cc = append(intent.Cc, value)
			case "bcc":
				if strings.TrimSpace(value) == "" {
					return invalidDraftComposeArgs("--bcc must not be empty")
				}
				intent.Bcc = append(intent.Bcc, value)
			}
		case "json":
			if jsonSet || (hasValue && value != "true") {
				return invalidDraftComposeArgs("--json accepts one flag without a value")
			}
			intent.JSON, jsonSet = true, true
		case "log-level", "verbose", "log-sql", "log-sql-slow-ms":
			if !hasValue && name != "verbose" && name != "log-sql" && len(rest) > 0 {
				rest = rest[1:]
			}
		default:
			return invalidDraftComposeArgs("unknown flag --%s", name)
		}
	}
	if !accountSet && !sourceIDSet {
		return invalidDraftComposeArgs("--account or --source-id is required")
	}
	if accountSet && sourceIDSet {
		return invalidDraftComposeArgs("--account and --source-id are mutually exclusive")
	}
	if len(intent.To)+len(intent.Cc)+len(intent.Bcc) == 0 {
		return invalidDraftComposeArgs("at least one of --to, --cc, or --bcc is required")
	}
	return intent, nil
}

func (a *storeAPIAdapter) runCLIComposeDraft(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft-compose accepts no environment or working directory"))
	}
	intent, err := parseDraftComposeArgs(req.Args)
	if err != nil {
		return err
	}
	target, from, _, err := a.resolveDraftTarget(
		ctx, nil, intent.Account, intent.SourceID, intent.SourceIDSet, intent.From, req.Grant,
	)
	if err != nil {
		return err
	}
	draft, err := imaplib.BuildCompose(imaplib.ComposeOptions{
		From: from, To: intent.To, Cc: intent.Cc, Bcc: intent.Bcc,
		Subject: intent.Subject, Body: intent.Body,
	}, time.Now(), "")
	if err != nil {
		return draftReplyError("invalid_compose_metadata", err)
	}
	return a.createDraft(ctx, target, draft, intent.JSON, emit)
}
