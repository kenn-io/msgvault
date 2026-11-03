package meta

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

type ActionKind string

const (
	ActionPublicReply  ActionKind = "public_reply"
	ActionPrivateReply ActionKind = "private_reply"
	ActionHide         ActionKind = "hide"
	ActionUnhide       ActionKind = "unhide"
	ActionDelete       ActionKind = "delete"
)

type ActionRequest struct {
	MessageID      int64      `json:"-"`
	Action         ActionKind `json:"action"`
	Body           string     `json:"body,omitempty"`
	DryRun         bool       `json:"dry_run,omitempty"`
	Confirm        string     `json:"confirm,omitempty"`
	ResolveUnknown string     `json:"resolve_unknown,omitempty"`
}
type Disclosure struct {
	Provider   string `json:"provider"`
	AssetID    string `json:"asset_id"`
	AssetName  string `json:"page_or_account_name"`
	CommentID  string `json:"comment_id"`
	AuthorName string `json:"author_display_name"`
	Body       string `json:"body"`
	Public     bool   `json:"public"`
}
type Receipt struct {
	Action          ActionKind `json:"action"`
	ErrorCode       string     `json:"error_code,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	ID              int64      `json:"id"`
	State           string     `json:"state"`
	RemoteID        string     `json:"remote_id,omitempty"`
	RecipientID     string     `json:"recipient_id,omitempty"`
	ArchiveComplete bool       `json:"archive_complete"`
	ReplyMessageID  int64      `json:"reply_message_id,omitempty"`
}
type ActionResult struct {
	MessageID  int64      `json:"message_id"`
	SourceID   int64      `json:"source_id"`
	Action     ActionKind `json:"action"`
	Visibility string     `json:"visibility"`
	DryRun     bool       `json:"dry_run"`
	Disclosure Disclosure `json:"disclosure"`
	Confirm    string     `json:"confirm"`
	Warnings   []string   `json:"warnings,omitempty"`
	Receipt    *Receipt   `json:"receipt,omitempty"`
}
type ActionError struct {
	Code                string        `json:"code"`
	Message             string        `json:"message"`
	RequiredPermissions []string      `json:"required_permissions,omitempty"`
	AbuseWarning        bool          `json:"abuse_warning,omitempty"`
	Result              *ActionResult `json:"result,omitempty"`
	Cause               error         `json:"-"`
}

func (e *ActionError) Error() string { return e.Message }
func (e *ActionError) Unwrap() error { return e.Cause }
func actionFailure(code string, result *ActionResult, cause error) *ActionError {
	return &ActionError{Code: code, Message: "Comment action: " + code, Result: result, Cause: cause}
}

func verificationFailure(result *ActionResult, err error) *ActionError {
	code := Classify(err, false)
	if code == ClassUnknown {
		// Verification happens before the action is dispatched, so no send can
		// have an uncertain outcome.
		code = ClassRejected
	}
	failure := actionFailure(string(code), result, err)
	failure.AbuseWarning = AbuseWarning(err)
	return failure
}

type Target struct {
	MessageID, SourceID                                                                                     int64
	SourceIdentifier, MessageType, App, Asset, CommentID, RootID, ParentID, AuthorID, AuthorName, AssetName string
	IsFromMe, Deleted                                                                                       bool
	SentAt                                                                                                  time.Time
}
type ActionAPI interface {
	VerifyPage(ctx context.Context, pageID string) (Page, error)
	SetCommentHidden(ctx context.Context, id string, hidden bool) error
	DeleteComment(ctx context.Context, id string) error
	ReplyToComment(ctx context.Context, id, body string) (Object, error)
	PrivateReply(ctx context.Context, asset, id, body string) (PrivateReplyResult, error)
}
type ActionsOptions struct {
	Allowed func(int64, ActionKind) (bool, error)
	OpenAPI func(context.Context, *store.Source) (ActionAPI, error)
	Now     func() time.Time
}
type Actions struct {
	st   *store.Store
	opts ActionsOptions
}

func NewActions(st *store.Store, opts ActionsOptions) *Actions {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Actions{st, opts}
}
func ResolveTarget(ctx context.Context, st *store.Store, id int64) (Target, error) {
	return resolveActionTarget(ctx, st, id, false)
}
func resolveActionTarget(ctx context.Context, st *store.Store, id int64, allowDeleted bool) (t Target, err error) {
	m, err := st.MetaCommentTargetContext(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrMessageNotFound) {
			return t, actionFailure("message_not_found", nil, err)
		}
		return t, err
	}
	src, err := st.GetSourceByIDContext(ctx, m.SourceID)
	if err != nil {
		return t, err
	}
	if src.MergedIntoSourceID != 0 {
		return t, actionFailure("stale_identity", nil, store.ErrSourceRetired)
	}
	if m.MessageType != SourceType || src.SourceType != SourceType {
		return t, actionFailure("unsupported_source", nil, nil)
	}
	if m.ParentMessageID == 0 {
		return t, actionFailure("not_a_comment", nil, nil)
	}
	if m.DeletedFromSource && !allowDeleted {
		return t, actionFailure("target_deleted", nil, nil)
	}
	var metadata objectMetadata
	if json.Unmarshal([]byte(m.Metadata), &metadata) != nil || metadata.Version != objectMetadataVersion || !ValidAccountID(metadata.AppID) || !ValidAccountID(metadata.PageID) || src.Identifier != SourceIdentifier(metadata.AppID, metadata.PageID) || metadata.InstagramAccountID != "" || metadata.MediaID != "" || !safeObjectID(m.CommentID) || !safeObjectID(metadata.PostID) || metadata.ParentID != m.ParentCommentID || metadata.ParentID == "" {
		return t, actionFailure("stale_identity", nil, nil)
	}
	raw, err := st.GetMessageRawContext(ctx, id)
	if err != nil {
		return t, err
	}
	var obj Object
	if len(raw) > 0 && json.Unmarshal(raw, &obj) != nil {
		return t, actionFailure("stale_identity", nil, nil)
	}
	t = Target{MessageID: id, SourceID: m.SourceID, SourceIdentifier: src.Identifier, MessageType: m.MessageType, App: metadata.AppID, Asset: metadata.PageID, CommentID: m.CommentID, RootID: metadata.PostID, ParentID: metadata.ParentID, IsFromMe: m.IsFromMe, Deleted: m.DeletedFromSource, SentAt: m.SentAt.Time, AssetName: src.DisplayName.String}
	if t.AssetName == "" {
		t.AssetName = src.Identifier
	}
	if obj.From != nil {
		t.AuthorID, t.AuthorName = obj.From.ID, obj.From.Name
	}
	if err := restoreArchivedActionAuthor(ctx, st, &t, m.SenderID, SourceType, ActorIdentifier(t.App, t.Asset, "")); err != nil {
		return t, err
	}
	return t, nil
}
func actionBinding(t Target, action ActionKind, body string) string {
	visibility := ""
	if action == ActionPublicReply {
		visibility = "public"
	}
	if action == ActionPrivateReply {
		visibility = "private"
	}
	parts := []string{string(action), strconv.FormatInt(t.MessageID, 10), strconv.FormatInt(t.SourceID, 10), t.SourceIdentifier, t.App, t.Asset, t.CommentID, visibility, body}
	return strings.Join(parts, "\n")
}
func ConfirmToken(t Target, action ActionKind, body string) string {
	sum := sha256.Sum256([]byte(actionBinding(t, action, body)))
	return hex.EncodeToString(sum[:16])
}
func RequiredPermissions(provider string, action ActionKind) []string {
	if provider != "facebook" {
		return nil
	}
	switch action {
	case ActionPublicReply:
		return []string{"pages_manage_engagement"}
	case ActionPrivateReply:
		return []string{"pages_messaging"}
	default:
		return []string{"pages_manage_engagement", "pages_read_user_content"}
	}
}
func receipt(a store.MetaCommentAction) *Receipt {
	return &Receipt{Action: ActionKind(a.Action), ErrorCode: a.ErrorCode, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, ID: a.ID, State: a.State, RemoteID: a.RemoteID, RecipientID: a.RemoteRecipientID, ArchiveComplete: a.ArchiveComplete, ReplyMessageID: a.ReplyMessageID.Int64}
}

func (a *Actions) Run(ctx context.Context, req ActionRequest) (*ActionResult, error) {
	if ctx.Err() != nil {
		return nil, actionFailure(string(ClassCancelled), nil, ctx.Err())
	}
	if req.MessageID <= 0 || (req.ResolveUnknown != "" && req.ResolveUnknown != "sent" && req.ResolveUnknown != "not-sent") || (req.DryRun && req.ResolveUnknown != "") {
		return nil, actionFailure("invalid_request", nil, nil)
	}
	switch req.Action {
	case ActionHide, ActionUnhide, ActionDelete, ActionPublicReply, ActionPrivateReply:
	default:
		return nil, actionFailure("invalid_request", nil, nil)
	}
	if isReply(req.Action) {
		if len(req.Body) == 0 || len(req.Body) > 8000 || strings.ContainsRune(req.Body, 0) || !utf8.ValidString(req.Body) {
			return nil, actionFailure("invalid_body", nil, nil)
		}
	} else if req.Body != "" {
		return nil, actionFailure("invalid_request", nil, nil)
	}
	// Deleted provider objects still have receipts to inspect and resolve.
	// Fresh writes are refused after the receipt lookup under source ownership.
	target, err := resolveActionTarget(ctx, a.st, req.MessageID, true)
	if err != nil {
		return nil, err
	}
	result := &ActionResult{MessageID: target.MessageID, SourceID: target.SourceID, Action: req.Action, DryRun: req.DryRun, Disclosure: Disclosure{Provider: "facebook", AssetID: target.Asset, AssetName: target.AssetName, CommentID: target.CommentID, AuthorName: target.AuthorName}, Confirm: ConfirmToken(target, req.Action, req.Body)}
	result.Disclosure.Body = req.Body
	result.Disclosure.Public = req.Action == ActionPublicReply
	if isReply(req.Action) {
		result.Visibility = "public"
		if req.Action == ActionPrivateReply {
			result.Visibility = "private"
		}
	}
	if req.Action == ActionPrivateReply {
		if err = a.privateReplyChecks(ctx, target, req, result); err != nil {
			return result, err
		}
	}
	keyHash := sha256.Sum256([]byte(actionBinding(target, req.Action, req.Body)))
	key := hex.EncodeToString(keyHash[:])
	if req.DryRun {
		return result, nil
	}
	if (req.Action == ActionDelete || isReply(req.Action)) && req.Confirm != result.Confirm {
		return result, actionFailure("confirmation_mismatch", result, nil)
	}
	execution, err := a.st.AcquireSyncExecutionContext(ctx, target.SourceID)
	if err != nil {
		code := "local_failed"
		if errors.Is(err, store.ErrSyncAlreadyActive) {
			code = "sync_active"
		}
		return result, actionFailure(code, result, err)
	}
	defer func() { _ = execution.Release() }()
	locked, err := resolveActionTarget(ctx, a.st, req.MessageID, true)
	if err != nil {
		return result, err
	}
	if locked != target {
		return result, actionFailure("stale_identity", result, nil)
	}
	previous, lookupErr := a.st.GetMetaCommentActionByKeyContext(ctx, key)
	if req.Action == ActionHide || req.Action == ActionUnhide {
		rows, e := a.st.ListMetaCommentActionsContext(ctx, target.SourceID, target.CommentID)
		if e != nil {
			return result, e
		}
		lookupErr = store.ErrMetaCommentActionNotFound
		for _, row := range rows {
			if row.Action == string(req.Action) && row.SourceIdentifier == target.SourceIdentifier && (row.State == "unknown" || row.State == "claimed" || row.State == "dispatched") {
				previous = row
				lookupErr = nil
			}
		}
	}
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrMetaCommentActionNotFound) {
		return result, lookupErr
	}
	if lookupErr == nil && previous.State != "failed" {
		return a.existing(ctx, target, req, result, previous)
	}
	if req.ResolveUnknown != "" {
		return result, actionFailure("invalid_request", result, nil)
	}
	if req.Action == ActionPrivateReply && target.IsFromMe {
		return result, actionFailure("private_reply_not_allowed", result, nil)
	}
	if req.Action == ActionPrivateReply && !a.privateReplyWindowOpen(target) {
		return result, actionFailure("private_reply_window_closed", result, nil)
	}
	if target.Deleted {
		return result, actionFailure("target_deleted", result, nil)
	}
	src, err := a.st.GetSourceByIDContext(ctx, target.SourceID)
	if err != nil {
		return result, err
	}
	if src.HistoryOnly || src.MergedIntoSourceID != 0 {
		return result, actionFailure("stale_identity", result, store.ErrSourceRetired)
	}
	// Provider opt-ins gate fresh dispatch, not owner-authorized local recovery.
	if a.opts.Allowed == nil {
		return result, actionFailure("action_not_enabled", result, nil)
	}
	allowed, err := a.opts.Allowed(target.SourceID, req.Action)
	if err != nil {
		return result, err
	}
	if !allowed {
		return result, actionFailure("action_not_enabled", result, nil)
	}
	if a.opts.OpenAPI == nil {
		return result, actionFailure("credential_unavailable", result, nil)
	}
	api, err := a.opts.OpenAPI(ctx, src)
	if err != nil {
		var providerErr *APIError
		if errors.As(err, &providerErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, verificationFailure(result, err)
		}
		return result, actionFailure("credential_unavailable", result, err)
	}
	if api == nil {
		return result, actionFailure("credential_unavailable", result, nil)
	}
	if _, err = api.VerifyPage(ctx, target.Asset); err != nil {
		return result, verificationFailure(result, err)
	}
	if ctx.Err() != nil {
		return result, actionFailure(string(ClassCancelled), result, ctx.Err())
	}
	if req.Action == ActionHide || req.Action == ActionUnhide {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return result, fmt.Errorf("create comment action nonce: %w", err)
		}
		key += "-" + hex.EncodeToString(nonce[:])
	}
	row, existing, err := a.st.ClaimMetaCommentActionContext(ctx, store.MetaCommentActionClaim{ActionKey: key, SourceID: target.SourceID, SourceIdentifier: target.SourceIdentifier, MessageID: target.MessageID, TargetCommentID: target.CommentID, TargetAuthorID: target.AuthorID, Action: string(req.Action), Body: req.Body})
	if err != nil && !errors.Is(err, store.ErrMetaCommentActionUnknown) {
		return result, err
	}
	if existing {
		return a.existing(ctx, target, req, result, row)
	}
	if ctx.Err() != nil {
		return a.finish(ctx, target, result, row, ctx.Err(), false)
	}
	row, err = a.st.MarkMetaCommentActionDispatchedContext(ctx, row.ID, row.RowRevision)
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return a.finish(ctx, target, result, row, ctx.Err(), false)
	}
	if isReply(req.Action) {
		return a.dispatchReply(ctx, target, req, result, row, api)
	}
	if req.Action == ActionDelete {
		err = api.DeleteComment(ctx, target.CommentID)
	} else {
		err = api.SetCommentHidden(ctx, target.CommentID, req.Action == ActionHide)
	}
	return a.finish(ctx, target, result, row, err, true)
}
func (a *Actions) existing(ctx context.Context, t Target, req ActionRequest, result *ActionResult, row store.MetaCommentAction) (*ActionResult, error) {
	// We hold exclusive source ownership, so no live action owns these states.
	if row.State == "claimed" {
		return a.finish(ctx, t, result, row, context.Canceled, false)
	}
	if row.State == "dispatched" {
		local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		saved, err := a.st.FinishMetaCommentActionContext(local, row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "unknown", ErrorCode: string(ClassUnknown)})
		if err != nil {
			result.Receipt = receipt(row)
			return result, actionFailure("local_failed", result, err)
		}
		row = saved
	}
	localEffectApplied := false
	if row.State == "unknown" && req.ResolveUnknown != "" {
		if req.ResolveUnknown == "sent" {
			result.Receipt = receipt(row)
			if _, err := a.localEffect(ctx, t, result, row); err != nil {
				return result, err
			}
			localEffectApplied = true
		}
		var err error
		row, err = a.st.ResolveUnknownMetaCommentActionContext(ctx, row.ID, row.RowRevision, req.ResolveUnknown == "sent")
		if err != nil {
			return result, err
		}
	}
	result.Receipt = receipt(row)
	if req.Action == ActionPrivateReply && row.State == "succeeded" && req.ResolveUnknown == "" {
		return result, actionFailure("already_sent", result, nil)
	}
	if row.State == "succeeded" {
		if localEffectApplied {
			return result, nil
		}
		return a.localEffect(ctx, t, result, row)
	}
	if row.State == "failed" {
		return result, nil
	}
	return result, actionFailure(string(ClassUnknown), result, nil)
}
func (a *Actions) finish(ctx context.Context, t Target, result *ActionResult, row store.MetaCommentAction, remoteErr error, dispatched bool, accepted ...store.MetaCommentActionOutcome) (*ActionResult, error) {
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	persistEffectBeforeReceipt := result.Action == ActionHide || result.Action == ActionUnhide
	if remoteErr == nil && persistEffectBeforeReceipt {
		// Persist the archive effect before marking the receipt succeeded. If
		// either write fails, the dispatched receipt remains recoverable.
		if _, err := a.localEffect(local, t, result, row); err != nil {
			result.Receipt = receipt(row)
			return result, err
		}
	}
	outcome := store.MetaCommentActionOutcome{State: "succeeded"}
	if len(accepted) > 0 {
		outcome = accepted[0]
		outcome.State = "succeeded"
	}
	if remoteErr != nil {
		outcome.ErrorCode = string(Classify(remoteErr, dispatched))
		outcome.State = "failed"
		if outcome.ErrorCode == string(ClassUnknown) {
			outcome.State = "unknown"
		}
	}
	saved, err := a.st.FinishMetaCommentActionContext(local, row.ID, row.RowRevision, outcome)
	if err != nil {
		result.Receipt = receipt(row)
		result.Receipt.RemoteID, result.Receipt.RecipientID = outcome.RemoteID, outcome.RemoteRecipientID
		code := "remote_accepted_local_failed"
		if remoteErr != nil {
			code = "local_failed"
		}
		return result, actionFailure(code, result, err)
	}
	result.Receipt = receipt(saved)
	if remoteErr != nil {
		failure := actionFailure(outcome.ErrorCode, result, remoteErr)
		failure.AbuseWarning = AbuseWarning(remoteErr)
		if outcome.ErrorCode == string(ClassPermissionMissing) || outcome.ErrorCode == string(ClassAppAccess) {
			failure.RequiredPermissions = RequiredPermissions("facebook", result.Action)
		}
		return result, failure
	}
	if remoteErr == nil && !persistEffectBeforeReceipt {
		if _, err := a.localEffect(local, t, result, saved); err != nil {
			return result, err
		}
	}
	return result, nil
}
func (a *Actions) localEffect(ctx context.Context, t Target, result *ActionResult, row store.MetaCommentAction) (*ActionResult, error) {
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	switch result.Action {
	case ActionDelete:
		err = a.st.MarkMessageDeletedContext(local, t.SourceID, t.CommentID)
	case ActionHide, ActionUnhide:
		err = a.setHiddenMetadata(local, t.MessageID, result.Action == ActionHide, row)
	default: // Reply receipts require no moderation effect.
	}
	if err != nil {
		return result, actionFailure("remote_accepted_local_failed", result, err)
	}
	return result, nil
}

func (a *Actions) setHiddenMetadata(ctx context.Context, messageID int64, hidden bool, row store.MetaCommentAction) error {
	saved, err := a.st.GetMessageMetadataContext(ctx, messageID)
	if err != nil {
		return err
	}
	var metadata objectMetadata
	if err = json.Unmarshal([]byte(saved.String), &metadata); err != nil {
		return err
	}
	// Recovery does not observe the provider again. Its evidence is no newer
	// than the original dispatch, even if resolution happens much later.
	effectTime := row.CreatedAt.Unix()
	if row.DispatchedAt != nil {
		effectTime = row.DispatchedAt.Unix()
	}
	if metadata.StateTime > effectTime {
		return nil
	}
	// Provider event times are seconds; receipt dispatch times retain the
	// order of local actions within the same second during later recovery.
	rows, err := a.st.ListMetaCommentActionsContext(ctx, row.SourceID, row.TargetCommentID)
	if err != nil {
		return err
	}
	for _, later := range rows {
		if later.ID == row.ID || later.State != "succeeded" || later.DispatchedAt == nil || (later.Action != string(ActionHide) && later.Action != string(ActionUnhide)) {
			continue
		}
		if row.DispatchedAt == nil || later.DispatchedAt.After(*row.DispatchedAt) || (later.DispatchedAt.Equal(*row.DispatchedAt) && later.ID > row.ID) {
			return nil
		}
	}
	metadata.Hidden = &hidden
	metadata.StateTime = effectTime
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return a.st.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(data), Valid: true})
}
