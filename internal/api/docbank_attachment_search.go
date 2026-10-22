package api

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
)

// MediaAttachmentResult links extracted text to a visible message attachment.
type MediaAttachmentResult struct {
	MessageID       int64      `json:"message_id"`
	ConversationID  int64      `json:"conversation_id"`
	AttachmentID    int64      `json:"attachment_id"`
	Filename        string     `json:"filename"`
	ContainingTitle string     `json:"containing_title"`
	OccurredAt      *time.Time `json:"occurred_at,omitempty"`
	Excerpt         string     `json:"excerpt"`
}

type MediaAttachmentSearch struct {
	ScopeTruncated bool                    `json:"scope_truncated" doc:"Only the newest 4096 mirrored versions in the selected scope were searched"`
	Results        []MediaAttachmentResult `json:"results"`
	Truncated      bool                    `json:"truncated"`
	Error          *ErrorResponse          `json:"error,omitempty"`
}

// SearchAttachments searches the newest delivered attachment versions.
// It keeps Docbank's ranking and maps only to currently visible messages.
func (reader *DocbankAttachmentReader) SearchAttachments(ctx, remoteCtx context.Context, query string, limit int, scope *personscope.Scope, resolve func() (*personscope.Scope, error)) (MediaAttachmentSearch, error) {
	response := MediaAttachmentSearch{Results: []MediaAttachmentResult{}}
	fail := func(err error) (MediaAttachmentSearch, error) {
		response.Error = attachmentSearchError(err)
		return response, err
	}
	versions, truncated, err := reader.Store.DocumentSearchVersions(ctx, reader.Destination, scope)
	if err != nil {
		return fail(err)
	}
	response.Truncated, response.ScopeTruncated = truncated, truncated
	if len(versions) == 0 {
		return response, nil
	}
	vault, err := reader.Client.VaultUID(remoteCtx)
	if err != nil {
		return fail(err)
	}
	// Docbank lexical search spans profiles but requires a registered name.
	report, err := reader.Client.SearchDocuments(remoteCtx, docbankmedia.SearchRequest{
		Query: query, Mode: "lexical", ContentFirst: true, Profile: suppliedTranscriptProfile,
		Limit: limit, Fence: &docbankmedia.SearchFence{VaultUID: vault, ContentVersionIDs: versions},
	})
	if err != nil {
		return fail(err)
	}
	response.Truncated = response.Truncated || report.Truncated
	if err := reader.visible(ctx, &response, report.Results, limit, resolve); err != nil {
		return fail(err)
	}
	return response, nil
}

// visible replaces response.Results with the hits' attachments in the current
// scope, in Docbank's rank order.
func (reader *DocbankAttachmentReader) visible(ctx context.Context, response *MediaAttachmentSearch, hits []docbankmedia.SearchHit, limit int, resolve func() (*personscope.Scope, error)) error {
	response.Results = []MediaAttachmentResult{}
	scope, err := resolve()
	if err != nil {
		return &mediaSearchScopeError{err}
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ContentVersionID)
	}
	occurrences, err := reader.Store.DocumentSearchOccurrences(ctx, reader.Destination, scope, ids)
	if err != nil {
		return err
	}
	byVersion := make(map[string][]store.DocumentSearchOccurrence)
	for _, o := range occurrences {
		byVersion[o.VersionID] = append(byVersion[o.VersionID], o)
	}
	for _, hit := range hits {
		for _, o := range byVersion[hit.ContentVersionID] {
			if len(response.Results) == limit {
				response.Truncated = true
				return nil
			}
			response.Results = append(response.Results, MediaAttachmentResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Filename: o.Filename, ContainingTitle: o.ContainingTitle, OccurredAt: o.OccurredAt, Excerpt: hit.Excerpt})
		}
	}
	return nil
}

// attachmentSearchError keeps Docbank's code, except that a rejected query
// reads as invalid_media_search, as it does for recordings.
func attachmentSearchError(err error) *ErrorResponse {
	if _, failure := mediaSearchFailure(err); failure.Error == "invalid_media_search" {
		return &ErrorResponse{Error: failure.Error, Message: "Attachment search rejected this query."}
	}
	return &ErrorResponse{Error: docbankmedia.ErrorCode(err), Message: "Attachment search is unavailable."}
}
