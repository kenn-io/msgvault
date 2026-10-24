package api

import (
	"cmp"
	"context"
	"slices"
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
	Results   []MediaAttachmentResult `json:"results"`
	Truncated bool                    `json:"truncated"`
	Error     *ErrorResponse          `json:"error,omitempty"`
}

// SearchAttachments searches delivered attachment versions in bounded batches.
// Results from each batch of 4,096 are interleaved by their position within
// that batch, so order across batches is approximate. Only visible messages appear.
func (reader *DocbankAttachmentReader) SearchAttachments(ctx, remoteCtx context.Context, query string, limit int, scope *personscope.Scope, resolve func() (*personscope.Scope, error)) (MediaAttachmentSearch, error) {
	response := MediaAttachmentSearch{Results: []MediaAttachmentResult{}}
	fail := func(err error) (MediaAttachmentSearch, error) {
		response.Error = attachmentSearchError(err)
		return response, err
	}
	versions, err := reader.Store.DocumentSearchVersions(ctx, reader.Destination, scope)
	if err != nil {
		response.Error = &ErrorResponse{Error: "server_error", Message: "Attachment search is unavailable."}
		return response, err
	}
	// Docbank lexical search spans profiles but requires a registered name.
	request := docbankmedia.SearchRequest{Query: query, Mode: "lexical", ContentFirst: true, Profile: suppliedTranscriptProfile, Limit: limit}
	if len(versions) == 0 {
		return response, nil
	}
	fence, err := reader.Client.VaultFence(remoteCtx, versions)
	if err != nil {
		return fail(err)
	}
	request.Fence = &fence
	var hits []docbankmedia.SearchHit
	for chunk := range slices.Chunk(versions, docbankmedia.MaxSearchVersions) {
		fence.ContentVersionIDs = chunk
		report, err := reader.Client.SearchDocuments(remoteCtx, request)
		if err != nil {
			return fail(err)
		}
		hits = append(hits, report.Results...)
		response.Truncated = response.Truncated || report.Truncated
	}
	if len(versions) > docbankmedia.MaxSearchVersions {
		slices.SortStableFunc(hits, func(a, b docbankmedia.SearchHit) int { return cmp.Compare(a.Rank, b.Rank) })
	}
	if err := reader.visible(ctx, &response, hits, limit, resolve); err != nil {
		response.Error = &ErrorResponse{Error: "server_error", Message: "Attachment search is unavailable."}
		return response, err
	}
	return response, nil
}

// visible replaces response.Results with the hits' attachments in the current
// scope, in the supplied hit order.
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
	occurrences, err := reader.Store.DocumentSearchOccurrences(ctx, reader.Destination, scope, ids, limit)
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

// attachmentSearchError reuses recordings' rejected-query code.
// Other failures keep Docbank's code.
func attachmentSearchError(err error) *ErrorResponse {
	_, failure := mediaSearchFailure(err)
	if failure.Error == "invalid_media_search" {
		return &ErrorResponse{Error: failure.Error, Message: "Attachment search rejected this query."}
	}
	return &ErrorResponse{Error: docbankmedia.ErrorCode(err), Message: "Attachment search is unavailable."}
}
