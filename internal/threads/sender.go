package threads

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrReplyNotPermitted       = errors.New("Threads reply is not permitted")
	ErrReplyQuotaExhausted     = errors.New("Threads reply quota is exhausted")
	ErrReplyReceiptPersistence = errors.New("published Threads media ID could not be saved")
)

type SenderOptions struct {
	Authorize  func(context.Context) error
	LoadPolicy func(context.Context, int64) (SendPolicy, error)
	OpenAPI    func(context.Context, int64) (PublishAPI, []string, error)
	Now        func() time.Time
	Wait       func(context.Context, time.Duration) error
}
type SendPolicy struct {
	Enabled          bool
	SourceIdentifier string
}
type SendRequest struct {
	DraftID      string
	Revision     int64
	AdoptMediaID string
}
type SendResult struct {
	DraftID         string   `json:"draft_id"`
	Status          string   `json:"status"`
	RemoteStatus    string   `json:"remote_status"`
	LocalStatus     string   `json:"local_status"`
	ContainerID     string   `json:"container_id,omitempty"`
	MediaID         string   `json:"media_id,omitempty"`
	ArchiveComplete bool     `json:"archive_complete"`
	BodyBytes       int      `json:"body_bytes"`
	MessageID       int64    `json:"message_id,omitempty"`
	Candidates      []string `json:"candidates,omitempty"`
	ErrorCategory   string   `json:"error_category,omitempty"`
}
type Sender struct {
	store               *store.Store
	options             SenderOptions
	afterDispatch       func() error
	afterPublish        func(string)
	beforeArchiveFinish func() error
}

func NewSender(st *store.Store, opts SenderOptions) *Sender {
	opts.Now, opts.Wait = clockDefaults(opts.Now, opts.Wait)
	return &Sender{store: st, options: opts}
}

// Send runs under the caller's source lifecycle/execution ownership. It never
// holds a database transaction while contacting the provider. The Store barrier
// independently prevents concurrent callers from publishing the same draft.
func (s *Sender) Send(ctx context.Context, request SendRequest) (SendResult, error) {
	result := SendResult{DraftID: request.DraftID, Status: "reply_not_permitted", RemoteStatus: "unpublished", LocalStatus: "draft_kept"}
	if s.options.Authorize == nil {
		return result, ErrReplyNotPermitted
	}
	if err := s.options.Authorize(ctx); err != nil {
		return result, err
	}
	if request.Revision <= 0 {
		return result, store.ErrChatDraftRevisionConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	started := s.options.Now()
	receipt, err := s.store.GetThreadsReplySendContext(ctx, request.DraftID)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrThreadsReplyNotFound) {
		return result, err
	}
	if exists {
		result = replyResult(receipt)
		if receipt.DraftRevision != request.Revision {
			return result, store.ErrChatDraftRevisionConflict
		}
	} else {
		draft, err := s.store.GetChatDraftContext(ctx, request.DraftID)
		if err != nil {
			return result, err
		}
		if draft.Revision != request.Revision {
			return result, store.ErrChatDraftRevisionConflict
		}
		if draft.SourceType != "threads" {
			return result, ErrReplyNotPermitted
		}
		receipt = store.ThreadsReplySend{DraftID: draft.DraftID, SourceID: draft.SourceID, SourceIdentifier: draft.SourceIdentifier, ConversationID: draft.ConversationID, DraftRevision: draft.Revision, RootMediaID: draft.SourceConversationID, ParentMediaID: draft.ReplyToSourceMessageID, Body: draft.Body}
		result.BodyBytes = len(draft.Body)
	}
	if err = s.checkSource(ctx, receipt); err != nil {
		return result, err
	}
	// The current source binding is required even for local-only replay; enabling
	// sends and valid credentials are not needed to retain a known outcome.
	policy, err := s.loadPolicy(ctx, receipt)
	if err != nil {
		return result, err
	}
	if request.AdoptMediaID != "" {
		if !exists {
			return result, store.ErrThreadsReplyNotFound
		}
		return s.reconcileReceipt(ctx, request, receipt)
	}
	if exists && receipt.MediaID != "" {
		return s.finishKnown(ctx, receipt, nil, nil)
	}
	if exists && (receipt.PublishDispatchedAt != nil || unpublishedTerminal(receipt)) {
		return replyResult(receipt), nil
	}
	if !policy.Enabled {
		return result, ErrReplyNotPermitted
	}
	if err = ValidateReplyBody(receipt.Body); err != nil {
		result.Status = "invalid_body"
		return result, err
	}
	if _, err = s.store.GetThreadsOwnRootContext(ctx, receipt.SourceID, receipt.RootMediaID); err != nil {
		return result, ErrReplyNotPermitted
	}
	if err = s.store.ValidateThreadsReplyTargetContext(ctx, receipt.SourceID, receipt.ConversationID, receipt.RootMediaID, receipt.RootMediaID); err != nil {
		return result, ErrReplyNotPermitted
	}
	if err = s.store.ValidateThreadsReplyTargetContext(ctx, receipt.SourceID, receipt.ConversationID, receipt.RootMediaID, receipt.ParentMediaID); err != nil {
		return result, ErrReplyNotPermitted
	}
	if s.options.OpenAPI == nil {
		return result, ErrReplyNotPermitted
	}
	api, scopes, err := s.options.OpenAPI(ctx, receipt.SourceID)
	if err != nil {
		return result, err
	}
	if api == nil || !hasReplyScopes(scopes) {
		result.Status = "reply_not_permitted"
		return result, ErrReplyNotPermitted
	}
	quotaChecked := false
	if receipt.ContainerID == "" {
		if err = checkReplyQuota(ctx, api); err != nil {
			result.Status = quotaFailureStatus(err)
			return result, err
		}
		quotaChecked = true
	}
	if !exists {
		receipt, err = s.store.ClaimThreadsReplySendContext(ctx, request.DraftID, request.Revision)
		if err != nil {
			return result, err
		}
		if receipt.PublishDispatchedAt != nil || receipt.MediaID != "" || unpublishedTerminal(receipt) {
			return replyResult(receipt), nil
		}
	}
	result = replyResult(receipt)
	if receipt.ContainerID == "" {
		if err = s.requireEnabled(ctx, receipt); err != nil {
			return result, err
		}
		container, createErr := api.CreateReply(ctx, receipt.ParentMediaID, receipt.Body)
		if container == "" {
			if createErr == nil {
				createErr = ErrDecodeResponse
			}
			return result, createErr
		}
		created := s.options.Now().UTC()
		firstPoll := ceilReplyClock(created.Add(30 * time.Second))
		receipt, err = s.store.AdvanceThreadsReplySendContext(ctx, receipt.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "container_saved", ContainerID: container, ContainerCreatedAt: &created, NextPollAt: &firstPoll})
		if err != nil {
			return result, err
		}
		result = replyResult(receipt)
		if createErr != nil {
			return result, createErr
		}
	}
	receipt, err = s.waitForContainer(ctx, receipt, api, started.Add(5*time.Minute))
	if err != nil {
		return replyResult(receipt), err
	}
	if receipt.State != "ready" {
		return replyResult(receipt), nil
	}
	if err = s.requireEnabled(ctx, receipt); err != nil {
		return replyResult(receipt), err
	}
	if !quotaChecked {
		if err = checkReplyQuota(ctx, api); err != nil {
			result = replyResult(receipt)
			result.Status = quotaFailureStatus(err)
			return result, err
		}
	}
	return s.dispatch(ctx, receipt, api)
}
func (s *Sender) checkSource(ctx context.Context, r store.ThreadsReplySend) error {
	source, err := s.store.GetSourceByIDContext(ctx, r.SourceID)
	if err != nil {
		return err
	}
	if source.SourceType != "threads" || source.Identifier != r.SourceIdentifier || source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return store.ErrSourceRetired
	}
	if _, _, err = ParseSourceIdentifier(r.SourceIdentifier); err != nil {
		return ErrReplyNotPermitted
	}
	return nil
}
func (s *Sender) loadPolicy(ctx context.Context, r store.ThreadsReplySend) (SendPolicy, error) {
	if s.options.LoadPolicy == nil {
		return SendPolicy{}, ErrReplyNotPermitted
	}
	policy, err := s.options.LoadPolicy(ctx, r.SourceID)
	if err != nil {
		return policy, err
	}
	if policy.SourceIdentifier != r.SourceIdentifier {
		return policy, ErrReplyNotPermitted
	}
	return policy, nil
}
func (s *Sender) requireEnabled(ctx context.Context, r store.ThreadsReplySend) error {
	if err := s.checkSource(ctx, r); err != nil {
		return err
	}
	policy, err := s.loadPolicy(ctx, r)
	if err != nil {
		return err
	}
	if !policy.Enabled {
		return ErrReplyNotPermitted
	}
	return nil
}
func hasReplyScopes(scopes []string) bool {
	for _, required := range []string{"threads_basic", "threads_content_publish", "threads_manage_replies"} {
		found := false
		for _, scope := range scopes {
			if scope == required {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func checkReplyQuota(ctx context.Context, api PublishAPI) error {
	quota, err := api.ReplyQuota(ctx)
	if err != nil {
		return err
	}
	if quota.Total <= 0 || quota.Duration <= 0 || quota.Usage < 0 {
		return ErrDecodeResponse
	}
	if quota.Usage >= quota.Total {
		return ErrReplyQuotaExhausted
	}
	return nil
}
func unpublishedTerminal(r store.ThreadsReplySend) bool {
	if r.PublishDispatchedAt != nil || r.MediaID != "" {
		return false
	}
	switch r.State {
	case "rate_limited", "reply_not_permitted", "invalid_body", "send_failed":
		return true
	}
	return false
}
func replyResult(r store.ThreadsReplySend) SendResult {
	out := SendResult{DraftID: r.DraftID, Status: r.State, ContainerID: r.ContainerID, MediaID: r.MediaID, ArchiveComplete: r.ArchiveComplete, BodyBytes: len(r.Body), MessageID: r.LocalMessageID, RemoteStatus: "unpublished", LocalStatus: "pending"}
	switch {
	case r.ArchiveComplete:
		out.Status = "complete"
		out.RemoteStatus = "sent"
		out.LocalStatus = "complete"
	case r.MediaID != "":
		out.Status = "sent_archive_pending"
		out.RemoteStatus = "sent"
	case r.PublishDispatchedAt != nil:
		out.Status = "remote_unknown"
		out.RemoteStatus = "unknown"
	case unpublishedTerminal(r):
		out.LocalStatus = "draft_kept"
	}
	return out
}
func ceilReplyClock(at time.Time) time.Time {
	rounded := at.UTC().Truncate(time.Microsecond)
	if rounded.Before(at) {
		rounded = rounded.Add(time.Microsecond)
	}
	return rounded
}

func (s *Sender) waitForContainer(ctx context.Context, r store.ThreadsReplySend, api PublishAPI, deadline time.Time) (store.ThreadsReplySend, error) {
	if r.State == "ready" {
		return r, nil
	}
	if r.State != "container_saved" || r.ContainerID == "" || r.ContainerCreatedAt == nil {
		return r, store.ErrThreadsReplyConflict
	}
	for {
		next := r.ContainerCreatedAt.Add(30 * time.Second)
		if r.NextPollAt != nil && r.NextPollAt.After(next) {
			next = *r.NextPollAt
		}
		remaining := deadline.Sub(s.options.Now())
		if remaining <= 0 {
			return r, nil
		}
		delay := next.Sub(s.options.Now())
		if delay > 0 {
			if delay >= remaining {
				if err := s.options.Wait(ctx, remaining); err != nil {
					return r, err
				}
				return r, nil
			}
			if err := s.options.Wait(ctx, delay); err != nil {
				return r, err
			}
		}
		if !s.options.Now().Before(deadline) {
			return r, nil
		}
		at := ceilReplyClock(s.options.Now())
		nextPoll := ceilReplyClock(at.Add(time.Minute))
		reserved, err := s.store.AdvanceThreadsReplySendContext(ctx, r.DraftID, r.RowRevision, store.ThreadsReplyTransition{LastStatusAt: &at, NextPollAt: &nextPoll})
		if err != nil {
			return r, err
		}
		r = reserved
		status, err := api.ContainerStatus(ctx, r.ContainerID)
		if err != nil {
			return r, err
		}
		switch status {
		case "IN_PROGRESS":
			continue
		case "ERROR", "EXPIRED":
			terminal, err := s.store.AdvanceThreadsReplySendContext(ctx, r.DraftID, r.RowRevision, store.ThreadsReplyTransition{State: "send_failed"})
			if err != nil {
				return r, err
			}
			return terminal, errors.New("Threads container ended without publication")
		case "FINISHED", "PUBLISHED":
			ready, err := s.store.AdvanceThreadsReplySendContext(ctx, r.DraftID, r.RowRevision, store.ThreadsReplyTransition{State: "ready"})
			if err != nil {
				return r, err
			}
			if status == "FINISHED" {
				return ready, nil
			}
			// A published container without an exact media ID is uncertain. Commit the
			// permanent barrier but never dispatch another publish request for it.
			locked, _, err := s.dispatchBarrier(ctx, ready)
			if err != nil {
				return locked, err
			}
			unknown, saveErr := s.markUnknown(ctx, locked)
			return unknown, errors.Join(errors.New("Threads container is published without a media ID"), saveErr)
		default:
			return r, ErrDecodeResponse
		}
	}
}
func (s *Sender) dispatchBarrier(ctx context.Context, r store.ThreadsReplySend) (store.ThreadsReplySend, bool, error) {
	won, err := s.store.TryDispatchThreadsReplyContext(ctx, r.DraftID, r.RowRevision)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	latest, readErr := s.store.GetThreadsReplySendContext(cleanup, r.DraftID)
	if readErr != nil {
		// The caller cannot determine the durable state. It does not publish, even
		// if the barrier transaction may have committed or reported success.
		at := s.options.Now()
		r.PublishDispatchedAt = &at
		r.State = "publish_dispatched"
		if won {
			r.RowRevision++
		}
		return r, false, errors.Join(err, readErr)
	}
	if err != nil {
		return latest, false, err
	}
	if !won {
		return latest, false, store.ErrThreadsReplyConflict
	}
	return latest, true, nil
}
func (s *Sender) markUnknown(ctx context.Context, r store.ThreadsReplySend) (store.ThreadsReplySend, error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	next, err := s.store.AdvanceThreadsReplySendContext(cleanup, r.DraftID, r.RowRevision, store.ThreadsReplyTransition{State: "remote_unknown"})
	if err != nil {
		return r, err
	}
	return next, nil
}
func (s *Sender) dispatch(ctx context.Context, r store.ThreadsReplySend, api PublishAPI) (SendResult, error) {
	if budgeted, ok := api.(interface{ checkPublishBudget(context.Context) error }); ok {
		if err := budgeted.checkPublishBudget(ctx); err != nil {
			return replyResult(r), err
		}
	}
	locked, won, err := s.dispatchBarrier(ctx, r)
	if err != nil || !won {
		return replyResult(locked), err
	}
	r = locked
	if s.afterDispatch != nil {
		if err = s.afterDispatch(); err != nil {
			unknown, saveErr := s.markUnknown(ctx, r)
			return replyResult(unknown), errors.Join(err, saveErr)
		}
	}
	_, owner, err := ParseSourceIdentifier(r.SourceIdentifier)
	if err != nil {
		return replyResult(r), err
	}
	media, publishErr := api.PublishReply(ctx, owner, r.ContainerID)
	if !validID(media) {
		if publishErr == nil {
			publishErr = ErrDecodeResponse
		}
		unknown, saveErr := s.markUnknown(ctx, r)
		return replyResult(unknown), errors.Join(publishErr, saveErr)
	}

	if s.afterPublish != nil {
		s.afterPublish(media)
	}
	saved, saveErr := s.persistKnownMedia(ctx, r, media)
	if saveErr != nil {
		return s.knownPersistenceFailure(ctx, r, media, saveErr)
	}
	return s.finishKnown(ctx, saved, api, nil)

}

func quotaFailureStatus(err error) string {
	if errors.Is(err, ErrReplyQuotaExhausted) {
		return "rate_limited"
	}
	if errors.Is(err, ErrReauthorizationRequired) {
		return "reply_not_permitted"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "paused"
	}
	if pause, ok := errors.AsType[*PauseError](err); ok {
		if pause.Reason == "high_usage" || pause.Reason == "retry_after" {
			return "rate_limited"
		}
		return "paused"
	}
	if api, ok := errors.AsType[*APIError](err); ok {
		if api.CredentialInvalid() || api.StatusCode == 401 || api.StatusCode == 403 {
			return "reply_not_permitted"
		}
		if api.StatusCode == 429 || api.Code == 4 || api.Code == 17 || api.Code == 32 || api.Code == 613 {
			return "rate_limited"
		}
	}
	return "send_failed"
}
