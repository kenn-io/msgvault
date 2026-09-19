package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
)

var (
	errDraftReplyBodyRequired = errors.New("--body is required")
)

const (
	draftReplyStatusCreated     = "created"
	draftReplyStatusLocalFailed = "remote_accepted_local_failed"
)

type draftReplyIntent struct {
	MessageID   int64
	From        string
	Body        string
	JSON        bool
	ReplyAll    bool
	Account     string
	SourceID    int64
	SourceIDSet bool
}

// draftReplyTarget is the archived parent message and the granted source
// mailbox that will hold the reply.
type draftReplyTarget struct {
	parent       *store.APIMessage
	parentSource *store.Source
	source       *store.Source
	mailbox      string
	raw          []byte
}

type draftReplyOutput struct {
	Status          string `json:"status"`
	DraftID         string `json:"draft_id,omitempty"`
	Revision        int64  `json:"revision,omitzero"`
	MessageID       int64  `json:"message_id,omitzero"`
	OperationRef    string `json:"operation_ref"`
	RFC822MessageID string `json:"rfc822_message_id"`
	SourceID        int64  `json:"source_id"`
	Mailbox         string `json:"mailbox"`
	UID             uint32 `json:"uid"`
	UIDValidity     uint32 `json:"uidvalidity"`
}

// draftReplyError pairs the fixed code a client sees with the cause the
// daemon logs. Causes must never include flag values or message content.
func draftReplyError(code string, cause error) error {
	return &api.CLIRunCodedError{Code: code, Err: cause}
}

func draftReplyNotPermitted(cause error) error {
	return draftReplyError("not_permitted", cause)
}

func invalidDraftReplyArgs(format string, args ...any) (draftReplyIntent, error) {
	return draftReplyIntent{}, draftReplyError("invalid_args", fmt.Errorf(format, args...))
}

func parseDraftReplyArgs(args []string) (draftReplyIntent, error) {
	if !api.IsCLIRunDraftReply(args) {
		return invalidDraftReplyArgs("expected %s as the first argument", api.CLIRunDraftReplyCommand)
	}
	var intent draftReplyIntent
	var positional string
	var fromSet, bodySet, jsonSet, allSet, accountSet, sourceIDSet bool
	rest := args[1:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		nameValue, ok := strings.CutPrefix(arg, "--")
		if !ok {
			if positional != "" {
				return invalidDraftReplyArgs("expected exactly one message ID")
			}
			positional = arg
			continue
		}
		name, value, hasValue := strings.Cut(nameValue, "=")
		switch name {
		case "from", "body", "account", "source-id":
			if !hasValue {
				if len(rest) == 0 {
					return invalidDraftReplyArgs("--%s requires a value", name)
				}
				value, rest = rest[0], rest[1:]
			}
			if (name == "from" && fromSet) || (name == "body" && bodySet) ||
				(name == "account" && accountSet) || (name == "source-id" && sourceIDSet) {
				return invalidDraftReplyArgs("--%s given more than once", name)
			}
			switch name {
			case "from":
				intent.From, fromSet = value, true
			case "body":
				intent.Body, bodySet = value, true
			case "account":
				if strings.TrimSpace(value) == "" {
					return invalidDraftReplyArgs("--account must not be empty")
				}
				intent.Account, accountSet = strings.TrimSpace(value), true
			case "source-id":
				id, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				if parseErr != nil || id <= 0 {
					return invalidDraftReplyArgs("source ID must be a positive integer")
				}
				intent.SourceID, intent.SourceIDSet, sourceIDSet = id, true, true
			}
		case "all":
			if allSet || (hasValue && value != "true") {
				return invalidDraftReplyArgs("--all accepts one flag without a value")
			}
			intent.ReplyAll, allSet = true, true
		case "json":
			if jsonSet {
				return invalidDraftReplyArgs("--json given more than once")
			}
			if hasValue && value != "true" {
				return invalidDraftReplyArgs("--json accepts no value")
			}
			intent.JSON, jsonSet = true, true
		case "log-level", "verbose", "log-sql", "log-sql-slow-ms":
			// The client forwards root logging flags to every daemon command.
			// This route runs in-process on the daemon's logger, so the flag
			// has nothing to configure. Consume its value and move on.
			if !hasValue && name != "verbose" && name != "log-sql" && len(rest) > 0 {
				rest = rest[1:]
			}
		default:
			return invalidDraftReplyArgs("unknown flag --%s", name)
		}
	}
	if fromSet && intent.From == "" {
		return invalidDraftReplyArgs("--from must not be empty")
	}
	if !bodySet {
		return invalidDraftReplyArgs("--body is required")
	}
	id, err := strconv.ParseInt(positional, 10, 64)
	if err != nil || id <= 0 {
		return invalidDraftReplyArgs("message ID must be a positive integer")
	}
	intent.MessageID = id
	if intent.SourceIDSet && intent.Account != "" {
		return invalidDraftReplyArgs("--account and --source-id are mutually exclusive")
	}
	return intent, nil
}

func marshalDraftReplyOutput(output draftReplyOutput) []byte {
	data, err := json.Marshal(output, json.Deterministic(true))
	if err != nil {
		return []byte("{\"status\":\"output_encoding_failed\"}")
	}
	return data
}

func authorizeIMAPDraft(policy []config.IMAPDraftSource, sourceID int64, sourceType string) (string, error) {
	if sourceType != "imap" {
		return "", draftReplyError("draft_disabled", fmt.Errorf("source %d is a %q source, not imap", sourceID, sourceType))
	}
	for _, grant := range policy {
		if grant.SourceID == sourceID && grant.Enabled {
			if err := imaplib.ValidateDraftMailbox(grant.Mailbox); err != nil {
				return "", draftReplyError("invalid_mailbox", fmt.Errorf("[[imap.drafts]] grant for source %d: %w", sourceID, err))
			}
			return grant.Mailbox, nil
		}
	}
	return "", draftReplyError("draft_disabled", fmt.Errorf("source %d has no enabled [[imap.drafts]] grant", sourceID))
}

func draftSourceRef(source *store.Source) agentgrant.SourceRef {
	return agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}
}

func parseDraftSender(value string) (*mail.Address, string, error) {
	addresses, err := mail.ParseAddressList(strings.TrimSpace(value))
	if err != nil || len(addresses) != 1 || addresses[0] == nil || addresses[0].Address == "" {
		return nil, "", errors.New("expected exactly one mailbox identity")
	}
	address := addresses[0]
	if !strings.Contains(address.Address, "@") || strings.ContainsAny(address.Address, "\r\n") {
		return nil, "", errors.New("mailbox identity is malformed")
	}
	return address, store.NormalizeIdentifierForCompare(address.Address), nil
}

func confirmedDraftIdentities(identities []store.AccountIdentity) (map[string]string, []string) {
	selected := make(map[string]string, len(identities))
	all := make([]string, 0, len(identities))
	for _, identity := range identities {
		if identity.ConfirmedAt.IsZero() {
			continue
		}
		address, key, err := parseDraftSender(identity.Address)
		if err != nil {
			continue
		}
		if _, seen := selected[key]; seen {
			continue
		}
		selected[key] = address.String()
		all = append(all, address.Address)
	}
	return selected, all
}

func (a *storeAPIAdapter) selectDraftSender(
	identities []store.AccountIdentity,
	requested string,
	grant *agentgrant.Grant,
	source *store.Source,
) (string, []string, error) {
	eligible, selfAddresses := confirmedDraftIdentities(identities)
	ref := draftSourceRef(source)
	if requested != "" {
		address, key, err := parseDraftSender(requested)
		if err != nil {
			return "", nil, draftReplyError("invalid_from", err)
		}
		if _, ok := eligible[key]; !ok {
			return "", nil, draftReplyError("invalid_from", errors.New("--from is not a confirmed identity on the selected source"))
		}
		if grant != nil && !grant.AllowsSender(agentgrant.PermissionDraftCreate, ref, key) {
			return "", nil, draftReplyNotPermitted(errors.New("selected sender is not in the grant"))
		}
		return address.String(), selfAddresses, nil
	}

	candidates := make([]string, 0, len(eligible))
	for key, value := range eligible {
		if grant != nil && !grant.AllowsSender(agentgrant.PermissionDraftCreate, ref, key) {
			continue
		}
		candidates = append(candidates, value)
	}
	if len(candidates) == 0 {
		if grant != nil {
			return "", nil, draftReplyNotPermitted(errors.New("the grant has no eligible sender identity on the selected source"))
		}
		return "", nil, draftReplyError("invalid_from", errors.New("the selected source has no confirmed mailbox identity"))
	}
	if len(candidates) > 1 {
		return "", nil, draftReplyError("from_ambiguous", errors.New("--from is required when the selected source has multiple eligible identities"))
	}
	return candidates[0], selfAddresses, nil
}

// resolveDraftTarget performs source, grant, sender, policy, and provider
// configuration checks before it reads an archived parent or opens IMAP.
func (a *storeAPIAdapter) resolveDraftTarget(
	ctx context.Context,
	parentID *int64,
	account string,
	sourceID int64,
	sourceIDSet bool,
	requestedFrom string,
	grant *agentgrant.Grant,
) (draftReplyTarget, string, []string, error) {
	var parentSource *store.Source
	if parentID != nil {
		var err error
		parentSource, err = a.store.GetMessageSourceContext(ctx, *parentID)
		if err != nil {
			if grant != nil {
				return draftReplyTarget{}, "", nil, draftReplyNotPermitted(errors.New("parent source is not available"))
			}
			return draftReplyTarget{}, "", nil, draftReplyError("invalid_parent", fmt.Errorf("load parent source: %w", err))
		}
		if err := authorizeDelegatedDraftSource(grant, parentSource); err != nil {
			return draftReplyTarget{}, "", nil, err
		}
	}

	var source *store.Source
	var err error
	if sourceIDSet || strings.TrimSpace(account) != "" {
		source, err = sourceops.ResolveExactOne(a.store, sourceops.Selector{
			Account: account, SourceID: sourceID, SourceIDSet: sourceIDSet,
		})
		if err != nil {
			if grant != nil {
				return draftReplyTarget{}, "", nil, draftReplyNotPermitted(errors.New("destination source is not available"))
			}
			return draftReplyTarget{}, "", nil, draftReplyError("invalid_source", fmt.Errorf("resolve destination source: %w", err))
		}
	} else if parentSource != nil && parentSource.SourceType == "imap" {
		source = parentSource
	} else if parentSource != nil {
		return draftReplyTarget{}, "", nil, draftReplyError("invalid_source", errors.New("an offline parent requires --account or --source-id for a live IMAP destination"))
	} else {
		return draftReplyTarget{}, "", nil, draftReplyError("invalid_source", errors.New("--account or --source-id is required"))
	}

	if err := authorizeDelegatedDraftSource(grant, source); err != nil {
		return draftReplyTarget{}, "", nil, err
	}
	identities, err := a.store.ListAccountIdentitiesContext(ctx, source.ID)
	if err != nil {
		return draftReplyTarget{}, "", nil, draftReplyError("invalid_from", fmt.Errorf("list identities for source %d: %w", source.ID, err))
	}
	from, selfAddresses, err := a.selectDraftSender(identities, requestedFrom, grant, source)
	if err != nil {
		return draftReplyTarget{}, "", nil, err
	}
	mailbox, err := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType)
	if err != nil {
		return draftReplyTarget{}, "", nil, err
	}
	if !source.SyncConfig.Valid {
		return draftReplyTarget{}, "", nil, draftReplyError("invalid_source", fmt.Errorf("source %d has no sync config", source.ID))
	}
	imapConfig, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
	if err != nil {
		return draftReplyTarget{}, "", nil, draftReplyError("invalid_source", fmt.Errorf("source %d sync config: %w", source.ID, err))
	}
	if imapConfig.Identifier() != source.Identifier {
		return draftReplyTarget{}, "", nil, draftReplyError("invalid_source", fmt.Errorf("source %d sync config identifier does not match the source", source.ID))
	}

	target := draftReplyTarget{parentSource: parentSource, source: source, mailbox: mailbox}
	if parentID != nil {
		parent, err := a.store.GetMessageContext(ctx, *parentID)
		if err != nil {
			return draftReplyTarget{}, "", nil, draftReplyError("invalid_parent", fmt.Errorf("load message %d: %w", *parentID, err))
		}
		if !store.IsEmailMessageType(parent.MessageType) {
			return draftReplyTarget{}, "", nil, draftReplyError("invalid_parent", errors.New("parent message is not an email"))
		}
		raw, err := a.store.GetMessageRawContext(ctx, parent.ID)
		if err != nil {
			return draftReplyTarget{}, "", nil, draftReplyError("invalid_parent", fmt.Errorf("load raw MIME for message %d: %w", parent.ID, err))
		}
		target.parent, target.raw = parent, raw
	}
	return target, from, selfAddresses, nil
}

func (a *storeAPIAdapter) runCLIReplyDraft(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft-reply accepts no environment or working directory"))
	}
	intent, err := parseDraftReplyArgs(req.Args)
	if err != nil {
		return err
	}
	target, from, selfAddresses, err := a.resolveDraftTarget(
		ctx, &intent.MessageID, intent.Account, intent.SourceID, intent.SourceIDSet,
		intent.From, req.Grant,
	)
	if err != nil {
		return err
	}
	reply, err := imaplib.BuildReplyWithOptions(target.raw, from, intent.Body, imaplib.ReplyOptions{
		ReplyAll: intent.ReplyAll, SelfAddresses: selfAddresses,
	}, time.Now(), "")
	if err != nil {
		return draftReplyError("invalid_reply_metadata", err)
	}
	if len(reply.Parsed.From) != 1 || len(reply.Parsed.To)+len(reply.Parsed.Cc)+len(reply.Parsed.Bcc) == 0 {
		return draftReplyError("invalid_reply_metadata", errors.New("composed reply needs one From and at least one To address"))
	}
	return a.createDraft(ctx, target, reply, intent.JSON, emit)
}

// refreshDraftCache runs the daemon's best-effort analytics rebuild once the
// draft is durable, so aggregate views include it before the next sync. A
// failure is logged and never changes the result the client already received.
func (a *storeAPIAdapter) refreshDraftCache(ctx context.Context, source *store.Source) {
	if a.draftCacheRefresh == nil {
		return
	}
	if err := a.draftCacheRefresh(ctx, source.Identifier); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		logger.Error("draft analytics cache refresh failed", "source_id", source.ID, "error", err)
	}
}

// authorizeDelegatedDraftSource checks whether the grant (if any) permits
// draft creation on the given source. Returns nil when grant is nil (owner
// path). The check runs before authorizeIMAPDraft so an out-of-scope source
// never discloses whether drafting is enabled.
func authorizeDelegatedDraftSource(grant *agentgrant.Grant, source *store.Source) error {
	if grant == nil {
		return nil
	}
	ref := draftSourceRef(source)
	if !grant.Allows(agentgrant.PermissionDraftCreate, ref) {
		return draftReplyNotPermitted(fmt.Errorf("source %d is not in grant %s", source.ID, grant.ID))
	}
	return nil
}

func (a *storeAPIAdapter) createDraft(
	ctx context.Context,
	target draftReplyTarget,
	draft imaplib.ReplyDraft,
	asJSON bool,
	emit func(api.CLIRunEvent) error,
) error {
	if len(draft.Parsed.From) != 1 || len(draft.Parsed.To)+len(draft.Parsed.Cc)+len(draft.Parsed.Bcc) == 0 {
		return draftReplyError("invalid_reply_metadata", errors.New("draft needs one From and at least one recipient"))
	}
	messageIDValue := mime.NormalizeMessageID(draft.Parsed.MessageID)
	if messageIDValue == "" {
		return draftReplyError("invalid_reply_metadata", errors.New("composed draft has no usable Message-ID"))
	}
	messageIDValue = "<" + messageIDValue + ">"

	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	defer func() { _ = execution.Release() }()

	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, target.source)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("build IMAP client for source %d: %w", target.source.ID, err))
	}
	defer func() { _ = client.Close() }()
	receipt, err := a.appendDraftReplyWithClient(ctx, client, target, draft.Raw, emit)
	if err != nil {
		return err
	}
	receiptModel := store.IMAPDraftReceipt{
		SourceID: target.source.ID, Mailbox: target.mailbox,
		UIDValidity: receipt.UIDValidity, UID: receipt.UID,
	}
	result := draftReplyOutput{
		Status:          draftReplyStatusCreated,
		OperationRef:    draftOperationRef(receiptModel),
		RFC822MessageID: messageIDValue,
		SourceID:        target.source.ID,
		Mailbox:         target.mailbox,
		UID:             receipt.UID,
		UIDValidity:     receipt.UIDValidity,
	}
	evidenceCtx, cancelEvidence := localDraftEvidenceContext(ctx)
	defer cancelEvidence()
	draftRecord, err := a.store.PersistIMAPDraftContext(evidenceCtx, receiptModel, draftReplyParticipants(draft.Parsed), func(ids []int64) *store.MessagePersistData {
		return draftReplyPersistData(target, draft, receiptModel, messageIDValue, ids)
	})
	if err != nil {
		result.Status = draftReplyStatusLocalFailed
		_ = emitDraftReplyOutput(emit, cliStreamStderr, asJSON, result)
		return draftReplyError(draftReplyStatusLocalFailed, err)
	}
	defer a.releaseDraftSourceAndRefreshCache(ctx, target.source, execution)
	result.DraftID = draftRecord.DraftID
	result.Revision = draftRecord.Revision
	result.MessageID = draftRecord.CurrentMessageID
	if err := emitDraftReplyOutput(emit, cliStreamStdout, asJSON, result); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}

func defaultDraftClientFactory(ctx context.Context, source *store.Source) (*imaplib.Client, error) {
	client, err := buildAPIClient(ctx, source, oauthManagerCache(), nil)
	if err != nil {
		return nil, err
	}
	imapClient, ok := client.(*imaplib.Client)
	if !ok {
		return nil, fmt.Errorf("source %d did not produce an IMAP client", source.ID)
	}
	return imapClient, nil
}

func (a *storeAPIAdapter) appendDraftReplyWithClient(
	ctx context.Context,
	client *imaplib.Client,
	target draftReplyTarget,
	raw []byte,
	emit func(api.CLIRunEvent) error,
) (imaplib.DraftAppendResult, error) {
	receipt, err := client.AppendDraft(ctx, target.mailbox, raw)
	if err != nil {
		if emit != nil {
			_ = emit(api.CLIRunEvent{Type: cliStreamStderr, Data: receipt.Code + "\n"})
		}
		if appendErr, ok := errors.AsType[*imaplib.DraftAppendError](err); ok {
			err = appendErr.Err
		}
		return receipt, draftReplyError(receipt.Code, err)
	}
	return receipt, nil
}

func draftReplyParticipants(parsed *mime.Message) []store.ParticipantPersistData {
	addresses := append([]mime.Address(nil), parsed.From...)
	addresses = append(addresses, parsed.To...)
	addresses = append(addresses, parsed.Cc...)
	addresses = append(addresses, parsed.Bcc...)
	participants := make([]store.ParticipantPersistData, 0, len(addresses))
	for _, address := range addresses {
		participants = append(participants, store.ParticipantPersistData{
			EmailAddress: address.Email,
			DisplayName:  address.Name,
			Domain:       address.Domain,
		})
	}
	return participants
}

// draftReplyPersistData builds the local row set for the accepted draft. ids
// holds the participant IDs in From-then-To order.
func draftReplyPersistData(
	target draftReplyTarget,
	reply imaplib.ReplyDraft,
	receipt store.IMAPDraftReceipt,
	messageIDValue string,
	ids []int64,
) *store.MessagePersistData {
	parsed := reply.Parsed
	fromCount := len(parsed.From)
	toCount := len(parsed.To)
	ccCount := len(parsed.Cc)
	bccCount := len(parsed.Bcc)
	toAddresses := addressStrings(parsed.To)
	ccAddresses := addressStrings(parsed.Cc)
	bccAddresses := addressStrings(parsed.Bcc)
	fromAddresses := addressStrings(parsed.From)
	var conversationKey string
	var replyToMessageID sql.NullInt64
	if target.parent != nil {
		conversationKey = target.parent.SourceConversationID
		replyToMessageID = sql.NullInt64{Int64: target.parent.ID, Valid: true}
		if conversationKey == "" {
			conversationKey = fmt.Sprintf("draft-reply-%d", target.parent.ID)
		}
		if target.parentSource != nil && target.parentSource.ID != target.source.ID {
			conversationKey = fmt.Sprintf("draft-reply-%d-%d-%s", target.parentSource.ID, target.source.ID, conversationKey)
		}
	} else {
		conversationKey = fmt.Sprintf("draft-compose-%d-%s", receipt.SourceID, store.IMAPDraftSourceMessageID(receipt))
	}
	at := fromCount
	fromIDs := ids[:fromCount]
	toIDs := ids[at : at+toCount]
	at += toCount
	ccIDs := ids[at : at+ccCount]
	at += ccCount
	bccIDs := ids[at : at+bccCount]
	return &store.MessagePersistData{
		Message: &store.Message{
			SourceID:        target.source.ID,
			SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
			RFC822MessageID: sql.NullString{String: messageIDValue, Valid: true},
			MessageType:     "email", IsFromMe: true, IdentityDerivedIsFromMe: true,
			SenderID:         sql.NullInt64{Int64: fromIDs[0], Valid: true},
			ReplyToMessageID: replyToMessageID,
			Subject:          sql.NullString{String: parsed.Subject, Valid: parsed.Subject != ""},
			Snippet:          sql.NullString{String: strings.TrimSpace(parsed.BodyText), Valid: parsed.BodyText != ""},
			SentAt:           sql.NullTime{Time: parsed.Date, Valid: !parsed.Date.IsZero()},
			InternalDate:     sql.NullTime{Time: parsed.Date, Valid: !parsed.Date.IsZero()},
			SizeEstimate:     int64(len(reply.Raw)), ArchivedAt: time.Now(),
		},
		Conversation: &store.ConversationPersistData{
			SourceConversationID: conversationKey,
			ConversationType:     "email_thread", Title: parsed.Subject,
		},
		BodyText: sql.NullString{String: parsed.BodyText, Valid: true},
		RawMIME:  reply.Raw, RawFormat: "mime",
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: fromIDs, EmailAddresses: fromAddresses},
			{Type: "to", ParticipantIDs: toIDs, EmailAddresses: toAddresses},
			{Type: "cc", ParticipantIDs: ccIDs, EmailAddresses: ccAddresses},
			{Type: "bcc", ParticipantIDs: bccIDs, EmailAddresses: bccAddresses},
		},
		FTS: &store.FTSDoc{Subject: parsed.Subject, Body: parsed.BodyText, FromAddr: parsed.From[0].Email, ToAddrs: strings.Join(toAddresses, " ")},
	}
}

func emitDraftReplyOutput(emit func(api.CLIRunEvent) error, stream string, asJSON bool, result draftReplyOutput) error {
	if emit == nil {
		return nil
	}
	var text string
	switch {
	case asJSON:
		text = string(marshalDraftReplyOutput(result)) + "\n"
	case result.Status == draftReplyStatusCreated:
		text = fmt.Sprintf("created draft message %d (%s|%d|%d), operation %s, draft %s revision %d\n",
			result.MessageID, result.Mailbox, result.UIDValidity, result.UID, result.OperationRef, result.DraftID, result.Revision)
	default:
		text = fmt.Sprintf("remote accepted; local persistence failed, inspect operation %s\n", result.OperationRef)
	}
	return emit(api.CLIRunEvent{Type: stream, Data: text})
}

func draftOperationRef(receipt store.IMAPDraftReceipt) string {
	return fmt.Sprintf("%d:%s|%d:%d", receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID)
}
