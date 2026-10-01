package cmd

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type beeperDraftOutput struct {
	Status           string  `json:"status"`
	DraftID          string  `json:"draft_id"`
	Revision         int64   `json:"revision"`
	Lifecycle        string  `json:"lifecycle"`
	SourceID         int64   `json:"source_id"`
	ChatID           string  `json:"chat_id"`
	Content          *string `json:"content"`
	PendingOperation string  `json:"pending_operation,omitempty"`
	CandidateContent string  `json:"candidate_content,omitempty"`
}

func emitBeeperDraft(emit func(api.CLIRunEvent) error, stream string, asJSON bool, status string, draft store.BeeperDraft) error {
	if emit == nil {
		return nil
	}
	output := beeperDraftOutput{
		Status: status, DraftID: draft.DraftID, Revision: draft.Revision, Lifecycle: draftLifecycleActive,
		SourceID: draft.SourceID, ChatID: draft.ChatID, Content: draft.Text,
	}
	if draft.DiscardedAt != nil {
		output.Lifecycle = "discarded"
	}
	if draft.Pending != nil {
		output.PendingOperation, output.CandidateContent = draft.Pending.Operation, draft.Pending.Text
	}
	if asJSON {
		data, err := jsonv2.Marshal(output)
		if err != nil {
			return err
		}
		return emit(api.CLIRunEvent{Type: stream, Data: string(data) + "\n"})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "draft %s revision %d %s\n", textutil.SanitizeTerminal(output.DraftID), output.Revision, output.Lifecycle)
	fmt.Fprintf(&b, "status: %s\n", textutil.SanitizeTerminal(output.Status))
	fmt.Fprintf(&b, "beeper chat: %s\n", textutil.SanitizeTerminal(output.ChatID))
	if output.Content != nil {
		fmt.Fprintf(&b, "content:\n%s\n", strings.TrimRight(textutil.SanitizeTerminalMultiline(*output.Content), "\n"))
	}
	if output.PendingOperation != "" {
		fmt.Fprintf(&b, "pending operation: %s\n", output.PendingOperation)
	}
	if output.CandidateContent != "" {
		fmt.Fprintf(&b, "candidate content:\n%s\n", strings.TrimRight(textutil.SanitizeTerminalMultiline(output.CandidateContent), "\n"))
	}
	return emit(api.CLIRunEvent{Type: stream, Data: b.String()})
}

// beeperDraftFailure shows the draft's recorded state before returning code.
func (a *storeAPIAdapter) beeperDraftFailure(ctx context.Context, emit func(api.CLIRunEvent) error, asJSON bool, draftID, code string, cause error) error {
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	if draft, err := a.store.GetBeeperDraftContext(evidenceCtx, draftID); err == nil {
		_ = emitBeeperDraft(emit, cliStreamStderr, asJSON, code, draft)
	}
	return draftReplyError(code, cause)
}

// lockBeeperSource serializes draft writes on a source, so one command's
// read, claim, write and finish never interleave with another's.
func (a *storeAPIAdapter) lockBeeperSource(ctx context.Context, sourceID int64) (*store.SyncExecution, error) {
	execution, err := a.store.AcquireSyncExecutionContext(ctx, sourceID)
	if errors.Is(err, store.ErrSyncAlreadyActive) {
		return nil, draftReplyError("sync_active", err)
	}
	if err != nil {
		return nil, draftReplyError("sync_lock_failed", err)
	}
	return execution, nil
}

func (a *storeAPIAdapter) beeperDraftClient() (*beeper.Client, error) {
	token, err := beeper.LoadToken(a.config.TokensDir())
	if err != nil {
		return nil, draftReplyError("provider_unavailable", err)
	}
	return beeperClient(a.config, token), nil
}

// readBeeperDraft reads the chat's composer from Beeper and checks that the
// chat belongs to the source's account.
func readBeeperDraft(ctx context.Context, client *beeper.Client, source *store.Source, chatID string) (beeper.DraftState, error) {
	chat, err := client.GetChat(ctx, chatID)
	if errors.Is(err, beeper.ErrNotFound) {
		return beeper.DraftState{}, draftReplyError("chat_not_found", err)
	}
	if err != nil {
		return beeper.DraftState{}, draftReplyError("provider_unavailable", err)
	}
	if chat.ID != chatID || chat.AccountID != source.Identifier {
		return beeper.DraftState{}, draftReplyError("provider_identity_mismatch", errors.New("beeper returned another chat"))
	}
	state, err := chat.DraftState()
	if err != nil {
		return beeper.DraftState{}, draftReplyError("not_supported", err)
	}
	return state, nil
}

// beeperDraftMatches reports whether Beeper still shows the committed draft.
func beeperDraftMatches(state beeper.DraftState, text *string) bool {
	if state.Other {
		return false
	}
	if text == nil {
		return state.Empty
	}
	return !state.Empty && state.Text == *text
}

func validBeeperDraftBody(body string) error {
	if strings.TrimSpace(body) == "" || strings.ContainsRune(body, 0) {
		return draftReplyError("invalid_args", errors.New("--body must be nonblank text"))
	}
	return nil
}

// runBeeperDraftCreate writes a new draft into an empty Beeper chat composer.
// --to names the chat.
func (a *storeAPIAdapter) runBeeperDraftCreate(ctx context.Context, grant *agentgrant.Grant, intent draftComposeIntent, source *store.Source, emit func(api.CLIRunEvent) error) error {
	if err := authorizeDelegatedDraftSource(grant, source); err != nil {
		return err
	}
	if len(intent.To) != 1 || len(intent.Cc)+len(intent.Bcc) != 0 || intent.Subject != "" || intent.From != "" {
		return draftReplyError("invalid_args", errors.New("a Beeper draft takes one --to chat ID, --body, and no other fields"))
	}
	if err := validBeeperDraftBody(intent.Body); err != nil {
		return err
	}
	if err := authorizeDraftPolicy("beeper", a.beeperDraftPolicy, source.ID, source.SourceType); err != nil {
		return err
	}
	client, err := a.beeperDraftClient()
	if err != nil {
		return err
	}
	execution, err := a.lockBeeperSource(ctx, source.ID)
	if err != nil {
		return err
	}
	defer func() { _ = execution.Release() }()
	chatID := strings.TrimSpace(intent.To[0])
	if existing, err := a.store.LiveBeeperDraftContext(ctx, source.ID, chatID); err == nil {
		if grant != nil {
			// Reading draft text needs draft.edit or draft.delete; creators get the ID.
			existing.Text, existing.Pending = nil, nil
		}
		_ = emitBeeperDraft(emit, cliStreamStderr, intent.JSON, "draft_exists", existing)
		return draftReplyError("draft_exists", store.ErrBeeperDraftExists)
	} else if !errors.Is(err, store.ErrBeeperDraftNotFound) {
		return draftReplyError("draft_read_failed", err)
	}
	state, err := readBeeperDraft(ctx, client, source, chatID)
	if err != nil {
		return err
	}
	if !state.Empty {
		return draftReplyError("draft_conflict", errors.New("beeper chat already has a draft"))
	}
	draft, err := a.store.CreateBeeperDraftContext(ctx, source.ID, chatID, intent.Body)
	if errors.Is(err, store.ErrBeeperDraftExists) {
		return draftReplyError("draft_exists", err)
	}
	if err != nil {
		return draftReplyError("local_persistence_failed", err)
	}
	return a.writeBeeperDraft(ctx, client, source, draft, state, intent.JSON, "created", emit)
}

// runBeeperDraftLifecycle serves draft-get, draft-edit and draft-delete for
// a Beeper draft. Each write first checks that Beeper still shows the
// committed draft, so text someone else typed in the chat is never replaced.
func (a *storeAPIAdapter) runBeeperDraftLifecycle(ctx context.Context, intent draftLifecycleIntent, grant *agentgrant.Grant, draft store.BeeperDraft, emit func(api.CLIRunEvent) error) error {
	if intent.Operation == api.CLIRunDraftRecoverCommand {
		if grant != nil {
			return draftReplyNotPermitted(errors.New("draft-recover supports IMAP drafts only"))
		}
		return draftReplyError("not_supported", errors.New("draft-recover supports IMAP drafts only"))
	}
	if grant != nil {
		source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
		if err != nil {
			return draftReplyNotPermitted(err)
		}
		permissions := api.CLIRunDraftLifecyclePermissions(intent.Operation)
		// Native drafts have no archived sender; create alone cannot read them.
		if intent.Operation == api.CLIRunDraftGetCommand {
			permissions = []agentgrant.Permission{agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete}
		}
		if err := chatDraftAuthorizer(grant, permissions...)(source.SourceType, source.Identifier); err != nil {
			return err
		}
	}
	if intent.Operation == api.CLIRunDraftGetCommand {
		return emitBeeperDraft(emit, cliStreamStdout, intent.JSON, "ok", draft)
	}
	if draft.Revision != intent.Revision {
		return draftReplyError("revision_mismatch", fmt.Errorf("expected revision %d, found %d", intent.Revision, draft.Revision))
	}
	if draft.DiscardedAt != nil {
		if intent.Operation == api.CLIRunDraftDeleteCommand {
			return emitBeeperDraft(emit, cliStreamStdout, intent.JSON, "already_discarded", draft)
		}
		return draftReplyError("draft_discarded", errors.New("discarded drafts cannot be edited"))
	}
	if intent.Operation == api.CLIRunDraftEditCommand {
		if err := validBeeperDraftBody(intent.Body); err != nil {
			return err
		}
	}
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return draftReplyError("invalid_source", err)
	}
	if err := authorizeDraftPolicy("beeper", a.beeperDraftPolicy, source.ID, source.SourceType); err != nil {
		return err
	}
	client, err := a.beeperDraftClient()
	if err != nil {
		return err
	}
	execution, err := a.lockBeeperSource(ctx, source.ID)
	if err != nil {
		return err
	}
	defer func() { _ = execution.Release() }()
	if draft, err = a.store.GetBeeperDraftContext(ctx, draft.DraftID); err != nil {
		return draftReplyError("draft_read_failed", err)
	}
	if draft.Revision != intent.Revision || draft.DiscardedAt != nil {
		return draftReplyError("revision_mismatch", errors.New("draft changed while acquiring source ownership"))
	}
	state, err := readBeeperDraft(ctx, client, source, draft.ChatID)
	if err != nil {
		return err
	}
	if draft.Pending != nil {
		// Settle an interrupted write from what Beeper shows now.
		switch {
		case draft.Pending.Operation == store.BeeperDraftOperationDelete && state.Empty:
			finished, err := a.store.FinishBeeperDraftContext(ctx, draft.DraftID, draft.Revision, "")
			if err != nil {
				return draftReplyError("local_persistence_failed", err)
			}
			if intent.Operation != api.CLIRunDraftDeleteCommand {
				_ = emitBeeperDraft(emit, cliStreamStderr, intent.JSON, "recovered", finished)
				return draftReplyError("draft_discarded", errors.New("an earlier delete finished; the draft is discarded"))
			}
			return emitBeeperDraft(emit, cliStreamStdout, intent.JSON, "deleted", finished)
		case state.Empty, beeperDraftMatches(state, draft.Text):
			if draft, err = a.store.AbortBeeperDraftContext(ctx, draft.DraftID, draft.Revision); err != nil {
				return draftReplyError("local_persistence_failed", err)
			}
			if draft.DiscardedAt != nil && intent.Operation == api.CLIRunDraftDeleteCommand {
				return emitBeeperDraft(emit, cliStreamStdout, intent.JSON, "deleted", draft)
			}
			if draft.DiscardedAt != nil {
				_ = emitBeeperDraft(emit, cliStreamStderr, intent.JSON, "recovered", draft)
				return draftReplyError("draft_discarded", errors.New("the draft's first write never reached Beeper; the draft is discarded"))
			}
		default:
			return a.beeperDraftFailure(ctx, emit, intent.JSON, draft.DraftID, "pending_operation",
				errors.New("beeper shows a draft that may be an unconfirmed write; clear it in Beeper to continue"))
		}
	}
	// An empty composer holds nothing to protect, so a write may proceed.
	if !state.Empty && !beeperDraftMatches(state, draft.Text) {
		return draftReplyError("draft_conflict", errors.New("the Beeper draft changed outside msgvault"))
	}
	operation, status := store.BeeperDraftOperationEdit, "edited"
	if intent.Operation == api.CLIRunDraftDeleteCommand {
		operation, status = store.BeeperDraftOperationDelete, "deleted"
	}
	claimed, err := a.store.ClaimBeeperDraftContext(ctx, draft.DraftID, draft.Revision, operation, intent.Body)
	if err != nil {
		if errors.Is(err, store.ErrBeeperDraftRevision) {
			return draftReplyError("revision_mismatch", err)
		}
		return draftReplyError("claim_failed", err)
	}
	return a.writeBeeperDraft(ctx, client, source, claimed, state, intent.JSON, status, emit)
}

// writeBeeperDraft sends a claimed write and commits what Beeper reports. A
// write Beeper refused before anything changed drops the claim; any other
// failure keeps it for the next command to settle.
func (a *storeAPIAdapter) writeBeeperDraft(ctx context.Context, client *beeper.Client, source *store.Source, claimed store.BeeperDraft, state beeper.DraftState, asJSON bool, status string, emit func(api.CLIRunEvent) error) error {
	changed := false
	fail := func(err error) error {
		evidenceCtx, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		var writeErr *beeper.DraftWriteError
		if errors.As(err, &writeErr) && writeErr.Status >= 400 && writeErr.Status < 500 && !changed {
			if _, abortErr := a.store.AbortBeeperDraftContext(evidenceCtx, claimed.DraftID, claimed.Revision); abortErr == nil {
				return a.beeperDraftFailure(ctx, emit, asJSON, claimed.DraftID, "provider_rejected", err)
			}
		}
		return a.beeperDraftFailure(ctx, emit, asJSON, claimed.DraftID, "remote_unknown", err)
	}
	if !state.Empty {
		chat, err := client.SetDraft(ctx, claimed.ChatID, nil)
		if err != nil {
			return fail(err)
		}
		changed = true
		if state, err = chat.DraftState(); err != nil || !state.Empty {
			return fail(errors.New("beeper did not confirm the cleared draft"))
		}
	}
	text := ""
	if claimed.Pending.Operation == store.BeeperDraftOperationEdit {
		chat, err := client.SetDraft(ctx, claimed.ChatID, &claimed.Pending.Text)
		if err != nil {
			return fail(err)
		}
		changed = true
		if state, err = chat.DraftState(); err != nil || state.Empty || state.Other || chat.ID != claimed.ChatID || chat.AccountID != source.Identifier {
			return fail(errors.New("beeper did not confirm the draft text"))
		}
		text = state.Text
	}
	evidenceCtx, cancel := localDraftEvidenceContext(ctx)
	defer cancel()
	finished, err := a.store.FinishBeeperDraftContext(evidenceCtx, claimed.DraftID, claimed.Revision, text)
	if err != nil {
		return a.beeperDraftFailure(ctx, emit, asJSON, claimed.DraftID, "accepted_local_failed", err)
	}
	return emitBeeperDraft(emit, cliStreamStdout, asJSON, status, finished)
}
