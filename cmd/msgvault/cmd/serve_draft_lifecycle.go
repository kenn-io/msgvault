package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

// draftLifecycleIntent holds the parsed arguments for a draft lifecycle command.
type draftLifecycleIntent struct {
	Command     string // "draft-get", "draft-edit", "draft-delete"
	DraftID     int64
	Revision    int64
	RevisionSet bool
	Body        string
	BodySet     bool
	Resume      bool
	JSON        bool
}

// draftLifecycleTarget holds the resolved draft and its source.
type draftLifecycleTarget struct {
	draft   *store.IMAPDraft
	source  *store.Source
	mailbox string
	raw     []byte // current raw MIME (nil for discard)
}

// draftLifecycleOutput is the JSON output for a draft lifecycle operation.
type draftLifecycleOutput struct {
	Status          string `json:"status"`
	DraftID         int64  `json:"draft_id,omitempty"`
	Lifecycle       string `json:"lifecycle,omitempty"`
	Revision        int64  `json:"revision,omitempty"`
	RFC822MessageID string `json:"rfc822_message_id,omitempty"`
	ProviderStatus  string `json:"provider_status,omitempty"`
	OperationRef    string `json:"operation_ref,omitempty"`
	SourceID        int64  `json:"source_id,omitempty"`
	Mailbox         string `json:"mailbox,omitempty"`
	UID             uint32 `json:"uid,omitempty"`
	UIDValidity     uint32 `json:"uidvalidity,omitempty"`
}

func parseDraftLifecycleArgs(args []string) (draftLifecycleIntent, error) {
	if len(args) == 0 {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("no command provided"))
	}
	command := args[0]
	if !api.IsCLIRunDraftLifecycle(args) {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", fmt.Errorf("unexpected command %q", command))
	}

	var intent draftLifecycleIntent
	intent.Command = command
	var positional string
	var revisionStr string
	var revisionSet, bodySet, resumeSet, jsonSet bool
	rest := args[1:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		nameValue, ok := strings.CutPrefix(arg, "--")
		if !ok {
			if positional != "" {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("expected exactly one draft ID"))
			}
			positional = arg
			continue
		}
		name, value, hasValue := strings.Cut(nameValue, "=")
		switch name {
		case "revision":
			if revisionSet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision given more than once"))
			}
			if !hasValue {
				if len(rest) == 0 {
					return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision requires a value"))
				}
				value, rest = rest[0], rest[1:]
			}
			revisionStr = value
			revisionSet = true
		case "body":
			if bodySet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--body given more than once"))
			}
			if !hasValue {
				if len(rest) == 0 {
					return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--body requires a value"))
				}
				value, rest = rest[0], rest[1:]
			}
			intent.Body, bodySet = value, true
		case "resume":
			if resumeSet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--resume given more than once"))
			}
			if hasValue && value != "true" {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--resume accepts no value"))
			}
			intent.Resume, resumeSet = true, true
		case "json":
			if jsonSet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--json given more than once"))
			}
			if hasValue && value != "true" {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--json accepts no value"))
			}
			intent.JSON, jsonSet = true, true
		case "log-level", "verbose", "log-sql", "log-sql-slow-ms":
			if !hasValue && name != "verbose" && name != "log-sql" && len(rest) > 0 {
				rest = rest[1:]
			}
		default:
			return draftLifecycleIntent{}, draftReplyError("invalid_args", fmt.Errorf("unknown flag --%s", name))
		}
	}
	// Validate --resume mutual exclusion.
	if intent.Resume && (revisionSet || bodySet) {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--resume is mutually exclusive with --revision and --body"))
	}
	// Validate required flags per command.
	if !intent.Resume {
		switch command {
		case api.CLIRunDraftEditCommand:
			if !revisionSet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision is required for draft-edit"))
			}
			if !bodySet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--body is required for draft-edit"))
			}
		case api.CLIRunDraftDeleteCommand:
			if !revisionSet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision is required for draft-delete"))
			}
		}
	}
	if revisionSet {
		rev, err := strconv.ParseInt(revisionStr, 10, 64)
		if err != nil || rev <= 0 {
			return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision must be a positive integer"))
		}
		intent.Revision = rev
		intent.RevisionSet = true
	}
	id, err := strconv.ParseInt(positional, 10, 64)
	if err != nil || id <= 0 {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft ID must be a positive integer"))
	}
	intent.DraftID = id
	return intent, nil
}

// resolveDraftLifecycleTarget loads the draft and authorizes it for mutation.
func (a *storeAPIAdapter) resolveDraftLifecycleTarget(ctx context.Context, intent draftLifecycleIntent) (draftLifecycleTarget, error) {
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftLifecycleTarget{}, draftReplyError("draft_not_found", fmt.Errorf("draft %d: %w", intent.DraftID, err))
		}
		return draftLifecycleTarget{}, draftReplyError("internal", fmt.Errorf("load draft %d: %w", intent.DraftID, err))
	}
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return draftLifecycleTarget{}, draftReplyError("invalid_source", fmt.Errorf("load source %d: %w", draft.SourceID, err))
	}
	if source.SourceType != "imap" {
		return draftLifecycleTarget{}, draftReplyError("draft_disabled", fmt.Errorf("source %d is type %q, not imap", source.ID, source.SourceType))
	}
	// Check operator grant and that the granted mailbox matches the draft's mailbox.
	grantedMailbox, err := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType)
	if err != nil {
		return draftLifecycleTarget{}, err
	}
	if grantedMailbox != draft.Mailbox {
		return draftLifecycleTarget{}, draftReplyError("draft_disabled", fmt.Errorf(
			"draft %d mailbox %q does not match granted mailbox %q",
			intent.DraftID, draft.Mailbox, grantedMailbox))
	}
	if !source.SyncConfig.Valid {
		return draftLifecycleTarget{}, draftReplyError("invalid_source", fmt.Errorf("source %d has no sync config", source.ID))
	}
	imapConfig, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
	if err != nil {
		return draftLifecycleTarget{}, draftReplyError("invalid_source", fmt.Errorf("source %d sync config: %w", source.ID, err))
	}
	if imapConfig.Identifier() != source.Identifier {
		return draftLifecycleTarget{}, draftReplyError("invalid_source", fmt.Errorf("source %d sync config identifier mismatch", source.ID))
	}
	// For edit: verify confirmed identity (need From address).
	if intent.Command == api.CLIRunDraftEditCommand || (intent.Resume && draft.PendingKind.String == "edit") {
		identities, identErr := a.store.ListAccountIdentitiesContext(ctx, source.ID)
		if identErr != nil {
			return draftLifecycleTarget{}, draftReplyError("invalid_from", fmt.Errorf("list identities for source %d: %w", source.ID, identErr))
		}
		if draft.FromAddress != "" && !hasConfirmedSourceIdentity(identities, draft.FromAddress) {
			return draftLifecycleTarget{}, draftReplyError("invalid_from", fmt.Errorf(
				"draft From address is not a confirmed identity on source %d", source.ID))
		}
	}
	var raw []byte
	if intent.Command == api.CLIRunDraftEditCommand || (intent.Resume && draft.PendingKind.String == "edit") {
		raw, err = a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
		if err != nil {
			return draftLifecycleTarget{}, draftReplyError("internal", fmt.Errorf("load raw MIME for draft %d: %w", intent.DraftID, err))
		}
	}
	return draftLifecycleTarget{draft: draft, source: source, mailbox: draft.Mailbox, raw: raw}, nil
}

// marshalDraftLifecycleOutput serializes output to JSON.
func marshalDraftLifecycleOutput(output draftLifecycleOutput) []byte {
	data, err := json.Marshal(output)
	if err != nil {
		return []byte(`{"status":"output_encoding_failed"}`)
	}
	return data
}

// emitDraftLifecycleOutput sends the result to the client.
func emitDraftLifecycleOutput(emit func(api.CLIRunEvent) error, stream string, asJSON bool, output draftLifecycleOutput) error {
	if emit == nil {
		return nil
	}
	var text string
	if asJSON {
		text = string(marshalDraftLifecycleOutput(output)) + "\n"
	} else {
		switch output.Status {
		case "present", "active":
			text = fmt.Sprintf("draft %d: lifecycle=%s revision=%d provider=%s\n",
				output.DraftID, output.Lifecycle, output.Revision, output.ProviderStatus)
		case "discarded":
			text = fmt.Sprintf("draft %d discarded (operation %s)\n", output.DraftID, output.OperationRef)
		case "replaced":
			text = fmt.Sprintf("draft %d replaced: new uid=%d revision=%d (operation %s)\n",
				output.DraftID, output.UID, output.Revision, output.OperationRef)
		default:
			text = fmt.Sprintf("draft %d: status=%s\n", output.DraftID, output.Status)
		}
	}
	return emit(api.CLIRunEvent{Type: stream, Data: text})
}

// runCLIDraftLifecycle dispatches draft-get, draft-edit, and draft-delete.
func (a *storeAPIAdapter) runCLIDraftLifecycle(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft lifecycle commands accept no environment or working directory"))
	}
	intent, err := parseDraftLifecycleArgs(req.Args)
	if err != nil {
		return err
	}

	switch intent.Command {
	case api.CLIRunDraftGetCommand:
		return a.runCLIDraftGet(ctx, intent, emit)
	case api.CLIRunDraftEditCommand:
		if intent.Resume {
			target, err := a.resolveDraftLifecycleTarget(ctx, intent)
			if err != nil {
				return err
			}
			return a.resumeDraftOperation(ctx, intent, target, emit)
		}
		return a.runCLIDraftEdit(ctx, intent, emit)
	case api.CLIRunDraftDeleteCommand:
		if intent.Resume {
			target, err := a.resolveDraftLifecycleTarget(ctx, intent)
			if err != nil {
				return err
			}
			return a.resumeDraftOperation(ctx, intent, target, emit)
		}
		return a.runCLIDraftDelete(ctx, intent, emit)
	default:
		return draftReplyError("invalid_args", fmt.Errorf("unknown lifecycle command %q", intent.Command))
	}
}

// runCLIDraftGet performs a live inspection without taking a sync lock or
// mutating any remote or local state.
func (a *storeAPIAdapter) runCLIDraftGet(
	ctx context.Context,
	intent draftLifecycleIntent,
	emit func(api.CLIRunEvent) error,
) error {
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", fmt.Errorf("draft %d: %w", intent.DraftID, err))
		}
		return draftReplyError("internal", fmt.Errorf("load draft %d: %w", intent.DraftID, err))
	}

	// Discarded drafts are returned from local snapshot without connecting.
	if draft.Lifecycle == "discarded" {
		output := draftLifecycleOutput{
			Status:          "discarded",
			DraftID:         draft.DraftID,
			Lifecycle:       draft.Lifecycle,
			Revision:        draft.Revision,
			RFC822MessageID: draft.RFC822MessageID,
			SourceID:        draft.SourceID,
			Mailbox:         draft.Mailbox,
		}
		return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}

	// For active/pending drafts, connect and inspect.
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("load source %d: %w", draft.SourceID, err))
	}
	var providerStatus string
	if source.SyncConfig.Valid {
		clientFactory := a.draftClientFactory
		if clientFactory == nil {
			clientFactory = defaultDraftClientFactory
		}
		client, clientErr := clientFactory(ctx, source)
		if clientErr == nil {
			defer func() { _ = client.Close() }()
			raw, rawErr := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
			if rawErr == nil {
				digest := sha256.Sum256(raw)
				target := imaplib.DraftTarget{
					Mailbox:     draft.Mailbox,
					UIDValidity: draft.UIDValidity,
					UID:         draft.UID,
					RawSHA256:   digest,
				}
				inspectResult, inspectErr := client.InspectDraft(ctx, target)
				if inspectErr == nil {
					providerStatus = inspectResult.State
				} else {
					providerStatus = "unknown"
				}
			} else {
				providerStatus = "unknown"
			}
		} else {
			providerStatus = "unknown"
		}
	} else {
		providerStatus = "unknown"
	}

	output := draftLifecycleOutput{
		Status:          "active",
		DraftID:         draft.DraftID,
		Lifecycle:       draft.Lifecycle,
		Revision:        draft.Revision,
		RFC822MessageID: draft.RFC822MessageID,
		ProviderStatus:  providerStatus,
		SourceID:        draft.SourceID,
		Mailbox:         draft.Mailbox,
		UID:             draft.UID,
		UIDValidity:     draft.UIDValidity,
	}
	return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
}

// runCLIDraftEdit replaces the body of the draft and advances the revision.
func (a *storeAPIAdapter) runCLIDraftEdit(
	ctx context.Context,
	intent draftLifecycleIntent,
	emit func(api.CLIRunEvent) error,
) error {
	target, err := a.resolveDraftLifecycleTarget(ctx, intent)
	if err != nil {
		return err
	}
	draft := target.draft

	// Build the new raw draft body.
	newDraft, err := imaplib.ReplaceDraftBody(target.raw, intent.Body, time.Now())
	if err != nil {
		return draftReplyError("invalid_reply_metadata", err)
	}
	messageIDValue := mime.NormalizeMessageID(newDraft.Parsed.MessageID)
	if messageIDValue == "" {
		return draftReplyError("invalid_reply_metadata", errors.New("composed draft has no usable Message-ID"))
	}
	messageIDValue = "<" + messageIDValue + ">"

	// Acquire the sync execution context.
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	defer func() { _ = execution.Release() }()

	// 4. InspectDraft on live old receipt.
	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, target.source)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("build IMAP client for source %d: %w", target.source.ID, err))
	}
	defer func() { _ = client.Close() }()

	oldRaw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftReplyError("internal", fmt.Errorf("load current raw for draft %d: %w", intent.DraftID, err))
	}
	oldDigest := sha256.Sum256(oldRaw)
	oldTarget := imaplib.DraftTarget{
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		RawSHA256:   oldDigest,
	}
	inspectResult, err := client.InspectDraft(ctx, oldTarget)
	if err != nil {
		var appendErr *imaplib.DraftAppendError
		if errors.As(err, &appendErr) {
			return draftReplyError(appendErr.Code, appendErr.Err)
		}
		return draftReplyError("remote_unknown", err)
	}
	switch inspectResult.State {
	case imaplib.DraftRemoteAbsent:
		return draftReplyError("draft_missing", fmt.Errorf("draft %d: remote copy absent", intent.DraftID))
	case imaplib.DraftRemoteFlagMissing, imaplib.DraftRemoteChanged:
		return draftReplyError("draft_changed", fmt.Errorf("draft %d: remote copy modified externally", intent.DraftID))
	}

	// 5. BeginIMAPDraftOperationContext.
	pendingRaw := newDraft.Raw
	claimedDraft, err := a.store.BeginIMAPDraftOperationContext(ctx, store.IMAPDraftIntent{
		DraftID:          intent.DraftID,
		ExpectedRevision: intent.Revision,
		Kind:             "edit",
		PendingRaw:       pendingRaw,
		PendingRfc822ID:  messageIDValue,
	})
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		msg := err.Error()
		switch {
		case strings.Contains(msg, "revision_conflict"):
			return draftReplyError("revision_conflict", err)
		case strings.Contains(msg, "operation_pending"):
			return draftReplyError("operation_pending", err)
		}
		return draftReplyError("internal", err)
	}

	// 6. APPEND the new copy.
	appendResult, appendErr := client.AppendDraft(ctx, draft.Mailbox, newDraft.Raw)
	recordCtx := context.WithoutCancel(ctx)
	var newReceipt *store.IMAPDraftReceipt
	if appendErr == nil {
		r := store.IMAPDraftReceipt{
			SourceID: target.source.ID, Mailbox: draft.Mailbox,
			UIDValidity: appendResult.UIDValidity, UID: appendResult.UID,
		}
		newReceipt = &r
		// Record the append with receipt.
		if recErr := a.store.RecordIMAPDraftAppendContext(recordCtx, intent.DraftID, claimedDraft.Revision, newReceipt); recErr != nil {
			logger.Error("record draft append receipt", "draft_id", intent.DraftID, "error", recErr)
		}
	} else {
		// Record attempt only.
		_ = a.store.RecordIMAPDraftAppendContext(recordCtx, intent.DraftID, claimedDraft.Revision, nil)
		var dae *imaplib.DraftAppendError
		if errors.As(appendErr, &dae) {
			return draftReplyError(dae.Code, dae.Err)
		}
		return draftReplyError("remote_unknown", appendErr)
	}

	// 7. Persist the new message row locally.
	participants := draftReplyParticipants(newDraft.Parsed)
	newMessageID, persistErr := a.store.PersistIMAPDraftReplacementContext(
		recordCtx,
		intent.DraftID, claimedDraft.Revision,
		*newReceipt,
		participants,
		func(ids []int64) *store.MessagePersistData {
			fromCount := len(newDraft.Parsed.From)
			toAddresses := make([]string, len(newDraft.Parsed.To))
			for i, addr := range newDraft.Parsed.To {
				toAddresses[i] = addr.Email
			}
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        target.source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(*newReceipt),
					ConversationID:  draft.ConversationID,
					RFC822MessageID: sql.NullString{String: messageIDValue, Valid: true},
					MessageType:     "email", IsFromMe: true, IdentityDerivedIsFromMe: true,
					SenderID:         sql.NullInt64{Int64: ids[0], Valid: true},
					ReplyToMessageID: sql.NullInt64{Int64: draft.DraftID, Valid: true},
					Subject:          sql.NullString{String: newDraft.Parsed.Subject, Valid: newDraft.Parsed.Subject != ""},
					Snippet:          sql.NullString{String: strings.TrimSpace(newDraft.Parsed.BodyText), Valid: newDraft.Parsed.BodyText != ""},
					SentAt:           sql.NullTime{Time: newDraft.Parsed.Date, Valid: !newDraft.Parsed.Date.IsZero()},
					InternalDate:     sql.NullTime{Time: newDraft.Parsed.Date, Valid: !newDraft.Parsed.Date.IsZero()},
					SizeEstimate:     int64(len(newDraft.Raw)), ArchivedAt: time.Now(),
				},
				BodyText: sql.NullString{String: newDraft.Parsed.BodyText, Valid: true},
				RawMIME:  newDraft.Raw, RawFormat: "mime",
				Recipients: []store.RecipientSet{
					{Type: "from", ParticipantIDs: ids[:fromCount], EmailAddresses: []string{newDraft.Parsed.From[0].Email}},
					{Type: "to", ParticipantIDs: ids[fromCount:], EmailAddresses: toAddresses},
				},
				FTS: &store.FTSDoc{
					Subject:  newDraft.Parsed.Subject,
					Body:     newDraft.Parsed.BodyText,
					FromAddr: newDraft.Parsed.From[0].Email,
					ToAddrs:  strings.Join(toAddresses, " "),
				},
			}
		},
	)
	if persistErr != nil {
		logger.Error("persist draft replacement", "draft_id", intent.DraftID, "error", persistErr)
		output := draftLifecycleOutput{
			Status:       "remote_accepted_local_failed",
			DraftID:      intent.DraftID,
			OperationRef: draftOperationRef(*newReceipt),
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("remote_accepted_local_failed", persistErr)
	}
	_ = newMessageID

	// Re-inspect the old copy to confirm it's still the same before deleting.
	removeResult, removeErr := client.RemoveDraft(recordCtx, oldTarget)
	if removeErr != nil {
		var dae *imaplib.DraftAppendError
		if errors.As(removeErr, &dae) && dae.Code == "uidvalidity_changed" {
			// Old copy is in a changed epoch - note the issue but finish locally.
			logger.Error("old draft epoch changed during edit", "draft_id", intent.DraftID)
		} else {
			logger.Error("remove old draft copy", "draft_id", intent.DraftID, "error", removeErr)
		}
	}
	_ = removeResult

	// 8. FinishIMAPDraftOperationContext.
	finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, intent.DraftID, claimedDraft.Revision, store.IMAPDraftOutcome{
		Lifecycle:   "active",
		SourceID:    target.source.ID,
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		MessageID:   draft.CurrentMessageID,
	})
	if finishErr != nil {
		logger.Error("finish draft edit operation", "draft_id", intent.DraftID, "error", finishErr)
		return draftReplyError("remote_accepted_local_failed", finishErr)
	}

	// Release lock before cache refresh.
	if err := execution.Release(); err != nil {
		logger.Error("release source after draft edit", "source_id", target.source.ID, "error", err)
	}

	// Load updated draft to get new revision.
	updatedDraft, _ := a.store.GetIMAPDraftContext(recordCtx, intent.DraftID)
	var newRevision int64
	if updatedDraft != nil {
		newRevision = updatedDraft.Revision
	}

	output := draftLifecycleOutput{
		Status:          "replaced",
		DraftID:         intent.DraftID,
		Lifecycle:       "active",
		Revision:        newRevision,
		RFC822MessageID: messageIDValue,
		OperationRef:    draftOperationRef(*newReceipt),
		SourceID:        target.source.ID,
		Mailbox:         draft.Mailbox,
		UID:             newReceipt.UID,
		UIDValidity:     newReceipt.UIDValidity,
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	a.refreshDraftCache(recordCtx, target.source)
	return nil
}

// runCLIDraftDelete permanently deletes the draft from the remote server.
func (a *storeAPIAdapter) runCLIDraftDelete(
	ctx context.Context,
	intent draftLifecycleIntent,
	emit func(api.CLIRunEvent) error,
) error {
	target, err := a.resolveDraftLifecycleTarget(ctx, intent)
	if err != nil {
		return err
	}
	draft := target.draft

	// Acquire the sync execution context.
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	defer func() { _ = execution.Release() }()

	// InspectDraft.
	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, target.source)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("build IMAP client for source %d: %w", target.source.ID, err))
	}
	defer func() { _ = client.Close() }()

	currentRaw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftReplyError("internal", fmt.Errorf("load current raw for draft %d: %w", intent.DraftID, err))
	}
	digest := sha256.Sum256(currentRaw)
	oldTarget := imaplib.DraftTarget{
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		RawSHA256:   digest,
	}
	inspectResult, err := client.InspectDraft(ctx, oldTarget)
	if err != nil {
		var appendErr *imaplib.DraftAppendError
		if errors.As(err, &appendErr) {
			return draftReplyError(appendErr.Code, appendErr.Err)
		}
		return draftReplyError("remote_unknown", err)
	}
	switch inspectResult.State {
	case imaplib.DraftRemoteAbsent:
		// For delete, absent is acceptable (idempotent). Continue to local cleanup.
	case imaplib.DraftRemoteFlagMissing, imaplib.DraftRemoteChanged:
		return draftReplyError("draft_changed", fmt.Errorf("draft %d: remote copy modified externally", intent.DraftID))
	}

	// BeginIMAPDraftOperationContext.
	claimedDraft, err := a.store.BeginIMAPDraftOperationContext(ctx, store.IMAPDraftIntent{
		DraftID:          intent.DraftID,
		ExpectedRevision: intent.Revision,
		Kind:             "discard",
	})
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "revision_conflict"):
			return draftReplyError("revision_conflict", err)
		case strings.Contains(err.Error(), "operation_pending"):
			return draftReplyError("operation_pending", err)
		case opserr.KindOf(err) == opserr.KindNotFound:
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", err)
	}

	recordCtx := context.WithoutCancel(ctx)

	// RemoveDraft.
	removeResult, removeErr := client.RemoveDraft(recordCtx, oldTarget)
	if removeErr != nil {
		var dae *imaplib.DraftAppendError
		if errors.As(removeErr, &dae) {
			// Leave pending state set; caller can --resume.
			output := draftLifecycleOutput{
				Status:       "delete_failed",
				DraftID:      intent.DraftID,
				Lifecycle:    "delete_pending",
				Revision:     claimedDraft.Revision,
				OperationRef: draftOperationRef(store.IMAPDraftReceipt{SourceID: target.source.ID, Mailbox: draft.Mailbox, UIDValidity: draft.UIDValidity, UID: draft.UID}),
			}
			_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
			return draftReplyError("delete_failed", dae.Err)
		}
		output := draftLifecycleOutput{
			Status:    "delete_failed",
			DraftID:   intent.DraftID,
			Lifecycle: "delete_pending",
			Revision:  claimedDraft.Revision,
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("delete_failed", removeErr)
	}
	_ = removeResult

	// FinishIMAPDraftOperationContext.
	finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, intent.DraftID, claimedDraft.Revision, store.IMAPDraftOutcome{
		Lifecycle:   "discarded",
		SourceID:    target.source.ID,
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		MessageID:   draft.CurrentMessageID,
	})
	if finishErr != nil {
		logger.Error("finish draft delete operation", "draft_id", intent.DraftID, "error", finishErr)
		return draftReplyError("remote_deleted_local_failed", finishErr)
	}

	// Release lock.
	if err := execution.Release(); err != nil {
		logger.Error("release source after draft delete", "source_id", target.source.ID, "error", err)
	}

	output := draftLifecycleOutput{
		Status:       "discarded",
		DraftID:      intent.DraftID,
		Lifecycle:    "discarded",
		OperationRef: draftOperationRef(store.IMAPDraftReceipt{SourceID: target.source.ID, Mailbox: draft.Mailbox, UIDValidity: draft.UIDValidity, UID: draft.UID}),
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	a.refreshDraftCache(recordCtx, target.source)
	return nil
}

// resumeDraftOperation continues a previously-started draft operation.
func (a *storeAPIAdapter) resumeDraftOperation(
	ctx context.Context,
	intent draftLifecycleIntent,
	target draftLifecycleTarget,
	emit func(api.CLIRunEvent) error,
) error {
	draft := target.draft
	if draft.PendingKind.String == "" || !draft.PendingKind.Valid {
		// No pending mutation: report current state.
		output := draftLifecycleOutput{
			Status:    "active",
			DraftID:   draft.DraftID,
			Lifecycle: draft.Lifecycle,
			Revision:  draft.Revision,
		}
		return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}

	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, target.source)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("build IMAP client for resume: %w", err))
	}
	defer func() { _ = client.Close() }()

	recordCtx := context.WithoutCancel(ctx)

	if draft.PendingKind.String == "edit" {
		// For edit resume: if APPEND was attempted but receipt not recorded,
		// try FindDraftAppend to locate the candidate.
		var newReceipt *store.IMAPDraftReceipt
		if draft.PendingAppendAttempted && !draft.PendingUID.Valid {
			var pendingDigest [32]byte
			if len(draft.PendingRaw) > 0 {
				pendingDigest = sha256.Sum256(draft.PendingRaw)
			}
			found, findErr := client.FindDraftAppend(ctx, imaplib.DraftTarget{
				Mailbox:     draft.Mailbox,
				UIDValidity: draft.UIDValidity,
				UID:         draft.UID,
				RawSHA256:   pendingDigest,
			}, draft.PendingRfc822ID.String)
			if findErr != nil {
				logger.Error("resume find draft append", "draft_id", draft.DraftID, "error", findErr)
				// Cannot recover - leave pending.
				return draftReplyError("remote_unknown", findErr)
			}
			if found != nil {
				r := store.IMAPDraftReceipt{
					SourceID:    target.source.ID,
					Mailbox:     draft.Mailbox,
					UIDValidity: found.UIDValidity,
					UID:         found.UID,
				}
				newReceipt = &r
				// Record the recovered receipt.
				_ = a.store.RecordIMAPDraftAppendContext(recordCtx, draft.DraftID, draft.Revision, newReceipt)
			} else {
				// Zero or multiple candidates: leave pending columns untouched.
				return draftReplyError("remote_unknown", errors.New("could not uniquely identify pending APPEND; retry later"))
			}
		} else if draft.PendingUID.Valid {
			r := store.IMAPDraftReceipt{
				SourceID:    target.source.ID,
				Mailbox:     draft.Mailbox,
				UIDValidity: uint32(draft.PendingUIDValidity.Int64),
				UID:         uint32(draft.PendingUID.Int64),
			}
			newReceipt = &r
		}

		if newReceipt == nil {
			// No receipt available yet; cannot complete.
			return draftReplyError("remote_unknown", errors.New("pending edit has no receipt to complete"))
		}

		// Remove the old copy.
		currentRaw, rawErr := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
		if rawErr != nil {
			return draftReplyError("internal", fmt.Errorf("load current raw for resume %d: %w", draft.DraftID, rawErr))
		}
		oldDigest := sha256.Sum256(currentRaw)
		oldTarget := imaplib.DraftTarget{
			Mailbox:     draft.Mailbox,
			UIDValidity: draft.UIDValidity,
			UID:         draft.UID,
			RawSHA256:   oldDigest,
		}
		_, _ = client.RemoveDraft(recordCtx, oldTarget) // best effort

		// FinishIMAPDraftOperationContext.
		finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, draft.DraftID, draft.Revision, store.IMAPDraftOutcome{
			Lifecycle:   "active",
			SourceID:    target.source.ID,
			Mailbox:     draft.Mailbox,
			UIDValidity: draft.UIDValidity,
			UID:         draft.UID,
			MessageID:   draft.CurrentMessageID,
		})
		if finishErr != nil {
			return draftReplyError("remote_accepted_local_failed", finishErr)
		}

		output := draftLifecycleOutput{
			Status:       "replaced",
			DraftID:      draft.DraftID,
			Lifecycle:    "active",
			OperationRef: draftOperationRef(*newReceipt),
		}
		if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
			return draftReplyError("output_failed", err)
		}
		a.refreshDraftCache(recordCtx, target.source)
		return nil
	}

	if draft.PendingKind.String == "discard" {
		// Re-attempt the delete.
		currentRaw, rawErr := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
		if rawErr != nil {
			return draftReplyError("internal", fmt.Errorf("load current raw for resume delete %d: %w", draft.DraftID, rawErr))
		}
		digest := sha256.Sum256(currentRaw)
		oldTarget := imaplib.DraftTarget{
			Mailbox:     draft.Mailbox,
			UIDValidity: draft.UIDValidity,
			UID:         draft.UID,
			RawSHA256:   digest,
		}
		_, removeErr := client.RemoveDraft(recordCtx, oldTarget)
		if removeErr != nil {
			var dae *imaplib.DraftAppendError
			if errors.As(removeErr, &dae) && dae.Code != "uidvalidity_changed" {
				// Leave pending.
				return draftReplyError("delete_failed", dae.Err)
			}
		}

		finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, draft.DraftID, draft.Revision, store.IMAPDraftOutcome{
			Lifecycle:   "discarded",
			SourceID:    target.source.ID,
			Mailbox:     draft.Mailbox,
			UIDValidity: draft.UIDValidity,
			UID:         draft.UID,
			MessageID:   draft.CurrentMessageID,
		})
		if finishErr != nil {
			return draftReplyError("remote_deleted_local_failed", finishErr)
		}

		output := draftLifecycleOutput{
			Status:    "discarded",
			DraftID:   draft.DraftID,
			Lifecycle: "discarded",
		}
		if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
			return draftReplyError("output_failed", err)
		}
		a.refreshDraftCache(recordCtx, target.source)
		return nil
	}

	return draftReplyError("invalid_args", fmt.Errorf("unknown pending kind %q", draft.PendingKind.String))
}
