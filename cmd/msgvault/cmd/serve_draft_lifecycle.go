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

// draftClient is the subset of imaplib.Client methods used by the draft reply
// and lifecycle commands. Using an interface enables test injection of custom
// behaviour (e.g. injecting a failing RemoveDraft) without requiring a real
// IMAP connection.
type draftClient interface {
	AppendDraft(ctx context.Context, mailbox string, raw []byte) (imaplib.DraftAppendResult, error)
	InspectDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error)
	RemoveDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error)
	FindDraftAppend(ctx context.Context, target imaplib.DraftTarget, messageID string) (*imaplib.DraftAppendResult, error)
	Close() error
}

// lifecycleDraftClient returns the adapter's lifecycle factory, falling back
// through draftClientFactory and then defaultDraftClientFactory.
func (a *storeAPIAdapter) lifecycleDraftClient(ctx context.Context, source *store.Source) (draftClient, error) {
	if a.draftLifecycleClientFactory != nil {
		return a.draftLifecycleClientFactory(ctx, source)
	}
	if a.draftClientFactory != nil {
		return a.draftClientFactory(ctx, source)
	}
	return defaultDraftClientFactory(ctx, source)
}

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
	Subject         string `json:"subject,omitempty"`
	FromAddress     string `json:"from_address,omitempty"`
	BodyText        string `json:"body_text,omitempty"`
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

	// P2-B: check the operator grant before opening any IMAP connection.
	var providerStatus string
	if _, grantErr := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType); grantErr != nil {
		providerStatus = "not_checked"
	} else if source.SyncConfig.Valid {
		client, clientErr := a.lifecycleDraftClient(ctx, source)
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
		Subject:         draft.Subject,
		FromAddress:     draft.FromAddress,
		BodyText:        draft.Snippet,
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
	client, err := a.lifecycleDraftClient(ctx, target.source)
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
	// P1-B: mark pending_append_attempted=TRUE BEFORE the network call so a
	// crash between the APPEND and receipt-recording is detectable on resume.
	if recErr := a.store.RecordIMAPDraftAppendContext(ctx, intent.DraftID, claimedDraft.Revision, nil); recErr != nil {
		logger.Error("pre-record draft append attempted", "draft_id", intent.DraftID, "error", recErr)
	}
	appendResult, appendErr := client.AppendDraft(ctx, draft.Mailbox, newDraft.Raw)
	recordCtx := context.WithoutCancel(ctx)
	var newReceipt *store.IMAPDraftReceipt
	if appendErr == nil {
		r := store.IMAPDraftReceipt{
			SourceID: target.source.ID, Mailbox: draft.Mailbox,
			UIDValidity: appendResult.UIDValidity, UID: appendResult.UID,
		}
		newReceipt = &r
		// Record the append (attempted flag already set above; this call is a no-op for pending_uid).
		if recErr := a.store.RecordIMAPDraftAppendContext(recordCtx, intent.DraftID, claimedDraft.Revision, newReceipt); recErr != nil {
			logger.Error("record draft append receipt", "draft_id", intent.DraftID, "error", recErr)
		}
	} else {
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
					ReplyToMessageID: draft.ReplyToMessageID,
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
	// P2-E: use ctx (not recordCtx) so the remote call is cancellable.
	// P1-A: only call Finish when RemoveDraft confirms absent or present (expunged).
	removeResult, removeErr := client.RemoveDraft(ctx, oldTarget)
	if removeErr != nil {
		var dae *imaplib.DraftAppendError
		if errors.As(removeErr, &dae) {
			// Leave pending state so --resume can retry the removal.
			logger.Error("remove old draft copy failed", "draft_id", intent.DraftID, "error", removeErr)
			output := draftLifecycleOutput{
				Status:       "remote_accepted_local_failed",
				DraftID:      intent.DraftID,
				OperationRef: draftOperationRef(*newReceipt),
			}
			_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
			return draftReplyError(dae.Code, dae.Err)
		}
		logger.Error("remove old draft copy failed", "draft_id", intent.DraftID, "error", removeErr)
		return draftReplyError("remote_unknown", removeErr)
	}
	switch removeResult.State {
	case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
		// Success: absent means already gone; present means just expunged.
	default:
		// Soft failure (flag_missing or changed): leave pending state.
		logger.Error("remove old draft copy soft fail", "draft_id", intent.DraftID, "state", removeResult.State)
		return draftReplyError("delete_failed", fmt.Errorf("remote draft state changed during edit: %s", removeResult.State))
	}

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
	client, err := a.lifecycleDraftClient(ctx, target.source)
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

	// RemoveDraft. P2-E: use ctx (cancellable) for the network call.
	// P1-A: only call Finish when RemoveDraft confirms absent or present (expunged).
	removeResult, removeErr := client.RemoveDraft(ctx, oldTarget)
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
	// P1-A: soft failures (flag_missing or changed) mean the remote copy was
	// externally modified after InspectDraft confirmed it; leave pending.
	switch removeResult.State {
	case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
		// Success: proceed to local cleanup.
	default:
		output := draftLifecycleOutput{
			Status:    "delete_failed",
			DraftID:   intent.DraftID,
			Lifecycle: "delete_pending",
			Revision:  claimedDraft.Revision,
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("delete_failed", fmt.Errorf("remote draft state changed: %s", removeResult.State))
	}

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

	// P1-D: acquire the sync lock before creating the IMAP client so that
	// resume and the regular sync never race over the same mailbox.
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	defer func() { _ = execution.Release() }()

	// Reload the draft under the lock to get a fresh, authoritative snapshot.
	freshDraft, reloadErr := a.store.GetIMAPDraftContext(ctx, draft.DraftID)
	if reloadErr != nil {
		return draftReplyError("internal", fmt.Errorf("reload draft %d under lock: %w", draft.DraftID, reloadErr))
	}
	draft = freshDraft

	client, err := a.lifecycleDraftClient(ctx, target.source)
	if err != nil {
		return draftReplyError("invalid_source", fmt.Errorf("build IMAP client for resume: %w", err))
	}
	defer func() { _ = client.Close() }()

	recordCtx := context.WithoutCancel(ctx)

	if draft.PendingKind.String == "edit" {
		// Determine the old UID from the pending tuple saved at Begin (P1-C).
		// After P1-C, BeginIMAPDraftOperationContext saves pending_uid = uid (old).
		// PersistIMAPDraftReplacementContext then updates imap_drafts.uid to the
		// new value, so pending_uid != uid once Persist has run.
		var oldUID uint32
		var oldUIDValidity uint32
		if draft.PendingUID.Valid {
			oldUID = uint32(draft.PendingUID.Int64)
			oldUIDValidity = uint32(draft.PendingUIDValidity.Int64)
		} else {
			// Fallback: test 13 manually sets pending_uid = NULL to simulate
			// pre-P1-C state; use current values in that case.
			oldUID = draft.UID
			oldUIDValidity = draft.UIDValidity
		}

		// Determine whether PersistIMAPDraftReplacementContext has already run.
		persistDone := draft.PendingUID.Valid && draft.PendingUID.Int64 != int64(draft.UID)

		var newReceipt *store.IMAPDraftReceipt
		if persistDone {
			// Persist ran: new uid is in imap_drafts.uid (draft.UID).
			r := store.IMAPDraftReceipt{
				SourceID:    target.source.ID,
				Mailbox:     draft.Mailbox,
				UIDValidity: draft.UIDValidity,
				UID:         draft.UID,
			}
			newReceipt = &r
		} else if draft.PendingAppendAttempted && (!draft.PendingUID.Valid || draft.PendingUID.Int64 == int64(draft.UID)) {
			// APPEND was attempted but Persist hasn't run (new uid not yet in table).
			// Use FindDraftAppend to locate the appended copy.
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
				_ = a.store.RecordIMAPDraftAppendContext(recordCtx, draft.DraftID, draft.Revision, newReceipt)
			} else {
				// Zero or multiple candidates: leave pending columns untouched.
				return draftReplyError("remote_unknown", errors.New("could not uniquely identify pending APPEND; retry later"))
			}
		}

		if newReceipt == nil {
			// No APPEND attempted yet; cannot complete.
			return draftReplyError("remote_unknown", errors.New("pending edit has no receipt to complete"))
		}

		// Locate the old message's raw bytes for the removal digest.
		// When Persist has run, current_message_id points to the new message so
		// we look up the old one via the membership table (which still holds the
		// old UID until Finish runs).
		var oldMessageID int64
		if persistDone {
			oldMessageID, _ = a.store.GetMessageIDByMembershipContext(ctx, target.source.ID, draft.Mailbox, oldUIDValidity, oldUID)
		}
		if oldMessageID == 0 {
			oldMessageID = draft.CurrentMessageID
		}

		oldRaw, rawErr := a.store.GetMessageRawContext(ctx, oldMessageID)
		if rawErr != nil {
			return draftReplyError("internal", fmt.Errorf("load old raw for resume %d: %w", draft.DraftID, rawErr))
		}
		oldDigest := sha256.Sum256(oldRaw)
		oldTarget := imaplib.DraftTarget{
			Mailbox:     draft.Mailbox,
			UIDValidity: oldUIDValidity,
			UID:         oldUID,
			RawSHA256:   oldDigest,
		}

		// P1-A + P2-E: use ctx for RemoveDraft; only Finish on success.
		removeResult, removeErr := client.RemoveDraft(ctx, oldTarget)
		if removeErr != nil {
			var dae *imaplib.DraftAppendError
			if errors.As(removeErr, &dae) {
				return draftReplyError(dae.Code, dae.Err)
			}
			return draftReplyError("remote_unknown", removeErr)
		}
		switch removeResult.State {
		case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
			// Success: proceed to Finish.
		default:
			return draftReplyError("delete_failed", fmt.Errorf("remote draft state is %s", removeResult.State))
		}

		// FinishIMAPDraftOperationContext uses the old UID (P1-C).
		// FinishIMAPDraftOperationContext will look up oldMessageID from the
		// membership it deletes; pass it as fallback in case the row is already gone.
		finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, draft.DraftID, draft.Revision, store.IMAPDraftOutcome{
			Lifecycle:   "active",
			SourceID:    target.source.ID,
			Mailbox:     draft.Mailbox,
			UIDValidity: oldUIDValidity,
			UID:         oldUID,
			MessageID:   oldMessageID,
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
		// Re-attempt the delete using the pending tuple as removal target (P1-C).
		// pending_uid was set to the old uid by BeginIMAPDraftOperationContext.
		var oldUID uint32
		var oldUIDValidity uint32
		if draft.PendingUID.Valid {
			oldUID = uint32(draft.PendingUID.Int64)
			oldUIDValidity = uint32(draft.PendingUIDValidity.Int64)
		} else {
			oldUID = draft.UID
			oldUIDValidity = draft.UIDValidity
		}

		currentRaw, rawErr := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
		if rawErr != nil {
			return draftReplyError("internal", fmt.Errorf("load current raw for resume delete %d: %w", draft.DraftID, rawErr))
		}
		digest := sha256.Sum256(currentRaw)
		oldTarget := imaplib.DraftTarget{
			Mailbox:     draft.Mailbox,
			UIDValidity: oldUIDValidity,
			UID:         oldUID,
			RawSHA256:   digest,
		}

		// P1-A + P2-E: use ctx; all DraftAppendErrors (including uidvalidity_changed)
		// must leave the pending state, not fall through to Finish.
		removeResult, removeErr := client.RemoveDraft(ctx, oldTarget)
		if removeErr != nil {
			var dae *imaplib.DraftAppendError
			if errors.As(removeErr, &dae) {
				// All protocol errors (including epoch mismatch) leave pending.
				return draftReplyError("delete_failed", dae.Err)
			}
			return draftReplyError("delete_failed", removeErr)
		}
		switch removeResult.State {
		case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
			// Success: proceed to local cleanup.
		default:
			// Soft failure (flag_missing or changed): leave pending.
			return draftReplyError("delete_failed", fmt.Errorf("remote draft state is %s", removeResult.State))
		}

		finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, draft.DraftID, draft.Revision, store.IMAPDraftOutcome{
			Lifecycle:   "discarded",
			SourceID:    target.source.ID,
			Mailbox:     draft.Mailbox,
			UIDValidity: oldUIDValidity,
			UID:         oldUID,
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
