package line

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/httpretry"
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
func WithProcessorMediaPolicy(policy attachmentpolicy.Policy) ProcessorOption {
	return func(p *Processor) {
		p.mediaPolicy = policy
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
	mediaPolicy               attachmentpolicy.Policy
	groupNames                map[string]string
	reconciledEvents          map[int64]struct{}
	botID                     string
	messagesAdded             int64
	messagesUpdated           int64
}

func NewProcessor(st *store.Store, client *Client, sourceID int64, channelID, attachmentsDir string, options ...ProcessorOption) *Processor {
	p := &Processor{store: st, client: client, sourceID: sourceID, channelID: channelID, attachmentsDir: attachmentsDir, now: time.Now, mediaPolicy: attachmentpolicy.Policy{MaxBytes: attachmentpolicy.DefaultChatMaxBytes}}
	for _, option := range options {
		option(p)
	}
	return p
}

func (p *Processor) recordMutation(ctx context.Context, added, updated int64) {
	p.messagesAdded += added
	p.messagesUpdated += updated
	jobctx.RecordProgress(ctx)
}

// Drain processes all work due at entry, across bounded passes. Later receipts
// and newly scheduled retries belong to a later drain.
func (p *Processor) Drain(ctx context.Context) error {
	cutoff := p.now()
	throughID, err := p.store.LineInboxHighWaterContext(ctx, p.sourceID)
	if err != nil {
		return err
	}
	if throughID == 0 {
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		due, err := p.store.DueLineEventsThroughContext(ctx, p.sourceID, cutoff, throughID, 1)
		if err != nil || len(due) == 0 {
			return err
		}
		if err := p.process(ctx, cutoff, throughID); err != nil {
			return err
		}
	}
}

func (p *Processor) Process(ctx context.Context) error {
	if p == nil || p.store == nil || p.client == nil {
		return errors.New("LINE processor unavailable")
	}
	return p.process(ctx, p.now(), 0)
}

func (p *Processor) process(ctx context.Context, dueAt time.Time, throughID int64) (resultErr error) {
	due, err := p.store.DueLineEventsThroughContext(ctx, p.sourceID, dueAt, throughID, 1)
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
	due, err = p.store.DueLineEventsThroughContext(ctx, p.sourceID, dueAt, throughID, 1)
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
	pass.reconciledEvents = map[int64]struct{}{}
	checkpoint := &store.Checkpoint{}
	saveCheckpoint := func(ctx context.Context) error {
		// Committed writes must invalidate the cache even if later work fails.
		checkpoint.MessagesAdded = pass.messagesAdded
		checkpoint.MessagesUpdated = pass.messagesUpdated
		return pass.store.UpdateSyncCheckpointContext(ctx, runID, checkpoint)
	}
	touched := make(map[string]struct{})
	var touchedConversations []string
	messageIDs := make(map[string][]string)
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
			if err := pass.store.ResolveLineQuotesForMessagesContext(ctx, p.sourceID, conversationKey, messageIDs[conversationKey]); err != nil {
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
	var localFailure error
	defer func() {
		resultErr = errors.Join(resultErr, localFailure)
		if !completed {
			settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			resultErr = errors.Join(resultErr, settle(settleCtx))
			cancelSettle()
			checkpoint.MessagesAdded = pass.messagesAdded
			checkpoint.MessagesUpdated = pass.messagesUpdated
			syncErr := resultErr
			if syncErr == nil {
				syncErr = errors.New("LINE processing interrupted")
			}
			resultErr = errors.Join(resultErr, p.store.InterruptSyncWithCheckpoint(ctx, runID, syncErr, checkpoint))
		}
	}()
	pass.botID, err = p.store.ValidateLineSourceBindingContext(ctx, p.sourceID, p.channelID)
	if err != nil {
		return err
	}
	rows, err := pass.store.DueLineEventsThroughContext(ctx, p.sourceID, dueAt, throughID, 500)
	if err != nil {
		return err
	}
	finishEvent := func(row store.LineWebhookEvent, event Event, state string, eventErr error) error {
		if eventErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, ok := errors.AsType[*invalidEventError](eventErr); ok {
				state = "failed"
			} else if retryableEvent(eventErr) {
				state = "retry"
			} else {
				if err = pass.store.FinishLineEventContext(ctx, row.ID, "pending", eventErr.Error(), pass.retryAt(row, eventErr)); err != nil {
					return err
				}
				if localFailure == nil {
					localFailure = fmt.Errorf("process LINE inbox event: %w", eventErr)
				}
				return nil
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
		return nil
	}
	type eventEnrichment struct {
		event Event
		run   func(context.Context) error
	}
	enrichments := make([]eventEnrichment, len(rows))
	for i, row := range rows {
		if err = ctx.Err(); err != nil {
			return err
		}
		// A late original can settle a mutation earlier in this same batch.
		if _, reconciled := pass.reconciledEvents[row.ID]; reconciled {
			continue
		}
		var event Event
		eventErr := json.Unmarshal([]byte(row.RawJSON), &event)
		conversationKey := ""
		if eventErr == nil {
			switch event.Type {
			case "message", "messageEdited", "unsend", "memberJoined", "memberLeft", "join", "leave":
				if key, _, keyErr := event.Source.ConversationID(); keyErr == nil {
					conversationKey = key
				}
			}
		}
		if conversationKey != "" {
			if event.Message != nil {
				messageIDs[conversationKey] = append(messageIDs[conversationKey], event.Message.ID)
			}
			if _, seen := touched[conversationKey]; !seen {
				touched[conversationKey] = struct{}{}
				touchedConversations = append(touchedConversations, conversationKey)
			}
		}
		state := "processed"
		var enrich func(context.Context) error
		if row.State == "retry" && p.now().Sub(row.ReceivedAt) >= retryHorizon {
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
			state, enrich, eventErr = pass.apply(ctx, event)
		}
		if eventErr == nil && enrich != nil {
			enrichments[i] = eventEnrichment{event, enrich}
			continue
		}
		if err = finishEvent(row, event, state, eventErr); err != nil {
			return err
		}
	}
	if err = settle(ctx); err != nil {
		return err
	}
	enriched := false
	for i, row := range rows {
		work := enrichments[i]
		if work.run == nil {
			continue
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if enriched && jobctx.PreemptionRequested(ctx) {
			return jobctx.ErrYieldedToWaiter
		}
		eventErr := work.run(ctx)
		enriched = true
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = finishEvent(row, work.event, "processed", eventErr); err != nil {
			return err
		}
	}
	if err = settle(ctx); err != nil {
		return err
	}
	if err = saveCheckpoint(ctx); err != nil {
		return err
	}
	if localFailure == nil {
		if err = p.store.CompleteSyncAndPreserveSourceCursorContext(ctx, runID, p.sourceID, ""); err != nil {
			return err
		}
		completed = true
	}
	return nil
}

func (p *Processor) retryAt(row store.LineWebhookEvent, eventErr error) time.Time {
	now := p.now()
	next := now.Add(httpretry.RetryAfterAtWithBase("", row.Attempts, 30*time.Second, time.Hour, now))
	if deadline := providerRetryDeadline(eventErr); deadline.After(next) {
		next = deadline
	}
	if retryableEvent(eventErr) {
		return minTime(next, row.ReceivedAt.Add(retryHorizon))
	}
	return next
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

func (p *Processor) apply(ctx context.Context, e Event) (string, func(context.Context) error, error) {
	switch e.Type {
	case "message":
		key, kind, err := e.Source.ConversationID()
		if err != nil {
			return "processed", nil, invalidEvent("invalid LINE message source")
		}
		previous, err := p.store.LineMessageContext(ctx, p.sourceID, e.Message.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "processed", nil, err
		}
		messageID, err := p.message(ctx, e, previous, key, kind)
		if err != nil || messageID == 0 {
			return "processed", nil, err
		}
		return "processed", func(ctx context.Context) error {
			return p.enrichMessage(ctx, e, messageID, previous, key, kind)
		}, nil
	case "messageEdited":
		return "processed", nil, p.edit(ctx, e)
	case "unsend":
		return "processed", nil, p.unsend(ctx, e)
	case "memberJoined", "memberLeft", "join", "leave":
		enrich, err := p.membership(ctx, e)
		return "processed", enrich, err
	default:
		return "ignored", nil, nil
	}
}

// reconcileMutations also considers failed orphans whose automatic retries
// expired. The original is not complete until every retained mutation applies.
func (p *Processor) reconcileMutations(ctx context.Context, messageID string) error {
	if p.reconciledEvents == nil {
		p.reconciledEvents = map[int64]struct{}{}
	}
	rows, err := p.store.LineMutationsContext(ctx, p.sourceID, messageID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var e Event
		if err = json.Unmarshal([]byte(row.RawJSON), &e); err != nil {
			err = invalidEvent("invalid LINE lifecycle evidence")
		} else {
			_, _, err = p.apply(ctx, e)
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
			p.reconciledEvents[row.ID] = struct{}{}
			continue
		}
		if row.State != "processed" {
			if err = p.store.FinishLineEventContext(ctx, row.ID, "processed", "", time.Time{}); err != nil {
				return err
			}
		}
		p.reconciledEvents[row.ID] = struct{}{}
	}
	return nil
}

func (p *Processor) unsend(ctx context.Context, e Event) error {
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
	if err = p.store.MarkMessageDeletedContext(ctx, p.sourceID, e.Unsend.MessageID); err != nil {
		return err
	}
	p.recordMutation(ctx, 0, 1)
	return p.finishUnstoredMedia(ctx, row.ID, attachmentpolicy.StateUnavailable, attachmentpolicy.SkipSourceUnavailable)
}

func (p *Processor) membership(ctx context.Context, e Event) (func(context.Context) error, error) {
	key, kind, err := e.Source.ConversationID()
	if err != nil {
		return nil, invalidEvent("invalid LINE membership source")
	}
	conversationID, err := p.store.EnsureConversationWithTypeContext(ctx, p.sourceID, key, kind, "")
	if err != nil {
		return nil, err
	}
	joined := e.Type == "memberJoined" || e.Type == "join"
	var users []string
	switch e.Type {
	case "join", "leave":
		if p.botID == "" {
			return nil, invalidEvent("LINE bot identity missing")
		}
		users = []string{p.botID}
	case "memberJoined":
		if e.Joined == nil {
			return nil, invalidEvent("LINE joined members missing")
		}
		for _, member := range e.Joined.Members {
			if member.UserID != "" {
				users = append(users, member.UserID)
			}
		}
	case "memberLeft":
		if e.Left == nil {
			return nil, invalidEvent("LINE left members missing")
		}
		for _, member := range e.Left.Members {
			if member.UserID != "" {
				users = append(users, member.UserID)
			}
		}
	}
	for _, userID := range users {
		pid, err := p.participant(ctx, key, userID, "")
		if err != nil {
			return nil, err
		}
		if err = p.store.SetLineMembershipContext(ctx, conversationID, pid, UserIdentifier(p.channelID, userID), joined, time.UnixMilli(e.Timestamp).UTC()); err != nil {
			return nil, err
		}
		p.recordMutation(ctx, 0, 1)
	}
	if !joined {
		return nil, nil //nolint:nilnil // Departures need no provider enrichment.
	}
	return func(ctx context.Context) error { return p.enrichMembership(ctx, e, users) }, nil
}

// Every member transition commits before the first optional provider request.
func (p *Processor) enrichMembership(ctx context.Context, e Event, users []string) error {
	var enrichmentErr error
	for _, userID := range users {
		if userID != p.botID {
			_, err := p.profile(ctx, e.Source, userID)
			enrichmentErr = errors.Join(enrichmentErr, err)
		}
	}
	return enrichmentErr
}
