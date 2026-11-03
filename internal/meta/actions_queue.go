package meta

import "time"

type CommentQueueRequest struct {
	Source   string `json:"source,omitempty"`
	State    string `json:"state,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	BeforeID int64  `json:"before_id,omitempty"`
}
type CommentQueueAuthor struct {
	ParticipantID int64  `json:"participant_id"`
	DisplayName   string `json:"display_name"`
}
type CommentQueueItem struct {
	MessageID             int64              `json:"message_id"`
	SourceID              int64              `json:"source_id"`
	SourceIdentifier      string             `json:"source_identifier"`
	Provider              string             `json:"provider"`
	ConversationID        int64              `json:"conversation_id"`
	CommentID             string             `json:"comment_id"`
	ParentMessageID       int64              `json:"parent_message_id"`
	Author                CommentQueueAuthor `json:"author"`
	Snippet               string             `json:"snippet"`
	SentAt                *time.Time         `json:"sent_at"`
	Ad                    bool               `json:"ad"`
	Triage                string             `json:"triage"`
	Moderation            string             `json:"moderation"`
	DeletedFromSource     bool               `json:"deleted_from_source"`
	PageReplied           bool               `json:"page_replied"`
	PrivateReplyOpenUntil *time.Time         `json:"private_reply_open_until"`
	Actions               []Receipt          `json:"actions"`
}
type CommentQueueResponse struct {
	Items        []CommentQueueItem `json:"items"`
	NextBeforeID int64              `json:"next_before_id,omitempty"`
}

func ValidateCommentQueueRequest(f CommentQueueRequest) error {
	if (f.State != "" && f.State != "open" && f.State != "handled" && f.State != "skipped" && f.State != "all") || f.Limit < 0 || f.Limit > 200 || f.BeforeID < 0 {
		return &ActionError{Code: "invalid_request", Message: "Invalid comment queue filters"}
	}
	return nil
}
