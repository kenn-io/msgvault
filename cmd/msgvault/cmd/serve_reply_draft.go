package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

var (
	errDraftReplyFromRequired = errors.New("--from is required")
	errDraftReplyBodyRequired = errors.New("--body is required")
)

const (
	draftReplyStatusCreated     = "created"
	draftReplyStatusLocalFailed = "remote_accepted_local_failed"
)

type draftReplyIntent struct {
	MessageID int64
	From      string
	Body      string
	JSON      bool
}

// draftReplyTarget is the archived parent message and the granted source
// mailbox that will hold the reply.
type draftReplyTarget struct {
	parent  *store.APIMessage
	source  *store.Source
	mailbox string
	raw     []byte
}

type draftReplyOutput struct {
	Status          string `json:"status"`
	MessageID       int64  `json:"message_id,omitempty"`
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

func invalidDraftReplyArgs(format string, args ...any) (draftReplyIntent, error) {
	return draftReplyIntent{}, draftReplyError("invalid_args", fmt.Errorf(format, args...))
}

func parseDraftReplyArgs(args []string) (draftReplyIntent, error) {
	if !api.IsCLIRunDraftReply(args) {
		return invalidDraftReplyArgs("expected %s as the first argument", api.CLIRunDraftReplyCommand)
	}
	var intent draftReplyIntent
	var positional string
	var fromSet, bodySet, jsonSet bool
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
		case "from", "body":
			if !hasValue {
				if len(rest) == 0 {
					return invalidDraftReplyArgs("--%s requires a value", name)
				}
				value, rest = rest[0], rest[1:]
			}
			if (name == "from" && fromSet) || (name == "body" && bodySet) {
				return invalidDraftReplyArgs("--%s given more than once", name)
			}
			if name == "from" {
				intent.From, fromSet = value, true
			} else {
				intent.Body, bodySet = value, true
			}
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
	if !fromSet || intent.From == "" {
		return invalidDraftReplyArgs("--from is required")
	}
	if !bodySet {
		return invalidDraftReplyArgs("--body is required")
	}
	id, err := strconv.ParseInt(positional, 10, 64)
	if err != nil || id <= 0 {
		return invalidDraftReplyArgs("message ID must be a positive integer")
	}
	intent.MessageID = id
	return intent, nil
}

func marshalDraftReplyOutput(output draftReplyOutput) []byte {
	data, err := json.Marshal(output)
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

func hasConfirmedSourceIdentity(identities []store.AccountIdentity, address string) bool {
	for _, identity := range identities {
		if !identity.ConfirmedAt.IsZero() && store.EqualIdentifier(identity.Address, address) {
			return true
		}
	}
	return false
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
	target, err := a.resolveDraftReplyTarget(ctx, intent)
	if err != nil {
		return err
	}
	reply, err := imaplib.BuildReply(target.raw, intent.From, intent.Body, time.Now(), "")
	if err != nil {
		return draftReplyError("invalid_reply_metadata", err)
	}
	if len(reply.Parsed.From) != 1 || len(reply.Parsed.To) == 0 {
		return draftReplyError("invalid_reply_metadata", errors.New("composed reply needs one From and at least one To address"))
	}
	messageIDValue := mime.NormalizeMessageID(reply.Parsed.MessageID)
	if messageIDValue == "" {
		return draftReplyError("invalid_reply_metadata", errors.New("composed reply has no usable Message-ID"))
	}
	messageIDValue = "<" + messageIDValue + ">"

	// Hold the source's sync lock from APPEND through local publication so a
	// concurrent sync cannot reconcile a stale mailbox snapshot over the draft.
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	defer func() { _ = execution.Release() }()

	receipt, err := a.appendDraftReply(ctx, target, reply.Raw, emit)
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
	localID, err := a.store.PersistIMAPDraftContext(ctx, receiptModel, draftReplyParticipants(reply.Parsed), func(ids []int64) *store.MessagePersistData {
		return draftReplyPersistData(target, reply, receiptModel, messageIDValue, ids)
	})
	if err != nil {
		result.Status = draftReplyStatusLocalFailed
		_ = emitDraftReplyOutput(emit, cliStreamStderr, intent.JSON, result)
		return draftReplyError(draftReplyStatusLocalFailed, err)
	}
	result.MessageID = localID
	if err := emitDraftReplyOutput(emit, cliStreamStdout, intent.JSON, result); err != nil {
		return draftReplyError("output_failed", err)
	}
	// The draft is durable and reported. Free the source for syncs before the
	// cache rebuild, which can take a while and needs no lock.
	if err := execution.Release(); err != nil {
		logger.Error("release source after draft", "source_id", target.source.ID, "error", err)
	}
	a.refreshDraftCache(ctx, target.source)
	return nil
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

// resolveDraftReplyTarget loads the parent, checks the operator grant, and
// confirms the sender identity. It runs before the sync lock is taken so a
// denied request never blocks a sync.
func (a *storeAPIAdapter) resolveDraftReplyTarget(ctx context.Context, intent draftReplyIntent) (draftReplyTarget, error) {
	parent, err := a.store.GetMessageContext(ctx, intent.MessageID)
	if err != nil {
		return draftReplyTarget{}, draftReplyError("invalid_parent", fmt.Errorf("load message %d: %w", intent.MessageID, err))
	}
	source, err := a.store.GetSourceByIDContext(ctx, parent.SourceID)
	if err != nil {
		return draftReplyTarget{}, draftReplyError("invalid_source", fmt.Errorf("load source %d: %w", parent.SourceID, err))
	}
	mailbox, err := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType)
	if err != nil {
		return draftReplyTarget{}, err
	}
	if !source.SyncConfig.Valid {
		return draftReplyTarget{}, draftReplyError("invalid_source", fmt.Errorf("source %d has no sync config", source.ID))
	}
	imapConfig, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
	if err != nil {
		return draftReplyTarget{}, draftReplyError("invalid_source", fmt.Errorf("source %d sync config: %w", source.ID, err))
	}
	if imapConfig.Identifier() != source.Identifier {
		return draftReplyTarget{}, draftReplyError("invalid_source", fmt.Errorf("source %d sync config identifier does not match the source", source.ID))
	}
	raw, err := a.store.GetMessageRawContext(ctx, parent.ID)
	if err != nil {
		return draftReplyTarget{}, draftReplyError("invalid_parent", fmt.Errorf("load raw MIME for message %d: %w", parent.ID, err))
	}
	identities, err := a.store.ListAccountIdentitiesContext(ctx, source.ID)
	if err != nil {
		return draftReplyTarget{}, draftReplyError("invalid_from", fmt.Errorf("list identities for source %d: %w", source.ID, err))
	}
	if !hasConfirmedSourceIdentity(identities, intent.From) {
		return draftReplyTarget{}, draftReplyError("invalid_from", fmt.Errorf("--from is not a confirmed identity on source %d", source.ID))
	}
	return draftReplyTarget{parent: parent, source: source, mailbox: mailbox, raw: raw}, nil
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

// appendDraftReply sends the single APPEND. Any failure after this point
// leaves a state the operator must inspect before retrying.
func (a *storeAPIAdapter) appendDraftReply(
	ctx context.Context,
	target draftReplyTarget,
	raw []byte,
	emit func(api.CLIRunEvent) error,
) (imaplib.DraftAppendResult, error) {
	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, target.source)
	if err != nil {
		return imaplib.DraftAppendResult{}, draftReplyError("invalid_source", fmt.Errorf("build IMAP client for source %d: %w", target.source.ID, err))
	}
	defer func() { _ = client.Close() }()
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
	participants := make([]store.ParticipantPersistData, 0, len(parsed.From)+len(parsed.To))
	for _, address := range append(append([]mime.Address(nil), parsed.From...), parsed.To...) {
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
	toAddresses := make([]string, len(parsed.To))
	for i, address := range parsed.To {
		toAddresses[i] = address.Email
	}
	conversationKey := target.parent.SourceConversationID
	if conversationKey == "" {
		conversationKey = fmt.Sprintf("draft-reply-%d", target.parent.ID)
	}
	return &store.MessagePersistData{
		Message: &store.Message{
			SourceID:        target.source.ID,
			SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
			RFC822MessageID: sql.NullString{String: messageIDValue, Valid: true},
			MessageType:     "email", IsFromMe: true, IdentityDerivedIsFromMe: true,
			SenderID:         sql.NullInt64{Int64: ids[0], Valid: true},
			ReplyToMessageID: sql.NullInt64{Int64: target.parent.ID, Valid: true},
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
			{Type: "from", ParticipantIDs: ids[:fromCount], EmailAddresses: []string{parsed.From[0].Email}},
			{Type: "to", ParticipantIDs: ids[fromCount:], EmailAddresses: toAddresses},
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
		text = fmt.Sprintf("created draft message %d (%s|%d|%d), operation %s\n",
			result.MessageID, result.Mailbox, result.UIDValidity, result.UID, result.OperationRef)
	default:
		text = fmt.Sprintf("remote accepted; local persistence failed, inspect operation %s\n", result.OperationRef)
	}
	return emit(api.CLIRunEvent{Type: stream, Data: text})
}

func draftOperationRef(receipt store.IMAPDraftReceipt) string {
	return fmt.Sprintf("%d:%s|%d:%d", receipt.SourceID, receipt.Mailbox, receipt.UIDValidity, receipt.UID)
}
