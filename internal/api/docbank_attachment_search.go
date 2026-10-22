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
	Results   []MediaAttachmentResult `json:"results"`
	Truncated bool                    `json:"truncated"`
	Error     *ErrorResponse          `json:"error,omitempty"`
}

// SearchAttachments searches delivered attachment versions newest chunk first.
// Each chunk keeps Docbank's ranking and maps only to currently visible messages.
func (reader *DocbankAttachmentReader) SearchAttachments(ctx context.Context, query string, limit int, scope *personscope.Scope, resolve func() (*personscope.Scope, error)) (MediaAttachmentSearch, error) {
	response := MediaAttachmentSearch{Results: []MediaAttachmentResult{}}
	remoteCtx, cancel := context.WithTimeout(ctx, docbankBudget(ctx))
	defer cancel()
	var after *store.DocumentSearchVersion
	var hits []docbankmedia.SearchHit
	fail := func(err error) (MediaAttachmentSearch, error) {
		response.Truncated = response.Truncated || after != nil || len(hits) > 0
		// A budget spent after the first chunk keeps the hits so far as a truncated result.
		if _, recheck := reader.visible(ctx, &response, hits, limit, resolve); recheck == nil && after != nil && remoteCtx.Err() != nil && ctx.Err() == nil {
			return response, nil
		}
		response.Error = attachmentSearchError(err)
		return response, err
	}
	for {
		versions, err := reader.Store.DocumentSearchVersions(remoteCtx, reader.Destination, scope, after)
		if err != nil {
			return fail(err)
		}
		if len(versions) == 0 {
			return response, nil
		}
		versionIDs := make([]string, 0, len(versions))
		for _, version := range versions {
			versionIDs = append(versionIDs, version.VersionID)
		}
		vault, err := reader.Client.VaultUID(remoteCtx)
		if err != nil {
			return fail(err)
		}
		// Docbank lexical search spans profiles but requires a registered name.
		report, err := reader.Client.SearchDocuments(remoteCtx, docbankmedia.SearchRequest{
			Query: query, Mode: "lexical", ContentFirst: true, Profile: suppliedTranscriptProfile,
			Limit: limit - len(response.Results), Fence: &docbankmedia.SearchFence{VaultUID: vault, ContentVersionIDs: versionIDs},
		})
		if err != nil {
			return fail(err)
		}
		hits = append(hits, report.Results...)
		response.Truncated = response.Truncated || report.Truncated
		// Remap every chunk's hits so messages hidden during this call drop out.
		if scope, err = reader.visible(remoteCtx, &response, hits, limit, resolve); err != nil {
			return fail(err)
		}
		after = &versions[len(versions)-1]
		if len(response.Results) == limit {
			if len(versions) == docbankmedia.MaxSearchVersions {
				next, err := reader.Store.DocumentSearchVersions(remoteCtx, reader.Destination, scope, after)
				if err != nil {
					return fail(err)
				}
				response.Truncated = response.Truncated || len(next) > 0
			}
			return response, nil
		}
		if len(versions) < docbankmedia.MaxSearchVersions {
			return response, nil
		}
	}
}

// visible replaces response.Results with the hits' attachments in the current
// scope, in Docbank's rank order, and returns that scope.
func (reader *DocbankAttachmentReader) visible(ctx context.Context, response *MediaAttachmentSearch, hits []docbankmedia.SearchHit, limit int, resolve func() (*personscope.Scope, error)) (*personscope.Scope, error) {
	response.Results = []MediaAttachmentResult{}
	scope, err := resolve()
	if err != nil {
		return nil, &mediaSearchScopeError{err}
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ContentVersionID)
	}
	occurrences, err := reader.Store.DocumentSearchOccurrences(ctx, reader.Destination, scope, ids)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[string][]store.DocumentSearchOccurrence)
	for _, o := range occurrences {
		byVersion[o.VersionID] = append(byVersion[o.VersionID], o)
	}
	for _, hit := range hits {
		for _, o := range byVersion[hit.ContentVersionID] {
			if len(response.Results) == limit {
				response.Truncated = true
				return scope, nil
			}
			response.Results = append(response.Results, MediaAttachmentResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Filename: o.Filename, ContainingTitle: o.ContainingTitle, OccurredAt: o.OccurredAt, Excerpt: hit.Excerpt})
		}
	}
	return scope, nil
}

// attachmentSearchError keeps Docbank's code, except that a rejected query
// reads as invalid_media_search, as it does for recordings.
func attachmentSearchError(err error) *ErrorResponse {
	if _, failure := mediaSearchFailure(err); failure.Error == "invalid_media_search" {
		return &ErrorResponse{Error: failure.Error, Message: "Attachment search rejected this query."}
	}
	return &ErrorResponse{Error: docbankmedia.ErrorCode(err), Message: "Attachment search is unavailable."}
}
