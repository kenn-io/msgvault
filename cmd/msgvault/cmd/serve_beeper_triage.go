package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/store"
)

type beeperTriageRow struct {
	store.ConversationGTD

	Native    *beeperNativeState `json:"native,omitempty"`
	Freshness string             `json:"freshness"`
	Draft     *beeperDraftOutput `json:"draft"`
}

// Enrichment exposes observations, never composer text or participant content.
type beeperNativeState struct {
	ID           string                   `json:"id"`
	AccountID    string                   `json:"accountID"`
	Labels       *[]beeper.Label          `json:"labels"`
	IsArchived   *bool                    `json:"isArchived"`
	Capabilities *beeper.ChatCapabilities `json:"capabilities"`
	LastActivity time.Time                `json:"lastActivity"`
}

func beeperChatIdentityMatches(chat *beeper.Chat, account, chatID string) bool {
	return chat.ID == chatID && chat.AccountID == account && chat.Merge == nil && chat.MergedIntoChatID == ""
}

func beeperEmit(emit func(api.CLIRunEvent) error, value any) error {
	if emit == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(body) + "\n"})
}

func (a *storeAPIAdapter) beeperSource(ctx context.Context, id int64, live bool) (*store.Source, error) {
	source, err := a.store.GetSourceByIDContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if source.SourceType != "beeper" {
		return nil, errors.New("source is not Beeper")
	}
	if live && (source.HistoryOnly || source.MergedIntoSourceID != 0 || source.ReanchorRequired) {
		return nil, errors.New("source is not an eligible live Beeper source")
	}
	if live && a.config == nil {
		return nil, errors.New("beeper configuration unavailable")
	}
	if live && !a.config.Beeper.AccountIncluded(source.Identifier) {
		return nil, errors.New("beeper account is excluded")
	}
	return source, nil
}

func (a *storeAPIAdapter) runCLIBeeper(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if req.Grant != nil {
		return errors.New("beeper triage requires owner authentication")
	}
	if req.Cwd != "" || len(req.Env) != 0 {
		return errors.New("beeper triage accepts no environment or working directory")
	}
	i, err := parseBeeperArgs(req.Args)
	if err != nil {
		return err
	}
	switch i.Operation {
	case beeperLabelsOperation:
		if _, err = a.beeperSource(ctx, i.SourceID, true); err != nil {
			return err
		}
		client, err := a.beeperDraftClient()
		if err != nil {
			return err
		}
		labels, err := client.ListLabels(ctx)
		if err != nil {
			return fmt.Errorf("native label catalog unavailable: %w", err)
		}
		return beeperEmit(emit, map[string]any{"scope": "beeper_installation", "native_label_writes": "unsupported", beeperLabelsOperation: labels})
	case beeperTriageOperation:
		return a.runBeeperTriage(ctx, i, emit)
	case "context":
		var lower *beeper.ContextAnchor
		if i.Lower != "" {
			lower = new(beeper.ContextAnchor)
			if err = json.Unmarshal([]byte(i.Lower), lower, json.RejectUnknownMembers(true)); err != nil || !lower.Valid() {
				return errors.New("invalid lower anchor")
			}
		}
		snapshot, err := a.runBeeperContextSnapshot(ctx, i.ConversationID, i.Bounds, lower, bool(i.InitialReview))
		if err != nil {
			return err
		}
		body, err := json.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("encode context: %w", err)
		}
		if len(body) > i.Bounds.MaxBytes {
			return errors.New("context byte limit cannot fit result metadata")
		}
		return beeperEmit(emit, snapshot)
	case "classify":
		return a.runBeeperClassify(ctx, i, emit)
	case beeperArchiveOperation:
		return a.runBeeperArchive(ctx, i, emit)
	}
	return errors.New("unknown Beeper operation")
}

func (a *storeAPIAdapter) runBeeperContextSnapshot(ctx context.Context, id int64, b beeper.ContextBounds, lower *beeper.ContextAnchor, initial bool) (beeper.ContextSnapshot, error) {
	row, err := a.store.GetConversationGTDContext(ctx, id)
	if err != nil {
		return beeper.ContextSnapshot{}, err
	}
	source, err := a.beeperSource(ctx, row.SourceID, true)
	if err != nil {
		return beeper.ContextSnapshot{}, err
	}
	if lower == nil && !initial && len(row.ReviewedHead) > 0 {
		lower = new(beeper.ContextAnchor)
		if err = json.Unmarshal(row.ReviewedHead, lower, json.RejectUnknownMembers(true)); err != nil {
			return beeper.ContextSnapshot{}, errors.New("invalid stored reviewed head")
		}
	}
	client, err := a.beeperDraftClient()
	if err != nil {
		return beeper.ContextSnapshot{Reason: "provider_unavailable"}, nil //nolint:nilerr // Provider setup failure is an explicit held context result, not a fatal command error.
	}
	return client.CaptureContext(ctx, source.Identifier, row.ChatID, lower, b)
}

func (a *storeAPIAdapter) runBeeperTriage(ctx context.Context, i beeperIntent, emit func(api.CLIRunEvent) error) error {
	source, err := a.beeperSource(ctx, i.SourceID, bool(i.Catalog))
	if err != nil {
		return err
	}
	if i.Catalog {
		client, err := a.beeperDraftClient()
		if err != nil {
			return err
		}
		// One page per call. The caller owns a bounded catalog traversal; this is
		// coverage observation, never a proof that all message history was synced.
		page, err := client.ForTriage().SearchChats(ctx, beeper.SearchChatsParams{AccountID: source.Identifier, Cursor: i.CatalogCursor})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		ids := []string{}
		if page.Items == nil || len(page.Items) > 200 {
			return errors.New("native catalog page exceeds bound")
		}
		omitted := 0
		for _, chat := range page.Items {
			if chat.ID == "" || chat.AccountID != source.Identifier {
				omitted++
				continue
			}
			if seen[chat.ID] {
				continue
			}
			seen[chat.ID] = true
			ids = append(ids, chat.ID)
		}
		if page.HasMore && (len(page.Items) == 0 || page.OldestCursor == "" || page.OldestCursor == i.CatalogCursor) {
			return errors.New("native catalog did not advance")
		}
		archived, err := a.store.BeeperConversationIDsContext(ctx, source.ID, ids)
		if err != nil {
			return err
		}
		catalog := []map[string]any{}
		for _, chat := range ids {
			catalog = append(catalog, map[string]any{"chat_id": chat, "conversation_id": archived[chat]})
		}
		return beeperEmit(emit, map[string]any{"source_id": source.ID, "source_identifier": source.Identifier, "catalog": catalog, "has_more": page.HasMore, "next_cursor": page.OldestCursor, "scope": "observed_native_chat_catalog", "omitted_count": omitted, "coverage_incomplete": omitted > 0})
	}
	if i.CatalogCursor != "" {
		return errors.New("catalog-cursor requires catalog")
	}
	rows, err := a.store.ListConversationGTDContext(ctx, source.ID, i.After, i.Limit+1, bool(i.NeedsAnswer))
	if err != nil {
		return err
	}
	more := len(rows) > i.Limit
	if more {
		rows = rows[:i.Limit]
	}
	ceiling, err := a.store.BeeperConversationCeilingContext(ctx, source.ID)
	if err != nil {
		return err
	}
	result := []beeperTriageRow{}
	var client *beeper.Client
	var liveErr error
	if i.Refresh {
		_, liveErr = a.beeperSource(ctx, source.ID, true)
		if liveErr == nil {
			client, _ = a.beeperDraftClient()
			if client != nil {
				client = client.ForTriage()
			}
		}
	}
	for _, row := range rows {
		view := beeperTriageRow{ConversationGTD: row, Freshness: "not_refreshed"}
		draft, err := a.store.LiveBeeperDraftContext(ctx, source.ID, row.ChatID)
		if err == nil {
			view.Draft = &beeperDraftOutput{DraftID: draft.DraftID, Revision: draft.Revision, Lifecycle: accountStateActive, SourceID: source.ID, ChatID: row.ChatID}
			if draft.Pending != nil {
				view.Draft.PendingOperation = draft.Pending.Operation
			}
		} else if !errors.Is(err, store.ErrBeeperDraftNotFound) {
			return err
		}
		if i.Refresh {
			view.Freshness = "unavailable"
			if liveErr != nil {
				view.Freshness = "source_ineligible"
			}
			if client != nil {
				chat, e := client.GetChat(ctx, row.ChatID)
				if e == nil && !beeperChatIdentityMatches(chat, source.Identifier, row.ChatID) {
					view.Freshness = "identity_mismatch"
				} else if e == nil {
					var labels *[]beeper.Label
					if chat.Labels != nil {
						labels = &chat.Labels
					}
					view.Native = &beeperNativeState{ID: chat.ID, AccountID: chat.AccountID, Labels: labels, IsArchived: chat.IsArchived, Capabilities: chat.Capabilities, LastActivity: chat.LastActivity}
					view.Freshness = "unreviewed"
					if row.ObservedActivity != "" {
						view.Freshness = "stale"
						if chat.LastActivity.UTC().Format(time.RFC3339Nano) == row.ObservedActivity {
							view.Freshness = "activity_matches"
						}
					}
				}
			}
		}
		result = append(result, view)
	}
	next := i.After
	if len(rows) > 0 {
		next = rows[len(rows)-1].ConversationID
	}
	return beeperEmit(emit, map[string]any{"source_id": source.ID, "source_identifier": source.Identifier, "conversations": result, "has_more": more, "next_after": next, "ceiling": ceiling, "scope": "archived_conversations"})
}

func (a *storeAPIAdapter) runBeeperClassify(ctx context.Context, i beeperIntent, emit func(api.CLIRunEvent) error) error {
	row, err := a.store.GetConversationGTDContext(ctx, i.ConversationID)
	if err != nil {
		return err
	}
	change := store.ConversationGTDChange{Category: i.Category, Clear: bool(i.Clear), ObservedActivity: i.Observed, ContextStamp: i.Stamp, ReviewedHead: jsontext.Value(i.Head)}
	if i.Flags.Changed("needs-answer") {
		v := bool(i.NeedsAnswer)
		change.NeedsAnswer = &v
	}
	if i.Flags.Changed("revision") {
		change.ExpectedRevision = &i.Revision
	}
	if i.Head != "" {
		var head beeper.ContextAnchor
		if err = json.Unmarshal([]byte(i.Head), &head, json.RejectUnknownMembers(true)); err != nil || !head.Valid() {
			return errors.New("invalid reviewed head")
		}
	}
	if change.ExpectedRevision != nil && row.Revision != i.Revision {
		return store.ErrConversationGTDRevision
	}
	if !i.Apply {
		answer := i.Category == "todo"
		if row.Category != "" {
			answer = row.NeedsAnswer
		}
		if change.NeedsAnswer != nil {
			answer = *change.NeedsAnswer
		}
		if i.Clear {
			answer = false
		}
		var reviewedHead any
		if i.Head != "" {
			reviewedHead = jsontext.Value(i.Head)
		}
		return beeperEmit(emit, map[string]any{
			multimodalStatusSubcommand: "preview", "conversation_id": row.ConversationID,
			"revision": row.Revision, "category": i.Category, "needs_answer": answer, "clear": bool(i.Clear),
			"observed_activity": i.Observed, "context_stamp": i.Stamp, "reviewed_head": reviewedHead,
		})
	}
	if i.Apply {
		lock, e := a.lockBeeperSource(ctx, row.SourceID)
		if e != nil {
			return e
		}
		defer func() { _ = lock.Release() }()
	}
	saved, err := a.store.SetConversationGTDContext(ctx, row.ConversationID, change)
	if err != nil {
		return err
	}
	return beeperEmit(emit, saved)
}

func (a *storeAPIAdapter) runBeeperArchive(ctx context.Context, i beeperIntent, emit func(api.CLIRunEvent) error) error {
	row, err := a.store.GetConversationGTDContext(ctx, i.ConversationID)
	if err != nil {
		return err
	}
	if i.Apply {
		lock, e := a.lockBeeperSource(ctx, row.SourceID)
		if e != nil {
			return e
		}
		defer func() { _ = lock.Release() }()
	}
	row, err = a.store.GetConversationGTDContext(ctx, i.ConversationID)
	if err != nil {
		return err
	}
	var scopedClient *beeper.Client
	outcome := func(status string) error {
		requests := 0
		if scopedClient != nil {
			requests = scopedClient.Requests()
		}
		return beeperEmit(emit, map[string]any{multimodalStatusSubcommand: status, "conversation_id": row.ConversationID, "source_id": row.SourceID, "chat_id": row.ChatID, "revision": row.Revision, "archive_hold": row.ArchiveHold, "requests": requests})
	}
	if i.ResolveHold {
		if err = a.store.SetConversationGTDHoldContext(ctx, row.ConversationID, row.Revision, false); err != nil {
			return err
		}
		row.ArchiveHold = false
		return outcome("hold_resolved")
	}
	if row.Revision != i.Revision {
		return store.ErrConversationGTDRevision
	}
	source, err := a.beeperSource(ctx, row.SourceID, true)
	if err != nil {
		return err
	}
	client, err := a.beeperDraftClient()
	if err != nil {
		return err
	}
	requestLimit := i.Bounds.MaxRequests
	if i.Reconcile {
		requestLimit = 1
	}
	client, closeScope, err := client.ForBoundedOperations(requestLimit)
	if err != nil {
		return err
	}
	defer closeScope()
	scopedClient = client
	ctx, cancel := context.WithTimeout(ctx, i.Bounds.Timeout)
	defer cancel()
	chat, err := client.GetChat(ctx, row.ChatID)
	if err != nil {
		return err
	}
	if !beeperChatIdentityMatches(chat, source.Identifier, row.ChatID) {
		return errors.New("archive chat identity mismatch")
	}
	if chat.IsArchived == nil {
		return errors.New("native archive state unavailable")
	}
	const draftHeld = "draft_held"
	var requiredDraft *store.BeeperDraft
	if i.RequiredDraftID != "" {
		draft, readErr := a.store.GetBeeperDraftContext(ctx, i.RequiredDraftID)
		if readErr != nil || draft.SourceID != row.SourceID || draft.ChatID != row.ChatID || draft.Revision != i.RequiredDraftRevision || draft.DiscardedAt != nil || draft.Pending != nil || draft.Text == nil || *draft.Text == "" {
			return outcome(draftHeld)
		}
		requiredDraft = &draft
	}
	requiredDraftMatches := func(native *beeper.Chat) bool {
		if requiredDraft == nil {
			return true
		}
		state, readErr := native.DraftState()
		return readErr == nil && beeperDraftMatches(state, requiredDraft.Text)
	}
	if !requiredDraftMatches(chat) {
		return outcome(draftHeld)
	}
	if *chat.IsArchived {
		if bool(i.Apply) && row.ArchiveHold {
			if err = a.store.SetConversationGTDHoldContext(ctx, row.ConversationID, row.Revision, false); err != nil {
				return err
			}
			row.ArchiveHold = false
		}
		return outcome("already_archived")
	}
	if i.Reconcile {
		return outcome("not_archived")
	}
	if row.ArchiveHold {
		return outcome("archive_hold")
	}
	if row.Category == "" {
		return outcome("unclassified")
	}
	if i.Policy == "retain-actionable" && (row.Category != "reference" || row.NeedsAnswer) {
		return outcome("retained")
	}
	if chat.Capabilities == nil || chat.Capabilities.Archive == nil || !*chat.Capabilities.Archive {
		return errors.New("native archive capability unavailable")
	}
	if row.ObservedActivity == "" || row.ObservedActivity != i.Observed || chat.LastActivity.UTC().Format(time.RFC3339Nano) != i.Observed {
		return errors.New("reviewed activity changed")
	}
	// The saved head is the result of review, never its original lower.
	var reviewedHead beeper.ContextAnchor
	if len(row.ContextStamp) != 64 || json.Unmarshal(row.ReviewedHead, &reviewedHead, json.RejectUnknownMembers(true)) != nil || !reviewedHead.Valid() {
		return outcome("unreviewed")
	}
	var lower *beeper.ContextAnchor
	if i.Lower != "" {
		lower = new(beeper.ContextAnchor)
		if json.Unmarshal([]byte(i.Lower), lower, json.RejectUnknownMembers(true)) != nil || !lower.Valid() {
			return outcome("invalid_reviewed_scope")
		}
	}
	// Early P is already charged; reserve final F, one W and readback B.
	bounds := i.Bounds
	bounds.MaxRequests = min(bounds.MaxRequests, requestLimit-client.Requests()-3)
	if bounds.MaxRequests < 1 {
		return outcome("context_request_limit")
	}
	packet, captureErr := client.CaptureContext(ctx, source.Identifier, row.ChatID, lower, bounds)
	if captureErr != nil || !packet.Complete || packet.Stamp != row.ContextStamp || packet.Activity != row.ObservedActivity || packet.Head.Cursor != reviewedHead.Cursor || packet.Head.MessageID != reviewedHead.MessageID || packet.Head.Digest != reviewedHead.Digest || !packet.Head.Timestamp.Equal(reviewedHead.Timestamp) {
		return outcome("context_changed_or_unavailable")
	}
	final, err := client.GetChat(ctx, row.ChatID)
	if err != nil || !beeperChatIdentityMatches(final, source.Identifier, row.ChatID) || final.IsArchived == nil {
		return outcome("provider_unavailable")
	}
	if !requiredDraftMatches(final) {
		return outcome(draftHeld)
	}
	if *final.IsArchived {
		return outcome("already_archived")
	}
	if final.Capabilities == nil || final.Capabilities.Archive == nil || !*final.Capabilities.Archive || final.LastActivity.UTC().Format(time.RFC3339Nano) != packet.Activity {
		return outcome("context_changed_or_unavailable")
	}
	if !i.Apply {
		return outcome("preview")
	}
	if err = a.store.SetConversationGTDHoldContext(ctx, row.ConversationID, row.Revision, true); err != nil {
		return err
	}
	row.ArchiveHold = true
	writeErr := client.ArchiveChat(ctx, row.ChatID, true)
	// One readback settles current state; a failed POST alone is not a retry.
	after, readErr := client.GetChat(ctx, row.ChatID)
	if readErr == nil && beeperChatIdentityMatches(after, source.Identifier, row.ChatID) && after.IsArchived != nil && *after.IsArchived {
		if err = a.store.SetConversationGTDHoldContext(ctx, row.ConversationID, row.Revision, false); err != nil {
			return err
		}
		row.ArchiveHold = false
		return outcome("archived")
	}
	var rejected *beeper.ArchiveWriteError
	if errors.As(writeErr, &rejected) && rejected.Rejected() {
		if err = a.store.SetConversationGTDHoldContext(ctx, row.ConversationID, row.Revision, false); err != nil {
			return err
		}
		row.ArchiveHold = false
		return outcome("provider_rejected")
	}
	return outcome("unknown")
}
