package cmd

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type beeperDraftIntent struct {
	Operation string
	SourceID  int64
	ChatID    string
	DraftID   string
	Revision  int64
	Body      string
	JSON      bool
}

func parseBeeperDraftArgs(args []string) (beeperDraftIntent, error) {
	if !api.IsCLIRunBeeperDraft(args) {
		return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("expected draft-beeper operation"))
	}
	intent := beeperDraftIntent{Operation: args[1]}
	var positional string
	var sourceSet, chatSet, revisionSet, bodySet bool
	rest := args[2:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		if !strings.HasPrefix(arg, "--") {
			if positional != "" {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("expected one draft ID"))
			}
			positional = arg
			continue
		}
		nameValue := strings.TrimPrefix(arg, "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if !hasValue && (name == "source-id" || name == "chat-id" || name == "body" || name == "revision") {
			if len(rest) == 0 {
				return beeperDraftIntent{}, draftReplyError("invalid_args", fmt.Errorf("--%s requires a value", name))
			}
			value, rest = rest[0], rest[1:]
			hasValue = true
		}
		switch name {
		case "source-id":
			if sourceSet {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--source-id given more than once"))
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--source-id must be positive"))
			}
			intent.SourceID, sourceSet = parsed, true
		case "chat-id":
			if chatSet || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--chat-id must be a nonblank canonical ID"))
			}
			intent.ChatID, chatSet = value, true
		case "body":
			if bodySet || value == "" || strings.ContainsRune(value, '\x00') {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--body must be non-empty"))
			}
			intent.Body, bodySet = value, true
		case "revision":
			if revisionSet {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--revision given more than once"))
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--revision must be positive"))
			}
			intent.Revision, revisionSet = parsed, true
		case "json":
			if hasValue && value != "true" {
				return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("--json accepts no value"))
			}
			intent.JSON = true
		case "log-level", "verbose", "log-sql", "log-sql-slow-ms":
			// Root logging flags may be preserved in the daemon request.
		default:
			return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("unsupported draft-beeper flag"))
		}
	}
	switch intent.Operation {
	case "create":
		if positional != "" || !sourceSet || !chatSet || !bodySet {
			return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("create requires source, chat, and body"))
		}
	case "get":
		if positional == "" || sourceSet || chatSet || bodySet || revisionSet {
			return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("get requires one draft ID"))
		}
		intent.DraftID = positional
	case "edit":
		if positional == "" || !revisionSet || !bodySet || sourceSet || chatSet {
			return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("edit requires draft ID, revision, and body"))
		}
		intent.DraftID = positional
	case "clear":
		if positional == "" || !revisionSet || sourceSet || chatSet || bodySet {
			return beeperDraftIntent{}, draftReplyError("invalid_args", errors.New("clear requires draft ID and revision"))
		}
		intent.DraftID = positional
	}
	return intent, nil
}

func authorizeBeeperDraft(policy []config.BeeperDraftSource, source *store.Source, grant *agentgrant.Grant, permission agentgrant.Permission) error {
	if source == nil || source.SourceType != "beeper" {
		return draftReplyError("draft_disabled", errors.New("selected source is not Beeper"))
	}
	enabled := false
	for _, entry := range policy {
		if entry.SourceID == source.ID {
			enabled = true
			break
		}
	}
	if !enabled {
		return draftReplyError("draft_disabled", errors.New("Beeper drafts are not enabled for this source"))
	}
	if grant != nil && !grant.Allows(permission, draftSourceRef(source)) {
		return draftReplyNotPermitted(errors.New("grant does not cover the selected Beeper source"))
	}
	return nil
}

type beeperDraftOutput struct {
	Status            string  `json:"status"`
	DraftID           string  `json:"draft_id"`
	SourceID          int64   `json:"source_id"`
	AccountID         string  `json:"account_id"`
	ChatID            string  `json:"chat_id"`
	Revision          int64   `json:"revision"`
	CommittedText     *string `json:"committed_text"`
	PendingOperation  string  `json:"pending_operation,omitempty"`
	PendingPhase      string  `json:"pending_phase,omitempty"`
	CandidateText     string  `json:"candidate_text,omitempty"`
	OutcomeCode       string  `json:"outcome_code,omitempty"`
	ProviderStatus    string  `json:"provider_status,omitempty"`
	NativePresent     bool    `json:"native_present"`
	NativeEmpty       bool    `json:"native_empty"`
	NativeText        string  `json:"native_text,omitempty"`
	NativeAttachments bool    `json:"native_attachments"`
	NativeUnknown     bool    `json:"native_unknown"`
	RaceNote          string  `json:"race_note,omitempty"`
}

const beeperDraftRaceNote = "Edit and clear compare the observed draft before PATCH. Beeper has no conditional clear token, so a Desktop edit after that observation can still be cleared."

func beeperDraftOutputFromDraft(draft store.BeeperDraft, status string) beeperDraftOutput {
	out := beeperDraftOutput{Status: status, DraftID: draft.DraftID, SourceID: draft.SourceID, AccountID: draft.AccountID, ChatID: draft.ChatID, Revision: draft.Revision, CommittedText: draft.CommittedText, RaceNote: beeperDraftRaceNote}
	if draft.Pending != nil {
		out.PendingOperation, out.PendingPhase, out.CandidateText, out.OutcomeCode = draft.Pending.Operation, draft.Pending.Phase, draft.Pending.Candidate, draft.Pending.OutcomeCode
	}
	return out
}

func emitBeeperDraftOutput(emit func(api.CLIRunEvent) error, stream string, asJSON bool, output beeperDraftOutput) error {
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
	var b strings.Builder
	fmt.Fprintf(&b, "draft %s revision %d %s\n", textutil.SanitizeTerminal(output.DraftID), output.Revision, textutil.SanitizeTerminal(output.Status))
	fmt.Fprintf(&b, "chat: %s\n", textutil.SanitizeTerminal(output.ChatID))
	if output.CommittedText != nil {
		fmt.Fprintf(&b, "content:\n%s\n", textutil.SanitizeTerminalMultiline(*output.CommittedText))
	} else {
		b.WriteString("content: <empty>\n")
	}
	if output.ProviderStatus != "" {
		fmt.Fprintf(&b, "provider: %s\n", textutil.SanitizeTerminal(output.ProviderStatus))
	}
	b.WriteString(beeperDraftRaceNote + "\n")
	return emit(api.CLIRunEvent{Type: stream, Data: b.String()})
}

func validateBeeperChat(source *store.Source, requested string, chat *beeper.Chat) (beeper.DraftObservation, error) {
	if chat == nil || chat.ID == "" || chat.AccountID == "" || chat.ID != requested || chat.AccountID != source.Identifier {
		return beeper.DraftObservation{}, draftReplyError("provider_identity_mismatch", errors.New("Beeper returned an unexpected chat identity"))
	}
	if chat.Merge != nil || chat.MergedIntoChatID != "" {
		return beeper.DraftObservation{}, draftReplyError("ambiguous_chat", errors.New("merged Beeper chats cannot own a managed draft"))
	}
	observation, err := chat.InspectDraft()
	if err != nil {
		return beeper.DraftObservation{}, draftReplyError("provider_unknown", err)
	}
	return observation, nil
}

func beeperDraftClient(a *storeAPIAdapter) (*beeper.Client, error) {
	if a == nil || a.config == nil {
		return nil, errors.New("configuration is unavailable")
	}
	token, err := beeper.LoadToken(a.config.TokensDir())
	if err != nil {
		return nil, err
	}
	return beeperClient(a.config, token), nil
}

func (a *storeAPIAdapter) runCLIBeeperDraft(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	intent, err := parseBeeperDraftArgs(req.Args)
	if err != nil {
		return err
	}
	if len(req.Env) != 0 || req.Cwd != "" {
		return draftReplyError("invalid_args", errors.New("draft-beeper accepts no environment or working directory"))
	}
	permission, ok := api.BeeperDraftPermission(req.Args)
	if !ok {
		return draftReplyError("invalid_args", errors.New("unsupported draft-beeper operation"))
	}
	var draft store.BeeperDraft
	var source *store.Source
	if intent.Operation == "create" {
		source, err = sourceops.ResolveExactOne(a.store, sourceops.Selector{SourceID: intent.SourceID, SourceIDSet: true})
		if err != nil {
			return draftReplyError("invalid_source", err)
		}
	} else {
		draft, err = a.store.GetBeeperDraftContext(ctx, intent.DraftID)
		if err != nil {
			return draftReplyError("draft_not_found", err)
		}
		source, err = a.store.GetSourceByIDContext(ctx, draft.SourceID)
		if err != nil {
			return draftReplyError("invalid_source", err)
		}
	}
	if err := authorizeBeeperDraft(a.beeperDraftPolicy, source, req.Grant, permission); err != nil {
		return err
	}
	if intent.Operation == "get" {
		execution, err := a.store.AcquireSyncExecutionContext(ctx, source.ID)
		if err != nil {
			return draftReplyError("sync_lock_failed", err)
		}
		defer execution.Release()
		return a.runBeeperDraftGet(ctx, intent, draft, source, emit)
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return draftReplyError("sync_lock_failed", err)
	}
	defer execution.Release()
	client, err := beeperDraftClient(a)
	if err != nil {
		return draftReplyError("provider_unavailable", err)
	}
	if intent.Operation == "create" {
		return a.runBeeperDraftCreate(ctx, intent, source, client, emit)
	}
	return a.runBeeperDraftMutation(ctx, intent, draft, source, client, emit)
}

func (a *storeAPIAdapter) runBeeperDraftGet(ctx context.Context, intent beeperDraftIntent, draft store.BeeperDraft, source *store.Source, emit func(api.CLIRunEvent) error) error {
	output := beeperDraftOutputFromDraft(draft, "ok")
	client, err := beeperDraftClient(a)
	if err != nil {
		output.ProviderStatus = "provider_unavailable"
		_ = emitBeeperDraftOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("provider_unavailable", err)
	}
	chat, err := client.GetChat(ctx, draft.ChatID)
	if err != nil {
		output.ProviderStatus = "provider_unavailable"
		_ = emitBeeperDraftOutput(emit, cliStreamStderr, intent.JSON, output)
		return draftReplyError("provider_unavailable", err)
	}
	observation, err := validateBeeperChat(source, draft.ChatID, chat)
	if err != nil {
		return err
	}
	output.NativePresent, output.NativeEmpty, output.NativeText, output.NativeAttachments, output.NativeUnknown = observation.Present, observation.Empty(), observation.Text, observation.AttachmentsPresent || len(observation.Attachments) > 0, observation.Unknown
	return emitBeeperDraftOutput(emit, cliStreamStdout, intent.JSON, output)
}

func (a *storeAPIAdapter) runBeeperDraftCreate(ctx context.Context, intent beeperDraftIntent, source *store.Source, client *beeper.Client, emit func(api.CLIRunEvent) error) error {
	chat, err := client.GetChat(ctx, intent.ChatID)
	if err != nil {
		return draftReplyError("provider_unavailable", err)
	}
	observation, err := validateBeeperChat(source, intent.ChatID, chat)
	if err != nil {
		return err
	}
	if !observation.Empty() {
		return draftReplyError("occupied", errors.New("Beeper chat already has a draft or an unrecognized draft shape"))
	}
	draft, err := a.store.BeginBeeperDraftCreateContext(ctx, source.ID, source.Identifier, intent.ChatID, intent.Body)
	if err != nil {
		return draftReplyError("claim_failed", err)
	}
	if err := a.store.RecordBeeperDraftOutcomeContext(ctx, draft.DraftID, draft.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"); err != nil {
		return draftReplyError("local_persistence_failed", err)
	}
	updated, err := client.UpdateDraft(ctx, intent.ChatID, &intent.Body)
	if err != nil {
		return a.handleBeeperDraftWriteError(ctx, intent, draft, err, emit)
	}
	observed, err := validateBeeperChat(source, intent.ChatID, updated)
	if err != nil {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, draft.DraftID, draft.Revision, store.BeeperDraftPhaseRemoteUnknown, "provider_identity_mismatch")
		return err
	}
	if observed.Empty() || observed.Unknown || observed.Text == "" {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, draft.DraftID, draft.Revision, store.BeeperDraftPhaseRemoteUnknown, "provider_unknown")
		return draftReplyError("remote_unknown", errors.New("Beeper did not return a managed text draft"))
	}
	finished, err := a.store.FinishBeeperDraftContext(ctx, draft.DraftID, draft.Revision, &observed.Text)
	if err != nil {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, draft.DraftID, draft.Revision, store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
		return draftReplyError("remote_accepted_local_failed", err)
	}
	return emitBeeperDraftOutput(emit, cliStreamStdout, intent.JSON, beeperDraftOutputFromDraft(finished, "created"))
}

func (a *storeAPIAdapter) runBeeperDraftMutation(ctx context.Context, intent beeperDraftIntent, draft store.BeeperDraft, source *store.Source, client *beeper.Client, emit func(api.CLIRunEvent) error) error {
	if draft.Revision != intent.Revision {
		return draftReplyError("revision_mismatch", fmt.Errorf("draft revision %d does not match %d", draft.Revision, intent.Revision))
	}
	chat, err := client.GetChat(ctx, draft.ChatID)
	if err != nil {
		return draftReplyError("provider_unavailable", err)
	}
	observation, err := validateBeeperChat(source, draft.ChatID, chat)
	if err != nil {
		return err
	}
	if observation.Unknown || (!observation.Empty() && observation.Text == "") || !observation.Present {
		return draftReplyError("conflict", errors.New("Beeper draft observation is missing or contains unsupported fields"))
	}
	if draft.Pending != nil {
		if draft.Pending.Operation != store.BeeperDraftOperationEdit || draft.Pending.Phase != store.BeeperDraftPhaseClearConfirmed {
			return draftReplyError("pending", errors.New("Beeper draft has an unresolved provider attempt"))
		}
		if intent.Body != draft.Pending.Candidate {
			return draftReplyError("pending_intent", errors.New("replacement does not match the saved Beeper draft candidate"))
		}
		if !observation.Empty() {
			return draftReplyError("conflict", errors.New("Beeper draft is not empty after the confirmed clear"))
		}
		return a.setBeeperDraft(ctx, intent, draft, source, client, emit)
	}
	if intent.Operation == "clear" && observation.Empty() {
		claimed, err := a.store.ClaimBeeperDraftContext(ctx, intent.DraftID, intent.Revision, store.BeeperDraftOperationDelete, "")
		if err != nil {
			return draftReplyError("claim_failed", err)
		}
		if err := a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseClearConfirmed, "already_empty"); err != nil {
			return draftReplyError("local_persistence_failed", err)
		}
		finished, err := a.store.FinishBeeperDraftContext(ctx, claimed.DraftID, claimed.Revision, nil)
		if err != nil {
			_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
			return draftReplyError("remote_accepted_local_failed", err)
		}
		return emitBeeperDraftOutput(emit, cliStreamStdout, intent.JSON, beeperDraftOutputFromDraft(finished, "cleared"))
	}
	if draft.CommittedText == nil {
		if !observation.Empty() {
			return draftReplyError("conflict", errors.New("Beeper draft changed outside msgvault"))
		}
	} else if observation.Empty() || observation.Text != *draft.CommittedText {
		return draftReplyError("conflict", errors.New("Beeper draft changed outside msgvault"))
	}
	if intent.Operation == "edit" && draft.CommittedText == nil {
		claimed, err := a.store.ClaimBeeperDraftContext(ctx, intent.DraftID, intent.Revision, store.BeeperDraftOperationEdit, intent.Body)
		if err != nil {
			return draftReplyError("claim_failed", err)
		}
		return a.setBeeperDraft(ctx, intent, claimed, source, client, emit)
	}
	claimed, err := a.store.ClaimBeeperDraftContext(ctx, intent.DraftID, intent.Revision, map[string]string{"edit": store.BeeperDraftOperationEdit, "clear": store.BeeperDraftOperationDelete}[intent.Operation], intent.Body)
	if err != nil {
		return draftReplyError("claim_failed", err)
	}
	if err := a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseClearDispatched, "dispatching"); err != nil {
		return draftReplyError("local_persistence_failed", err)
	}
	cleared, err := client.UpdateDraft(ctx, claimed.ChatID, nil)
	if err != nil {
		return a.handleBeeperDraftWriteError(ctx, intent, claimed, err, emit)
	}
	clearObservation, err := validateBeeperChat(source, claimed.ChatID, cleared)
	if err != nil {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseRemoteUnknown, "provider_identity_mismatch")
		return err
	}
	if !clearObservation.Empty() {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseRemoteUnknown, "clear_not_confirmed")
		return draftReplyError("remote_unknown", errors.New("Beeper did not confirm an empty draft"))
	}
	if err := a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseClearConfirmed, "cleared"); err != nil {
		return draftReplyError("local_persistence_failed", err)
	}
	if intent.Operation == "clear" {
		finished, err := a.store.FinishBeeperDraftContext(ctx, claimed.DraftID, claimed.Revision, nil)
		if err != nil {
			_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
			return draftReplyError("remote_accepted_local_failed", err)
		}
		return emitBeeperDraftOutput(emit, cliStreamStdout, intent.JSON, beeperDraftOutputFromDraft(finished, "cleared"))
	}
	claimed.Pending.Phase = store.BeeperDraftPhaseClearConfirmed
	return a.setBeeperDraft(ctx, intent, claimed, source, client, emit)
}

func (a *storeAPIAdapter) setBeeperDraft(ctx context.Context, intent beeperDraftIntent, claimed store.BeeperDraft, source *store.Source, client *beeper.Client, emit func(api.CLIRunEvent) error) error {
	if err := a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"); err != nil {
		return draftReplyError("local_persistence_failed", err)
	}
	updated, err := client.UpdateDraft(ctx, claimed.ChatID, &intent.Body)
	if err != nil {
		return a.handleBeeperDraftWriteError(ctx, intent, claimed, err, emit)
	}
	observed, err := validateBeeperChat(source, claimed.ChatID, updated)
	if err != nil {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseRemoteUnknown, "provider_identity_mismatch")
		return err
	}
	if observed.Empty() || observed.Unknown || observed.Text == "" {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseRemoteUnknown, "set_not_confirmed")
		return draftReplyError("remote_unknown", errors.New("Beeper did not return a managed text draft"))
	}
	finished, err := a.store.FinishBeeperDraftContext(ctx, claimed.DraftID, claimed.Revision, &observed.Text)
	if err != nil {
		_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
		return draftReplyError("remote_accepted_local_failed", err)
	}
	return emitBeeperDraftOutput(emit, cliStreamStdout, intent.JSON, beeperDraftOutputFromDraft(finished, "edited"))
}

func (a *storeAPIAdapter) handleBeeperDraftWriteError(ctx context.Context, intent beeperDraftIntent, claimed store.BeeperDraft, writeErr error, emit func(api.CLIRunEvent) error) error {
	var providerErr *beeper.DraftWriteError
	if errors.As(writeErr, &providerErr) && providerErr.State == beeper.DraftWriteRejected {
		if claimed.Pending != nil && claimed.Pending.Operation == store.BeeperDraftOperationEdit && claimed.Pending.Phase == store.BeeperDraftPhaseClearConfirmed {
			code := "set_rejected:" + providerErr.Code
			if err := a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseClearConfirmed, code); err != nil {
				return draftReplyError("local_persistence_failed", err)
			}
			output := beeperDraftOutputFromDraft(claimed, "rejected")
			output.PendingPhase = store.BeeperDraftPhaseClearConfirmed
			output.OutcomeCode = code
			output.ProviderStatus = providerErr.Code
			_ = emitBeeperDraftOutput(emit, cliStreamStderr, intent.JSON, output)
			return draftReplyError(providerErr.Code, errors.New("Beeper rejected the replacement after clearing the managed draft"))
		}
		if err := a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseRejected, providerErr.Code); err != nil {
			return draftReplyError("local_persistence_failed", err)
		}
		if err := a.store.AbortBeeperDraftClaimContext(ctx, claimed.DraftID, claimed.Revision); err != nil {
			return draftReplyError("local_persistence_failed", err)
		}
		return draftReplyError(providerErr.Code, errors.New("Beeper rejected the draft update"))
	}
	_ = a.store.RecordBeeperDraftOutcomeContext(ctx, claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseRemoteUnknown, "remote_unknown")
	output := beeperDraftOutputFromDraft(claimed, "remote_unknown")
	output.ProviderStatus = "remote_unknown"
	_ = emitBeeperDraftOutput(emit, cliStreamStderr, intent.JSON, output)
	return draftReplyError("remote_unknown", errors.New("Beeper draft update outcome is unknown"))
}
