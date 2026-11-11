package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
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
	RequestedMode string                      `json:"requested_mode"`
	ActualMode    string                      `json:"actual_mode"`
	Coverage      docbankmedia.SearchCoverage `json:"coverage" doc:"Coverage of delivered mirrored versions, excluding files awaiting upload"`
	Partial       bool                        `json:"partial"`
	Degradations  []string                    `json:"degradations"`
	Results       []MediaAttachmentResult     `json:"results"`
	Truncated     bool                        `json:"truncated"`
	Error         *ErrorResponse              `json:"error,omitempty"`
}

// SearchAttachments searches delivered attachment versions in bounded batches.
// Batch ranks are interleaved and mapped only to currently visible messages.
func (reader *DocbankAttachmentReader) SearchAttachments(ctx, remoteCtx context.Context, query, mode string, limit int, scope *personscope.Scope, resolve func() (*personscope.Scope, error)) (MediaAttachmentSearch, error) {
	response := MediaAttachmentSearch{Results: []MediaAttachmentResult{}, RequestedMode: mode, ActualMode: "unavailable", Coverage: docbankmedia.SearchCoverage{State: "unknown"}, Degradations: []string{}}
	fail := func(err error) (MediaAttachmentSearch, error) {
		response.Error = attachmentSearchError(err)
		return response, err
	}
	versions, err := reader.Store.DocumentSearchVersions(ctx, reader.Destination, scope)
	if err != nil {
		response.Error = &ErrorResponse{Error: "server_error", Message: "Attachment search is unavailable."}
		return response, err
	}
	profile := reader.SearchProfile
	if mode == mediaSearchModeLexical {
		profile = suppliedTranscriptProfile
	}
	actual := mode
	if profile == "" {
		// Lexical spans profiles but still requires a registered profile name.
		profile = suppliedTranscriptProfile
		if mode == "semantic" {
			return fail(errMediaSearchMode)
		}
		if mode == "hybrid" {
			actual = mediaSearchModeLexical
			response.Partial = true
			response.Degradations = append(response.Degradations, "semantic_unconfigured")
		}
	}
	request := docbankmedia.SearchRequest{Query: query, Mode: actual, ContentFirst: true, Profile: profile, Limit: limit}
	if len(versions) == 0 {
		response.Partial = response.Partial || mode != mediaSearchModeLexical
		return response, nil
	}
	fence, err := reader.Client.VaultFence(remoteCtx, versions)
	if err != nil {
		response.Error = attachmentSearchError(err)
		return response, err
	}
	request.Fence = &fence
	var hits []docbankmedia.SearchHit
	batchSucceeded := false
	for chunk := range slices.Chunk(versions, docbankmedia.MaxSearchVersions) {
		fence.ContentVersionIDs = chunk
		report, err := reader.Client.SearchDocuments(remoteCtx, request)
		if err != nil {
			if !batchSucceeded {
				response.Error = attachmentSearchError(err)
				return response, err
			}
			slog.WarnContext(ctx, "search mirrored attachments", "code", attachmentSearchError(err).Error)
			response.Truncated = true
			response.Partial = true
			response.Coverage.State = "incomplete"
			break
		}
		if !batchSucceeded {
			response.ActualMode = report.ActualMode
			response.Coverage.State = report.Coverage.State
		} else {
			if response.ActualMode != report.ActualMode {
				response.ActualMode = mediaSearchModeLexical
			}
			if response.Coverage.State != report.Coverage.State {
				response.Coverage.State = "incomplete"
			}
		}
		response.Coverage.BindingRequired = response.Coverage.BindingRequired || report.Coverage.BindingRequired
		response.Coverage.ScopedDocuments += report.Coverage.ScopedDocuments
		response.Coverage.CompleteDocuments += report.Coverage.CompleteDocuments
		response.Degradations = append(response.Degradations, report.Degradations...)
		response.Partial = response.Partial || mode != report.ActualMode || len(response.Degradations) > 0 ||
			(report.ActualMode != mediaSearchModeLexical && report.Coverage.State != "complete")
		batchSucceeded = true
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
	occurrences, err := reader.Store.DocumentSearchOccurrences(ctx, reader.Destination, scope, ids, limit, 0)
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
	if remote, ok := errors.AsType[*docbankmedia.HTTPError](err); ok {
		switch remote.Reason {
		case "processing_profile_unavailable":
			failure.Error = remote.Reason
		case "processing_consent_required":
			return &ErrorResponse{Error: remote.Reason, Message: "Attachment search is unavailable."}
		}
	}
	if failure.Error == "media_search_mode_unavailable" || failure.Error == "processing_profile_unavailable" {
		failure.Message = "Set integrations.docbank.attachment_search_profile to an existing Docbank processing profile with embeddings for semantic attachment search."
		return &failure
	}
	if failure.Error == "invalid_media_search" {
		return &ErrorResponse{Error: failure.Error, Message: "Attachment search rejected this query."}
	}
	return &ErrorResponse{Error: docbankmedia.ErrorCode(err), Message: "Attachment search is unavailable."}
}
