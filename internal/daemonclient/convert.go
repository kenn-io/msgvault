package daemonclient

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/cacheops"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func cliCacheStatsFromGenerated(resp *generated.GetCLICacheStatsResponse) *cacheops.CacheStats {
	if resp == nil {
		return &cacheops.CacheStats{Status: cacheops.StatusNoCacheFiles}
	}
	return &cacheops.CacheStats{
		Status:              resp.Status,
		TotalMessages:       int64Value(resp.TotalMessages),
		Sources:             int64Value(resp.Sources),
		UniqueSenders:       int64Value(resp.UniqueSenders),
		UniqueDomains:       int64Value(resp.UniqueDomains),
		MinYear:             copyInt64(resp.MinYear),
		MaxYear:             copyInt64(resp.MaxYear),
		TotalSizeBytes:      int64Value(resp.TotalSizeBytes),
		AttachmentSizeBytes: int64Value(resp.AttachmentSizeBytes),
		LastSyncAt:          copyTime(resp.LastSyncAt),
		LastMessageID:       copyInt64(resp.LastMessageID),
		Warnings:            append([]string(nil), resp.Warnings...),
	}
}

func cliStatsFromGenerated(resp *generated.GetCLIStatsResponse) *CLIStats {
	if resp == nil {
		return &CLIStats{Stats: &store.Stats{}}
	}
	return &CLIStats{
		Stats:            storeStatsFromGenerated(resp.Stats),
		ScopeLabel:       stringValue(resp.ScopeLabel),
		ScopeSourceCount: intValue(resp.ScopeSourceCount),
	}
}

func storeStatsFromGenerated(stats generated.StatsResponse) *store.Stats {
	return &store.Stats{
		MessageCount:       stats.TotalMessages,
		SourceDeletedCount: stats.SourceDeletedMessages,
		ThreadCount:        stats.TotalThreads,
		SourceCount:        stats.TotalAccounts,
		LabelCount:         stats.TotalLabels,
		AttachmentCount:    stats.TotalAttachments,
		DatabaseSize:       stats.DatabaseSizeBytes,
	}
}

func cliInitDBFromGenerated(resp *generated.InitCLIArchiveResponse) *CLIInitDB {
	if resp == nil {
		return &CLIInitDB{Stats: &store.Stats{}}
	}
	return &CLIInitDB{
		Stats:  storeStatsFromGenerated(resp.Stats),
		Notice: stringValue(resp.Notice),
	}
}

func cliAddCalendarPlanFromGenerated(resp *generated.PlanCLIAddCalendarResponse) *CLIAddCalendarPlan {
	if resp == nil {
		return &CLIAddCalendarPlan{}
	}
	return &CLIAddCalendarPlan{
		NeedsScopeEscalation: resp.NeedsScopeEscalation,
		Headline:             stringValue(resp.Headline),
		BodyLines:            append([]string(nil), resp.BodyLines...),
		CancelHint:           stringValue(resp.CancelHint),
		OAuthApp:             stringValue(resp.OauthApp),
		OAuthAppResolved:     boolValue(resp.OauthAppResolved),
		NeedsClientCheck:     boolValue(resp.NeedsClientCheck),
	}
}

func cliEmbeddingsPlanFromGenerated(resp *generated.PlanCLIEmbeddingsResponse) *CLIEmbeddingsPlan {
	if resp == nil {
		return &CLIEmbeddingsPlan{}
	}
	return &CLIEmbeddingsPlan{
		NeedsConfirmation: resp.NeedsConfirmation,
		Prompt:            stringValue(resp.Prompt),
	}
}

func cliDeleteStagedPlanFromGenerated(resp *generated.PlanCLIDeleteStagedResponse) *CLIDeleteStagedPlan {
	if resp == nil {
		return &CLIDeleteStagedPlan{}
	}
	return &CLIDeleteStagedPlan{
		Stdout:                    stringValue(resp.Stdout),
		NeedsExecution:            resp.NeedsExecution,
		NeedsConfirmation:         resp.NeedsConfirmation,
		ConfirmationMode:          stringValue(resp.ConfirmationMode),
		PlannedBatchIDs:           append([]string(nil), resp.PlannedBatchIds...),
		PlanFingerprint:           stringValue(resp.PlanFingerprint),
		ResolvedSourceID:          resp.ResolvedSourceID,
		NeedsScopeEscalation:      boolValue(resp.NeedsScopeEscalation),
		ScopeEscalationHeadline:   stringValue(resp.ScopeEscalationHeadline),
		ScopeEscalationBodyLines:  append([]string(nil), resp.ScopeEscalationBodyLines...),
		ScopeEscalationCancelHint: stringValue(resp.ScopeEscalationCancelHint),
		ScopeEscalationAccount:    stringValue(resp.ScopeEscalationAccount),
		ScopeEscalationOAuthApp:   stringValue(resp.ScopeEscalationOauthApp),
		BlockedError:              stringValue(resp.BlockedError),
		RemoteDeleteEnvVar:        stringValue(resp.RemoteDeleteEnvVar),
	}
}

func cliDeletionManifestResultFromGenerated(resp *generated.CreateCLIDeletionManifestResponse) *CLIDeletionManifestResult {
	if resp == nil {
		return &CLIDeletionManifestResult{}
	}
	return &CLIDeletionManifestResult{
		ID:           resp.ID,
		MessageCount: int(resp.MessageCount),
	}
}

func cliDeletionManifestToGenerated(manifest *deletion.Manifest) generated.CreateCLIDeletionManifestBody {
	if manifest == nil {
		return generated.CreateCLIDeletionManifestBody{}
	}
	out := generated.CreateCLIDeletionManifestBody{
		CreatedAt:   manifest.CreatedAt,
		CreatedBy:   manifest.CreatedBy,
		Description: manifest.Description,
		Filters:     cliDeletionFiltersToGenerated(manifest.Filters),
		GmailIds:    append([]string(nil), manifest.GmailIDs...),
		ID:          manifest.ID,
		Status:      string(manifest.Status),
		Version:     int64(manifest.Version),
	}
	if manifest.Execution != nil {
		out.Execution = cliDeletionExecutionToGenerated(manifest.Execution)
	}
	if manifest.Source != nil {
		out.Source = &generated.SourceReference{
			ID: manifest.Source.ID, Type: manifest.Source.Type, Identifier: manifest.Source.Identifier,
		}
	}
	if manifest.Summary != nil {
		out.Summary = cliDeletionSummaryToGenerated(manifest.Summary)
	}
	return out
}

func cliDeletionFiltersToGenerated(filters deletion.Filters) generated.Filters {
	return generated.Filters{
		Account:       optionalString(filters.Account),
		After:         optionalString(filters.After),
		Before:        optionalString(filters.Before),
		Labels:        append([]string(nil), filters.Labels...),
		Recipients:    append([]string(nil), filters.Recipients...),
		SenderDomains: append([]string(nil), filters.SenderDomains...),
		Senders:       append([]string(nil), filters.Senders...),
	}
}

func cliDeletionExecutionToGenerated(execution *deletion.Execution) *generated.Execution {
	if execution == nil {
		return nil
	}
	return &generated.Execution{
		CompletedAt:        execution.CompletedAt,
		Failed:             int64(execution.Failed),
		FailedIds:          append([]string(nil), execution.FailedIDs...),
		LastProcessedIndex: int64(execution.LastProcessedIndex),
		Method:             string(execution.Method),
		StartedAt:          execution.StartedAt,
		Succeeded:          int64(execution.Succeeded),
	}
}

func cliDeletionSummaryToGenerated(summary *deletion.Summary) *generated.Summary {
	if summary == nil {
		return nil
	}
	topSenders := make([]generated.SenderCount, len(summary.TopSenders))
	for i, sender := range summary.TopSenders {
		topSenders[i] = generated.SenderCount{
			Count:  int64(sender.Count),
			Sender: sender.Sender,
		}
	}
	return &generated.Summary{
		Accounts:       append([]string(nil), summary.Accounts...),
		DateRange:      []string{summary.DateRange[0], summary.DateRange[1]},
		MessageCount:   int64(summary.MessageCount),
		TopSenders:     topSenders,
		TotalSizeBytes: summary.TotalSizeBytes,
	}
}

func cliDeduplicatePlanFromGenerated(resp *generated.PlanCLIDeduplicateResponse) *CLIDeduplicatePlan {
	if resp == nil {
		return &CLIDeduplicatePlan{}
	}
	items := make([]CLIDeduplicatePlanItem, len(resp.Items))
	for i, item := range resp.Items {
		items[i] = CLIDeduplicatePlanItem{
			SourceID:          int64Value(item.SourceID),
			ScopeLabel:        stringValue(item.ScopeLabel),
			ScopeIsCollection: boolValue(item.ScopeIsCollection),
			Stdout:            stringValue(item.Stdout),
			DuplicateMessages: int(int64Value(item.DuplicateMessages)),
			BackfilledCount:   int64Value(item.BackfilledCount),
			PlanFingerprint:   stringValue(item.PlanFingerprint),
			NeedsConfirmation: item.NeedsConfirmation,
		}
	}
	return &CLIDeduplicatePlan{
		PrefixStdout: stringValue(resp.PrefixStdout),
		Items:        items,
		FooterStdout: stringValue(resp.FooterStdout),
	}
}

func cliSearchFromGenerated(resp *generated.SearchCLIResponse) *CLISearch {
	if resp == nil {
		return &CLISearch{Results: []query.MessageSummary{}}
	}
	results := make([]query.MessageSummary, len(resp.Results))
	for i, msg := range resp.Results {
		results[i] = queryMessageSummaryFromGenerated(msg)
	}
	return &CLISearch{
		Results:          results,
		ScopeLabel:       stringValue(resp.ScopeLabel),
		ScopeSourceCount: intValue(resp.ScopeSourceCount),
		IndexBuilt:       boolValue(resp.IndexBuilt),
		IndexedMessages:  int64Value(resp.IndexedMessages),
		IndexState:       stringValue(resp.IndexState),
	}
}

func cliHybridSearchFromGenerated(resp generated.HybridSearchResponse) (*CLIHybridSearch, error) {
	results := make([]CLIHybridSearchResult, len(resp.Results))
	for i, item := range resp.Results {
		result, err := cliHybridSearchResultFromGenerated(item)
		if err != nil {
			return nil, fmt.Errorf("decode CLI hybrid search result %d: %w", i, err)
		}
		results[i] = result
	}
	return &CLIHybridSearch{
		Results: results,
		Generation: CLIHybridGeneration{
			ID:          resp.Generation.ID,
			Model:       resp.Generation.Model,
			Dimension:   int(resp.Generation.Dimension),
			Fingerprint: resp.Generation.Fingerprint,
			State:       resp.Generation.State,
		},
		PoolSaturated:    resp.PoolSaturated,
		ReturnedCount:    int(resp.Returned),
		ScopeLabel:       stringValue(resp.ScopeLabel),
		ScopeSourceCount: intValue(resp.ScopeSourceCount),
		HasMore:          resp.HasMore,
	}, nil
}

func cliHybridSearchResultFromGenerated(item generated.HybridSearchItem) (CLIHybridSearchResult, error) {
	var sentAt time.Time
	if item.SentAt != "" {
		parsed, err := time.Parse(time.RFC3339, item.SentAt)
		if err != nil {
			return CLIHybridSearchResult{}, fmt.Errorf("parse sent_at %q: %w", item.SentAt, err)
		}
		sentAt = parsed
	}
	var deletedAt *time.Time
	if item.DeletedAt != nil && *item.DeletedAt != "" {
		parsed, err := time.Parse(time.RFC3339, *item.DeletedAt)
		if err != nil {
			return CLIHybridSearchResult{}, fmt.Errorf("parse deleted_at: %w", err)
		}
		deletedAt = &parsed
	}
	out := CLIHybridSearchResult{
		Message: query.MessageSummary{
			ID:              item.ID,
			SourceID:        int64Value(item.SourceID),
			SourceMessageID: stringValue(item.SourceMessageID),
			ConversationID:  int64Value(item.ConversationID),
			Subject:         item.Subject,
			Snippet:         item.Snippet,
			FromEmail:       stringValue(item.FromEmail),
			FromName:        stringValue(item.FromName),
			FromPhone:       stringValue(item.FromPhone),
			To:              queryAddressesFromStrings(item.To),
			Cc:              queryAddressesFromStrings(item.Cc),
			Bcc:             queryAddressesFromStrings(item.Bcc),
			SentAt:          sentAt,
			SizeEstimate:    item.SizeBytes,
			HasAttachments:  item.HasAttachments,
			Labels:          item.Labels,
			DeletedAt:       deletedAt,
			MessageType:     stringValue(item.MessageType),
		},
		ID:               item.ID,
		Subject:          item.Subject,
		FromEmail:        stringValue(item.FromEmail),
		SentAt:           sentAt,
		MatchesTruncated: boolValue(item.MatchesTruncated),
	}
	if len(item.Matches) > 0 {
		out.Matches = make([]CLIHybridSearchMatch, len(item.Matches))
		for i, match := range item.Matches {
			out.Matches[i] = CLIHybridSearchMatch{
				CharOffset: optionalIntFromInt64(match.CharOffset),
				Snippet:    match.Snippet,
				Line:       optionalIntFromInt64(match.Line),
				Score:      match.Score,
			}
		}
	}
	if out.FromEmail == "" {
		out.FromEmail = item.From
	}
	if out.Message.FromEmail == "" {
		out.Message.FromEmail = item.From
	}
	if item.Score != nil {
		out.RRFScore = item.Score.Rrf
		out.BM25Score = item.Score.Bm25
		out.VectorScore = item.Score.Vector
		out.SubjectBoosted = boolValue(item.Score.SubjectBoosted)
	}
	return out, nil
}

func queryAddressesFromStrings(addresses []string) []query.Address {
	if addresses == nil {
		return nil
	}
	out := make([]query.Address, len(addresses))
	for i, address := range addresses {
		out[i] = query.Address{Email: address}
	}
	return out
}

func queryMessageSummaryFromGenerated(msg generated.CLIQueryMessageSummary) query.MessageSummary {
	return query.MessageSummary{
		ID:                   msg.ID,
		SourceID:             int64Value(msg.SourceID),
		SourceMessageID:      msg.SourceMessageID,
		ConversationID:       msg.ConversationID,
		SourceConversationID: msg.SourceConversationID,
		Subject:              msg.Subject,
		Snippet:              msg.Snippet,
		FromEmail:            msg.FromEmail,
		FromName:             msg.FromName,
		FromPhone:            stringValue(msg.FromPhone),
		To:                   queryAddressesFromGenerated(msg.To),
		Cc:                   queryAddressesFromGenerated(msg.Cc),
		Bcc:                  queryAddressesFromGenerated(msg.Bcc),
		SentAt:               msg.SentAt,
		SizeEstimate:         msg.SizeEstimate,
		HasAttachments:       msg.HasAttachments,
		AttachmentCount:      int(msg.AttachmentCount),
		Labels:               msg.Labels,
		DeletedAt:            msg.DeletedAt,
		MessageType:          stringValue(msg.MessageType),
		ConversationTitle:    stringValue(msg.ConversationTitle),
		BodyText:             stringValue(msg.BodyText),
	}
}

func queryAddressesFromGenerated(addresses []generated.Address) []query.Address {
	if addresses == nil {
		return nil
	}
	out := make([]query.Address, len(addresses))
	for i, address := range addresses {
		out[i] = query.Address{
			Email: address.Email,
			Name:  address.Name,
		}
	}
	return out
}

func cliMessageDetailFromGenerated(resp *generated.GetCLIMessageResponse) *query.MessageDetail {
	if resp == nil {
		return &query.MessageDetail{}
	}
	return &query.MessageDetail{
		ID:                   resp.ID,
		SourceMessageID:      resp.SourceMessageID,
		ConversationID:       resp.ConversationID,
		SourceConversationID: resp.SourceConversationID,
		Subject:              resp.Subject,
		MessageType:          stringValue(resp.MessageType),
		Snippet:              resp.Snippet,
		SentAt:               resp.SentAt,
		ReceivedAt:           resp.ReceivedAt,
		DeletedAt:            resp.DeletedAt,
		SizeEstimate:         resp.SizeEstimate,
		HasAttachments:       resp.HasAttachments,
		From:                 cliMessageAddressesFromGenerated(resp.From),
		To:                   cliMessageAddressesFromGenerated(resp.To),
		Cc:                   cliMessageAddressesFromGenerated(resp.Cc),
		Bcc:                  cliMessageAddressesFromGenerated(resp.Bcc),
		Labels:               resp.Labels,
		Attachments:          cliMessageAttachmentsFromGenerated(resp.Attachments),
		BodyText:             resp.BodyText,
		BodyHTML:             resp.BodyHTML,
	}
}

func cliMessageAddressesFromGenerated(addresses []generated.CliMessageAddress) []query.Address {
	if addresses == nil {
		return nil
	}
	out := make([]query.Address, len(addresses))
	for i, address := range addresses {
		out[i] = query.Address{
			Email: address.Email,
			Name:  address.Name,
		}
	}
	return out
}

func cliMessageAttachmentsFromGenerated(
	attachments []generated.CliMessageAttachment,
) []query.AttachmentInfo {
	if attachments == nil {
		return nil
	}
	out := make([]query.AttachmentInfo, len(attachments))
	for i, attachment := range attachments {
		out[i] = query.AttachmentInfo{
			ID:          attachment.ID,
			Filename:    attachment.Filename,
			MimeType:    attachment.MimeType,
			Size:        attachment.Size,
			ContentHash: attachment.ContentHash,
			URL:         stringValue(attachment.URL),
		}
	}
	return out
}

func cliAccountsFromGenerated(resp *generated.ListCLIAccountsResponse) []CLIAccount {
	if resp == nil || resp.Accounts == nil {
		return []CLIAccount{}
	}
	out := make([]CLIAccount, len(resp.Accounts))
	for i, account := range resp.Accounts {
		out[i] = CLIAccount{
			ID:                 account.ID,
			Email:              account.Email,
			Type:               account.Type,
			DisplayName:        account.DisplayName,
			OAuthApp:           stringValue(account.OauthApp),
			MessageCount:       account.MessageCount,
			SourceDeletedCount: account.SourceDeletedCount,
			LastSync:           account.LastSync,
		}
	}
	return out
}

func cliCollectionsFromGenerated(resp *generated.ListCLICollectionsResponse) []CLICollection {
	if resp == nil || resp.Collections == nil {
		return []CLICollection{}
	}
	out := make([]CLICollection, len(resp.Collections))
	for i, collection := range resp.Collections {
		out[i] = cliCollectionFromGenerated(collection)
	}
	return out
}

func cliCollectionFromGenerated(collection generated.CliCollectionResponse) CLICollection {
	return CLICollection{
		ID:                 collection.ID,
		Name:               collection.Name,
		Description:        stringValue(collection.Description),
		CreatedAt:          collection.CreatedAt,
		SourceIDs:          collection.SourceIds,
		MessageCount:       collection.MessageCount,
		SourceDeletedCount: collection.SourceDeletedCount,
		Sources:            cliCollectionSourcesFromGenerated(collection.Sources),
	}
}

func cliCollectionSourcesFromGenerated(sources []generated.CliCollectionSourceResponse) []CLICollectionSource {
	if sources == nil {
		return nil
	}
	out := make([]CLICollectionSource, len(sources))
	for i, source := range sources {
		out[i] = CLICollectionSource{
			ID:          source.ID,
			Identifier:  source.Identifier,
			DisplayName: stringValue(source.DisplayName),
		}
	}
	return out
}

func cliIdentitiesFromGenerated(resp *generated.ListCLIIdentitiesResponse) []CLIIdentityRow {
	if resp == nil || resp.Rows == nil {
		return []CLIIdentityRow{}
	}
	out := make([]CLIIdentityRow, len(resp.Rows))
	for i, row := range resp.Rows {
		out[i] = CLIIdentityRow{
			Account:     row.Account,
			SourceID:    row.SourceID,
			SourceType:  row.SourceType,
			Identifier:  stringValue(row.Identifier),
			Signals:     row.Signals,
			ConfirmedAt: row.ConfirmedAt,
			None:        boolValue(row.None),
		}
	}
	return out
}

func cliCollectionMutationResultFromGenerated(
	resp *generated.MutationResult,
) *CLICollectionMutationResult {
	if resp == nil {
		return &CLICollectionMutationResult{}
	}
	return &CLICollectionMutationResult{
		Name:        resp.Name,
		SourceCount: intValue(resp.SourceCount),
	}
}

func cliIdentityAddResultFromGenerated(resp *generated.AddResult) *CLIIdentityAddResult {
	if resp == nil {
		return &CLIIdentityAddResult{}
	}
	return &CLIIdentityAddResult{
		Account:    resp.Account,
		Identifier: resp.Identifier,
		Signal:     resp.Signal,
		Outcome:    resp.Outcome,
	}
}

func cliIdentityRemoveResultFromGenerated(resp *generated.RemoveResult) *CLIIdentityRemoveResult {
	if resp == nil {
		return &CLIIdentityRemoveResult{}
	}
	return &CLIIdentityRemoveResult{
		Account:    resp.Account,
		Identifier: resp.Identifier,
		Removed:    resp.Removed,
		NoIdentity: boolValue(resp.NoIdentity),
	}
}

func cliIdentityDiscoverBodyFromRequest(
	req identityops.DiscoverRequest,
) *generated.DiscoverCLIIdentitiesBody {
	body := &generated.DiscoverCLIIdentitiesBody{
		Account:  optionalString(req.Account),
		Apply:    optionalBool(req.Apply),
		Confirm:  append([]string(nil), req.Confirm...),
		Provider: optionalBool(req.Provider),
		SourceID: optionalCLIIdentitySourceID(req.SourceID, req.SourceID != 0),
	}
	if req.PageSize != 0 {
		pageSize := int64(req.PageSize)
		body.PageSize = &pageSize
	}
	return body
}

func cliIdentityDiscoverEventFromGenerated(event generated.DiscoverEvent) identityops.DiscoverEvent {
	out := identityops.DiscoverEvent{Type: string(event.Type)}
	if event.ErrorData != nil {
		out.Error = &identityops.DiscoverError{
			Code:    event.ErrorData.Code,
			Message: event.ErrorData.Message,
		}
	}
	if event.Progress != nil {
		out.Progress = &identityops.DiscoverProgress{
			Done:       event.Progress.Done,
			Total:      event.Progress.Total,
			Candidates: int(event.Progress.Candidates),
		}
	}
	if event.Result != nil {
		out.Result = cliIdentityDiscoverResultFromGenerated(event.Result)
	}
	return out
}

func cliIdentityDiscoverResultFromGenerated(result *generated.DiscoverResult) *identityops.DiscoverResult {
	if result == nil {
		return nil
	}
	candidates := make([]identityops.Candidate, len(result.Candidates))
	for i, candidate := range result.Candidates {
		candidates[i] = identityops.Candidate{
			Identifier:           candidate.Identifier,
			NormalizedIdentifier: candidate.NormalizedIdentifier,
			Classification:       string(candidate.Classification),
			AlreadyConfirmed:     candidate.AlreadyConfirmed,
			Signals:              append([]string{}, candidate.Signals...),
			ProviderStates:       append([]string{}, candidate.ProviderStates...),
			SentMessageCount:     candidate.SentMessageCount,
			ReceivedMessageCount: candidate.ReceivedMessageCount,
			FirstSeenAt:          candidate.FirstSeenAt,
			LastSeenAt:           candidate.LastSeenAt,
		}
	}
	rejected := make([]identityops.RejectedCandidate, len(result.Rejected))
	for i, candidate := range result.Rejected {
		rejected[i] = identityops.RejectedCandidate{
			Identifier: candidate.Identifier,
			Reason:     candidate.Reason,
		}
	}
	applied := make([]store.IdentityConfirmationOutcome, len(result.Applied))
	for i, outcome := range result.Applied {
		applied[i] = store.IdentityConfirmationOutcome{
			Identifier: outcome.Identifier,
			Added:      outcome.Added,
			Signals:    append([]string{}, outcome.Signals...),
		}
	}
	return &identityops.DiscoverResult{
		Account:         result.Account,
		SourceID:        result.SourceID,
		SourceType:      result.SourceType,
		ScannedMessages: result.ScannedMessages,
		Candidates:      candidates,
		Rejected:        rejected,
		Applied:         applied,
	}
}

func cliIdentityImportBodyFromRequest(
	req identityops.ImportRequest,
) *generated.ImportCLIIdentitiesBody {
	entries := make([]generated.ImportEntry, len(req.Entries))
	for i, entry := range req.Entries {
		entries[i] = generated.ImportEntry{
			Identifier: entry.Identifier,
			State:      optionalString(entry.State),
		}
	}
	return &generated.ImportCLIIdentitiesBody{
		Account:  optionalString(req.Account),
		Apply:    optionalBool(req.Apply),
		Entries:  entries,
		Signal:   optionalString(req.Signal),
		SourceID: optionalCLIIdentitySourceID(req.SourceID, req.SourceID != 0),
	}
}

func cliIdentityImportResultFromGenerated(result *generated.ImportResult) *identityops.ImportResult {
	if result == nil {
		return &identityops.ImportResult{}
	}
	candidates := make([]identityops.Candidate, len(result.Candidates))
	for i, candidate := range result.Candidates {
		candidates[i] = cliIdentityCandidateFromGenerated(candidate)
	}
	applied := make([]store.IdentityConfirmationOutcome, len(result.Applied))
	for i, outcome := range result.Applied {
		applied[i] = store.IdentityConfirmationOutcome{
			Identifier: outcome.Identifier,
			Added:      outcome.Added,
			Signals:    append([]string{}, outcome.Signals...),
		}
	}
	return &identityops.ImportResult{
		Account:    result.Account,
		SourceID:   result.SourceID,
		Signal:     result.Signal,
		Candidates: candidates,
		Applied:    applied,
	}
}

func cliIdentityCandidateFromGenerated(candidate generated.Candidate) identityops.Candidate {
	return identityops.Candidate{
		Identifier:           candidate.Identifier,
		NormalizedIdentifier: candidate.NormalizedIdentifier,
		Classification:       string(candidate.Classification),
		AlreadyConfirmed:     candidate.AlreadyConfirmed,
		Signals:              append([]string{}, candidate.Signals...),
		ProviderStates:       append([]string{}, candidate.ProviderStates...),
		SentMessageCount:     candidate.SentMessageCount,
		ReceivedMessageCount: candidate.ReceivedMessageCount,
		FirstSeenAt:          candidate.FirstSeenAt,
		LastSeenAt:           candidate.LastSeenAt,
	}
}

func cliDeleteDedupedPlanBodyFromRequest(req CLIDeleteDedupedRequest) *generated.PlanCLIDeleteDedupedBody {
	allHidden := req.AllHidden
	return &generated.PlanCLIDeleteDedupedBody{
		AllHidden: &allHidden,
		BatchIds:  append([]string(nil), req.BatchIDs...),
	}
}

func cliDeleteDedupedExecuteBodyFromRequest(req CLIDeleteDedupedRequest) *generated.ExecuteCLIDeleteDedupedBody {
	allHidden := req.AllHidden
	noBackup := req.NoBackup
	body := &generated.ExecuteCLIDeleteDedupedBody{
		AllHidden:       &allHidden,
		BatchIds:        append([]string(nil), req.BatchIDs...),
		ExpectedBatches: cliDeleteDedupedBatchesToGenerated(req.ExpectedBatches),
		NoBackup:        &noBackup,
	}
	if req.ExpectedBatchCount != nil {
		body.ExpectedBatchCount = *req.ExpectedBatchCount
	}
	if req.ExpectedTotal != nil {
		body.ExpectedTotal = *req.ExpectedTotal
	}
	return body
}

func cliDeleteDedupedBatchesToGenerated(
	batches []CLIDeleteDedupedBatch,
) []generated.CliDeleteDedupedBatchResponse {
	if batches == nil {
		return nil
	}
	out := make([]generated.CliDeleteDedupedBatchResponse, len(batches))
	for i, batch := range batches {
		out[i] = generated.CliDeleteDedupedBatchResponse{
			ID:    batch.ID,
			Count: batch.Count,
		}
	}
	return out
}

func cliDeleteDedupedPlanFromGenerated(
	resp *generated.PlanCLIDeleteDedupedResponse,
) *CLIDeleteDedupedPlan {
	if resp == nil {
		return &CLIDeleteDedupedPlan{}
	}
	batches := make([]CLIDeleteDedupedBatch, len(resp.Batches))
	for i, batch := range resp.Batches {
		batches[i] = CLIDeleteDedupedBatch{
			ID:    batch.ID,
			Count: batch.Count,
		}
	}
	return &CLIDeleteDedupedPlan{
		Total:      resp.Total,
		BatchCount: resp.BatchCount,
		Batches:    batches,
	}
}

func cliDeleteDedupedExecuteFromGenerated(
	resp *generated.ExecuteCLIDeleteDedupedResponse,
) *CLIDeleteDedupedExecute {
	if resp == nil {
		return &CLIDeleteDedupedExecute{}
	}
	return &CLIDeleteDedupedExecute{
		Deleted:    resp.Deleted,
		BatchCount: resp.BatchCount,
		BackupPath: stringValue(resp.BackupPath),
	}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalBool(value bool) *bool {
	if !value {
		return nil
	}
	return &value
}

func optionalFloat32(value float64) *float32 {
	if value == 0 {
		return nil
	}
	v := float32(value)
	return &v
}

func optionalIntFromInt64(value *int64) *int {
	if value == nil {
		return nil
	}
	v := int(*value)
	return &v
}

func optionalPositiveInt64(value int) *int64 {
	if value <= 0 {
		return nil
	}
	out := int64(value)
	return &out
}

func optionalMessageTypes(values []string) *string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	out := strings.Join(filtered, ",")
	return &out
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boolValue(value *bool) bool {
	if value == nil {
		return false
	}
	return *value
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func copyInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func intValue(value *int64) int {
	if value == nil {
		return 0
	}
	return int(*value)
}
