package daemonclient

import (
	"context"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// SearchDocuments runs dedicated extracted-document search through the daemon.
func (c *Client) SearchDocuments(
	ctx context.Context,
	request store.DocumentSearchRequest,
) (store.DocumentSearchResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchDocumentsResp, error) {
		return client.SearchDocumentsWithResponse(ctx, &generated.SearchDocumentsRequestOptions{
			Query: &generated.SearchDocumentsQuery{
				Q: request.Query, SourceID: request.SourceIDs, MessageType: request.MessageTypes,
				AttachmentID:  optionalPositiveInt64Value(request.AttachmentID),
				MessageID:     optionalPositiveInt64Value(request.MessageID),
				PersonID:      optionalPositiveInt64Value(request.PersonID),
				ParticipantID: optionalPositiveInt64Value(request.ParticipantID),
				Direction:     documentDirectionStrings(request.Directions),
				After:         optionalTimeRFC3339(request.After),
				Before:        optionalTimeRFC3339(request.Before),
				Limit:         optionalPositiveInt64(request.PageSize), Cursor: optionalString(request.Cursor),
			},
		})
	})
	if err != nil {
		return store.DocumentSearchResponse{}, err
	}
	return documentSearchFromGenerated(response.JSON200), nil
}

// GetDocumentIndexStatus reads scoped document coverage through the selected daemon.
func (c *Client) GetDocumentIndexStatus(
	ctx context.Context,
	request store.DocumentIndexStatusRequest,
) (store.DocumentIndexStatusResponse, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetDocumentIndexStatusResp, error) {
		return client.GetDocumentIndexStatusWithResponse(ctx, &generated.GetDocumentIndexStatusRequestOptions{
			Query: &generated.GetDocumentIndexStatusQuery{
				ProfileID: request.ProfileID, InputKey: request.ExtractionInputKey,
				MediaType: request.AllowedMediaTypes, MessageType: request.AllowedMessageTypes,
			},
		})
	})
	if err != nil {
		return store.DocumentIndexStatusResponse{}, err
	}
	return documentIndexStatusFromGenerated(response.JSON200), nil
}

func optionalPositiveInt64Value(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func documentSearchFromGenerated(response *generated.DocumentSearchResponse) store.DocumentSearchResponse {
	if response == nil {
		return store.DocumentSearchResponse{Results: []store.DocumentSearchResult{}}
	}
	result := store.DocumentSearchResponse{
		Revision: response.Revision,
		Results:  make([]store.DocumentSearchResult, len(response.Results)),
	}
	if response.Truncated != nil {
		result.Truncated = *response.Truncated
	}
	if response.NextCursor != nil {
		result.NextCursor = *response.NextCursor
	}
	for index, row := range response.Results {
		result.Results[index] = store.DocumentSearchResult{
			AttachmentID: row.AttachmentID, MessageID: row.MessageID,
			ConversationID: row.ConversationID, SourceID: row.SourceID,
			SourceMessageID: stringValue(row.SourceMessageID), OccurredAt: row.OccurredAt,
			OccurrenceKey: row.OccurrenceKey, SourcePartKey: stringValue(row.SourcePartKey),
			Filename: stringValue(row.Filename), ContainingTitle: stringValue(row.ContainingTitle),
			MIMEType: stringValue(row.MimeType), CanonicalBlobHash: row.CanonicalBlobHash,
			OtherLiveCopies: int(row.OtherLiveCopies), ChunkKey: row.ChunkKey,
			ChunkOrdinal: int(row.ChunkOrdinal), HeadingPath: row.HeadingPath,
			FirstUnitIndex: int(row.FirstUnitIndex), LastUnitIndex: int(row.LastUnitIndex),
			Excerpt: row.Excerpt, HighlightStart: int(row.HighlightStart), HighlightEnd: int(row.HighlightEnd),
			ProfileID: row.ProfileID, ExtractionID: row.ExtractionID,
			Provider: row.Provider, Model: row.Model, MatchedSignals: row.MatchedSignals,
			Truncated: row.Truncated, Rank: int(row.Rank),
		}
		if row.PersonProvenance != nil {
			result.Results[index].PersonProvenance = &personscope.Provenance{
				ParticipantIDs: row.PersonProvenance.ParticipantIds,
				Roles:          make([]personscope.Role, len(row.PersonProvenance.Roles)),
				Directions:     make([]personscope.Direction, len(row.PersonProvenance.Directions)),
			}
			for i, role := range row.PersonProvenance.Roles {
				result.Results[index].PersonProvenance.Roles[i] = personscope.Role(role)
			}
			for i, direction := range row.PersonProvenance.Directions {
				result.Results[index].PersonProvenance.Directions[i] = personscope.Direction(direction)
			}
		}
	}
	return result
}

func documentDirectionStrings(directions []personscope.Direction) []string {
	if len(directions) == 0 {
		return nil
	}
	result := make([]string, len(directions))
	for i, direction := range directions {
		result[i] = string(direction)
	}
	return result
}

func documentIndexStatusFromGenerated(
	response *generated.DocumentIndexStatusResponse,
) store.DocumentIndexStatusResponse {
	if response == nil {
		return store.DocumentIndexStatusResponse{}
	}
	generatedStatus := response.Status
	result := store.DocumentIndexStatusResponse{Status: store.DocumentIndexStatus{
		ProfileExists: generatedStatus.ProfileExists, ProfileEnabled: generatedStatus.ProfileEnabled,
		ExactConsent: generatedStatus.ExactConsent, ExtractionAttempts: generatedStatus.ExtractionAttempts,
		SuccessfulAttempts: generatedStatus.SuccessfulAttempts, FailedAttempts: generatedStatus.FailedAttempts,
		ProviderRequests: generatedStatus.ProviderRequests, ProviderRetries: generatedStatus.ProviderRetries,
		ProviderLatencyMillis:      generatedStatus.ProviderLatencyMillis,
		AverageProviderLatencyMS:   generatedStatus.AverageProviderLatencyMillis,
		VerifiedUploadBytes:        generatedStatus.VerifiedUploadBytes,
		ProcessedProviderUnits:     generatedStatus.ProcessedProviderUnits,
		ReportedProviderBytes:      generatedStatus.ReportedProviderBytes,
		MissingProviderByteReports: generatedStatus.MissingProviderByteReports,
		EligibleOccurrences:        generatedStatus.EligibleOccurrences, EligibleOwners: generatedStatus.EligibleOwners,
		EligibleBytes:             generatedStatus.EligibleBytes,
		UnknownRoleOccurrences:    generatedStatus.UnknownRoleOccurrences,
		IneligibleRoleOccurrences: generatedStatus.IneligibleRoleOccurrences,
		ReadyOwners:               generatedStatus.ReadyOwners, StagingOwners: generatedStatus.StagingOwners,
		RetryOwners: generatedStatus.RetryOwners, TerminalOwners: generatedStatus.TerminalOwners,
		MissingOwners:         generatedStatus.MissingOwners,
		StoredPlaintextChunks: generatedStatus.StoredPlaintextChunks,
	}}
	if response.ActiveRebuild != nil {
		result.ActiveRebuild = &store.DocumentIndexRebuildStatus{
			SnapshotOwners:  response.ActiveRebuild.SnapshotOwners,
			RemainingOwners: response.ActiveRebuild.RemainingOwners,
		}
	}
	return result
}
