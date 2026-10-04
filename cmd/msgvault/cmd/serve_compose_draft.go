package cmd

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
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
	// ConversationID selects a local chat draft instead of an IMAP draft.
	ConversationID int64
	ReplyTo        int64
	// PersonID lists the person's archived addresses instead of creating a draft.
	PersonID int64
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
		case draftFromFlag, "account", "source-id", "subject", "body", "to", "cc", "bcc", "conversation", "reply-to", "person-id":
			if !hasValue {
				if len(rest) == 0 {
					return invalidDraftComposeArgs("--%s requires a value", name)
				}
				value, rest = rest[0], rest[1:]
			}
			switch name {
			case draftFromFlag:
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
			case "conversation", "reply-to", "person-id":
				id, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				if parseErr != nil || id <= 0 {
					return invalidDraftComposeArgs("--%s must be a positive integer", name)
				}
				target := &intent.ReplyTo
				switch name {
				case "conversation":
					target = &intent.ConversationID
				case "person-id":
					target = &intent.PersonID
				}
				if *target != 0 {
					return invalidDraftComposeArgs("--%s given more than once", name)
				}
				*target = id
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
	if intent.PersonID != 0 {
		if intent.ConversationID != 0 || intent.ReplyTo != 0 || fromSet || accountSet || sourceIDSet || subjectSet || bodySet ||
			len(intent.To)+len(intent.Cc)+len(intent.Bcc) > 0 {
			return invalidDraftComposeArgs("--person-id accepts only --json")
		}
		return intent, nil
	}
	if intent.ConversationID != 0 {
		if fromSet || accountSet || sourceIDSet || subjectSet || len(intent.To)+len(intent.Cc)+len(intent.Bcc) > 0 {
			return invalidDraftComposeArgs("--conversation accepts only --body, --reply-to, and --json")
		}
		return intent, nil
	}
	if intent.ReplyTo != 0 {
		return invalidDraftComposeArgs("--reply-to requires --conversation")
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
	if intent.PersonID != 0 {
		if req.Grant != nil {
			return draftReplyNotPermitted(errors.New("contact-directed drafts are owner-only"))
		}
		rows, err := a.personDraftAddresses(ctx, intent.PersonID)
		if err != nil {
			return err
		}
		return emitPersonDraftAddresses(emit, intent.JSON, intent.PersonID, rows)
	}
	if intent.ConversationID != 0 {
		return a.runCLIChatDraftCreate(ctx, intent, req.Grant, emit)
	}
	if source, err := sourceops.ResolveExactOne(a.store, sourceops.Selector{
		Account: intent.Account, SourceID: intent.SourceID, SourceIDSet: intent.SourceIDSet,
	}); err == nil && source.SourceType == "beeper" {
		return a.runBeeperDraftCreate(ctx, req.Grant, intent, source, emit)
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

// personDraftAddress is one archived identity of a person. Only email
// addresses are supported draft destinations.
type personDraftAddress struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	Supported bool   `json:"supported"`
}

type personDraftAddressesOutput struct {
	PersonID  int64                `json:"person_id"`
	Addresses []personDraftAddress `json:"addresses"`
}

// personDraftAddresses lists the archived participant identities currently
// bound to the person. Curated contact points and postal addresses are not archived
// participant identities, so they never appear here.
func (a *storeAPIAdapter) personDraftAddresses(
	ctx context.Context, personID int64,
) ([]personDraftAddress, error) {
	person, err := a.store.GetPersonContext(ctx, personID)
	if errors.Is(err, store.ErrPersonNotFound) {
		return nil, draftReplyError("invalid_args", fmt.Errorf("person %d not found", personID))
	}
	if err != nil {
		return nil, draftReplyError("draft_read_failed", fmt.Errorf("load person %d: %w", personID, err))
	}
	identity, err := a.store.GetParticipantIdentityContext(ctx, person.ParticipantIDs)
	if err != nil {
		return nil, draftReplyError("draft_read_failed", fmt.Errorf("load identities for person %d: %w", personID, err))
	}
	rows := make([]personDraftAddress, 0)
	supported := make(map[string]bool)
	unsupported := make(map[[2]string]bool)
	add := func(kind, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if kind == "email" {
			if address, key, err := parseStoredMailbox(value); err == nil {
				if !supported[key] {
					supported[key] = true
					// String keeps local-part quoting so the listed value parses back as --to.
					mailbox := address.String()
					rows = append(rows, personDraftAddress{Kind: kind, Value: mailbox[1 : len(mailbox)-1], Supported: true})
				}
				return
			}
		}
		// Only email compares case-insensitively; chat IDs such as Matrix IDs are case-sensitive.
		key := [2]string{kind, value}
		if kind == "email" {
			key[1] = strings.ToLower(value)
		}
		if !unsupported[key] {
			unsupported[key] = true
			rows = append(rows, personDraftAddress{Kind: kind, Value: value})
		}
	}
	for _, member := range identity.Members {
		add("email", member.Email)
		add("phone", member.Phone)
	}
	for _, identifier := range identity.Identifiers {
		add(identifier.Type, identifier.Value)
	}
	return rows, nil
}

func emitPersonDraftAddresses(
	emit func(api.CLIRunEvent) error, asJSON bool, personID int64, rows []personDraftAddress,
) error {
	if emit == nil {
		return nil
	}
	if asJSON {
		data, err := jsonv2.Marshal(personDraftAddressesOutput{PersonID: personID, Addresses: rows})
		if err != nil {
			return draftReplyError("output_failed", err)
		}
		return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(data) + "\n"})
	}
	if len(rows) == 0 {
		return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: fmt.Sprintf("person %d has no archived addresses\n", personID)})
	}
	var data strings.Builder
	for _, row := range rows {
		support := "unsupported"
		if row.Supported {
			support = "supported"
		}
		fmt.Fprintf(&data, "%s\t%s\t%s\n", textutil.SanitizeTerminal(row.Kind), textutil.SanitizeTerminal(row.Value), support)
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data.String()})
}
