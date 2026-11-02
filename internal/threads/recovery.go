package threads

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Reconcile inspects or explicitly adopts an existing dispatched attempt. It
// never creates a container or issues a publish request. Candidates are hints.
func (s *Sender) Reconcile(ctx context.Context, request SendRequest) (SendResult, error) {
	out := SendResult{DraftID: request.DraftID, Status: "reply_not_permitted", RemoteStatus: "unknown", LocalStatus: "pending"}
	if s.options.Authorize == nil {
		return out, ErrReplyNotPermitted
	}
	if err := s.options.Authorize(ctx); err != nil {
		return out, err
	}
	if request.Revision <= 0 {
		return out, store.ErrChatDraftRevisionConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	r, err := s.store.GetThreadsReplySendContext(ctx, request.DraftID)
	if err != nil {
		return out, err
	}
	out = replyResult(r)
	if r.DraftRevision != request.Revision {
		return out, store.ErrChatDraftRevisionConflict
	}
	if err = s.checkSource(ctx, r); err != nil {
		return out, err
	}
	if _, err = s.loadPolicy(ctx, r); err != nil {
		return out, err
	}
	return s.reconcileReceipt(ctx, request, r)
}
func (s *Sender) reconcileReceipt(ctx context.Context, request SendRequest, r store.ThreadsReplySend) (SendResult, error) {
	out := replyResult(r)
	if r.MediaID != "" {
		if request.AdoptMediaID != "" && request.AdoptMediaID != r.MediaID {
			return out, ErrReplyNotPermitted
		}
		return s.finishKnown(ctx, r, nil, nil)
	}
	if r.PublishDispatchedAt == nil {
		return out, ErrReplyNotPermitted
	}
	if request.AdoptMediaID != "" && !validID(request.AdoptMediaID) {
		return out, ErrReplyNotPermitted
	}
	if s.options.OpenAPI == nil {
		return out, ErrReplyNotPermitted
	}
	api, scopes, err := s.options.OpenAPI(ctx, r.SourceID)
	if err != nil {
		return out, err
	}
	if api == nil || !hasReadReplyScopes(scopes) {
		return out, ErrReplyNotPermitted
	}
	if request.AdoptMediaID == "" {
		return s.candidateHints(ctx, r, api)
	}
	media, err := api.Media(ctx, request.AdoptMediaID)
	if err != nil && (!isPause(err) || media.ID == "") {
		return out, err
	}
	if !provesAdoption(r, request.AdoptMediaID, media) {
		return out, ErrReplyNotPermitted
	}
	saved, err := s.persistKnownMedia(ctx, r, media.ID)
	if err != nil {
		return s.knownPersistenceFailure(ctx, r, media.ID, err)
	}
	return s.finishKnown(ctx, saved, nil, &media)
}
func hasReadReplyScopes(scopes []string) bool {
	basic, replies := false, false
	for _, scope := range scopes {
		basic = basic || scope == "threads_basic"
		replies = replies || scope == "threads_read_replies"
	}
	return basic && replies
}
func provesAdoption(r store.ThreadsReplySend, id string, m Media) bool {
	if m.ID != id || m.IsReply == nil || !*m.IsReply || m.IsReplyOwnedByMe == nil || !*m.IsReplyOwnedByMe || m.Text == nil || *m.Text != r.Body {
		return false
	}
	if m.RootPost == nil || m.RootPost.ID != r.RootMediaID || m.RepliedTo == nil || m.RepliedTo.ID != r.ParentMediaID {
		return false
	}
	_, owner, err := ParseSourceIdentifier(r.SourceIdentifier)
	return err == nil && (m.Owner == nil || m.Owner.ID == owner)
}

func (s *Sender) persistKnownMedia(ctx context.Context, r store.ThreadsReplySend, id string) (store.ThreadsReplySend, error) {
	saved, err := s.store.AdvanceThreadsReplySendContext(ctx, r.DraftID, r.RowRevision, store.ThreadsReplyTransition{State: "sent_archive_pending", MediaID: id})
	if err == nil {
		return saved, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, readErr := s.store.GetThreadsReplySendContext(cleanup, r.DraftID)
	if readErr == nil && sameReplySnapshot(r, current) && current.MediaID == id && current.PublishDispatchedAt != nil {
		return current, nil
	}
	if ctx.Err() != nil && readErr == nil && sameReplySnapshot(r, current) && current.MediaID == "" && current.PublishDispatchedAt != nil {
		retried, retryErr := s.store.AdvanceThreadsReplySendContext(cleanup, r.DraftID, current.RowRevision, store.ThreadsReplyTransition{State: "sent_archive_pending", MediaID: id})
		if retryErr == nil {
			return retried, nil
		}
		return r, errors.Join(err, retryErr)
	}
	return r, errors.Join(err, readErr)
}
func sameReplySnapshot(a, b store.ThreadsReplySend) bool {
	return a.DraftID == b.DraftID && a.DraftRevision == b.DraftRevision && a.SourceID == b.SourceID && a.SourceIdentifier == b.SourceIdentifier && a.ConversationID == b.ConversationID && a.RootMediaID == b.RootMediaID && a.ParentMediaID == b.ParentMediaID && a.Body == b.Body && a.BodyHash == b.BodyHash
}
func (s *Sender) knownPersistenceFailure(ctx context.Context, r store.ThreadsReplySend, id string, cause error) (SendResult, error) {
	unknown, unknownErr := s.markUnknown(ctx, r)
	result := replyResult(unknown)
	result.MediaID = id
	result.RemoteStatus = "sent"
	result.LocalStatus = "receipt_pending"
	result.ErrorCategory = "receipt_persistence_failed"
	return result, errors.Join(ErrReplyReceiptPersistence, cause, unknownErr)
}

// finishKnown performs only local replay unless a client from this invocation
// is already available. Later retries never open credentials to repair archives.
func (s *Sender) finishKnown(ctx context.Context, r store.ThreadsReplySend, api PublishAPI, supplied *Media) (SendResult, error) {
	result := replyResult(r)
	if r.ArchiveComplete {
		return result, nil
	}
	if !validID(r.MediaID) || r.PublishDispatchedAt == nil {
		return result, ErrReplyNotPermitted
	}
	at := r.PublishDispatchedAt.UTC()
	native := Media{ID: r.MediaID, Timestamp: at, Text: &r.Body, RootPost: &MediaReference{ID: r.RootMediaID}, RepliedTo: &MediaReference{ID: r.ParentMediaID}, IsReply: new(true), IsReplyOwnedByMe: new(true)}
	origin := ItemOrigin{Kind: "sent_provisional", RootID: r.RootMediaID}
	var credentialErr error
	candidate := supplied
	existing, found, err := s.store.ThreadsArchiveMessageMetadataContext(ctx, r.SourceID, r.MediaID)
	if err != nil {
		return result, err
	}
	canonicalExists := false
	if found {
		e, err := decodeEvidence(existing.Metadata.String)
		if err != nil {
			return result, err
		}
		canonicalExists = !e.flag("sent_provisional")
	}
	if candidate == nil && api != nil && !canonicalExists && ctx.Err() == nil {
		fetched, fetchErr := api.Media(ctx, r.MediaID)
		if fetchErr == nil || isPause(fetchErr) && fetched.ID != "" {
			candidate = &fetched
		} else if invalid, ok := errors.AsType[*APIError](fetchErr); ok && invalid.CredentialInvalid() {
			credentialErr = fetchErr
		}
	}
	if candidate != nil {
		_, owner, _ := ParseSourceIdentifier(r.SourceIdentifier)
		m := *candidate
		if m.ID != r.MediaID || (m.RootPost != nil && m.RootPost.ID != r.RootMediaID) || (m.RepliedTo != nil && m.RepliedTo.ID != r.ParentMediaID) || (m.IsReply != nil && !*m.IsReply) || (m.IsReplyOwnedByMe != nil && !*m.IsReplyOwnedByMe) || (m.Owner != nil && m.Owner.ID != owner) {
			return result, ErrReplyNotPermitted
		}
		// A sparse response is not a complete canonical snapshot. Preserve the
		// immutable sent text provisionally until ordinary sync exposes the rest.
		if m.RootPost != nil && m.RepliedTo != nil && m.IsReply != nil && *m.IsReply && m.Text != nil && !m.Timestamp.IsZero() {
			native = m
			origin.Kind = "own_replies"
		}
	}
	writer := NewWriter(s.store, r.SourceID, r.SourceIdentifier)
	id, err := writer.Apply(ctx, native, origin)
	if err != nil {
		return result, errors.Join(credentialErr, err)
	}
	result.MessageID = id
	if err = writer.ResolveParents(ctx); err != nil {
		return result, errors.Join(credentialErr, err)
	}
	if s.beforeArchiveFinish != nil {
		if err = s.beforeArchiveFinish(); err != nil {
			return result, errors.Join(credentialErr, err)
		}
	}
	if err = s.store.FinishThreadsReplyArchiveContext(ctx, r.DraftID, r.DraftRevision, id); err != nil {
		return result, errors.Join(credentialErr, err)
	}
	r.State = "complete"
	r.ArchiveComplete = true
	r.LocalMessageID = id
	result = replyResult(r)
	if credentialErr != nil {
		result.ErrorCategory = "reauthorization_required"
	}
	return result, credentialErr
}

func (s *Sender) candidateHints(ctx context.Context, r store.ThreadsReplySend, api PublishAPI) (SendResult, error) {
	result := replyResult(r)
	since := r.PublishDispatchedAt.Add(-time.Hour)
	if since.Before(earliestHistoryTime()) {
		since = earliestHistoryTime()
	}
	until := s.options.Now().UTC()
	if since.Unix() >= until.Unix() {
		return result, nil
	}
	cursor := ""
	guard := WalkGuard{}
	examined := 0
	for examined < 100 {
		page, err := api.OwnReplies(ctx, ListQuery{Since: since.Truncate(time.Second), Until: until, After: cursor})
		if err != nil && (!isPause(err) || page.Items == nil) {
			return result, err
		}
		if guardErr := guard.observe(cursor, page); guardErr != nil {
			return result, guardErr
		}
		if len(page.Items) == 0 {
			return result, err
		}
		for _, m := range page.Items {
			if examined == 100 {
				break
			}
			examined++
			if m.Timestamp.Before(since) || m.Timestamp.After(until) || (m.IsReplyOwnedByMe != nil && !*m.IsReplyOwnedByMe) || (m.IsReply != nil && !*m.IsReply) || m.RootPost == nil || m.RootPost.ID != r.RootMediaID || m.RepliedTo == nil || m.RepliedTo.ID != r.ParentMediaID || m.Text == nil || *m.Text != r.Body {
				continue
			}
			result.Candidates = append(result.Candidates, m.ID)
		}
		if err != nil {
			return result, err
		}
		cursor = page.After
	}
	return result, nil
}
