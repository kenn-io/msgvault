package line

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
)

const retryHorizon = 24 * time.Hour

var errTargetMissing = errors.New("LINE target message has not arrived")

type invalidEventError struct{ error }

func (e *invalidEventError) Unwrap() error { return e.error }
func invalidEvent(message string) error    { return &invalidEventError{errors.New(message)} }

// retryEventError distinguishes recoverable provider work from archive-write
// failures, which must keep the inbox pending instead of expiring its text.
type retryEventError struct{ error }

func (e *retryEventError) Unwrap() error { return e.error }
func (*retryEventError) lineRetryable()  {}
func retryEvent(err error) error {
	if err == nil {
		return nil
	}
	return &retryEventError{err}
}

func retryableEvent(err error) bool {
	if errors.Is(err, errTargetMissing) {
		return true
	}
	// Only this node's marker applies. Searching through a joined error would
	// let one provider failure hide an independent archive-write failure.
	if _, ok := err.(interface{ lineRetryable() }); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if child != nil && !retryableEvent(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryableEvent(wrapped.Unwrap())
	}
	return false
}

type ProcessorOption func(*Processor)

func WithProcessorClock(now func() time.Time) ProcessorOption {
	return func(p *Processor) {
		if now != nil {
			p.now = now
		}
	}
}
func WithProcessorMediaLimit(maxBytes int64) ProcessorOption {
	return func(p *Processor) {
		if maxBytes > 0 {
			p.maxBytes = maxBytes
		}
	}
}

// Processor applies one bounded pass. Text commits before any provider lookup;
// each event remains durable until all its enrichment has finished or expired.
type Processor struct {
	store                     *store.Store
	client                    *Client
	sourceID                  int64
	channelID, attachmentsDir string
	now                       func() time.Time
	maxBytes                  int64
	groupNames                map[string]string
	botID                     string
	messagesAdded             int64
	messagesUpdated           int64
}

func NewProcessor(st *store.Store, client *Client, sourceID int64, channelID, attachmentsDir string, options ...ProcessorOption) *Processor {
	p := &Processor{store: st, client: client, sourceID: sourceID, channelID: channelID, attachmentsDir: attachmentsDir, now: time.Now, maxBytes: attachmentpolicy.DefaultChatMaxBytes}
	for _, option := range options {
		option(p)
	}
	return p
}

func (p *Processor) Process(ctx context.Context) (resultErr error) {
	if p == nil || p.store == nil || p.client == nil {
		return errors.New("LINE processor unavailable")
	}
	if _, err := p.store.ValidateLineSourceBindingContext(ctx, p.sourceID, p.channelID); err != nil {
		return err
	}
	// Retention runs on idle passes too, so a quiet channel still clears old payloads.
	if err := p.store.PruneLineEventsContext(ctx, p.sourceID, p.now().Add(-30*24*time.Hour)); err != nil {
		return err
	}
	due, err := p.store.DueLineEventsContext(ctx, p.sourceID, p.now(), 1)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}
	execution, err := p.store.AcquireSyncExecutionContext(ctx, p.sourceID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, execution.Release()) }()
	due, err = p.store.DueLineEventsContext(ctx, p.sourceID, p.now(), 1)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}
	runID, err := execution.StartSyncContext(ctx, SourceType, "")
	if err != nil {
		return err
	}
	pass := *p
	pass.store = p.store.ScopedToSync(p.sourceID, runID)
	// Group names can change between passes; fetch each group's summary once per pass.
	pass.groupNames = map[string]string{}
	checkpoint := &store.Checkpoint{}
	touched := make(map[string]struct{})
	var touchedConversations []string
	type finishedEvent struct {
		id               int64
		state, lastError string
	}
	var finished []finishedEvent
	// Terminal events finish only after their conversations settle. A crash
	// leaves them for replay; an interrupted pass settles what it completed so
	// repeated preemption still makes progress.
	settle := func(ctx context.Context) error {
		for _, conversationKey := range touchedConversations {
			if err := pass.store.ResolveLineQuotesForConversationContext(ctx, p.sourceID, conversationKey); err != nil {
				return err
			}
			if err := pass.store.RecomputeConversationStatsForSourceConversationContext(ctx, p.sourceID, conversationKey); err != nil {
				return err
			}
		}
		for len(finished) > 0 {
			done := finished[0]
			if err := pass.store.FinishLineEventContext(ctx, done.id, done.state, done.lastError, time.Time{}); err != nil {
				return err
			}
			finished = finished[1:]
		}
		return nil
	}
	completed := false
	defer func() {
		if !completed {
			settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			resultErr = errors.Join(resultErr, settle(settleCtx))
			cancelSettle()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			message := "LINE processing interrupted"
			if resultErr != nil {
				message = resultErr.Error()
			}
			resultErr = errors.Join(resultErr, p.store.FailSyncContext(cleanup, runID, message))
		}
	}()
	pass.botID, err = p.store.ValidateLineSourceBindingContext(ctx, p.sourceID, p.channelID)
	if err != nil {
		return err
	}
	rows, err := pass.store.DueLineEventsContext(ctx, p.sourceID, p.now(), 500)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err = ctx.Err(); err != nil {
			return err
		}
		var event Event
		eventErr := json.Unmarshal([]byte(row.RawJSON), &event)
		conversationKey := ""
		if eventErr == nil {
			switch event.Type {
			case "message", "messageEdited", "unsend", "memberJoined", "memberLeft", "join", "leave", "follow", "unfollow":
				if key, _, keyErr := event.Source.ConversationID(); keyErr == nil {
					conversationKey = key
				}
			}
		}
		if conversationKey != "" {
			if _, seen := touched[conversationKey]; !seen {
				touched[conversationKey] = struct{}{}
				touchedConversations = append(touchedConversations, conversationKey)
			}
		}
		state := "processed"
		if row.State == "retry" && p.now().Sub(row.ReceivedAt) >= retryHorizon {
			currentState, stateErr := pass.store.LineEventStateContext(ctx, row.ID)
			if stateErr != nil {
				return stateErr
			}
			if currentState != "retry" {
				continue
			}
			if err = pass.expireMedia(ctx, row.TargetMessageID); err != nil {
				return err
			}
			message := row.LastError
			if message == "" {
				message = "LINE retry window expired"
			}
			eventErr = invalidEvent(message)
		} else if eventErr != nil {
			eventErr = invalidEvent("invalid archived LINE event")
		} else {
			state, eventErr = pass.apply(ctx, event)
		}
		if eventErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, ok := errors.AsType[*invalidEventError](eventErr); ok {
				state = "failed"
			} else if retryableEvent(eventErr) {
				state = "retry"
			} else {
				if err = pass.store.FinishLineEventContext(ctx, row.ID, "pending", eventErr.Error(), time.Time{}); err != nil {
					return err
				}
				return fmt.Errorf("process LINE inbox event: %w", eventErr)
			}
			if state == "retry" && p.now().Sub(row.ReceivedAt) >= retryHorizon {
				state = "failed"
				if err = pass.expireMedia(ctx, row.TargetMessageID); err != nil {
					return err
				}
			}
			next := time.Time{}
			if state == "retry" {
				next = pass.retryAt(row, eventErr)
			}
			// A failed event may already have committed its message, so it also
			// waits for finalization. reconcileMutations finishes edit and unsend
			// rows itself during the pass, so those keep finishing here.
			if state == "failed" && event.Type != "messageEdited" && event.Type != "unsend" {
				finished = append(finished, finishedEvent{row.ID, state, eventErr.Error()})
			} else if err = pass.store.FinishLineEventContext(ctx, row.ID, state, eventErr.Error(), next); err != nil {
				return err
			}
			checkpoint.ErrorsCount++
			if err = pass.store.RecordSyncRunItemContext(ctx, store.SyncRunItem{SyncRunID: runID, SourceMessageID: row.TargetMessageID, Phase: "webhook", Status: store.SyncRunItemStatusError, ErrorKind: "line_event", ErrorMessage: eventErr.Error()}); err != nil {
				return err
			}
		} else {
			finished = append(finished, finishedEvent{row.ID, state, ""})
			checkpoint.MessagesProcessed++
		}
		jobctx.RecordProgress(ctx)
	}
	if err = settle(ctx); err != nil {
		return err
	}
	// Edits rewrite already exported messages; the analytics cache rebuilds on this counter.
	checkpoint.MessagesAdded = pass.messagesAdded
	checkpoint.MessagesUpdated = pass.messagesUpdated
	if err = pass.store.UpdateSyncCheckpointContext(ctx, runID, checkpoint); err != nil {
		return err
	}
	if err = p.store.CompleteSyncAndPreserveSourceCursorContext(ctx, runID, p.sourceID, ""); err != nil {
		return err
	}
	completed = true
	return nil
}

func (p *Processor) retryAt(row store.LineWebhookEvent, eventErr error) time.Time {
	delay := 30 * time.Second
	for i := 0; i < row.Attempts && delay < time.Hour; i++ {
		delay = min(2*delay, time.Hour)
	}
	next := p.now().Add(delay)
	if deadline := providerRetryDeadline(eventErr); deadline.After(next) {
		next = deadline
	}
	return minTime(next, row.ReceivedAt.Add(retryHorizon))
}
func providerRetryDeadline(err error) time.Time {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var latest time.Time
		for _, child := range joined.Unwrap() {
			if at := providerRetryDeadline(child); at.After(latest) {
				latest = at
			}
		}
		return latest
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return providerRetryDeadline(wrapped.Unwrap())
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr.RetryAfter
	}
	return time.Time{}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (p *Processor) apply(ctx context.Context, e Event) (string, error) {
	switch e.Type {
	case "message":
		return "processed", p.message(ctx, e)
	case "messageEdited":
		return "processed", p.edit(ctx, e)
	case "unsend":
		return "processed", p.unsend(ctx, e)
	case "memberJoined", "memberLeft", "join", "leave", "follow", "unfollow":
		return "processed", p.membership(ctx, e)
	default:
		return "ignored", nil
	}
}

// reconcileMutations also considers failed orphans whose automatic retries
// expired. The original is not complete until every retained mutation applies.
func (p *Processor) reconcileMutations(ctx context.Context, messageID string) error {
	rows, err := p.store.LineMutationsContext(ctx, p.sourceID, messageID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var e Event
		if err = json.Unmarshal([]byte(row.RawJSON), &e); err != nil {
			err = invalidEvent("invalid LINE lifecycle evidence")
		} else {
			_, err = p.apply(ctx, e)
		}
		if err != nil {
			if _, ok := errors.AsType[*invalidEventError](err); !ok {
				return err
			}
			if row.State != "failed" || row.LastError != err.Error() {
				if finishErr := p.store.FinishLineEventContext(ctx, row.ID, "failed", err.Error(), time.Time{}); finishErr != nil {
					return finishErr
				}
			}
			continue
		}
		if row.State != "processed" {
			if err = p.store.FinishLineEventContext(ctx, row.ID, "processed", "", time.Time{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Processor) unsend(ctx context.Context, e Event) error {
	if e.Unsend == nil || e.Unsend.MessageID == "" || strings.ContainsAny(e.Unsend.MessageID, "\r\n") {
		return invalidEvent("invalid LINE unsend target")
	}
	row, err := p.store.LineMessageContext(ctx, p.sourceID, e.Unsend.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return errTargetMissing
	}
	if err != nil {
		return err
	}
	key, _, err := e.Source.ConversationID()
	if err != nil {
		return invalidEvent("invalid LINE unsend source")
	}
	if !row.SourceConversationID.Valid || row.SourceConversationID.String != key {
		return invalidEvent("LINE unsend target belongs to a different chat")
	}
	if err = p.store.MarkMessagesDeletedFromReaderContext(ctx, p.sourceID, strings.NewReader(e.Unsend.MessageID+"\n"), 1); err != nil {
		return err
	}
	jobctx.RecordProgress(ctx)
	return p.finishUnstoredMedia(ctx, row.ID, attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
}

func (p *Processor) membership(ctx context.Context, e Event) error {
	key, kind, err := e.Source.ConversationID()
	if err != nil {
		return invalidEvent("invalid LINE membership source")
	}
	conversationID, err := p.store.EnsureConversationWithTypeContext(ctx, p.sourceID, key, kind, "")
	if err != nil {
		return err
	}
	joined := e.Type == "memberJoined" || e.Type == "join" || e.Type == "follow"
	var users []string
	switch e.Type {
	case "join", "leave":
		if p.botID == "" {
			return invalidEvent("LINE bot identity missing")
		}
		users = []string{p.botID}
	case "follow", "unfollow":
		users = []string{e.Source.UserID}
	case "memberJoined":
		if e.Joined == nil {
			return invalidEvent("LINE joined members missing")
		}
		for _, member := range e.Joined.Members {
			if member.UserID != "" {
				users = append(users, member.UserID)
			}
		}
	case "memberLeft":
		if e.Left == nil {
			return invalidEvent("LINE left members missing")
		}
		for _, member := range e.Left.Members {
			if member.UserID != "" {
				users = append(users, member.UserID)
			}
		}
	}
	for _, userID := range users {
		if userID == "" {
			return invalidEvent("LINE member identity missing")
		}
		pid, err := p.participant(ctx, key, userID, "")
		if err != nil {
			return err
		}
		if err = p.store.SetLineMembershipContext(ctx, conversationID, pid, UserIdentifier(p.channelID, userID), joined, time.UnixMilli(e.Timestamp).UTC()); err != nil {
			return err
		}
	}
	// Persist every member transition before the first optional provider request.
	// A failed lookup may delay presentation, never another member's membership.
	if joined {
		for _, userID := range users {
			if userID != p.botID {
				if _, err = p.profile(ctx, e.Source, userID); err != nil {
					return fmt.Errorf("LINE member enrichment: %w", err)
				}
			}
		}
	}
	return nil
}
