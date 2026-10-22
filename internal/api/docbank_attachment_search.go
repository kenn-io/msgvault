package api

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
)

// MediaDocumentResult links extracted text to a visible message attachment.
type MediaDocumentResult struct {
	MessageID       int64      `json:"message_id"`
	ConversationID  int64      `json:"conversation_id"`
	AttachmentID    int64      `json:"attachment_id"`
	Filename        string     `json:"filename"`
	ContainingTitle string     `json:"containing_title"`
	OccurredAt      *time.Time `json:"occurred_at,omitempty"`
	Excerpt         string     `json:"excerpt"`
}

type MediaDocumentSearch struct {
	Results   []MediaDocumentResult `json:"results"`
	Truncated bool                  `json:"truncated"`
	Error     *ErrorResponse        `json:"error,omitempty"`
}

// SearchDocuments searches delivered attachment versions newest chunk first.
// Each chunk keeps Docbank's ranking and maps only to currently visible messages.
func (reader *DocbankAttachmentReader) SearchDocuments(ctx context.Context, query string, limit int, scope *personscope.Scope) MediaDocumentSearch {
	response := MediaDocumentSearch{Results: []MediaDocumentResult{}}
	remoteCtx, cancel := context.WithTimeout(ctx, docbankBudget(ctx))
	defer cancel()
	offset := 0
	fail := func(err error) MediaDocumentSearch {
		response.Error = &ErrorResponse{Error: docbankmedia.ErrorCode(err), Message: "Attachment search is unavailable."}
		response.Truncated = response.Truncated || offset > 0 || len(response.Results) > 0
		return response
	}
	for ; ; offset += docbankmedia.MaxSearchVersions {
		versions, err := reader.Store.DocumentSearchVersions(remoteCtx, reader.Destination, scope, offset)
		if err != nil {
			return fail(err)
		}
		if len(versions) == 0 {
			return response
		}
		vault, err := reader.Client.VaultUID(remoteCtx)
		if err != nil {
			return fail(err)
		}
		report, err := reader.Client.SearchDocuments(remoteCtx, docbankmedia.SearchRequest{
			Query: query, Mode: "lexical", ContentFirst: true, Profile: suppliedTranscriptProfile,
			Limit: limit - len(response.Results), Fence: &docbankmedia.SearchFence{VaultUID: vault, ContentVersionIDs: versions},
		})
		if err != nil {
			return fail(err)
		}
		ids := make([]string, 0, len(report.Results))
		for _, hit := range report.Results {
			ids = append(ids, hit.ContentVersionID)
		}
		occurrences, err := reader.Store.DocumentSearchOccurrences(remoteCtx, reader.Destination, scope, ids)
		if err != nil {
			return fail(err)
		}
		byVersion := make(map[string][]store.DocumentSearchOccurrence)
		for _, o := range occurrences {
			byVersion[o.VersionID] = append(byVersion[o.VersionID], o)
		}
		response.Truncated = response.Truncated || report.Truncated
		for _, hit := range report.Results {
			for _, o := range byVersion[hit.ContentVersionID] {
				if len(response.Results) == limit {
					response.Truncated = true
					return response
				}
				response.Results = append(response.Results, MediaDocumentResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Filename: o.Filename, ContainingTitle: o.ContainingTitle, OccurredAt: o.OccurredAt, Excerpt: hit.Excerpt})
			}
		}
		if len(response.Results) == limit {
			if len(versions) == docbankmedia.MaxSearchVersions {
				next, err := reader.Store.DocumentSearchVersions(remoteCtx, reader.Destination, scope, offset+len(versions))
				if err != nil {
					return fail(err)
				}
				response.Truncated = response.Truncated || len(next) > 0
			}
			return response
		}
		if len(versions) < docbankmedia.MaxSearchVersions {
			return response
		}
	}
}
