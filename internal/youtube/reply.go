package youtube

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

const replyReceiptWriteTimeout = 10 * time.Second

var (
	ErrReplyUncertain = errors.New("YouTube reply outcome is uncertain; resolve the receipt with an explicit provider comment ID before continuing")
	ErrReplyRejected  = errors.New("YouTube reply rejected")
)

// ReplyTarget records both the selected message and its actual public parent.
// Resolve these fields from the archive, never from caller-supplied provider IDs.
type ReplyTarget struct {
	MessageID    int64  `json:"message_id"`
	SourceID     int64  `json:"source_id"`
	ChannelID    string `json:"channel_id"`
	VideoID      string `json:"video_id"`
	CommentID    string `json:"comment_id"`
	ParentID     string `json:"parent_id"`
	ThreadID     string `json:"thread_id"`
	CanReply     bool   `json:"can_reply"`
	ParentStatus string `json:"parent_status"`
}

// ReplyReceipt is a durable intent. Sending is deliberately never reset after
// a crash: YouTube has no insert idempotency key. Accepted retries archive only.
type ReplyReceipt struct {
	Target    ReplyTarget `json:"target"`
	Body      string      `json:"body"`
	Token     string      `json:"token"`
	State     string      `json:"state"`
	Comment   *Comment    `json:"comment,omitempty"`
	MessageID int64       `json:"message_id,omitempty"`
}

type ReplyProvider interface {
	CheckReply(ctx context.Context, target ReplyTarget) error
	InsertReply(ctx context.Context, parentID, body string) (Comment, error)
	GetReply(ctx context.Context, commentID string) (Comment, error)
}

type ReplyService struct {
	Store    *store.Store
	Provider ReplyProvider
	Persist  func(context.Context, Comment) (int64, error)
}

func replyKey(target ReplyTarget, body string) (string, error) {
	// JSON preserves tuple boundaries, including hostile delimiters in body.
	// Selected local ID is omitted so another alias cannot bypass an unknown send.
	encoded, err := json.Marshal([]string{target.ChannelID, target.VideoID, target.ParentID, body})
	if err != nil {
		return "", errors.New("encode YouTube reply fingerprint")
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func validateReplyIdentity(target ReplyTarget, body string) error {
	if target.MessageID <= 0 || target.SourceID <= 0 || !validID(target.ChannelID) || !validID(target.VideoID) || !validCommentID(target.CommentID) || !validCommentID(target.ParentID) || !validCommentID(target.ThreadID) || strings.HasPrefix(target.CommentID, "video:") {
		return errors.New("reply target is not an archived YouTube comment")
	}
	if strings.TrimSpace(body) == "" || !utf8.ValidString(body) {
		return errors.New("reply body must be nonempty UTF-8 text")
	}
	return nil
}

func validateReply(target ReplyTarget, body string) error {
	if err := validateReplyIdentity(target, body); err != nil {
		return err
	}
	if !target.CanReply || target.ParentStatus != "published" {
		return errors.New("public reply requires a published parent with known reply permission")
	}
	return nil
}

func (s *ReplyService) load(ctx context.Context, key string) (*ReplyReceipt, string, error) {
	raw, err := s.Store.GetYouTubeReplyReceiptContext(ctx, key)
	if err != nil || raw == "" {
		return nil, raw, err
	}
	var receipt ReplyReceipt
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return nil, raw, errors.New("invalid stored YouTube reply receipt")
	}
	return &receipt, raw, nil
}

func (s *ReplyService) transition(ctx context.Context, key, old string, receipt *ReplyReceipt) (bool, string, error) {
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return false, "", errors.New("encode YouTube reply receipt")
	}
	next := string(encoded)
	won, err := s.Store.CompareAndSwapYouTubeReplyReceiptContext(ctx, receipt.Target.SourceID, key, old, next)
	return won, next, err
}

// Preview does not call the provider, refresh credentials or load remote media.
func (s *ReplyService) Preview(ctx context.Context, target ReplyTarget, body string) (*ReplyReceipt, error) {
	if err := validateReply(target, body); err != nil {
		return nil, err
	}
	key, err := replyKey(target, body)
	if err != nil {
		return nil, err
	}
	receipt, old, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if receipt != nil {
		if receipt.State == "discarded" {
			return nil, errors.New("reply preview was discarded during source merge")
		}
		if receipt.State == "removed" {
			return nil, errors.New("this reply operation was redacted with its source and cannot send again")
		}
		if receipt.State == "sending" {
			return receipt, ErrReplyUncertain
		}
		if receipt.Target.MessageID != target.MessageID {
			return nil, errors.New("this reply already has a receipt for another selected message")
		}
		if receipt.State != "rejected" {
			return receipt, nil
		}
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return nil, fmt.Errorf("create reply confirmation: %w", err)
	}
	receipt = &ReplyReceipt{Target: target, Body: body, Token: hex.EncodeToString(token), State: "prepared"}
	won, _, err := s.transition(ctx, key, old, receipt)
	if err != nil {
		return nil, err
	}
	if !won {
		return s.Preview(ctx, target, body)
	}
	return receipt, nil
}

func (s *ReplyService) confirmed(ctx context.Context, target ReplyTarget, body, token string) (*ReplyReceipt, string, error) {
	if err := validateReplyIdentity(target, body); err != nil {
		return nil, "", err
	}
	key, err := replyKey(target, body)
	if err != nil {
		return nil, "", err
	}
	receipt, raw, err := s.load(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if receipt != nil && receipt.State == "discarded" {
		return nil, "", errors.New("reply preview was discarded during source merge")
	}
	if receipt == nil || token == "" || subtle.ConstantTimeCompare([]byte(receipt.Token), []byte(token)) != 1 || receipt.Target.MessageID != target.MessageID || receipt.Target.SourceID != target.SourceID || receipt.Target.CommentID != target.CommentID || receipt.Target.ThreadID != target.ThreadID {
		return nil, "", errors.New("confirmation does not match this archived target and exact reply body; run --dry-run")
	}
	return receipt, raw, nil
}

func matchesReplyIdentity(comment Comment, target ReplyTarget) bool {
	return validCommentID(comment.ID) && comment.Snippet.ParentID == target.ParentID && (comment.Snippet.VideoID == "" || comment.Snippet.VideoID == target.VideoID) && comment.Snippet.AuthorChannelID.Value == target.ChannelID
}

func matchesReply(comment Comment, target ReplyTarget, body string) bool {
	return matchesReplyIdentity(comment, target) && comment.Snippet.TextOriginal == body
}

func (s *ReplyService) archivedReply(ctx context.Context, target ReplyTarget, accepted Comment) (int64, bool, error) {
	storedMessages, err := s.Store.StoredMessagesContext(ctx, target.SourceID, "youtube_json", []string{accepted.ID})
	if err != nil {
		return 0, false, fmt.Errorf("look up archived YouTube reply: %w", err)
	}
	stored, ok := storedMessages[accepted.ID]
	if !ok {
		return 0, false, nil
	}
	if len(stored.Raw) == 0 {
		return 0, false, errors.New("saved reply cannot be verified")
	}
	var comment Comment
	if err := json.Unmarshal(stored.Raw, &comment); err != nil {
		return 0, false, fmt.Errorf("cannot read saved reply: %w", err)
	}
	if comment.ID != accepted.ID || !matchesReplyIdentity(comment, target) {
		return 0, false, errors.New("saved reply does not match YouTube's reply")
	}
	return stored.ID, true, nil
}

// Confirm dispatches at most one POST for an intent, including across restarts.
func (s *ReplyService) Confirm(ctx context.Context, target ReplyTarget, body, token string) (*ReplyReceipt, error) {
	receipt, raw, err := s.confirmed(ctx, target, body, token)
	if err != nil {
		return nil, err
	}
	key, err := replyKey(target, body)
	if err != nil {
		return nil, err
	}
	switch receipt.State {
	case "archived":
		return receipt, nil
	case "accepted":
		return s.archive(ctx, key, raw, receipt)
	case "sending":
		return receipt, ErrReplyUncertain
	case "rejected":
		return receipt, ErrReplyRejected
	case "prepared":
	default:
		return nil, errors.New("invalid reply receipt state")
	}
	if err = validateReply(target, body); err != nil {
		return receipt, err
	}
	if err = s.Provider.CheckReply(ctx, target); err != nil {
		return receipt, err
	}
	receipt.State = "sending"
	won, sending, err := s.transition(ctx, key, raw, receipt)
	if err != nil {
		return nil, err
	}
	if !won {
		return s.Confirm(ctx, target, body, token)
	}
	comment, sendErr := s.Provider.InsertReply(ctx, target.ParentID, body)
	// Once dispatch might have occurred, cancellation must not erase the receipt.
	// A fresh retry can recover an accepted ID even when local archival failed.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyReceiptWriteTimeout)
	defer cancel()
	if sendErr != nil {
		// A provider rejection or a failure proven to precede the comments API
		// request cannot have created a reply, so a fresh preview may retry.
		if errors.Is(sendErr, ErrReplyRejected) || errors.Is(sendErr, errAPIRequestNotDispatched) {
			receipt.State = "rejected"
			_, _, err = s.transition(saveCtx, key, sending, receipt)
			return receipt, errors.Join(sendErr, err)
		}
		return receipt, ErrReplyUncertain
	}
	if !matchesReply(comment, target, body) {
		return receipt, ErrReplyUncertain
	}
	receipt.State = "accepted"
	receipt.Comment = &comment
	won, accepted, err := s.transition(saveCtx, key, sending, receipt)
	if err != nil || !won {
		return receipt, errors.Join(ErrReplyUncertain, err)
	}
	return s.archive(ctx, key, accepted, receipt)
}

func (s *ReplyService) archive(ctx context.Context, key, raw string, receipt *ReplyReceipt) (*ReplyReceipt, error) {
	if receipt.Comment == nil || !matchesReply(*receipt.Comment, receipt.Target, receipt.Body) {
		return receipt, errors.New("invalid accepted reply evidence")
	}
	messageID, alreadyArchived, err := s.archivedReply(ctx, receipt.Target, *receipt.Comment)
	if err != nil {
		return receipt, err
	}
	if !alreadyArchived {
		messageID, err = s.Persist(ctx, *receipt.Comment)
		if err != nil {
			return receipt, fmt.Errorf("reply accepted; archive retry will not send again: %w", err)
		}
	} else if err := s.Store.RecomputeConversationStatsForMessageContext(ctx, messageID); err != nil {
		return receipt, fmt.Errorf("refresh saved reply conversation statistics: %w", err)
	}
	receipt.State = "archived"
	receipt.MessageID = messageID
	won, _, err := s.transition(ctx, key, raw, receipt)
	if err != nil {
		return receipt, err
	}
	if !won {
		current, _, readErr := s.load(ctx, key)
		if readErr != nil {
			return receipt, readErr
		}
		if current != nil && current.State == "archived" {
			return current, nil
		}
		return receipt, errors.New("reply receipt changed during archival")
	}
	return receipt, nil
}

// Resolve accepts explicit provider evidence for an uncertain send. An absent
// list result or matching text alone can never authorize another POST.
func (s *ReplyService) Resolve(ctx context.Context, target ReplyTarget, body, token, commentID string) (*ReplyReceipt, error) {
	receipt, raw, err := s.confirmed(ctx, target, body, token)
	if err != nil {
		return nil, err
	}
	if receipt.State == "accepted" || receipt.State == "archived" {
		return s.Confirm(ctx, target, body, token)
	}
	if receipt.State != "sending" || !validCommentID(commentID) {
		return receipt, errors.New("resolution needs an uncertain receipt and explicit provider comment ID")
	}
	comment, err := s.Provider.GetReply(ctx, commentID)
	if err != nil {
		return receipt, err
	}
	if comment.ID != commentID || !matchesReply(comment, target, body) {
		return receipt, errors.New("provider comment does not match receipt channel, video, parent and body")
	}
	receipt.State = "accepted"
	receipt.Comment = &comment
	key, err := replyKey(target, body)
	if err != nil {
		return receipt, err
	}
	won, accepted, err := s.transition(ctx, key, raw, receipt)
	if err != nil {
		return receipt, err
	}
	if !won {
		return receipt, errors.New("reply receipt changed during resolution")
	}
	return s.archive(ctx, key, accepted, receipt)
}
