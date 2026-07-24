package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const (
	defaultConversationBound = 25
	maxConversationBound     = 50
)

// ConversationResponse is a bounded chronological conversation window.
type ConversationResponse struct {
	ID        int64           `json:"id"`
	AnchorID  int64           `json:"anchor_id"`
	Messages  []MessageDetail `json:"messages"`
	HasBefore bool            `json:"has_before"`
	HasAfter  bool            `json:"has_after"`
	Total     int64           `json:"total"`
}

type conversationStore interface {
	ConversationExists(conversationID int64) (bool, error)
	GetConversationWindow(conversationID, anchorID int64, before, after int) (*store.ConversationWindow, error)
}

// ConversationWindowStore is the context-aware conversation reader that
// production store adapters should implement so conversation endpoints work
// under a cancellable request context instead of falling back to the
// legacy background-context path.
type ConversationWindowStore interface {
	ConversationExistsContext(ctx context.Context, conversationID int64) (bool, error)
	GetConversationWindowContext(
		ctx context.Context,
		conversationID, anchorID int64,
		before, after int,
		start, end *time.Time,
	) (*store.ConversationWindow, error)
}

func (s *Server) conversationExists(ctx context.Context, conversationID int64) (bool, error) {
	if reader, ok := s.store.(ConversationWindowStore); ok {
		return reader.ConversationExistsContext(ctx, conversationID)
	}
	reader, ok := s.store.(conversationStore)
	if !ok {
		return false, errors.New("conversation reader unavailable")
	}
	return reader.ConversationExists(conversationID)
}

func (s *Server) conversationWindow(
	ctx context.Context,
	conversationID, anchorID int64,
	before, after int,
	start, end *time.Time,
) (*store.ConversationWindow, error) {
	if reader, ok := s.store.(ConversationWindowStore); ok {
		return reader.GetConversationWindowContext(ctx, conversationID, anchorID, before, after, start, end)
	}
	if start != nil || end != nil {
		return nil, errors.New("conversation reader does not support time-bounded windows")
	}
	reader, ok := s.store.(conversationStore)
	if !ok {
		return nil, errors.New("conversation reader unavailable")
	}
	return reader.GetConversationWindow(conversationID, anchorID, before, after)
}

func conversationBound(r *http.Request, name string) (int, error) {
	value, present, err := queryInt(r, name)
	if err != nil {
		return 0, err
	}
	if !present {
		return defaultConversationBound, nil
	}
	if value < 0 || value > maxConversationBound {
		return 0, newParamError(name,
			fmt.Sprintf("query parameter %q must be between 0 and %d", name, maxConversationBound))
	}
	return value, nil
}

// conversationTimeRange parses the optional "start"/"end" query params
// (RFC3339, UTC) that scope the conversation window to a half-open
// [start, end) range. Either or both may be absent. A present-but-unparseable
// value, or start >= end when both are present, is a paramError.
func conversationTimeRange(r *http.Request) (start, end *time.Time, err error) {
	startValue, startPresent, err := queryDate(r, "start")
	if err != nil {
		return nil, nil, err
	}
	endValue, endPresent, err := queryDate(r, "end")
	if err != nil {
		return nil, nil, err
	}
	if startPresent {
		start = &startValue
	}
	if endPresent {
		end = &endValue
	}
	if start != nil && end != nil && !start.Before(*end) {
		return nil, nil, newParamError("end", "query parameter \"end\" must be after \"start\"")
	}
	return start, end, nil
}

func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	conversationID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || conversationID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "Conversation ID must be a positive integer")
		return
	}
	anchorID, present, err := queryInt64(r, "anchor")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if !present || anchorID <= 0 {
		writeError(w, http.StatusBadRequest, "missing_anchor", "Query parameter 'anchor' is required")
		return
	}
	before, err := conversationBound(r, "before")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	after, err := conversationBound(r, "after")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	start, end, err := conversationTimeRange(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "conversation_unavailable", "Conversation details are unavailable")
		return
	}
	found, err := s.conversationExists(r.Context(), conversationID)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		writeError(w, http.StatusServiceUnavailable, "conversation_unavailable", "Conversation details are unavailable")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "conversation_not_found", "Conversation not found")
		return
	}
	window, err := s.conversationWindow(r.Context(), conversationID, anchorID, before, after, start, end)
	if err != nil {
		if errors.Is(err, store.ErrConversationAnchorOutsideRange) {
			writeError(w, http.StatusBadRequest, "conversation_anchor_outside_range",
				"Anchor message is outside the requested time range")
			return
		}
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("get conversation", "conversation_id", conversationID, "anchor_id", anchorID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not retrieve conversation")
		return
	}
	if window == nil || len(window.Messages) == 0 {
		writeError(w, http.StatusNotFound, "conversation_anchor_not_found", "Anchor message is not in this conversation")
		return
	}
	messages := make([]MessageDetail, 0, len(window.Messages))
	for _, message := range window.Messages {
		detail := MessageDetail{
			MessageSummary: toMessageSummary(message),
			Body:           message.Body,
			BodyHTML:       message.BodyHTML,
			BodyOmitted:    message.BodyOmitted,
			Attachments:    make([]AttachmentInfo, 0, len(message.Attachments)),
		}
		for _, attachment := range message.Attachments {
			detail.Attachments = append(detail.Attachments, attachmentInfoFromStore(attachment))
		}
		messages = append(messages, detail)
	}
	writeJSON(w, http.StatusOK, ConversationResponse{
		ID:        conversationID,
		AnchorID:  anchorID,
		Messages:  messages,
		HasBefore: window.AnchorPosition-int64(before) > 1,
		HasAfter:  window.AnchorPosition+int64(after) < window.Total,
		Total:     window.Total,
	})
}
