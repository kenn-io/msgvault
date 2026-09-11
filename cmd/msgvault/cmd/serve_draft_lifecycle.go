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

// pendingEditStalenessThreshold is the minimum age for a pending-edit marker
// to be treated as an interrupted (crashed) operation. Holding the sync
// execution lock already proves no live caller is between Begin and Finish;
// this threshold guards against a lock-release/re-acquire race in the same
// wall-clock window by refusing to clear a marker younger than the maximum
// conceivable lock hold time.
const pendingEditStalenessThreshold = 5 * time.Minute

const draftLifecycleDiscarded = "discarded"

// Fixed recovery instructions for a failed or partially-completed mutation.
// They deliberately carry no revision: a revision read off a failure is only
// a snapshot, and any other writer can invalidate it before the retry lands,
// so the caller re-reads with draft-get exactly as the other conflict handlers
// in this repository require.
const (
	draftReloadInstruction = "reload the draft with draft-get before another attempt"

	draftInspectInstruction = "inspect the Drafts mailbox for an extra or leftover copy, " +
		"then reload the draft with draft-get before another attempt"

	// draftRemoveStaleInstruction is used only alongside a result that also
	// names the leftover copy's UID, and only after a live InspectDraft has
	// re-established that the UID still identifies the copy msgvault wrote. A
	// UID identifies a copy to remove in a mail client; it is not a number any
	// draft command accepts back.
	draftRemoveStaleInstruction = "remove the copy named by uid from the Drafts mailbox, " +
		"then reload the draft with draft-get before another attempt"

	// draftLocateDuplicateInstruction replaces draftRemoveStaleInstruction on
	// every path where a leftover copy may exist but no UID could be confirmed
	// live to still name it. A UID that fails that confirmation may identify a
	// different message — a changed mailbox epoch reassigns every UID — so the
	// operator is told how to recognize the copy instead of being handed a
	// number to delete.
	draftLocateDuplicateInstruction = "find the older copy of this draft in the Drafts mailbox " +
		"by its subject and date and remove only that copy, " +
		"then reload the draft with draft-get before another attempt"

	draftCompleteDiscardInstruction = "complete the discard with draft-delete, " +
		"then reload the draft with draft-get before another attempt"

	draftResolveEditInstruction = "resolve the pending edit with draft-edit, " +
		"then reload the draft with draft-get before another attempt"

	draftPendingWaitInstruction = "wait for the in-flight operation to finish, " +
		"then reload the draft with draft-get before another attempt"
)

// draftClient is the subset of imaplib.Client methods used by the draft reply
// and lifecycle commands. Using an interface enables test injection of custom
// behaviour (e.g. injecting a failing RemoveDraft) without requiring a real
// IMAP connection.
type draftClient interface {
	AppendDraft(ctx context.Context, mailbox string, raw []byte) (imaplib.DraftAppendResult, error)
	InspectDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error)
	SupportsAtomicDraftRemoval() bool
	RemoveDraft(ctx context.Context, target imaplib.DraftTarget) (imaplib.DraftInspectResult, error)
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
	Command  string // "draft-get", "draft-edit", "draft-delete"
	DraftID  int64
	Revision int64
	Body     string
	BodySet  bool
	JSON     bool
}

// draftLifecycleTarget holds the resolved draft and its source.
type draftLifecycleTarget struct {
	draft  *store.IMAPDraft
	source *store.Source
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
	// Instructions is the fixed recovery guidance for a failed or partially
	// completed operation. It never contains a revision, a UID, or any other
	// value a client could feed back into a retry.
	Instructions string `json:"instructions,omitempty"`
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
	var revisionSet, bodySet, jsonSet bool
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
	// Validate required flags per command.
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
	if revisionSet {
		rev, err := strconv.ParseInt(revisionStr, 10, 64)
		if err != nil || rev <= 0 {
			return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision must be a positive integer"))
		}
		intent.Revision = rev
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
	if intent.Command == api.CLIRunDraftEditCommand {
		identities, identErr := a.store.ListAccountIdentitiesContext(ctx, source.ID)
		if identErr != nil {
			return draftLifecycleTarget{}, draftReplyError("invalid_from", fmt.Errorf("list identities for source %d: %w", source.ID, identErr))
		}
		if draft.FromAddress != "" && !hasConfirmedSourceIdentity(identities, draft.FromAddress) {
			return draftLifecycleTarget{}, draftReplyError("invalid_from", fmt.Errorf(
				"draft From address is not a confirmed identity on source %d", source.ID))
		}
	}
	return draftLifecycleTarget{draft: draft, source: source}, nil
}

func (a *storeAPIAdapter) verifyDraftMailboxGrant(source *store.Source, draft *store.IMAPDraft) error {
	grantedMailbox, err := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType)
	if err != nil {
		return err
	}
	if grantedMailbox != draft.Mailbox {
		return draftReplyError("draft_disabled", fmt.Errorf(
			"draft %d mailbox %q does not match granted mailbox %q",
			draft.DraftID, draft.Mailbox, grantedMailbox))
	}
	return nil
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
		case "present", consentActive:
			text = fmt.Sprintf("draft %d: lifecycle=%s revision=%d provider=%s\n",
				output.DraftID, output.Lifecycle, output.Revision, output.ProviderStatus)
		case draftLifecycleDiscarded:
			if output.OperationRef == "" {
				text = fmt.Sprintf("draft %d is already discarded\n", output.DraftID)
			} else {
				text = fmt.Sprintf("draft %d discarded (operation %s)\n", output.DraftID, output.OperationRef)
			}
		case "replaced":
			text = fmt.Sprintf("draft %d replaced: new uid=%d revision=%d (operation %s)\n",
				output.DraftID, output.UID, output.Revision, output.OperationRef)
		default:
			// A failure result names the removable copy before the guidance
			// that refers to it, so the plain-text stream carries the same
			// operator-actionable content the JSON form does.
			details := make([]string, 0, 2)
			if output.UID != 0 {
				details = append(details, fmt.Sprintf("stale copy uid=%d", output.UID))
			}
			if output.Instructions != "" {
				details = append(details, output.Instructions)
			}
			text = fmt.Sprintf("draft %d: status=%s\n", output.DraftID, output.Status)
			if len(details) > 0 {
				text = fmt.Sprintf("draft %d: status=%s; %s\n",
					output.DraftID, output.Status, strings.Join(details, "; "))
			}
		}
	}
	return emit(api.CLIRunEvent{Type: stream, Data: text})
}

// refuseDraftLifecycle reports a refused draft operation to the client and
// returns the coded error the caller propagates.
//
// A CLIRunCodedError carries only its code across the daemon boundary:
// handleCLIRun logs the cause server-side and streams err.Error(), which is
// the code alone. Anything the operator has to act on therefore has to travel
// as a CLI event — the fixed recovery instruction, and the UID of a leftover
// copy when one is identifiable. staleUID is 0 when no copy is identifiable,
// and no instruction ever carries a revision.
func refuseDraftLifecycle(
	emit func(api.CLIRunEvent) error,
	intent draftLifecycleIntent,
	code string,
	instructions string,
	staleUID uint32,
	cause error,
) error {
	_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, draftLifecycleOutput{
		Status:       code,
		DraftID:      intent.DraftID,
		UID:          staleUID,
		Instructions: instructions,
	})
	return draftReplyError(code, cause)
}

// verifiedStaleDraftUID returns target.UID only when a live InspectDraft finds
// that the UID still identifies the copy msgvault wrote: the mailbox epoch this
// SELECT reports matches the recorded one, the \Draft flag is present, and the
// remote bytes hash to target.RawSHA256. InspectDraft is exactly that
// conjunction, so this asks it rather than deriving the answer a second way.
//
// Every other outcome returns 0, including an error, an unknown remote state, a
// changed epoch, a cleared flag, and different bytes. A caller must name no UID
// when this returns 0: under a changed epoch the same number identifies a
// different message, and naming it tells an operator to delete someone's mail.
func verifiedStaleDraftUID(ctx context.Context, client draftClient, target imaplib.DraftTarget) uint32 {
	if client == nil {
		return 0
	}
	result, err := client.InspectDraft(ctx, target)
	if err != nil || result.State != imaplib.DraftRemotePresent {
		return 0
	}
	return target.UID
}

// verifyInterruptedEditStaleCopy re-establishes, live, whether the pending
// receipt left by an interrupted edit still names the pre-edit copy. It opens
// its own client because this branch refuses before the edit path builds one.
//
// The digest cannot come from the draft's current raw: once Persist committed,
// current_message_id names the replacement, while the pending receipt names the
// copy that held the previous bytes. Those bytes belong to the message the
// pending receipt's membership still points at, which only Finish removes.
// It returns the live state even when the copy is confirmed absent, so recovery
// can distinguish a completed remote removal from an unresolved mailbox.
func (a *storeAPIAdapter) verifyInterruptedEditStaleCopy(
	ctx context.Context,
	source *store.Source,
	draft *store.IMAPDraft,
) (uint32, string) {
	pendingUIDValidity, ok := draftUIDValue(draft.PendingUIDValidity)
	if !ok {
		return 0, ""
	}
	pendingUID, ok := draftUIDValue(draft.PendingUID)
	if !ok {
		return 0, ""
	}
	messageID, err := a.store.GetIMAPDraftPendingMessageIDContext(
		ctx, source.ID, draft.Mailbox, pendingUIDValidity, pendingUID)
	var rawSHA256 [32]byte
	if err == nil {
		raw, rawErr := a.store.GetMessageRawContext(ctx, messageID)
		if rawErr != nil {
			return 0, ""
		}
		rawSHA256 = sha256.Sum256(raw)
	} else if opserr.KindOf(err) != opserr.KindNotFound {
		return 0, ""
	}
	client, err := a.lifecycleDraftClient(ctx, source)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = client.Close() }()
	result, err := client.InspectDraft(ctx, imaplib.DraftTarget{
		Mailbox:     draft.Mailbox,
		UIDValidity: pendingUIDValidity,
		UID:         pendingUID,
		RawSHA256:   rawSHA256,
	})
	if err != nil {
		return 0, ""
	}
	if result.State == imaplib.DraftRemotePresent {
		return pendingUID, result.State
	}
	return 0, result.State
}

func draftUIDValue(value sql.NullInt64) (uint32, bool) {
	if !value.Valid || value.Int64 < 0 || value.Int64 > int64(^uint32(0)) {
		return 0, false
	}
	return uint32(value.Int64), true
}

// draftInspectFailureInstruction returns the recovery instruction for a
// pre-Begin InspectDraft failure, or "" when nothing in the mailbox and nothing
// in the local record has to change before another attempt.
//
// The two actionable codes are actionable for different reasons.
// uidvalidity_changed means every UID the caller holds — including the one
// draft-get last reported — now names a different message, so the caller has to
// re-read before acting on any of them. remote_unknown means the mailbox state
// this operation depends on was never established, so the caller has to look at
// the mailbox before deciding what a retry would do. The remaining codes
// (invalid_mailbox, uidplus_required, cancelled) describe the configuration or
// the request rather than the mailbox, and leave nothing to recover.
func draftInspectFailureInstruction(code string) string {
	switch code {
	case "uidvalidity_changed":
		return draftReloadInstruction
	case "remote_unknown":
		return draftInspectInstruction
	}
	return ""
}

// refuseDraftInspectFailure reports a pre-Begin InspectDraft failure, routing
// the actionable codes through refuseDraftLifecycle so the instruction reaches
// the caller. A CLIRunCodedError carries only its code across the daemon
// boundary, so a code returned bare arrives as a bare code.
func refuseDraftInspectFailure(
	emit func(api.CLIRunEvent) error,
	intent draftLifecycleIntent,
	err error,
) error {
	code, cause := "remote_unknown", err
	if appendErr, ok := errors.AsType[*imaplib.DraftAppendError](err); ok {
		code, cause = appendErr.Code, appendErr.Err
	}
	if instructions := draftInspectFailureInstruction(code); instructions != "" {
		return refuseDraftLifecycle(emit, intent, code, instructions, 0, cause)
	}
	return draftReplyError(code, cause)
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
		return a.runCLIDraftEdit(ctx, intent, emit)
	case api.CLIRunDraftDeleteCommand:
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
	if draft.Lifecycle == draftLifecycleDiscarded {
		output := draftLifecycleOutput{
			Status:          draftLifecycleDiscarded,
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
	if grantedMailbox, grantErr := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType); grantErr != nil {
		providerStatus = "not_checked"
	} else if grantedMailbox != draft.Mailbox {
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

	// Revision is the stored current revision with no arithmetic, including
	// while a pending marker is set. It is the value an ordinary mutation must
	// present as --revision now; it is not a promise that a later call will
	// still accept it, because another writer may advance the draft first.
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

	// Acquire the sync execution context.
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	defer func() { _ = execution.Release() }()

	if err := a.store.RefreshIMAPDraftReceiptContext(ctx, intent.DraftID); err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", fmt.Errorf("refresh draft %d: %w", intent.DraftID, err))
	}
	// Reload draft under the lock to get authoritative pending state. Any
	// pending marker visible here must be from a crashed prior holder because
	// holding the sync execution lock guarantees no other caller is currently
	// between Begin and Finish.
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", fmt.Errorf("reload draft %d: %w", intent.DraftID, err))
	}
	if err := a.verifyDraftMailboxGrant(target.source, draft); err != nil {
		return err
	}
	// A discarded draft is a terminal local state, decided here so no IMAP
	// connection is opened for a draft that no longer exists. A stale revision
	// stays an ordinary reload-and-retry conflict.
	if draft.Lifecycle == draftLifecycleDiscarded {
		if intent.Revision != draft.Revision {
			return refuseDraftLifecycle(emit, intent, "revision_conflict", draftReloadInstruction, 0,
				fmt.Errorf("draft %d: revision %d is stale; %s", intent.DraftID, intent.Revision, draftReloadInstruction))
		}
		// Terminal: the draft has no remote copy and no later attempt can
		// succeed, so there is nothing for the operator to act on.
		return draftReplyError("draft_missing",
			fmt.Errorf("draft %d is discarded and has no remote copy to edit", intent.DraftID))
	}
	if draft.PendingKind.Valid && draft.PendingKind.String != "" {
		switch draft.PendingKind.String {
		case "discard":
			return refuseDraftLifecycle(emit, intent, "operation_pending", draftCompleteDiscardInstruction, 0,
				fmt.Errorf("draft %d has a pending discard; use draft-delete to complete it", intent.DraftID))
		case "edit":
			// A marker younger than pendingEditStalenessThreshold is treated
			// conservatively as a potentially-live operation (defense-in-depth).
			if draft.PendingStartedAt.Valid && time.Since(draft.PendingStartedAt.Time) < pendingEditStalenessThreshold {
				return refuseDraftLifecycle(emit, intent, "operation_pending", draftPendingWaitInstruction, 0,
					fmt.Errorf("draft %d has a recent pending edit", intent.DraftID))
			}
			pendingUID, pendingUIDOK := draftUIDValue(draft.PendingUID)
			pendingUIDValidity, pendingUIDValidityOK := draftUIDValue(draft.PendingUIDValidity)
			if !pendingUIDOK || !pendingUIDValidityOK {
				return refuseDraftLifecycle(emit, intent, "edit_recovery_unverifiable", draftLocateDuplicateInstruction, 0,
					fmt.Errorf("draft %d has an invalid pending edit receipt", intent.DraftID))
			}
			verifiedUID, verifiedState := a.verifyInterruptedEditStaleCopy(ctx, target.source, draft)
			if verifiedUID == 0 && verifiedState != imaplib.DraftRemoteAbsent {
				return refuseDraftLifecycle(emit, intent, "edit_recovery_unverifiable", draftLocateDuplicateInstruction, 0,
					fmt.Errorf("draft %d pending edit copy could not be verified; keep the pending marker until the mailbox is resolved", intent.DraftID))
			}
			if verifiedState == imaplib.DraftRemoteAbsent && pendingUID != draft.UID {
				finishErr := a.store.FinishIMAPDraftOperationContext(context.WithoutCancel(ctx), intent.DraftID, draft.Revision, store.IMAPDraftOutcome{
					Lifecycle:   "active",
					SourceID:    target.source.ID,
					Mailbox:     draft.Mailbox,
					UIDValidity: pendingUIDValidity,
					UID:         pendingUID,
					MessageID:   draft.CurrentMessageID,
				})
				if finishErr != nil {
					return refuseDraftLifecycle(emit, intent, "remote_accepted_local_failed", draftReloadInstruction, 0, finishErr)
				}
			} else if clearErr := a.store.ClearIMAPDraftPendingEditContext(ctx, intent.DraftID); clearErr != nil {
				// The marker survives, and the interrupted edit it records may
				// have left a second copy in the mailbox, so the operator has
				// both something to look at and something to re-read.
				return refuseDraftLifecycle(emit, intent, "internal", draftInspectInstruction, 0,
					fmt.Errorf("clear pending edit for draft %d: %w", intent.DraftID, clearErr))
			}
			// Recovery only clears the marker. It never applies the body this
			// call supplied and never reuses the revision this call supplied,
			// so the caller must re-read the draft before editing again.
			//
			// pending_uid differing from uid means Persist committed, so the
			// pending receipt names the pre-edit copy rather than the live one.
			// The live verification above settles whether that UID can be named.
			if pendingUID != draft.UID && verifiedState != imaplib.DraftRemoteAbsent {
				return refuseDraftLifecycle(emit, intent, "edit_interrupted", draftRemoveStaleInstruction,
					verifiedUID,
					fmt.Errorf("draft %d had an interrupted edit and the requested body was not applied; stale copy UID=%d may remain in the Drafts mailbox — remove it, then %s",
						intent.DraftID, verifiedUID, draftReloadInstruction))
			}
			return refuseDraftLifecycle(emit, intent, "edit_interrupted", draftInspectInstruction, 0,
				fmt.Errorf("draft %d had an interrupted edit and the requested body was not applied; an untracked duplicate may remain in the Drafts mailbox — the tracked copy is the one local state still points at, so remove a duplicate only if you see one, then %s",
					intent.DraftID, draftReloadInstruction))
		}
	}

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
	newDraft, err := imaplib.ReplaceDraftBody(oldRaw, intent.Body, time.Now())
	if err != nil {
		if err.Error() == "invalid_message" {
			return draftReplyError("invalid_message", err)
		}
		return draftReplyError("invalid_reply_metadata", err)
	}
	if len(newDraft.Parsed.From) != 1 {
		return draftReplyError("invalid_reply_metadata",
			errors.New("composed draft needs exactly one From address"))
	}
	messageIDValue := mime.NormalizeMessageID(newDraft.Parsed.MessageID)
	if messageIDValue == "" {
		return draftReplyError("invalid_reply_metadata", errors.New("composed draft has no usable Message-ID"))
	}
	messageIDValue = "<" + messageIDValue + ">"
	oldDigest := sha256.Sum256(oldRaw)
	oldTarget := imaplib.DraftTarget{
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		RawSHA256:   oldDigest,
	}
	inspectResult, err := client.InspectDraft(ctx, oldTarget)
	if err != nil {
		return refuseDraftInspectFailure(emit, intent, err)
	}
	switch inspectResult.State {
	case imaplib.DraftRemoteAbsent:
		return refuseDraftLifecycle(emit, intent, "draft_missing", draftReloadInstruction, 0,
			fmt.Errorf("draft %d: remote copy absent", intent.DraftID))
	case imaplib.DraftRemoteFlagMissing, imaplib.DraftRemoteChanged:
		return refuseDraftLifecycle(emit, intent, "draft_changed", draftReloadInstruction, 0,
			fmt.Errorf("draft %d: remote copy modified externally", intent.DraftID))
	}
	if !client.SupportsAtomicDraftRemoval() {
		return refuseDraftInspectFailure(emit, intent, &imaplib.DraftAppendError{
			State: imaplib.DraftRemotePresent, Code: "atomic_expunge_required",
			Err: errors.New("atomic conditional draft removal is unavailable"),
		})
	}

	// 5. BeginIMAPDraftOperationContext.
	claimedDraft, err := a.store.BeginIMAPDraftOperationContext(ctx, store.IMAPDraftIntent{
		DraftID:          intent.DraftID,
		ExpectedRevision: intent.Revision,
		Kind:             "edit",
	})
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		msg := err.Error()
		switch {
		case strings.Contains(msg, "revision_conflict"):
			return refuseDraftLifecycle(emit, intent, "revision_conflict", draftReloadInstruction, 0,
				fmt.Errorf("%w; %s", err, draftReloadInstruction))
		case strings.Contains(msg, "operation_pending"):
			return refuseDraftLifecycle(emit, intent, "operation_pending", draftPendingWaitInstruction, 0, err)
		case strings.Contains(msg, "draft_discarded"):
			// Raced with a concurrent discard between the reload and the claim.
			return refuseDraftLifecycle(emit, intent, "draft_missing", draftReloadInstruction, 0,
				fmt.Errorf("draft %d was discarded concurrently: %w", intent.DraftID, err))
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
	} else {
		// The claim is already recorded, and an APPEND whose outcome the server
		// did not confirm may still have landed a copy, so both arms carry the
		// inspect-and-reload guidance.
		if dae, ok := errors.AsType[*imaplib.DraftAppendError](appendErr); ok {
			return refuseDraftLifecycle(emit, intent, dae.Code, draftInspectInstruction, 0, dae.Err)
		}
		return refuseDraftLifecycle(emit, intent, "remote_unknown", draftInspectInstruction, 0, appendErr)
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
			Instructions: draftInspectInstruction,
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("remote_accepted_local_failed", persistErr)
	}
	_ = newMessageID

	// Re-inspect the old copy to confirm it's still the same before deleting.
	// P2-E: use ctx (not recordCtx) so the remote call is cancellable.
	// P1-A: only call Finish when RemoveDraft confirms absent or present (expunged).
	//
	// Control only reaches here with persistErr == nil, so the new body is live
	// both remotely and locally: current_message_id, uid, and uidvalidity all
	// name the new copy. The only outcome still at risk is the removal of the
	// pre-edit copy, which fails in the direction of a leftover old copy and
	// never in the direction of an unapplied edit.
	removeResult, removeErr := client.RemoveDraft(ctx, oldTarget)
	if removeErr != nil {
		// Leave the pending marker set. draft-delete refuses a pending edit and
		// redirects to draft-edit, so a later draft-edit is what clears the
		// marker, and it names this leftover copy because pending_uid no longer
		// equals uid.
		logger.Error("remove old draft copy failed", "draft_id", intent.DraftID, "error", removeErr)
		cause := removeErr
		if dae, ok := errors.AsType[*imaplib.DraftAppendError](removeErr); ok {
			cause = fmt.Errorf("%s: %w", dae.Code, dae.Err)
		}
		// A failed removal leaves the remote side unsettled: the STORE may have
		// landed, the EXPUNGE may have landed, the connection may have died
		// before either, or the epoch may have moved. The pre-edit UID is a
		// removal instruction addressed to a human, so it is named only when a
		// live InspectDraft still finds the pre-edit bytes under that UID.
		if staleUID := verifiedStaleDraftUID(ctx, client, oldTarget); staleUID != 0 {
			return reportEditAppliedOldCopyRemains(emit, intent, *newReceipt,
				staleUID, draftRemoveStaleInstruction, cause)
		}
		return reportEditAppliedOldCopyRemains(emit, intent, *newReceipt,
			0, draftLocateDuplicateInstruction, cause)
	}
	switch removeResult.State {
	case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
		// Success: absent means already gone; present means just expunged.
	default:
		// Soft failure (flag_missing or changed): leave pending state. The old
		// copy no longer holds the bytes msgvault wrote, so it is not named as
		// removable; the operator inspects it instead.
		logger.Error("remove old draft copy soft fail", "draft_id", intent.DraftID, "state", removeResult.State)
		return reportEditAppliedOldCopyRemains(emit, intent, *newReceipt, 0, draftInspectInstruction,
			fmt.Errorf("remote draft state changed during edit: %s", removeResult.State))
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
		// The remote is correct and holds one copy; only the local record was
		// left unfinished, so there is nothing extra in the mailbox to remove.
		return refuseDraftLifecycle(emit, intent, "remote_accepted_local_failed",
			draftReloadInstruction, 0, finishErr)
	}

	// Release lock before cache refresh.
	if err := execution.Release(); err != nil {
		logger.Error("release source after draft edit", "source_id", target.source.ID, "error", err)
	}

	output := draftLifecycleOutput{
		Status:          "replaced",
		DraftID:         intent.DraftID,
		Lifecycle:       "active",
		Revision:        claimedDraft.Revision,
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

// reportEditAppliedOldCopyRemains reports the one partial outcome an edit can
// reach once its APPEND and its local persist have both committed: the new body
// is live on the server and in the local record, and only the pre-edit copy
// could not be removed. It is the inverse of remote_accepted_local_failed,
// where the server holds the new copy and the local record does not.
//
// staleUID names the leftover copy only when the caller has re-established that
// condition live through verifiedStaleDraftUID, and is 0 otherwise — including
// when the removal's outcome is unknown, when the epoch moved, and when another
// client changed the copy. A 0 here must be paired with an instruction that
// names no UID.
func reportEditAppliedOldCopyRemains(
	emit func(api.CLIRunEvent) error,
	intent draftLifecycleIntent,
	newReceipt store.IMAPDraftReceipt,
	staleUID uint32,
	instructions string,
	cause error,
) error {
	const code = "edit_applied_old_copy_remains"
	_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, draftLifecycleOutput{
		Status:       code,
		DraftID:      intent.DraftID,
		Lifecycle:    "active",
		OperationRef: draftOperationRef(newReceipt),
		UID:          staleUID,
		Instructions: instructions,
	})
	return draftReplyError(code, cause)
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

	// Acquire the sync execution context BEFORE checking pending state so that
	// the pending-kind read and any subsequent Begin are both inside the same
	// mutual-exclusion window. No concurrent caller can be between Begin and
	// Finish while we hold the lock.
	execution, err := a.store.AcquireSyncExecutionContext(ctx, target.source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", fmt.Errorf("source %d: %w", target.source.ID, err))
		}
		return draftReplyError("sync_lock_failed", fmt.Errorf("source %d: %w", target.source.ID, err))
	}
	lockReleased := false
	defer func() {
		if !lockReleased {
			_ = execution.Release()
		}
	}()

	if err := a.store.RefreshIMAPDraftReceiptContext(ctx, intent.DraftID); err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", fmt.Errorf("refresh draft %d: %w", intent.DraftID, err))
	}
	// Reload draft under the lock for authoritative pending state.
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", fmt.Errorf("reload draft %d: %w", intent.DraftID, err))
	}
	target.draft = draft
	if err := a.verifyDraftMailboxGrant(target.source, draft); err != nil {
		return err
	}

	// A discarded draft is a terminal local state, decided here so no IMAP
	// connection is opened for a draft that no longer exists. Deleting an
	// already-discarded draft at its current revision is the idempotent
	// success the state table records; a stale revision stays an ordinary
	// reload-and-retry conflict.
	if draft.Lifecycle == draftLifecycleDiscarded {
		if intent.Revision != draft.Revision {
			return refuseDraftLifecycle(emit, intent, "revision_conflict", draftReloadInstruction, 0,
				fmt.Errorf("draft %d: revision %d is stale; %s", intent.DraftID, intent.Revision, draftReloadInstruction))
		}
		lockReleased = true
		if releaseErr := execution.Release(); releaseErr != nil {
			logger.Error("release source after discarded draft delete", "source_id", target.source.ID, "error", releaseErr)
		}
		output := draftLifecycleOutput{
			Status:    draftLifecycleDiscarded,
			DraftID:   intent.DraftID,
			Lifecycle: draftLifecycleDiscarded,
		}
		if emitErr := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); emitErr != nil {
			return draftReplyError("output_failed", emitErr)
		}
		return nil
	}

	// Handle a prior interrupted operation (under the lock so state is authoritative).
	if draft.PendingKind.Valid && draft.PendingKind.String != "" {
		switch draft.PendingKind.String {
		case "edit":
			return refuseDraftLifecycle(emit, intent, "operation_pending", draftResolveEditInstruction, 0,
				fmt.Errorf("draft %d has a pending edit; use draft-edit to resolve it", intent.DraftID))
		case "discard":
			// Pass the held execution into the replay so the entire operation
			// runs under one uninterrupted lock. The defer above must not
			// also call Release, so mark it handled here.
			lockReleased = true
			return a.replayPendingDiscard(ctx, execution, intent, target, emit)
		}
	}

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
		return refuseDraftInspectFailure(emit, intent, err)
	}
	switch inspectResult.State {
	case imaplib.DraftRemoteAbsent:
		// For delete, absent is acceptable (idempotent). Continue to local cleanup.
	case imaplib.DraftRemoteFlagMissing, imaplib.DraftRemoteChanged:
		return refuseDraftLifecycle(emit, intent, "draft_changed", draftReloadInstruction, 0,
			fmt.Errorf("draft %d: remote copy modified externally", intent.DraftID))
	}
	if inspectResult.State == imaplib.DraftRemotePresent {
		if !client.SupportsAtomicDraftRemoval() {
			return refuseDraftInspectFailure(emit, intent, &imaplib.DraftAppendError{
				State: imaplib.DraftRemotePresent, Code: "atomic_expunge_required",
				Err: errors.New("atomic conditional draft removal is unavailable"),
			})
		}
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
			return refuseDraftLifecycle(emit, intent, "revision_conflict", draftReloadInstruction, 0,
				fmt.Errorf("%w; %s", err, draftReloadInstruction))
		case strings.Contains(err.Error(), "operation_pending"):
			return refuseDraftLifecycle(emit, intent, "operation_pending", draftPendingWaitInstruction, 0, err)
		case strings.Contains(err.Error(), "draft_discarded"):
			// Raced with a concurrent discard between the reload and the claim.
			return refuseDraftLifecycle(emit, intent, "draft_discarded", draftReloadInstruction, 0,
				fmt.Errorf("draft %d was discarded concurrently: %w", intent.DraftID, err))
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
		// Leave pending state set; caller reloads and retries draft-delete.
		cause := removeErr
		if dae, ok := errors.AsType[*imaplib.DraftAppendError](removeErr); ok {
			cause = dae.Err
		}
		output := draftLifecycleOutput{
			Status:       "delete_failed",
			DraftID:      intent.DraftID,
			Lifecycle:    "active",
			Instructions: draftInspectInstruction,
		}
		// The operation reference is the copy's live coordinates. A failed
		// removal does not establish that they still are, so they are reported
		// only when a live InspectDraft still finds the owned copy there.
		if verifiedStaleDraftUID(ctx, client, oldTarget) != 0 {
			output.OperationRef = draftOperationRef(store.IMAPDraftReceipt{
				SourceID: target.source.ID, Mailbox: draft.Mailbox,
				UIDValidity: draft.UIDValidity, UID: draft.UID,
			})
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("delete_failed", cause)
	}
	// P1-A: soft failures (flag_missing or changed) mean the remote copy was
	// externally modified after InspectDraft confirmed it; leave pending.
	switch removeResult.State {
	case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
		// Success: proceed to local cleanup.
	default:
		output := draftLifecycleOutput{
			Status:       "delete_failed",
			DraftID:      intent.DraftID,
			Lifecycle:    "active",
			Instructions: draftInspectInstruction,
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("delete_failed", fmt.Errorf("remote draft state changed: %s", removeResult.State))
	}

	// FinishIMAPDraftOperationContext.
	finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, intent.DraftID, claimedDraft.Revision, store.IMAPDraftOutcome{
		Lifecycle:   draftLifecycleDiscarded,
		SourceID:    target.source.ID,
		Mailbox:     draft.Mailbox,
		UIDValidity: draft.UIDValidity,
		UID:         draft.UID,
		MessageID:   draft.CurrentMessageID,
	})
	if finishErr != nil {
		logger.Error("finish draft delete operation", "draft_id", intent.DraftID, "error", finishErr)
		// The server copy is gone; only the local record is unfinished, and
		// another draft-delete at the reloaded revision completes it.
		return refuseDraftLifecycle(emit, intent, "remote_deleted_local_failed",
			draftCompleteDiscardInstruction, 0, finishErr)
	}

	// Release lock before cache refresh.
	lockReleased = true
	if err := execution.Release(); err != nil {
		logger.Error("release source after draft delete", "source_id", target.source.ID, "error", err)
	}

	output := draftLifecycleOutput{
		Status:       draftLifecycleDiscarded,
		DraftID:      intent.DraftID,
		Lifecycle:    draftLifecycleDiscarded,
		OperationRef: draftOperationRef(store.IMAPDraftReceipt{SourceID: target.source.ID, Mailbox: draft.Mailbox, UIDValidity: draft.UIDValidity, UID: draft.UID}),
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	a.refreshDraftCache(recordCtx, target.source)
	return nil
}

// replayPendingDiscard re-attempts a draft-delete whose Begin ran but whose
// RemoveDraft+Finish did not complete. It uses the execution lock already held
// by the caller so there is no gap between the pending-kind decision and the
// re-read. The caller must set its own lockReleased flag before calling here.
func (a *storeAPIAdapter) replayPendingDiscard(
	ctx context.Context,
	execution *store.SyncExecution,
	intent draftLifecycleIntent,
	target draftLifecycleTarget,
	emit func(api.CLIRunEvent) error,
) error {
	replayReleased := false
	defer func() {
		if !replayReleased {
			_ = execution.Release()
		}
	}()

	hasMembership, err := a.store.RefreshIMAPDraftDiscardReceiptContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", fmt.Errorf("refresh pending discard for draft %d: %w", intent.DraftID, err))
	}
	// Reload under the lock for authoritative revision and pending coordinates.
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if opserr.KindOf(err) == opserr.KindNotFound {
			return draftReplyError("draft_not_found", err)
		}
		return draftReplyError("internal", fmt.Errorf("reload draft %d for replay: %w", intent.DraftID, err))
	}
	if err := a.verifyDraftMailboxGrant(target.source, draft); err != nil {
		return err
	}
	// The supplied revision is validated against the stored current revision,
	// exactly as an ordinary mutation is. A caller still holding the pre-Begin
	// revision gets a conflict, reloads with draft-get, and supplies the
	// current value; there is no second meaning for --revision here.
	if intent.Revision != draft.Revision {
		return refuseDraftLifecycle(emit, intent, "revision_conflict", draftReloadInstruction, 0,
			fmt.Errorf("draft %d: revision %d is stale; %s", intent.DraftID, intent.Revision, draftReloadInstruction))
	}

	// The schema CHECK guarantees pending_uid IS NOT NULL when pending_kind IS NOT NULL,
	// so PendingUID is always valid here and no fallback to draft.UID is needed.
	oldUID, uidOK := draftUIDValue(draft.PendingUID)
	oldUIDValidity, uidValidityOK := draftUIDValue(draft.PendingUIDValidity)
	if !uidOK || !uidValidityOK {
		return refuseDraftLifecycle(emit, intent, "internal", draftCompleteDiscardInstruction, 0,
			fmt.Errorf("draft %d has an invalid pending IMAP receipt", intent.DraftID))
	}

	// The pending marker records a discard this daemon still owes, so a failure
	// to reach the mailbox leaves owed work the operator has to drive to
	// completion rather than a dead end.
	client, err := a.lifecycleDraftClient(ctx, target.source)
	if err != nil {
		return refuseDraftLifecycle(emit, intent, "invalid_source", draftCompleteDiscardInstruction, 0,
			fmt.Errorf("build IMAP client for source %d: %w", target.source.ID, err))
	}
	defer func() { _ = client.Close() }()

	currentRaw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return refuseDraftLifecycle(emit, intent, "internal", draftCompleteDiscardInstruction, 0,
			fmt.Errorf("load current raw for draft %d: %w", intent.DraftID, err))
	}
	digest := sha256.Sum256(currentRaw)
	oldTarget := imaplib.DraftTarget{
		Mailbox:     draft.Mailbox,
		UIDValidity: oldUIDValidity,
		UID:         oldUID,
		RawSHA256:   digest,
	}

	// A failed replay leaves the pending marker set and reports the same
	// numberless guidance as the ordinary delete path. The operation reference
	// is the copy's live coordinates, so it is reported only when a live
	// InspectDraft still finds the owned copy at the pending receipt.
	failReplay := func(cause error) error {
		output := draftLifecycleOutput{
			Status:       "delete_failed",
			DraftID:      draft.DraftID,
			Lifecycle:    "active",
			Instructions: draftInspectInstruction,
		}
		if verifiedStaleDraftUID(ctx, client, oldTarget) != 0 {
			output.OperationRef = draftOperationRef(store.IMAPDraftReceipt{
				SourceID: target.source.ID, Mailbox: draft.Mailbox,
				UIDValidity: oldUIDValidity, UID: oldUID,
			})
		}
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("delete_failed", cause)
	}

	var removeResult imaplib.DraftInspectResult
	if !hasMembership {
		inspectResult, inspectErr := client.InspectDraft(ctx, oldTarget)
		if inspectErr != nil {
			return failReplay(inspectErr)
		}
		if inspectResult.State != imaplib.DraftRemoteAbsent {
			return refuseDraftLifecycle(emit, intent, "discard_recovery_unverifiable", draftInspectInstruction, 0,
				fmt.Errorf("draft %d has no current archived membership and its pending receipt is not absent", intent.DraftID))
		}
		removeResult = inspectResult
	} else {
		inspectResult, inspectErr := client.InspectDraft(ctx, oldTarget)
		if inspectErr != nil {
			return failReplay(inspectErr)
		}
		if inspectResult.State == imaplib.DraftRemoteFlagMissing || inspectResult.State == imaplib.DraftRemoteChanged {
			return failReplay(errors.New("remote draft changed during pending discard recovery"))
		}
		if inspectResult.State == imaplib.DraftRemotePresent {
			if !client.SupportsAtomicDraftRemoval() {
				return refuseDraftInspectFailure(emit, intent, &imaplib.DraftAppendError{
					State: imaplib.DraftRemotePresent, Code: "atomic_expunge_required",
					Err: errors.New("atomic conditional draft removal is unavailable"),
				})
			}
		}

		var removeErr error
		removeResult, removeErr = client.RemoveDraft(ctx, oldTarget)
		if removeErr != nil {
			if dae, ok := errors.AsType[*imaplib.DraftAppendError](removeErr); ok {
				return failReplay(dae.Err)
			}
			return failReplay(removeErr)
		}
	}
	switch removeResult.State {
	case imaplib.DraftRemoteAbsent, imaplib.DraftRemotePresent:
		// proceed
	default:
		return failReplay(fmt.Errorf("remote draft state is %s", removeResult.State))
	}

	recordCtx := context.WithoutCancel(ctx)
	finishErr := a.store.FinishIMAPDraftOperationContext(recordCtx, draft.DraftID, draft.Revision, store.IMAPDraftOutcome{
		Lifecycle:   draftLifecycleDiscarded,
		SourceID:    target.source.ID,
		Mailbox:     draft.Mailbox,
		UIDValidity: oldUIDValidity,
		UID:         oldUID,
		MessageID:   draft.CurrentMessageID,
	})
	if finishErr != nil {
		logger.Error("finish replayed draft discard", "draft_id", draft.DraftID, "error", finishErr)
		// The server copy is gone; only the local record is unfinished, and
		// another draft-delete at the reloaded revision completes it.
		return refuseDraftLifecycle(emit, intent, "remote_deleted_local_failed",
			draftCompleteDiscardInstruction, 0, finishErr)
	}

	replayReleased = true
	if err := execution.Release(); err != nil {
		logger.Error("release source after replay discard", "source_id", target.source.ID, "error", err)
	}

	output := draftLifecycleOutput{
		Status:       draftLifecycleDiscarded,
		DraftID:      draft.DraftID,
		Lifecycle:    draftLifecycleDiscarded,
		OperationRef: draftOperationRef(store.IMAPDraftReceipt{SourceID: target.source.ID, Mailbox: draft.Mailbox, UIDValidity: oldUIDValidity, UID: oldUID}),
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	a.refreshDraftCache(recordCtx, target.source)
	return nil
}
