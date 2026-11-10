package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
)

func pollingSource(ctx context.Context, identifier string, s *store.Store, state *invocation) (*store.Source, error) {
	rows, err := s.GetSourcesByIdentifierOrDisplayNameContext(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("look up polling source: %w", err)
	}
	var source *store.Source
	for _, row := range rows {
		if !strings.EqualFold(row.Identifier, identifier) && (!row.DisplayName.Valid || !strings.EqualFold(row.DisplayName.String, identifier)) {
			continue
		}
		if row.MergedIntoSourceID != 0 || (row.SourceType != sourceTypeGmail && row.SourceType != sourceTypeIMAP) {
			continue
		}
		if source != nil {
			return nil, fmt.Errorf("polling account %q matches multiple sources", identifier)
		}
		source = row
	}
	if source == nil {
		return nil, fmt.Errorf("polling account %q matches no active Gmail or IMAP source", identifier)
	}
	if source.SourceType == sourceTypeIMAP && len(parseFolderFilter(state.cfg.Sync.IMAPSkipFolders[source.Identifier])) != 0 {
		return nil, fmt.Errorf("polling account %q has an IMAP folder filter", identifier)
	}
	return source, nil
}

func runEmailPoll(ctx context.Context, identifier string, s *store.Store, getOAuthMgr func(context.Context, string) (*oauth.Manager, error), state *invocation) error {
	src, err := pollingSource(ctx, identifier, s, state)
	if err != nil {
		return err
	}
	var incomplete bool
	if src.SourceType == sourceTypeGmail {
		var summaryErr error
		summary, syncErr := runScheduledGmailSyncWithProfile(ctx, identifier, src, s, getOAuthMgr, state, true)
		err = syncErr
		if err == nil {
			incomplete = summary.Errors > 0
			var debt bool
			debt, summaryErr = sync.New(nil, s, nil).HasPendingAcquisition(ctx, src.ID)
			incomplete = incomplete || debt
			if summaryErr == nil {
				_, summaryErr = s.GetLatestCheckpointedSyncContext(ctx, src.ID)
				if errors.Is(summaryErr, store.ErrSyncRunNotFound) {
					summaryErr = nil
				} else if summaryErr == nil {
					incomplete = true
				}
			}
			err = summaryErr
		}
	} else {
		_, complete, syncErr := runScheduledIMAPSyncWithCoverage(ctx, src, s, state)
		err = syncErr
		incomplete = !complete
	}
	if scheduledSyncYielded(ctx) {
		return scheduledSyncYieldResult(ctx, err)
	}
	state.syncFailures.Record(ctx, src.SourceType, src.Identifier, err)
	// Preserve the refresh request after provider work has written data.
	if queueErr := rebuildCacheAfterScheduledSourceRun(context.WithoutCancel(ctx), identifier); queueErr != nil {
		state.logger.Warn("queue analytics refresh", "identifier", identifier, "error", queueErr)
	}
	if err != nil {
		return err
	}
	if incomplete {
		return scheduler.ErrAcquisitionIncomplete
	}
	return nil
}
