package cmd

import (
	"context"
	"database/sql"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type draftLifecycleIntent struct {
	Operation string
	DraftID   string
	Revision  int64
	Body      string
	JSON      bool
}

const draftLifecycleActive = "active"

type draftLifecycleReceipt struct {
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uidvalidity"`
	UID         uint32 `json:"uid"`
}

type draftLifecycleObservation struct {
	State       string   `json:"state"`
	Code        string   `json:"code,omitempty"`
	Mailbox     string   `json:"mailbox,omitempty"`
	UIDValidity uint32   `json:"uidvalidity,omitempty"`
	UID         uint32   `json:"uid,omitempty"`
	Flags       []string `json:"flags,omitempty"`
	Present     bool     `json:"present"`
	Draft       bool     `json:"draft"`
	Deleted     bool     `json:"deleted"`
	Complete    bool     `json:"complete"`
	UIDPlus     bool     `json:"uidplus"`
}

type draftLifecycleOutput struct {
	Status               string                     `json:"status"`
	DraftID              string                     `json:"draft_id"`
	Revision             int64                      `json:"revision"`
	Lifecycle            string                     `json:"lifecycle"`
	MessageID            int64                      `json:"message_id"`
	SourceID             int64                      `json:"source_id"`
	Receipt              draftLifecycleReceipt      `json:"receipt"`
	Content              string                     `json:"content,omitempty"`
	RawMIME              string                     `json:"raw_mime,omitempty"`
	CandidateContent     string                     `json:"candidate_content,omitempty"`
	PendingOperation     string                     `json:"pending_operation,omitempty"`
	PendingCode          string                     `json:"pending_code,omitempty"`
	RefusalCode          string                     `json:"refusal_code,omitempty"`
	PendingReceipt       *draftLifecycleReceipt     `json:"pending_receipt,omitempty"`
	ProviderObservation  *draftLifecycleObservation `json:"provider_observation,omitempty"`
	Observation          *draftLifecycleObservation `json:"observation,omitempty"`
	ManualReconciliation bool                       `json:"manual_reconciliation,omitempty"`
}

func parseDraftLifecycleArgs(args []string) (draftLifecycleIntent, error) {
	if !api.IsCLIRunDraftLifecycle(args) {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("expected a draft lifecycle command"))
	}
	intent := draftLifecycleIntent{Operation: args[0]}
	var positional string
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
			if !hasValue {
				if len(rest) == 0 {
					return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision requires a value"))
				}
				value, rest = rest[0], rest[1:]
			}
			if revisionSet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision given more than once"))
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--revision must be a positive integer"))
			}
			intent.Revision, revisionSet = parsed, true
		case "body":
			if !hasValue {
				if len(rest) == 0 {
					return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--body requires a value"))
				}
				value, rest = rest[0], rest[1:]
			}
			if bodySet {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--body given more than once"))
			}
			intent.Body, bodySet = value, true
		case "json":
			if jsonSet || (hasValue && value != "true") {
				return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("--json accepts one flag without a value"))
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
	if positional == "" || !utf8.ValidString(positional) || strings.TrimSpace(positional) == "" || strings.ContainsAny(positional, "\x00\r\n") {
		return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft ID is required"))
	}
	intent.DraftID = positional
	switch intent.Operation {
	case api.CLIRunDraftGetCommand:
		if revisionSet || bodySet {
			return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft-get accepts only --json"))
		}
	case api.CLIRunDraftEditCommand:
		if !revisionSet || !bodySet {
			return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft-edit requires --revision and --body"))
		}
	case api.CLIRunDraftDeleteCommand:
		if !revisionSet || bodySet {
			return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft-delete requires --revision and no body"))
		}
	case api.CLIRunDraftRecoverCommand:
		if !revisionSet || bodySet {
			return draftLifecycleIntent{}, draftReplyError("invalid_args", errors.New("draft-recover requires --revision and no body"))
		}
	}
	return intent, nil
}

func draftLifecycleReceiptOutput(receipt store.IMAPDraftReceipt) draftLifecycleReceipt {
	return draftLifecycleReceipt{Mailbox: receipt.Mailbox, UIDValidity: receipt.UIDValidity, UID: receipt.UID}
}

func draftLifecycleObservationOutput(observation imaplib.DraftObservation) *draftLifecycleObservation {
	flags := make([]string, len(observation.Flags))
	for i, flag := range observation.Flags {
		flags[i] = string(flag)
	}
	return &draftLifecycleObservation{
		State: observation.State, Code: observation.Code,
		Mailbox: observation.Mailbox, UIDValidity: observation.UIDValidity,
		UID: observation.UID, Flags: flags, Present: observation.Present,
		Draft: observation.Draft, Deleted: observation.Deleted,
		Complete: observation.Complete, UIDPlus: observation.UIDPlus,
	}
}

func draftLifecycleObservationCode(observation imaplib.DraftObservation, fallback string) string {
	if observation.Code != "" {
		return observation.Code
	}
	return fallback
}

func draftLifecycleMetadata(
	draft store.IMAPDraft,
	status string,
	providerObservation *draftLifecycleObservation,
	observation *draftLifecycleObservation,
) draftLifecycleOutput {
	lifecycle := draftLifecycleActive
	if draft.DiscardedAt != nil {
		lifecycle = "discarded"
	}
	output := draftLifecycleOutput{
		Status: status, DraftID: draft.DraftID, Revision: draft.Revision,
		Lifecycle: lifecycle, MessageID: draft.CurrentMessageID,
		SourceID: draft.SourceID, Receipt: draftLifecycleReceiptOutput(draft.CurrentReceipt),
		ProviderObservation: providerObservation, Observation: observation,
	}
	if draft.Pending != nil {
		output.PendingOperation = draft.Pending.Operation
		output.PendingCode = draft.Pending.Code
		if draft.Pending.ReplacementReceipt != nil {
			receipt := draftLifecycleReceiptOutput(*draft.Pending.ReplacementReceipt)
			output.PendingReceipt = &receipt
		}
	}
	return output
}

func (a *storeAPIAdapter) draftLifecycleOutput(
	ctx context.Context,
	draft store.IMAPDraft,
	status string,
	providerObservation *draftLifecycleObservation,
	observation *draftLifecycleObservation,
) (draftLifecycleOutput, error) {
	message, err := a.store.GetMessageContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftLifecycleOutput{}, fmt.Errorf("load managed draft message: %w", err)
	}
	raw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftLifecycleOutput{}, fmt.Errorf("load managed draft MIME: %w", err)
	}
	output := draftLifecycleMetadata(draft, status, providerObservation, observation)
	output.Content = message.BodyText
	output.RawMIME = string(raw)
	if draft.Pending != nil {
		output.CandidateContent = string(draft.Pending.Raw)
	}
	return output, nil
}

func (a *storeAPIAdapter) draftRecoveryOutput(
	ctx context.Context,
	draft store.IMAPDraft,
	status string,
	providerObservation *draftLifecycleObservation,
	observation *draftLifecycleObservation,
	grant *agentgrant.Grant,
) (draftLifecycleOutput, error) {
	if grant != nil {
		return draftLifecycleMetadata(draft, status, providerObservation, observation), nil
	}
	return a.draftLifecycleOutput(ctx, draft, status, providerObservation, observation)
}

func emitDraftLifecycleOutput(
	emit func(api.CLIRunEvent) error,
	stream string,
	asJSON bool,
	output draftLifecycleOutput,
) error {
	if emit == nil {
		return nil
	}
	if asJSON {
		data, err := jsonv2.Marshal(output)
		if err != nil {
			return err
		}
		return emit(api.CLIRunEvent{Type: stream, Data: string(data) + "\n"})
	}
	var data strings.Builder
	fmt.Fprintf(&data, "draft %s revision %d %s\n",
		textutil.SanitizeTerminal(output.DraftID), output.Revision, textutil.SanitizeTerminal(output.Lifecycle))
	fmt.Fprintf(&data, "status: %s\n", textutil.SanitizeTerminal(output.Status))
	fmt.Fprintf(&data, "receipt (revision %d): %s\n",
		output.Revision, textutil.SanitizeTerminal(formatDraftLifecycleReceipt(output.Receipt)))
	fmt.Fprintf(&data, "content:\n%s\n",
		strings.TrimRight(textutil.SanitizeTerminalMultiline(output.Content), "\n"))
	if output.PendingOperation != "" {
		fmt.Fprintf(&data, "pending operation: %s\n", textutil.SanitizeTerminal(output.PendingOperation))
	}
	if output.CandidateContent != "" {
		fmt.Fprintf(&data, "candidate content:\n%s\n",
			strings.TrimRight(textutil.SanitizeTerminalMultiline(output.CandidateContent), "\n"))
	}
	if output.PendingReceipt != nil {
		fmt.Fprintf(&data, "pending receipt (revision %d): %s\n",
			output.Revision, textutil.SanitizeTerminal(formatDraftLifecycleReceipt(*output.PendingReceipt)))
	}
	if output.Status == "accepted_local_failed" && output.ProviderObservation != nil &&
		output.ProviderObservation.State == "present" && output.ProviderObservation.Present &&
		output.ProviderObservation.Mailbox != "" && output.ProviderObservation.UIDValidity != 0 &&
		output.ProviderObservation.UID != 0 {
		fmt.Fprintf(&data, "acknowledged replacement receipt: %s\n",
			textutil.SanitizeTerminal(formatDraftLifecycleObservationReceipt(*output.ProviderObservation)))
	}
	if output.Observation != nil && output.Status == "pending" {
		fmt.Fprintf(&data, "old provider receipt: %s\n",
			textutil.SanitizeTerminal(formatDraftLifecycleObservationReceipt(*output.Observation)))
	}
	if output.RefusalCode != "" {
		fmt.Fprintf(&data, "recovery refusal: %s\n", textutil.SanitizeTerminal(output.RefusalCode))
	}
	providerOutcome := output.PendingCode
	if providerOutcome == "" {
		observations := []*draftLifecycleObservation{output.ProviderObservation, output.Observation}
		if output.Status == "pending" {
			observations = []*draftLifecycleObservation{output.Observation, output.ProviderObservation}
		}
		for _, observation := range observations {
			if observation == nil {
				continue
			}
			providerOutcome = observation.Code
			if providerOutcome == "" {
				providerOutcome = observation.State
			}
			if providerOutcome != "" {
				break
			}
		}
	}
	if providerOutcome != "" {
		fmt.Fprintf(&data, "provider outcome: %s\n", textutil.SanitizeTerminal(providerOutcome))
	}
	if output.Status == "pending" || output.Status == "accepted_local_failed" || output.ManualReconciliation {
		fmt.Fprintf(&data, "old draft ID remains blocked at revision %d\n", output.Revision)
		fmt.Fprintln(&data, "manual action: reconcile the provider receipt and local state before retrying")
	}
	return emit(api.CLIRunEvent{Type: stream, Data: data.String()})
}

func formatDraftLifecycleReceipt(receipt draftLifecycleReceipt) string {
	return fmt.Sprintf("%s uidvalidity=%d uid=%d", receipt.Mailbox, receipt.UIDValidity, receipt.UID)
}

func formatDraftLifecycleObservationReceipt(observation draftLifecycleObservation) string {
	return fmt.Sprintf("%s uidvalidity=%d uid=%d (%s)",
		observation.Mailbox, observation.UIDValidity, observation.UID, observation.Code)
}

func (a *storeAPIAdapter) loadManagedDraftSource(ctx context.Context, draft store.IMAPDraft) (*store.Source, error) {
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return nil, draftReplyError("invalid_source", err)
	}
	if err := a.validateManagedDraftSource(draft, source); err != nil {
		return nil, err
	}
	return source, nil
}

func (a *storeAPIAdapter) validateManagedDraftSource(draft store.IMAPDraft, source *store.Source) error {
	mailbox, err := authorizeIMAPDraft(a.draftPolicy, source.ID, source.SourceType)
	if err != nil {
		return err
	}
	if mailbox != draft.CurrentReceipt.Mailbox {
		return draftReplyError("invalid_mailbox", errors.New("draft receipt mailbox is outside the current owner grant"))
	}
	if !source.SyncConfig.Valid {
		return draftReplyError("invalid_source", errors.New("source has no sync config"))
	}
	config, err := imaplib.ConfigFromJSON(source.SyncConfig.String)
	if err != nil || config.Identifier() != source.Identifier {
		return draftReplyError("invalid_source", errors.New("source sync config identity does not match the source"))
	}
	return nil
}

func localDraftEvidenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func (a *storeAPIAdapter) releaseDraftSourceAndRefreshCache(ctx context.Context, source *store.Source, execution *store.SyncExecution) {
	if err := execution.Release(); err != nil {
		logger.Error("release source after draft write", "source_id", source.ID, "error", err)
	}
	// Committed changes must reach the cache even if cleanup or output fails.
	refreshCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	a.refreshDraftCache(refreshCtx, source)
}

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
	draft, err := a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if req.Grant != nil {
			return draftReplyNotPermitted(err)
		}
		return draftReplyError("draft_not_found", err)
	}
	if intent.Operation == api.CLIRunDraftGetCommand {
		provider := &draftLifecycleObservation{State: "not_checked", Code: "not_checked"}
		output, err := a.draftLifecycleOutput(ctx, draft, "ok", provider, nil)
		if err != nil {
			return draftReplyError("draft_read_failed", err)
		}
		return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
	}
	if intent.Operation == api.CLIRunDraftRecoverCommand {
		return a.runDraftRecover(ctx, intent, draft, req.Grant, emit)
	}
	if draft.Revision != intent.Revision {
		return draftReplyError("revision_mismatch", fmt.Errorf("expected revision %d, found %d", intent.Revision, draft.Revision))
	}
	if draft.DiscardedAt != nil {
		if intent.Operation == api.CLIRunDraftDeleteCommand {
			output, err := a.draftLifecycleOutput(ctx, draft, "already_discarded", nil, nil)
			if err != nil {
				return draftReplyError("draft_read_failed", err)
			}
			return emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output)
		}
		return draftReplyError("draft_discarded", errors.New("discarded drafts cannot be edited"))
	}
	if draft.Pending != nil && draft.Pending.Code != store.IMAPDraftCodeRemoved {
		return draftReplyError("pending_operation", store.ErrIMAPDraftPending)
	}
	source, err := a.loadManagedDraftSource(ctx, draft)
	if err != nil {
		return err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", err)
		}
		return draftReplyError("sync_lock_failed", err)
	}
	defer func() { _ = execution.Release() }()
	draft, err = a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		return draftReplyError("draft_not_found", err)
	}
	if draft.Revision != intent.Revision {
		return draftReplyError("revision_mismatch", errors.New("draft changed while acquiring source ownership"))
	}
	if draft.Pending != nil && (draft.Pending.Code != store.IMAPDraftCodeRemoved || "draft-"+draft.Pending.Operation != intent.Operation) {
		return draftReplyError("pending_operation", store.ErrIMAPDraftPending)
	}
	source, err = a.loadManagedDraftSource(ctx, draft)
	if err != nil {
		return err
	}
	currentRaw, err := a.store.GetMessageRawContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	var replacement imaplib.ReplyDraft
	if intent.Operation == api.CLIRunDraftEditCommand {
		replacement, err = imaplib.BuildDraftReplacement(currentRaw, intent.Body, time.Now(), "")
		if err != nil {
			return draftReplyError("invalid_draft", err)
		}
	}
	if draft.Pending != nil {
		if intent.Operation == api.CLIRunDraftEditCommand {
			current, err := msgmime.Parse(currentRaw)
			if err != nil {
				return draftReplyError("draft_read_failed", err)
			}
			if current.BodyText != replacement.Parsed.BodyText {
				return draftReplyError("pending_operation", errors.New("retry must use the already published edit body"))
			}
		}
		finished, err := a.store.FinishIMAPDraftRemovalContext(ctx, intent.DraftID, intent.Revision)
		if err != nil {
			return draftReplyError("cleanup_local_failed", err)
		}
		defer a.releaseDraftSourceAndRefreshCache(ctx, source, execution)
		status := "edited"
		if intent.Operation == api.CLIRunDraftDeleteCommand {
			status = "deleted"
		}
		output, err := a.draftLifecycleOutput(ctx, finished, status, nil, nil)
		if err != nil {
			return draftReplyError("draft_read_failed", err)
		}
		if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
			return draftReplyError("output_failed", err)
		}
		return nil
	}
	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, source)
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	defer func() { _ = client.Close() }()

	providerReceipt := imaplib.DraftReceipt{
		Mailbox:     draft.CurrentReceipt.Mailbox,
		UIDValidity: draft.CurrentReceipt.UIDValidity,
		UID:         draft.CurrentReceipt.UID,
	}
	inspection, err := client.InspectDraft(ctx, providerReceipt)
	if err != nil || !inspection.Present || inspection.Deleted || !inspection.Draft || !inspection.UIDPlus {
		provider := draftLifecycleObservationOutput(inspection)
		output, outputErr := a.draftLifecycleOutput(ctx, draft, "refused", provider, nil)
		if outputErr == nil {
			_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		}
		if err != nil {
			code := inspection.Code
			if code == "" {
				code = "provider_refused"
			}
			return draftReplyError(code, err)
		}
		code := inspection.Code
		if code == "" {
			code = "provider_refused"
		}
		return draftReplyError(code, errors.New("provider draft inspection refused mutation"))
	}
	if intent.Operation == api.CLIRunDraftEditCommand {
		return a.runDraftEdit(ctx, intent, draft, source, client, currentRaw, replacement, inspection, execution, emit)
	}
	return a.runDraftDelete(ctx, intent, draft, source, client, inspection, execution, emit)
}

func (a *storeAPIAdapter) runDraftEdit(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	source *store.Source,
	client *imaplib.Client,
	currentRaw []byte,
	replacement imaplib.ReplyDraft,
	inspection imaplib.DraftObservation,
	execution *store.SyncExecution,
	emit func(api.CLIRunEvent) error,
) error {
	claimed, err := a.store.ClaimIMAPDraftContext(ctx, intent.DraftID, intent.Revision, store.IMAPDraftOperationEdit, replacement.Raw)
	if err != nil {
		return draftReplyError("claim_failed", err)
	}
	appendResult, err := client.AppendDraft(ctx, draft.CurrentReceipt.Mailbox, replacement.Raw)
	if err != nil {
		code := appendResult.Code
		if code == "" {
			code = "append_failed"
		}
		evidenceCtx, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		latest := claimed
		var persistenceErr error
		if appendResult.State == imaplib.DraftStateRejected || appendResult.State == imaplib.DraftStateCancelled {
			latest, persistenceErr = a.store.AbortIMAPDraftContext(evidenceCtx, intent.DraftID, intent.Revision, appendResult.State)
			if persistenceErr != nil {
				latest = claimed
			}
		} else {
			persistenceErr = a.store.RecordIMAPDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, code, nil)
			if loaded, loadErr := a.store.GetIMAPDraftContext(evidenceCtx, intent.DraftID); loadErr == nil {
				latest = loaded
			}
		}
		status := "pending"
		if latest.Pending == nil && persistenceErr == nil {
			status = draftLifecycleActive
		}
		output, outputErr := a.draftLifecycleOutput(evidenceCtx, latest, status, draftLifecycleObservationOutput(inspection), nil)
		if outputErr != nil {
			output = draftLifecycleOutput{
				Status: status, DraftID: latest.DraftID, Revision: latest.Revision, Lifecycle: draftLifecycleActive,
				MessageID: latest.CurrentMessageID, SourceID: latest.SourceID,
				Receipt: draftLifecycleReceiptOutput(latest.CurrentReceipt), RawMIME: string(currentRaw),
			}
			if latest.Pending != nil {
				output.PendingOperation = latest.Pending.Operation
				output.CandidateContent = string(latest.Pending.Raw)
			}
		}
		output.PendingCode = code
		output.ManualReconciliation = latest.Pending != nil || persistenceErr != nil
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		if persistenceErr != nil {
			return draftReplyError("local_persistence_failed", errors.Join(err, persistenceErr))
		}
		if coded, ok := errors.AsType[*imaplib.DraftAppendError](err); ok {
			err = coded.Err
		}
		return draftReplyError(code, err)
	}
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: draft.CurrentReceipt.Mailbox, UIDValidity: appendResult.UIDValidity, UID: appendResult.UID}
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	if err := a.store.RecordIMAPDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, appendResult.Code, &receipt); err != nil {
		appendObservation := &draftLifecycleObservation{
			State: "present", Code: appendResult.Code, Mailbox: receipt.Mailbox,
			UIDValidity: receipt.UIDValidity, UID: receipt.UID, Present: true,
		}
		output, outputErr := a.draftLifecycleOutput(evidenceCtx, claimed, "accepted_local_failed", appendObservation, nil)
		if outputErr == nil {
			output.ManualReconciliation = true
			_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		}
		return draftReplyError("accepted_local_failed", err)
	}
	reportAcceptedLocalFailure := func(cause error) error {
		output := draftLifecycleOutput{
			Status: "accepted_local_failed", DraftID: claimed.DraftID, Revision: claimed.Revision,
			Lifecycle: draftLifecycleActive, MessageID: claimed.CurrentMessageID, SourceID: claimed.SourceID,
			Receipt: draftLifecycleReceiptOutput(claimed.CurrentReceipt), RawMIME: string(currentRaw),
			PendingOperation: store.IMAPDraftOperationEdit, PendingCode: appendResult.Code,
			CandidateContent: string(replacement.Raw), ManualReconciliation: true,
		}
		if loaded, loadErr := a.draftLifecycleOutput(evidenceCtx, claimed, "accepted_local_failed", nil, nil); loadErr == nil {
			output = loaded
		}
		output.PendingCode = appendResult.Code
		output.ManualReconciliation = true
		pendingReceipt := draftLifecycleReceiptOutput(receipt)
		output.PendingReceipt = &pendingReceipt
		_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("accepted_local_failed", cause)
	}
	currentMessage, currentMessageErr := a.store.GetMessageContext(evidenceCtx, draft.CurrentMessageID)
	if currentMessageErr != nil {
		return reportAcceptedLocalFailure(currentMessageErr)
	}
	replyTo, replyToErr := a.store.GetMessageReplyToMessageIDContext(evidenceCtx, draft.CurrentMessageID)
	if replyToErr != nil {
		return reportAcceptedLocalFailure(replyToErr)
	}
	participants, build := draftLifecyclePersistData(currentMessage.ConversationID, replyTo, replacement, receipt)
	published, err := a.store.PublishIMAPDraftReplacementContext(evidenceCtx, intent.DraftID, intent.Revision, participants, build)
	if err != nil {
		return reportAcceptedLocalFailure(err)
	}
	defer a.releaseDraftSourceAndRefreshCache(ctx, source, execution)
	if ctx.Err() != nil {
		output, outputErr := a.draftLifecycleOutput(evidenceCtx, published, "pending", nil, nil)
		if outputErr == nil {
			output.ManualReconciliation = true
			_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
		}
		return draftReplyError("cancelled", ctx.Err())
	}
	removed, err := client.RemoveDraft(ctx, imaplib.DraftReceipt{Mailbox: draft.CurrentReceipt.Mailbox, UIDValidity: draft.CurrentReceipt.UIDValidity, UID: draft.CurrentReceipt.UID})
	evidenceCtx, cancelCleanupEvidence := localDraftEvidenceContext(ctx)
	defer cancelCleanupEvidence()
	if err != nil || !removed.Complete {
		code := draftLifecycleObservationCode(removed, "cleanup_incomplete")
		recordErr := a.store.RecordIMAPDraftOutcomeContext(evidenceCtx, intent.DraftID, published.Revision, code, nil)
		a.emitDraftLifecyclePending(evidenceCtx, intent, published, nil, nil, removed, emit)
		if err == nil {
			err = errors.New("provider cleanup is incomplete")
		}
		if recordErr != nil {
			return draftReplyError("local_persistence_failed", errors.Join(err, recordErr))
		}
		return draftReplyError(code, err)
	}
	if err := a.store.RecordIMAPDraftOutcomeContext(
		evidenceCtx, intent.DraftID, published.Revision, store.IMAPDraftCodeRemoved, nil,
	); err != nil {
		a.emitDraftLifecyclePending(evidenceCtx, intent, published, nil, nil, removed, emit)
		return draftReplyError("local_persistence_failed", err)
	}
	finished, err := a.store.FinishIMAPDraftRemovalContext(evidenceCtx, intent.DraftID, published.Revision)
	if err != nil {
		a.emitDraftLifecyclePending(evidenceCtx, intent, published, nil, nil, removed, emit)
		return draftReplyError("cleanup_local_failed", err)
	}
	output, err := a.draftLifecycleOutput(evidenceCtx, finished, "edited", nil, draftLifecycleObservationOutput(removed))
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}

func (a *storeAPIAdapter) emitDraftLifecyclePending(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	grant *agentgrant.Grant,
	providerObservation *draftLifecycleObservation,
	observation imaplib.DraftObservation,
	emit func(api.CLIRunEvent) error,
) {
	if latest, err := a.store.GetIMAPDraftContext(ctx, draft.DraftID); err == nil {
		draft = latest
	}
	output, err := a.draftRecoveryOutput(ctx, draft, "pending", providerObservation, draftLifecycleObservationOutput(observation), grant)
	if err != nil {
		output = draftLifecycleMetadata(draft, "pending", providerObservation, draftLifecycleObservationOutput(observation))
		if grant == nil && draft.Pending != nil {
			output.CandidateContent = string(draft.Pending.Raw)
		}
	}
	output.ManualReconciliation = true
	_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
}

func (a *storeAPIAdapter) runDraftDelete(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	source *store.Source,
	client *imaplib.Client,
	inspection imaplib.DraftObservation,
	execution *store.SyncExecution,
	emit func(api.CLIRunEvent) error,
) error {
	claimed, err := a.store.ClaimIMAPDraftContext(ctx, intent.DraftID, intent.Revision, store.IMAPDraftOperationDelete, nil)
	if err != nil {
		return draftReplyError("claim_failed", err)
	}
	removed, err := client.RemoveDraft(ctx, imaplib.DraftReceipt{Mailbox: draft.CurrentReceipt.Mailbox, UIDValidity: draft.CurrentReceipt.UIDValidity, UID: draft.CurrentReceipt.UID})
	if err != nil || !removed.Complete {
		evidenceCtx, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		code := draftLifecycleObservationCode(removed, "cleanup_incomplete")
		if !removed.WriteAttempted {
			active, abortErr := a.store.AbortIMAPDraftContext(evidenceCtx, intent.DraftID, intent.Revision, "not_attempted")
			if abortErr != nil {
				a.emitDraftLifecyclePending(evidenceCtx, intent, claimed, nil, draftLifecycleObservationOutput(inspection), removed, emit)
				return draftReplyError("local_persistence_failed", errors.Join(err, abortErr))
			}
			output, outputErr := a.draftLifecycleOutput(evidenceCtx, active, draftLifecycleActive, draftLifecycleObservationOutput(inspection), draftLifecycleObservationOutput(removed))
			if outputErr != nil {
				return draftReplyError("draft_read_failed", outputErr)
			}
			_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
			return draftReplyError(code, err)
		}
		recordErr := a.store.RecordIMAPDraftOutcomeContext(evidenceCtx, intent.DraftID, intent.Revision, code, nil)
		a.emitDraftLifecyclePending(evidenceCtx, intent, claimed, nil, draftLifecycleObservationOutput(inspection), removed, emit)
		if err == nil {
			err = errors.New("provider cleanup is incomplete")
		}
		if recordErr != nil {
			return draftReplyError("local_persistence_failed", errors.Join(err, recordErr))
		}
		return draftReplyError(code, err)
	}
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	if err := a.store.RecordIMAPDraftOutcomeContext(
		evidenceCtx, intent.DraftID, intent.Revision, store.IMAPDraftCodeRemoved, nil,
	); err != nil {
		a.emitDraftLifecyclePending(evidenceCtx, intent, claimed, nil, draftLifecycleObservationOutput(inspection), removed, emit)
		return draftReplyError("local_persistence_failed", err)
	}
	finished, err := a.store.FinishIMAPDraftRemovalContext(evidenceCtx, intent.DraftID, intent.Revision)
	if err != nil {
		a.emitDraftLifecyclePending(evidenceCtx, intent, claimed, nil, draftLifecycleObservationOutput(inspection), removed, emit)
		return draftReplyError("cleanup_local_failed", err)
	}
	defer a.releaseDraftSourceAndRefreshCache(ctx, source, execution)
	output, err := a.draftLifecycleOutput(evidenceCtx, finished, "deleted", nil, draftLifecycleObservationOutput(removed))
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}

func draftRecoveryPermission(draft store.IMAPDraft) (agentgrant.Permission, error) {
	if draft.DiscardedAt != nil {
		return agentgrant.PermissionDraftDelete, nil
	}
	if draft.Pending == nil {
		return agentgrant.PermissionDraftEdit, nil
	}
	switch draft.Pending.Operation {
	case store.IMAPDraftOperationEdit:
		return agentgrant.PermissionDraftEdit, nil
	case store.IMAPDraftOperationDelete:
		return agentgrant.PermissionDraftDelete, nil
	default:
		return "", draftReplyError("invalid_state", fmt.Errorf("unknown pending draft operation %q", draft.Pending.Operation))
	}
}

func draftProviderReceipt(receipt store.IMAPDraftReceipt) imaplib.DraftReceipt {
	return imaplib.DraftReceipt{
		Mailbox: receipt.Mailbox, UIDValidity: receipt.UIDValidity, UID: receipt.UID,
	}
}

func (a *storeAPIAdapter) authorizeDraftRecovery(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	grant *agentgrant.Grant,
) (*store.Source, error) {
	permission, err := draftRecoveryPermission(draft)
	if err != nil {
		return nil, err
	}
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		if grant != nil {
			return nil, draftReplyNotPermitted(fmt.Errorf("load source %d: %w", draft.SourceID, err))
		}
		return nil, draftReplyError("invalid_source", err)
	}
	if grant != nil {
		ref := agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}
		if !grant.Allows(permission, ref) {
			return nil, draftReplyNotPermitted(fmt.Errorf("source %d is not in grant %s", source.ID, grant.ID))
		}
	}
	if draft.Revision != intent.Revision {
		return nil, draftReplyError("revision_mismatch", fmt.Errorf("expected revision %d, found %d", intent.Revision, draft.Revision))
	}
	if err := a.validateManagedDraftSource(draft, source); err != nil {
		return nil, err
	}
	return source, nil
}

func (a *storeAPIAdapter) settleDraftRecovery(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	grant *agentgrant.Grant,
	emit func(api.CLIRunEvent) error,
) (bool, error) {
	var status string
	switch {
	case draft.DiscardedAt != nil:
		status = "already_discarded"
	case draft.Pending == nil:
		status = draftLifecycleActive
	case draft.Pending.Operation == store.IMAPDraftOperationEdit && draft.Pending.ReplacementReceipt == nil:
		output, err := a.draftRecoveryOutput(ctx, draft, "unknown_replacement", nil, nil, grant)
		if err != nil {
			return true, draftReplyError("draft_read_failed", err)
		}
		output.RefusalCode = "unknown_replacement"
		output.ManualReconciliation = true
		if err := emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output); err != nil {
			return true, draftReplyError("output_failed", err)
		}
		return true, draftReplyError("unknown_replacement", errors.New("pending edit has no recorded replacement receipt"))
	default:
		return false, nil
	}
	output, err := a.draftRecoveryOutput(ctx, draft, status, nil, nil, grant)
	if err != nil {
		return true, draftReplyError("draft_read_failed", err)
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return true, draftReplyError("output_failed", err)
	}
	return true, nil
}

func (a *storeAPIAdapter) refuseDraftRecovery(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	grant *agentgrant.Grant,
	code string,
	cause error,
	observation *imaplib.DraftObservation,
	emit func(api.CLIRunEvent) error,
) error {
	var providerObservation *draftLifecycleObservation
	if observation != nil {
		providerObservation = draftLifecycleObservationOutput(*observation)
	}
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	output, outputErr := a.draftRecoveryOutput(evidenceCtx, draft, "refused", providerObservation, nil, grant)
	if outputErr != nil {
		return draftReplyError("draft_read_failed", errors.Join(cause, outputErr))
	}
	output.RefusalCode = code
	output.ManualReconciliation = true
	if emitErr := emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output); emitErr != nil {
		return draftReplyError("output_failed", errors.Join(cause, emitErr))
	}
	if cause == nil {
		cause = errors.New("provider refused draft recovery")
	}
	return draftReplyError(code, cause)
}

func (a *storeAPIAdapter) publishRecoveredDraftReplacement(
	ctx context.Context,
	draft store.IMAPDraft,
) (store.IMAPDraft, error) {
	if draft.Pending == nil || draft.Pending.Operation != store.IMAPDraftOperationEdit || draft.Pending.ReplacementReceipt == nil {
		return store.IMAPDraft{}, errors.New("known replacement is required for draft publication")
	}
	parsed, err := msgmime.Parse(draft.Pending.Raw)
	if err != nil {
		return store.IMAPDraft{}, fmt.Errorf("parse recorded draft replacement: %w", err)
	}
	replacement := imaplib.ReplyDraft{
		Raw:    append([]byte(nil), draft.Pending.Raw...),
		Parsed: parsed,
	}
	message, err := a.store.GetMessageContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return store.IMAPDraft{}, fmt.Errorf("load draft message for replacement: %w", err)
	}
	replyTo, err := a.store.GetMessageReplyToMessageIDContext(ctx, draft.CurrentMessageID)
	if err != nil {
		return store.IMAPDraft{}, fmt.Errorf("load draft reply link for replacement: %w", err)
	}
	receipt := *draft.Pending.ReplacementReceipt
	participants, build := draftLifecyclePersistData(message.ConversationID, replyTo, replacement, receipt)
	return a.store.PublishIMAPDraftReplacementContext(ctx, draft.DraftID, draft.Revision, participants, build)
}

func (a *storeAPIAdapter) runDraftRecover(
	ctx context.Context,
	intent draftLifecycleIntent,
	draft store.IMAPDraft,
	grant *agentgrant.Grant,
	emit func(api.CLIRunEvent) error,
) error {
	source, err := a.authorizeDraftRecovery(ctx, intent, draft, grant)
	if err != nil {
		return err
	}
	settled, err := a.settleDraftRecovery(ctx, intent, draft, grant, emit)
	if settled || err != nil {
		return err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			return draftReplyError("sync_active", err)
		}
		return draftReplyError("sync_lock_failed", err)
	}
	refreshScheduled := false
	defer func() {
		if refreshScheduled {
			a.releaseDraftSourceAndRefreshCache(ctx, source, execution)
			return
		}
		_ = execution.Release()
	}()

	draft, err = a.store.GetIMAPDraftContext(ctx, intent.DraftID)
	if err != nil {
		if grant != nil {
			return draftReplyNotPermitted(err)
		}
		return draftReplyError("draft_not_found", err)
	}
	source, err = a.authorizeDraftRecovery(ctx, intent, draft, grant)
	if err != nil {
		return err
	}
	settled, err = a.settleDraftRecovery(ctx, intent, draft, grant, emit)
	if settled || err != nil {
		return err
	}

	if draft.Pending.Code == store.IMAPDraftCodeRemoved {
		evidenceCtx, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		finished, finishErr := a.store.FinishIMAPDraftRemovalContext(evidenceCtx, draft.DraftID, draft.Revision)
		if finishErr != nil {
			return draftReplyError("cleanup_local_failed", finishErr)
		}
		refreshScheduled = true
		status := "edited"
		if finished.DiscardedAt != nil {
			status = "deleted"
		}
		output, outputErr := a.draftRecoveryOutput(evidenceCtx, finished, status, nil, nil, grant)
		if outputErr != nil {
			return draftReplyError("draft_read_failed", outputErr)
		}
		if outputErr := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); outputErr != nil {
			return draftReplyError("output_failed", outputErr)
		}
		return nil
	}
	clientFactory := a.draftClientFactory
	if clientFactory == nil {
		clientFactory = defaultDraftClientFactory
	}
	client, err := clientFactory(ctx, source)
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	defer func() { _ = client.Close() }()

	originalObservation, err := client.InspectDraft(ctx, draftProviderReceipt(draft.Pending.OriginalReceipt))
	if err != nil {
		code := draftLifecycleObservationCode(originalObservation, "provider_refused")
		return a.refuseDraftRecovery(ctx, intent, draft, grant, code, err, &originalObservation, emit)
	}
	published := false
	if draft.Pending.Operation == store.IMAPDraftOperationEdit {
		replacementReceipt := *draft.Pending.ReplacementReceipt
		published = draft.CurrentReceipt == replacementReceipt
		if !published {
			if draft.Pending.OriginalReceipt.UIDValidity != replacementReceipt.UIDValidity {
				return a.refuseDraftRecovery(
					ctx, intent, draft, grant, "uidvalidity_mismatch",
					errors.New("recorded original and replacement generations differ"),
					&originalObservation, emit,
				)
			}
			replacementObservation, replacementErr := client.InspectDraft(ctx, draftProviderReceipt(replacementReceipt))
			if replacementErr != nil || !replacementObservation.Present || !replacementObservation.Draft || replacementObservation.Deleted {
				code := draftLifecycleObservationCode(replacementObservation, "provider_refused")
				if replacementErr == nil {
					switch {
					case !replacementObservation.Present:
						code = "absent"
					case replacementObservation.Deleted:
						code = "already_deleted"
					case !replacementObservation.Draft:
						code = "not_draft"
					}
					replacementErr = errors.New("recorded replacement is unavailable for publication")
				}
				return a.refuseDraftRecovery(ctx, intent, draft, grant, code, replacementErr, &replacementObservation, emit)
			}
			publicationCtx, cancel := localDraftEvidenceContext(ctx)
			publishedDraft, publishErr := a.publishRecoveredDraftReplacement(publicationCtx, draft)
			if publishErr != nil {
				output, outputErr := a.draftRecoveryOutput(publicationCtx, draft, "accepted_local_failed", draftLifecycleObservationOutput(replacementObservation), nil, grant)
				if outputErr == nil {
					output.ManualReconciliation = true
					_ = emitDraftLifecycleOutput(emit, cliStreamStderr, intent.JSON, output)
				}
				cancel()
				return draftReplyError("accepted_local_failed", publishErr)
			}
			cancel()
			draft = publishedDraft
			published = true
			refreshScheduled = true
		}
	}

	cleanupObservation := originalObservation
	if originalObservation.Present {
		cleanupObservation, err = client.RemoveDraft(ctx, draftProviderReceipt(draft.Pending.OriginalReceipt))
		if err != nil || !cleanupObservation.Complete {
			code := draftLifecycleObservationCode(cleanupObservation, "cleanup_incomplete")
			if !cleanupObservation.WriteAttempted {
				if published {
					evidenceCtx, cancel := localDraftEvidenceContext(ctx)
					defer cancel()
					a.emitDraftLifecyclePending(evidenceCtx, intent, draft, grant, nil, cleanupObservation, emit)
					if err == nil {
						err = errors.New("provider cleanup was not attempted")
					}
					return draftReplyError(code, err)
				}
				return a.refuseDraftRecovery(ctx, intent, draft, grant, code, err, &cleanupObservation, emit)
			}
			evidenceCtx, cancel := localDraftEvidenceContext(ctx)
			defer cancel()
			recordErr := a.store.RecordIMAPDraftOutcomeContext(evidenceCtx, draft.DraftID, draft.Revision, code, nil)
			a.emitDraftLifecyclePending(evidenceCtx, intent, draft, grant, nil, cleanupObservation, emit)
			if recordErr != nil {
				if err == nil {
					err = errors.New("provider cleanup is incomplete")
				}
				return draftReplyError("local_persistence_failed", errors.Join(err, recordErr))
			}
			if err == nil {
				err = errors.New("provider cleanup is incomplete")
			}
			return draftReplyError(code, err)
		}
	}

	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	if err := a.store.RecordIMAPDraftOutcomeContext(evidenceCtx, draft.DraftID, draft.Revision, store.IMAPDraftCodeRemoved, nil); err != nil {
		a.emitDraftLifecyclePending(evidenceCtx, intent, draft, grant, nil, cleanupObservation, emit)
		return draftReplyError("local_persistence_failed", err)
	}
	finished, err := a.store.FinishIMAPDraftRemovalContext(evidenceCtx, draft.DraftID, draft.Revision)
	if err != nil {
		a.emitDraftLifecyclePending(evidenceCtx, intent, draft, grant, nil, cleanupObservation, emit)
		return draftReplyError("cleanup_local_failed", err)
	}
	refreshScheduled = true
	status := "edited"
	if finished.DiscardedAt != nil {
		status = "deleted"
	}
	output, err := a.draftRecoveryOutput(evidenceCtx, finished, status, nil, draftLifecycleObservationOutput(cleanupObservation), grant)
	if err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	if err := emitDraftLifecycleOutput(emit, cliStreamStdout, intent.JSON, output); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}

func draftLifecyclePersistData(
	conversationID int64,
	replyTo sql.NullInt64,
	replacement imaplib.ReplyDraft,
	receipt store.IMAPDraftReceipt,
) ([]store.ParticipantPersistData, func([]int64) *store.MessagePersistData) {
	parsed := replacement.Parsed
	addresses := append([]msgmime.Address(nil), parsed.From...)
	addresses = append(addresses, parsed.To...)
	addresses = append(addresses, parsed.Cc...)
	addresses = append(addresses, parsed.Bcc...)
	participants := make([]store.ParticipantPersistData, len(addresses))
	for i, address := range addresses {
		participants[i] = store.ParticipantPersistData{EmailAddress: address.Email, DisplayName: address.Name, Domain: address.Domain}
	}
	fromCount, toCount := len(parsed.From), len(parsed.To)
	ccCount, bccCount := len(parsed.Cc), len(parsed.Bcc)
	build := func(ids []int64) *store.MessagePersistData {
		at := 0
		fromIDs := ids[at : at+fromCount]
		at += fromCount
		toIDs := ids[at : at+toCount]
		at += toCount
		ccIDs := ids[at : at+ccCount]
		at += ccCount
		bccIDs := ids[at : at+bccCount]
		toAddresses := addressStrings(parsed.To)
		ccAddresses := addressStrings(parsed.Cc)
		bccAddresses := addressStrings(parsed.Bcc)
		fromAddresses := addressStrings(parsed.From)
		rfc822 := msgmime.NormalizeMessageID(parsed.MessageID)
		if rfc822 != "" {
			rfc822 = "<" + rfc822 + ">"
		}
		message := &store.Message{
			SourceID: receipt.SourceID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
			RFC822MessageID: sql.NullString{String: rfc822, Valid: rfc822 != ""},
			ConversationID:  conversationID,
			MessageType:     store.MessageTypeEmail, IsFromMe: true, IdentityDerivedIsFromMe: true,
			SenderID:         sql.NullInt64{Int64: fromIDs[0], Valid: len(fromIDs) > 0},
			ReplyToMessageID: replyTo,
			ListID:           sql.NullString{String: parsed.ListID, Valid: parsed.ListID != ""},
			Subject:          sql.NullString{String: parsed.Subject, Valid: parsed.Subject != ""},
			Snippet:          sql.NullString{String: strings.TrimSpace(parsed.BodyText), Valid: parsed.BodyText != ""},
			SentAt:           sql.NullTime{Time: parsed.Date, Valid: !parsed.Date.IsZero()},
			InternalDate:     sql.NullTime{Time: parsed.Date, Valid: !parsed.Date.IsZero()},
			SizeEstimate:     int64(len(replacement.Raw)), ArchivedAt: time.Now(),
		}
		return &store.MessagePersistData{
			Message: message, BodyText: sql.NullString{String: parsed.BodyText, Valid: true},
			RawMIME: replacement.Raw, RawFormat: "mime",
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: fromIDs, EmailAddresses: fromAddresses},
				{Type: "to", ParticipantIDs: toIDs, EmailAddresses: toAddresses},
				{Type: "cc", ParticipantIDs: ccIDs, EmailAddresses: ccAddresses},
				{Type: "bcc", ParticipantIDs: bccIDs, EmailAddresses: bccAddresses},
			},
			FTS: &store.FTSDoc{Subject: parsed.Subject, Body: parsed.BodyText, FromAddr: firstAddress(parsed.From), ToAddrs: strings.Join(toAddresses, " ")},
		}
	}
	return participants, build
}

func addressStrings(addresses []msgmime.Address) []string {
	result := make([]string, len(addresses))
	for i, address := range addresses {
		result[i] = address.Email
	}
	return result
}

func firstAddress(addresses []msgmime.Address) string {
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0].Email
}
