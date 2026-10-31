package x

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// SendRequest carries authenticated adapter authority, never an owner flag
// decoded from a public request. The draft determines the source and target.
type SendRequest struct {
	DraftID   string
	Revision  int64
	AdoptPost string
	Owner     bool
}

// SendResult is content-free durable recovery information.
type SendResult struct {
	Association    string `json:"association,omitempty"`
	Status         string `json:"status"`
	Code           string `json:"code,omitempty"`
	RemoteID       string `json:"remote_id,omitempty"`
	AttemptID      string `json:"attempt_id,omitempty"`
	DraftID        string `json:"draft_id"`
	Revision       int64  `json:"revision"`
	ReservedMicros int64  `json:"reserved_micros"`
}

// ReplyPreparation is owner-only review data. Receipt/status/repair modes never
// need the deleted draft, credentials or current write opt-in.
type ReplyPreparation struct {
	AdoptText      string      `json:"adopt_text,omitempty"`
	AdoptAuthorID  string      `json:"adopt_author_id,omitempty"`
	Mode           string      `json:"mode"`
	DraftID        string      `json:"draft_id"`
	Revision       int64       `json:"revision"`
	TargetPostID   string      `json:"target_post_id,omitempty"`
	Text           string      `json:"text,omitempty"`
	ReservedMicros int64       `json:"reserved_micros"`
	AdoptPost      string      `json:"adopt_post,omitempty"`
	Receipt        *SendResult `json:"receipt,omitempty"`
}

// ReplyError exposes only a bounded local code. Its cause is for internal
// classification, not provider-controlled response text or credential paths.
type ReplyError struct {
	Code  string
	cause error
}

func (e *ReplyError) Error() string             { return e.Code }
func (e *ReplyError) Unwrap() error             { return e.cause }
func replyError(code string, cause error) error { return &ReplyError{Code: code, cause: cause} }

func replyStoreError(err error) error {
	if err == nil {
		return nil
	}
	code := "local_store_failed"
	switch {
	case errors.Is(err, store.ErrSyncAlreadyActive):
		code = "operation_busy"
	case errors.Is(err, store.ErrChatDraftRevisionConflict):
		code = "revision_mismatch"
	case errors.Is(err, store.ErrXTargetChanged):
		code = "target_changed"
	case errors.Is(err, store.ErrChatDraftInvalidDestination):
		code = "invalid_destination"
	case errors.Is(err, store.ErrXReplyFrozen):
		code = "reply_frozen"
	case errors.Is(err, store.ErrChatDraftNotFound):
		code = "draft_not_found"
	case errors.Is(err, store.ErrSourceRetired):
		code = "source_retired"
	case errors.Is(err, store.ErrXBudgetExceeded):
		code = "budget_exhausted"
	case errors.Is(err, store.ErrXBudgetCapacity):
		code = "budget_capacity"
	case errors.Is(err, store.ErrXBudgetIdentity):
		code = "credential_error"
	case errors.Is(err, store.ErrXAttemptState):
		code = "invalid_attempt"
	}
	return replyError(code, err)
}

func ValidateReplyText(text string) error {
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) || strings.TrimSpace(text) == "" || len(text) > 16*1024 {
		return replyError("invalid_text", nil)
	}
	return nil
}

type Sender struct {
	store       *store.Store
	api         API
	policy      config.XAccountConfig
	credentials Credentials
	now         func() time.Time
}

// NewSender consumes a single-request raw API, never the admitted read wrapper.
// Nil network dependencies are sufficient for content-free receipts/local repair.
func NewSender(st *store.Store, api API, policy config.XAccountConfig, credentials Credentials) *Sender {
	return &Sender{store: st, api: api, policy: policy, credentials: credentials, now: time.Now}
}
func (s *Sender) authorize(req SendRequest) error {
	if !req.Owner {
		return replyError("not_permitted", nil)
	}
	if s == nil || s.store == nil {
		return replyError("unavailable", nil)
	}
	if !strings.HasPrefix(req.DraftID, store.ChatDraftIDPrefix) || len(req.DraftID) > 128 || req.Revision <= 0 || (req.AdoptPost != "" && ValidateID(req.AdoptPost) != nil) {
		return replyError("invalid_args", nil)
	}
	return nil
}
func checkReplyReceipt(a store.XReplyAttempt, req SendRequest) error {
	if a.State != "rejected" && a.Revision != req.Revision {
		return store.ErrChatDraftRevisionConflict
	}
	if a.SourceID <= 0 || a.AttemptID == "" || a.DraftID != req.DraftID || ValidateID(a.OwnerID) != nil || ValidateID(a.TargetPostID) != nil || ValidateID(a.SourceConversationID) != nil || len(a.BodyDigest) != 64 || a.ReservedMicros < 200000 {
		return store.ErrXAttemptState
	}
	if _, err := hex.DecodeString(a.BodyDigest); err != nil {
		return store.ErrXAttemptState
	}
	switch a.State {
	case "sent", "remote_accepted":
		if ValidateID(a.RemoteID) != nil || (req.AdoptPost != "" && req.AdoptPost != a.RemoteID) {
			return store.ErrXAttemptState
		}
	case "admitted", "remote_unknown", "rejected":
		if a.RemoteID != "" {
			return store.ErrXAttemptState
		}
	default:
		return store.ErrXAttemptState
	}
	return nil
}
func replyResult(a store.XReplyAttempt) SendResult {
	r := SendResult{Status: a.State, Code: a.Code, DraftID: a.DraftID, Revision: a.Revision, AttemptID: a.AttemptID, RemoteID: a.RemoteID, ReservedMicros: a.ReservedMicros}
	if a.OwnerConfirmed {
		r.Association = "owner_confirmed"
	}
	switch a.State {
	case "admitted", "remote_unknown":
		r.Status = "remote_unknown"
		r.Code = "remote_unknown"
	case "remote_accepted":
		r.Status = "sent_archive_pending"
	}
	return r
}
func (s *Sender) receipt(ctx context.Context, req SendRequest) (store.XReplyAttempt, bool, error) {
	a, err := s.store.GetXReplyReceiptContext(ctx, req.DraftID)
	if errors.Is(err, store.ErrXReplyReceiptNotFound) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	if err := checkReplyReceipt(a, req); err != nil {
		return a, false, err
	}
	return a, true, nil
}
func (s *Sender) review(ctx context.Context, req SendRequest) (store.ChatDraft, error) {
	d, err := s.store.GetChatDraftContext(ctx, req.DraftID)
	if err != nil {
		return d, err
	}
	if d.Revision != req.Revision {
		return d, store.ErrChatDraftRevisionConflict
	}
	if d.SourceType != "x" {
		return d, store.ErrChatDraftUnsupportedSource
	}
	if err := s.policy.Validate(d.SourceIdentifier); err != nil {
		return d, store.ErrXBudgetIdentity
	}
	if !s.policy.AllowSend {
		return d, replyError("send_disabled", nil)
	}
	source, err := s.store.GetSourceByIDContext(ctx, d.SourceID)
	if err != nil {
		return d, err
	}
	if source.SourceType != "x" || source.Identifier != d.SourceIdentifier {
		return d, store.ErrXBudgetIdentity
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return d, store.ErrSourceRetired
	}
	if err := ValidateReplyText(d.Body); err != nil {
		return d, err
	}
	target, err := s.store.ResolveXPostContext(ctx, d.SourceID, d.ReplyToSourceMessageID)
	if err != nil || target.MessageID == 0 || target.Availability != "public" {
		return d, store.ErrChatDraftInvalidDestination
	}
	if target.CurrentID != d.ReplyToSourceMessageID || target.ConversationID != d.ConversationID {
		return d, store.ErrXTargetChanged
	}
	state, err := s.store.LoadXControlContext(ctx, d.SourceID)
	if err != nil {
		return d, err
	}
	if state.OwnerID != d.SourceIdentifier || state.CredentialError != "" {
		return d, store.ErrXBudgetIdentity
	}
	return d, nil
}
func (s *Sender) PrepareReply(ctx context.Context, req SendRequest) (ReplyPreparation, error) {
	if err := s.authorize(req); err != nil {
		return ReplyPreparation{}, err
	}
	a, found, err := s.receipt(ctx, req)
	if err != nil {
		return ReplyPreparation{}, replyStoreError(err)
	}
	if found && a.State != "rejected" {
		if req.AdoptPost != "" && (a.State == "admitted" || a.State == "remote_unknown") {
			return s.prepareAdoption(ctx, req)
		}
		mode := "status"
		if a.State == "sent" {
			mode = "receipt"
		} else if a.State == "remote_accepted" {
			mode = "repair"
		}
		r := replyResult(a)
		return ReplyPreparation{Mode: mode, DraftID: a.DraftID, Revision: a.Revision, ReservedMicros: a.ReservedMicros, Receipt: &r}, nil
	}
	if req.AdoptPost != "" {
		return ReplyPreparation{}, replyStoreError(store.ErrXAttemptState)
	}
	d, err := s.review(ctx, req)
	if err != nil {
		return ReplyPreparation{}, replyReviewError(err)
	}
	return ReplyPreparation{Mode: "new_send", DraftID: d.DraftID, Revision: d.Revision, TargetPostID: d.ReplyToSourceMessageID, Text: d.Body, ReservedMicros: s.policy.CreateReservationMicros}, nil
}
func replyReviewError(err error) error {
	var coded *ReplyError
	if errors.As(err, &coded) {
		return err
	}
	return replyStoreError(err)
}

// Real credential lifecycle ownership nests inside source execution. Prepare
// takes only its existing inner file lock, so refreshing here cannot deadlock
// by trying to acquire the lifecycle lock a second time.
func (s *Sender) withCredentialLifecycle(ctx context.Context, fn func() error) error {
	if credentials, ok := s.credentials.(*accountCredentials); ok {
		return credentials.manager.WithLifecycleLock(ctx, credentials.owner, fn)
	}
	return fn()
}
func (s *Sender) Send(ctx context.Context, req SendRequest) (result SendResult, retErr error) {
	if err := s.authorize(req); err != nil {
		return result, err
	}
	a, found, err := s.receipt(ctx, req)
	if err != nil {
		return result, replyStoreError(err)
	}
	if found && a.State == "sent" {
		return replyResult(a), nil
	}
	var sourceID int64
	if found {
		sourceID = a.SourceID
	} else {
		d, err := s.store.GetChatDraftContext(ctx, req.DraftID)
		if err != nil {
			return result, replyStoreError(err)
		}
		sourceID = d.SourceID
	}
	execution, err := s.store.AcquireSyncExecutionContext(ctx, sourceID)
	if err != nil {
		return result, replyStoreError(err)
	}
	defer func() {
		if err := execution.Release(); err != nil {
			retErr = errors.Join(retErr, replyStoreError(err))
		}
	}()
	// Another invocation may have completed while this caller acquired ownership.
	a, found, err = s.receipt(ctx, req)
	if err != nil {
		return result, replyStoreError(err)
	}
	if found && a.State != "rejected" {
		if req.AdoptPost != "" && (a.State == "admitted" || a.State == "remote_unknown") {
			return s.adopt(ctx, req, a)
		}
		return s.existing(ctx, a)
	}
	if req.AdoptPost != "" {
		return result, replyStoreError(store.ErrXAttemptState)
	}
	if s.api == nil || s.credentials == nil {
		return result, replyError("unavailable", nil)
	}
	retErr = s.withCredentialLifecycle(ctx, func() error {
		d, err := s.review(ctx, req)
		if err != nil {
			return replyReviewError(err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := s.credentials.Prepare(ctx)
		if err != nil {
			var invalid *InvalidGrantError
			if errors.As(err, &invalid) {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = s.store.RecordXCredentialErrorContext(cleanup, d.SourceID, invalid.OwnerID, invalid.Generation, "invalid_grant")
			}
			return replyError("credential_error", err)
		}
		if snapshot.OwnerID != d.SourceIdentifier {
			return replyStoreError(store.ErrXBudgetIdentity)
		}
		cost := store.XRequestCost{Kind: store.XRequestCreate, OwnerID: snapshot.OwnerID, Generation: snapshot.Generation, GrantedScopes: snapshot.GrantedScopes, MonthlyBudgetMicros: s.policy.MonthlyBudgetMicros, CreateReservationMicros: s.policy.CreateReservationMicros, AllowSend: s.policy.AllowSend, At: s.now().UTC()}
		admitted, err := s.store.AdmitXReplyContext(ctx, store.XReplyAdmission{SourceID: d.SourceID, OwnerID: d.SourceIdentifier, DraftID: d.DraftID, Revision: d.Revision, TargetPostID: d.ReplyToSourceMessageID, Cost: cost})
		if err != nil {
			return replyStoreError(err)
		}
		a = admitted.Attempt
		result = replyResult(a)
		if !admitted.Created {
			var err error
			result, err = s.existing(ctx, a)
			return err
		}
		sum := sha256.Sum256([]byte(d.Body))
		if hex.EncodeToString(sum[:]) != a.BodyDigest {
			return s.unknown(ctx, a, "reply_frozen", store.ErrXReplyFrozen)
		}
		// No read retry wrapper, redirect replay or refresh-and-resend is allowed.
		remoteID, postErr := s.api.CreateReply(ctx, d.Body, a.TargetPostID)
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if postErr == nil && ValidateID(remoteID) == nil {
			result.Status = "sent_archive_pending"
			result.Code = ""
			result.RemoteID = remoteID
			if err := s.store.RecordXReplyAcceptedContext(cleanup, a.XAttemptKey, remoteID); err != nil {
				result.Code = "acceptance_write_failed"
				return replyError(result.Code, err)
			}
			a.RemoteID = remoteID
			a.State = "remote_accepted"
			result, err = s.finish(cleanup, a)
			return err
		}
		var rejected *APIError
		if errors.As(postErr, &rejected) && rejected.Definite && rejected.StatusCode >= 400 && rejected.StatusCode < 500 {
			code := "provider_rejected"
			switch rejected.StatusCode {
			case 401:
				code = "unauthorized"
			case 402:
				code = "credits_exhausted"
			case 422:
				code = "text_rejected"
			case 429:
				code = "rate_limited"
			}
			if err := s.store.RejectXReplyContext(cleanup, a.XAttemptKey, code); err != nil {
				return s.unknown(cleanup, a, "rejection_write_failed", err)
			}
			result.Status = "rejected"
			result.Code = code
			if rejected.StatusCode == 401 {
				if err := s.store.RecordXCredentialErrorContext(cleanup, a.SourceID, a.OwnerID, a.Generation, code); err != nil {
					return replyError("credential_write_failed", err)
				}
			}
			return replyError(code, postErr)
		}
		return s.unknown(cleanup, a, "remote_unknown", postErr)
	})
	return result, retErr
}
func (s *Sender) unknown(ctx context.Context, a store.XReplyAttempt, code string, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := s.store.MarkXReplyUnknownContext(cleanup, a.XAttemptKey)
	return replyError(code, errors.Join(cause, err))
}
func (s *Sender) existing(ctx context.Context, a store.XReplyAttempt) (SendResult, error) {
	if a.State == "remote_accepted" {
		return s.finish(ctx, a)
	}
	if a.State == "admitted" {
		if err := s.store.MarkXReplyUnknownContext(ctx, a.XAttemptKey); err != nil {
			return replyResult(a), replyStoreError(err)
		}
		a.State = "remote_unknown"
	}
	return replyResult(a), nil
}
func (s *Sender) finish(ctx context.Context, a store.XReplyAttempt) (SendResult, error) {
	done, err := s.store.FinishXReplyContext(ctx, a.XAttemptKey)
	if err != nil {
		r := replyResult(a)
		r.Code = "local_archive_failed"
		return r, replyError(r.Code, err)
	}
	return replyResult(done), nil
}

// frozenAdoptionDraft checks only existing attempt authority, not new-send
// eligibility. A changed/removed parent and write opt-out cannot authorize a
// new POST, nor prevent owner reconciliation of an already uncertain outcome.
func (s *Sender) frozenAdoptionDraft(ctx context.Context, req SendRequest, a store.XReplyAttempt) (store.ChatDraft, error) {
	if a.State != "admitted" && a.State != "remote_unknown" {
		return store.ChatDraft{}, store.ErrXAttemptState
	}
	d, err := s.store.GetChatDraftContext(ctx, a.DraftID)
	if err != nil {
		return d, err
	}
	sum := sha256.Sum256([]byte(d.Body))
	if d.SourceType != "x" || d.SourceID != a.SourceID || d.SourceIdentifier != a.OwnerID || d.Revision != a.Revision || d.Revision != req.Revision || d.SourceConversationID != a.SourceConversationID || d.ReplyToSourceMessageID != a.TargetPostID || hex.EncodeToString(sum[:]) != a.BodyDigest {
		return d, store.ErrXReplyFrozen
	}
	return d, nil
}
func (s *Sender) lookupAdoption(ctx context.Context, req SendRequest, a store.XReplyAttempt) (Post, error) {
	if s.api == nil || s.credentials == nil {
		return Post{}, replyError("unavailable", nil)
	}
	if err := s.policy.Validate(a.OwnerID); err != nil {
		return Post{}, replyError("credential_error", err)
	}
	read := NewAdmittedAPI(s.store, a.SourceID, s.api, s.policy, s.credentials, ReadScope{})
	page, err := read.Lookup(ctx, []string{req.AdoptPost})
	if err != nil {
		return Post{}, replyReviewError(err)
	}
	// Profile evidence has the same retention effect as collection/refresh,
	// independently of whether this post matches the requested association.
	for _, user := range page.Users {
		if user.Protected {
			if err := s.store.QueueXAuthorRemovalContext(ctx, a.SourceID, user.ID); err != nil {
				return Post{}, replyStoreError(err)
			}
		}
	}
	if len(page.Posts) != 1 || len(page.Errors) != 0 {
		return Post{}, replyError("adoption_mismatch", nil)
	}
	p := page.Posts[0]
	parents := 0
	for _, ref := range p.References {
		if ref.Type == "retweeted" {
			return Post{}, replyError("adoption_mismatch", nil)
		}
		if ref.Type == "replied_to" {
			parents++
			if ref.ID != a.TargetPostID {
				return Post{}, replyError("adoption_mismatch", nil)
			}
		}
	}
	if p.ID != req.AdoptPost || p.AuthorID != a.OwnerID || p.ConversationID != a.SourceConversationID || parents != 1 || p.Withheld {
		return Post{}, replyError("adoption_mismatch", nil)
	}
	// The fetched provider snapshot, including suppression effects, is durable
	// before its read reservation is settled. No caller-supplied price is used.
	if err := WritePage(ctx, s.store, a.SourceID, a.OwnerID, page, page.FetchedAt); err != nil {
		return Post{}, replyReviewError(err)
	}
	if err := s.store.SettleXRequestContext(ctx, a.SourceID, page.ReservationID, store.XResourceUsage{Posts: page.PostCount, Users: page.UserCount, Persisted: true, Complete: true}); err != nil {
		return Post{}, replyStoreError(err)
	}
	return p, nil
}
func (s *Sender) prepareAdoption(ctx context.Context, req SendRequest) (preparation ReplyPreparation, retErr error) {
	a, found, err := s.receipt(ctx, req)
	if err != nil {
		return preparation, replyStoreError(err)
	}
	if !found {
		return preparation, replyStoreError(store.ErrXAttemptState)
	}
	execution, err := s.store.AcquireSyncExecutionContext(ctx, a.SourceID)
	if err != nil {
		return preparation, replyStoreError(err)
	}
	defer func() {
		if err := execution.Release(); err != nil {
			retErr = errors.Join(retErr, replyStoreError(err))
		}
	}()
	a, found, err = s.receipt(ctx, req)
	if err != nil {
		return preparation, replyStoreError(err)
	}
	if !found {
		return preparation, replyStoreError(store.ErrXAttemptState)
	}
	if a.State == "sent" || a.State == "remote_accepted" {
		mode := "receipt"
		if a.State == "remote_accepted" {
			mode = "repair"
		}
		r := replyResult(a)
		return ReplyPreparation{Mode: mode, DraftID: a.DraftID, Revision: a.Revision, ReservedMicros: a.ReservedMicros, Receipt: &r}, nil
	}
	retErr = s.withCredentialLifecycle(ctx, func() error {
		d, err := s.frozenAdoptionDraft(ctx, req, a)
		if err != nil {
			return replyStoreError(err)
		}
		p, err := s.lookupAdoption(ctx, req, a)
		if err != nil {
			return err
		}
		text := p.Text
		if p.LongText != "" {
			text = p.LongText
		}
		preparation = ReplyPreparation{Mode: "adopt", DraftID: a.DraftID, Revision: a.Revision, TargetPostID: a.TargetPostID, Text: d.Body, ReservedMicros: a.ReservedMicros, AdoptPost: req.AdoptPost, AdoptText: text, AdoptAuthorID: p.AuthorID}
		return nil
	})
	return preparation, retErr
}

// adopt runs only under this Send invocation's already-held source execution.
func (s *Sender) adopt(ctx context.Context, req SendRequest, a store.XReplyAttempt) (result SendResult, retErr error) {
	result = replyResult(a)
	retErr = s.withCredentialLifecycle(ctx, func() error {
		if _, err := s.frozenAdoptionDraft(ctx, req, a); err != nil {
			return replyStoreError(err)
		}
		if _, err := s.lookupAdoption(ctx, req, a); err != nil {
			return err
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.store.RecordXReplyAdoptedContext(cleanup, a.XAttemptKey, req.AdoptPost); err != nil {
			return replyStoreError(err)
		}
		a.RemoteID = req.AdoptPost
		a.State = "remote_accepted"
		a.Code = ""
		a.OwnerConfirmed = true
		var err error
		result, err = s.finish(cleanup, a)
		return err
	})
	return result, retErr
}
