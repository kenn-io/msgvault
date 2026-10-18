package loops

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
)

// Deriver applies bounded, resumable batches to the daemon-owned projection.
type Deriver struct {
	st  *store.Store
	cfg config.LoopsConfig
	now func() time.Time
	mu  sync.Mutex
}
type RunResult struct {
	FeedPages              int   `json:"feed_pages"`
	ChangedMessages        int   `json:"changed_messages"`
	RescanConversations    int   `json:"rescan_conversations"`
	ProcessedConversations int   `json:"processed_conversations"`
	Promoted               int64 `json:"promoted"`
	Unsnoozed              int64 `json:"unsnoozed"`
	BackfillScanned        int   `json:"backfill_scanned"`
	BackfillParsed         int   `json:"backfill_parsed"`
	BackfillMissing        int   `json:"backfill_missing"`
	BackfillEncrypted      int   `json:"backfill_encrypted"`
	BackfillUnsupported    int   `json:"backfill_unsupported"`
	BackfillTooLarge       int   `json:"backfill_too_large"`
	BackfillInvalid        int   `json:"backfill_invalid"`
}

func NewDeriver(st *store.Store, cfg *config.LoopsConfig) (*Deriver, error) {
	if st == nil {
		return nil, errors.New("open loops deriver requires a store")
	}
	c := config.DefaultLoopsConfig()
	if cfg != nil {
		c = *cfg
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Deriver{st: st, cfg: c, now: time.Now}, nil
}
func (d *Deriver) State(ctx context.Context) (store.LoopDerivationState, error) {
	return d.st.LoopDerivationStateContext(ctx)
}
func (d *Deriver) Freshness(ctx context.Context) (freshness.ProjectionState, error) {
	state, err := d.State(ctx)
	if err != nil {
		return freshness.ProjectionState{}, err
	}
	return freshness.ProjectionState{ComputedAt: state.AppliedThrough, LastSuccessAt: state.LastSuccessAt, BackfillComplete: &state.SignalsBackfillComplete, Extra: map[string]any{"signals_backfill_complete": state.SignalsBackfillComplete, "queue_depth": state.QueueDepth, "rescan_reason": state.RescanReason}}, nil
}
func (d *Deriver) RunOnce(ctx context.Context) (result RunResult, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().UTC()
	defer func() {
		// A preempted pass still records its error after earlier batches committed.
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, d.st.RecordLoopRunContext(recordCtx, now, err))
	}()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if _, err = synctechsms.BackfillNativeSendStateContext(ctx, d.st); err != nil {
		return result, fmt.Errorf("backfill Synctech SMS native send state: %w", err)
	}
	epoch, err := d.prepareRescan(ctx, now)
	if err != nil {
		return result, err
	}
	result.RescanConversations, err = d.st.EnqueueLoopRescanContext(ctx, epoch, d.cutoff(now), d.cfg.RescanBatch, now)
	if err != nil {
		return result, err
	}
	if err = d.consumeFeed(ctx, now, &result); err != nil {
		return result, err
	}
	if err = d.drain(ctx, now, &result); err != nil {
		return result, err
	}
	result.Promoted, result.Unsnoozed, err = d.st.AdvanceLoopTimersContext(ctx, now)
	if err != nil {
		return result, err
	}
	if err = d.backfill(ctx, now, &result); err != nil {
		return result, err
	}
	err = d.st.FinalizeLoopProgressContext(ctx, epoch, d.cutoff(now), now)
	return result, err
}
func (d *Deriver) drain(ctx context.Context, now time.Time, result *RunResult) error {
	queued, err := d.st.PendingLoopConversationsContext(ctx, d.cfg.BatchConversations)
	if err != nil {
		return err
	}
	suppressions, err := d.st.LoadLoopSuppressionsContext(ctx)
	if err != nil {
		return err
	}
	supp := Suppressions{}
	for _, row := range suppressions {
		if row.ParticipantID != nil {
			supp.Participants = append(supp.Participants, *row.ParticipantID)
		}
		if row.PersonID != nil {
			supp.Persons = append(supp.Persons, *row.PersonID)
		}
		if row.ConversationID != nil {
			supp.Conversations = append(supp.Conversations, *row.ConversationID)
		}
	}
	thresholds := Thresholds{ReplyOwedAfter: d.cfg.ReplyOwedAfter, AwaitingReplyAfter: d.cfg.AwaitingReplyAfter, Lookback: d.cfg.Lookback, MaxGroupCounterparts: d.cfg.MaxGroupCounterparts, RunMessageLimit: d.cfg.RunMessageLimit}
	for _, item := range queued {
		if err := ctx.Err(); err != nil {
			return err
		}
		view, err := d.loadView(ctx, item.ConversationID, d.cfg.ConversationMessageLimit)
		if errors.Is(err, store.ErrLoopNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		rows, err := d.st.ListLiveConversationLoopsContext(ctx, item.ConversationID)
		if err != nil {
			return err
		}
		live := []LiveLoop{}
		withdrawn := []store.LoopCloseDecision{}
		for _, row := range rows {
			if row.Detector != "turn_state" {
				continue
			}
			if row.AnchorMessageID == nil {
				withdrawn = append(withdrawn, store.LoopCloseDecision{ID: row.ID, Reason: "withdrawn", At: now})
				continue
			}
			ownerParticipationMessageID, err := ownerParticipationFromEvidence(row.EvidenceJSON)
			if err != nil {
				return fmt.Errorf("read owner participation evidence for loop %d: %w", row.ID, err)
			}
			loop := LiveLoop{ID: row.ID, Kind: row.Kind, AnchorMessageID: *row.AnchorMessageID, OwnerParticipationMessageID: ownerParticipationMessageID}
			if row.CounterpartyParticipantID != nil {
				loop.CounterpartyParticipantID = *row.CounterpartyParticipantID
			}
			if row.CounterpartyPersonID != nil {
				loop.CounterpartyPersonID = *row.CounterpartyPersonID
			}
			live = append(live, loop)
		}
		verdict := Detect(view, live, now, thresholds, supp)
		if verdict.Err != nil {
			return verdict.Err
		}
		applied, err := storedVerdict(view, verdict)
		if err != nil {
			return err
		}
		applied.Close = append(applied.Close, withdrawn...)
		if err := d.st.ApplyLoopVerdictContext(ctx, store.LoopApply{ConversationID: item.ConversationID, ObservedRevision: item.Revision, Epoch: item.Epoch, Now: now, Verdict: applied}); err != nil {
			return err
		}
		result.ProcessedConversations++
	}
	return nil
}
func (d *Deriver) loadView(ctx context.Context, id int64, limit int) (ConversationView, error) {
	row, err := d.st.LoadLoopConversationContext(ctx, id, limit)
	if err != nil {
		return ConversationView{}, err
	}
	view := ConversationView{ConversationID: row.ConversationID, SourceID: row.SourceID, SourceType: row.SourceType, ConversationType: row.ConversationType, LastMessageAt: row.LastMessageAt}
	for _, p := range row.Participants {
		view.Participants = append(view.Participants, detectorParticipant(p))
	}
	for _, m := range row.Messages {
		if m.OccurredAt.IsZero() {
			// An unknown date cannot anchor or settle a timed obligation. Keeping
			// its live row out of the window makes settlement retain a coverage gap.
			continue
		}
		message := LoopMessage{ID: m.ID, OccurredAt: m.OccurredAt, Direction: Direction(m.Direction), OwnerAddress: m.OwnerAddress, Sender: detectorParticipant(m.Sender), ListID: m.ListID, Labels: m.Labels, Deleted: m.Deleted, Subject: m.Subject, Snippet: m.Snippet}
		for _, recipient := range m.Recipients {
			message.Recipients = append(message.Recipients, LoopRecipient{LoopParticipant: detectorParticipant(recipient.LoopParticipant), Type: recipient.RecipientType})
		}
		if m.Signals != nil {
			message.Signals = &mime.AutomationSignals{AutoSubmitted: m.Signals.AutoSubmitted, PrecedenceBulk: m.Signals.PrecedenceBulk, ListUnsubscribe: m.Signals.ListUnsubscribe, CalendarPart: m.Signals.CalendarPart, ReplyToNoReply: m.Signals.ReplyToNoReply}
		}
		view.Messages = append(view.Messages, message)
	}
	view.BodyText = func(messageID int64) (string, error) { return d.st.LoopMessageBodyContext(ctx, messageID) }
	view.AnchorExists = func(messageID int64) (bool, error) {
		return d.st.LoopConversationAnchorExistsContext(ctx, id, messageID)
	}
	view.Extend = func() (ConversationView, error) { return d.loadView(ctx, id, d.cfg.ConversationMessageLimitExtended) }
	return view, nil
}
func detectorParticipant(p store.LoopParticipant) LoopParticipant {
	out := LoopParticipant{ID: p.ParticipantID, Email: p.EmailAddress, Phone: p.PhoneNumber, DisplayName: p.DisplayName, CanonicalID: p.CanonicalID, IsOwner: p.IsOwner}
	if p.PersonID != nil {
		out.PersonID = *p.PersonID
	}
	return out
}
func storedVerdict(view ConversationView, v Verdict) (store.LoopVerdict, error) {
	out := store.LoopVerdict{CoverageGaps: v.CoverageGaps}
	for _, close := range v.Close {
		decision := store.LoopCloseDecision{ID: close.ID, Reason: close.Reason, At: close.At}
		if close.Reason == "turn_flip" {
			id := close.MessageID
			decision.MessageID = &id
		}
		out.Close = append(out.Close, decision)
	}
	if v.Open == nil {
		return out, nil
	}
	spec := v.Open
	evidence, err := json.Marshal([]storedEvidence{{MessageID: spec.Anchor.ID, ConversationID: view.ConversationID, OccurredAt: spec.Anchor.OccurredAt, Subject: spec.Anchor.Subject, TruncatedRun: spec.TruncatedRun, OwnerParticipationMessageID: spec.OwnerParticipationMessageID}}, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return out, err
	}
	channel := "email"
	if view.ConversationType == "direct_chat" || view.ConversationType == "group_chat" {
		channel = "chat"
	}
	open := &store.LoopOpenDecision{TurnKey: fmt.Sprintf("turn:%d:%s:%d", view.ConversationID, spec.Kind, spec.Anchor.ID), Kind: spec.Kind, Status: spec.Status, Channel: channel, OwnerAddress: spec.Anchor.OwnerAddress, Summary: spec.Summary, ConversationID: view.ConversationID, SourceID: view.SourceID, AnchorMessageID: spec.Anchor.ID, LastActivityMessageID: spec.LastActivity.ID, OwedSinceAt: spec.OwedSinceAt, LastActivityAt: spec.LastActivity.OccurredAt, OpensAt: spec.OpensAt, EvidenceJSON: evidence, RulesetVersion: RulesetVersion, TurnMessageIDs: spec.TurnMessageIDs, PreviousTurnKnown: spec.PreviousTurnKnown, PreviousTurnAt: spec.PreviousTurnAt, PreviousTurnMessageID: spec.PreviousTurnMessageID}
	if spec.Counterparty.ID != 0 {
		id := spec.Counterparty.ID
		open.CounterpartyParticipantID = &id
	}
	if spec.Counterparty.PersonID != 0 {
		id := spec.Counterparty.PersonID
		open.CounterpartyPersonID = &id
	}
	out.Open = open
	return out, nil
}

func ownerParticipationFromEvidence(raw jsontext.Value) (int64, error) {
	var evidence []storedEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return 0, err
	}
	if len(evidence) == 0 {
		return 0, nil
	}
	return evidence[0].OwnerParticipationMessageID, nil
}

// storedEvidence preserves the persisted evidence JSON shape without changing
// the Reader's independently versioned Evidence contract.
type storedEvidence struct {
	MessageID                   int64     `json:"message_id"`
	ConversationID              int64     `json:"conversation_id"`
	OccurredAt                  time.Time `json:"occurred_at"`
	Subject                     string    `json:"subject"`
	TruncatedRun                bool      `json:"truncated_run,omitzero"`
	OwnerParticipationMessageID int64     `json:"owner_participation_message_id,omitzero"`
}
