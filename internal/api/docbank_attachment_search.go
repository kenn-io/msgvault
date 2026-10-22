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
	fail := func(err error) (MediaAttachmentSearch, error) {
		response.Error = &ErrorResponse{Error: docbankmedia.ErrorCode(err), Message: "Attachment search is unavailable."}
		response.Truncated = response.Truncated || after != nil || len(response.Results) > 0
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
		report, err := reader.Client.SearchDocuments(remoteCtx, docbankmedia.SearchRequest{
			Query: query, Mode: "lexical", ContentFirst: true, Profile: suppliedTranscriptProfile,
			Limit: limit - len(response.Results), Fence: &docbankmedia.SearchFence{VaultUID: vault, ContentVersionIDs: versionIDs},
		})
		if err != nil {
			return fail(err)
		}
		ids := make([]string, 0, len(report.Results))
		for _, hit := range report.Results {
			ids = append(ids, hit.ContentVersionID)
		}
		scope, err = resolve()
		if err != nil {
			return response, &mediaSearchScopeError{err}
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
					return response, nil
				}
				response.Results = append(response.Results, MediaAttachmentResult{MessageID: o.MessageID, ConversationID: o.ConversationID, AttachmentID: o.AttachmentID, Filename: o.Filename, ContainingTitle: o.ContainingTitle, OccurredAt: o.OccurredAt, Excerpt: hit.Excerpt})
			}
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
