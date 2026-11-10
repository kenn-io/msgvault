package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
)

type pollingTarget struct {
	source *store.Source
	err    error
}

func validatePollingTargets(ctx context.Context, cfg *config.Config, s *store.Store, state *invocation) map[int]pollingTarget {
	targets := make(map[int]pollingTarget)
	seen := make(map[int64]bool)
	names := make(map[string]bool)
	for i, account := range cfg.Accounts {
		if !account.Enabled || account.PollInterval == 0 {
			continue
		}
		if err := config.ValidatePollingAccounts([]config.AccountSchedule{account}); err != nil {
			targets[i] = pollingTarget{err: err}
			continue
		}
		key := strings.ToLower(account.Email)
		if names[key] {
			targets[i] = pollingTarget{err: fmt.Errorf("duplicate polling account %q", account.Email)}
			continue
		}
		names[key] = true
		source, err := pollingSource(ctx, account.Email, s, state)
		if err != nil {
			targets[i] = pollingTarget{err: fmt.Errorf("polling account %q: %w", account.Email, err)}
			continue
		}
		if seen[source.ID] {
			targets[i] = pollingTarget{err: fmt.Errorf("polling account %q: source already has a polling account", account.Email)}
			continue
		}
		targets[i] = pollingTarget{source: source}
		seen[source.ID] = true
	}
	return targets
}

func pollingSource(ctx context.Context, identifier string, s *store.Store, state *invocation) (*store.Source, error) {
	rows, err := s.GetSourcesByIdentifierOrDisplayNameContext(ctx, identifier)
	if err != nil {
		return nil, fmt.Errorf("look up polling source: %w", err)
	}
	var exact, named []*store.Source
	for _, row := range rows {
		if row.MergedIntoSourceID != 0 || (row.SourceType != sourceTypeGmail && row.SourceType != sourceTypeIMAP) {
			continue
		}
		if strings.EqualFold(row.Identifier, identifier) {
			exact = append(exact, row)
		} else {
			named = append(named, row)
		}
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = named
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("polling account %q matches no active Gmail or IMAP source", identifier)
	}
	if len(candidates) > 1 {
		return nil, fmt.Errorf("polling account %q matches multiple sources", identifier)
	}
	source := candidates[0]
	if source.SourceType == sourceTypeIMAP {
		if len(parseFolderFilter(state.cfg.Sync.IMAPSkipFolders[source.Identifier])) != 0 {
			return nil, fmt.Errorf("polling account %q has an IMAP folder filter", identifier)
		}
		if source.SyncConfig.Valid && source.SyncConfig.String != "" {
			cfg, err := imap.ConfigFromJSON(source.SyncConfig.String)
			if err != nil {
				return nil, fmt.Errorf("polling account %q: %w", identifier, err)
			}
			if len(cfg.Folders) != 0 {
				return nil, fmt.Errorf("polling account %q has an IMAP folder filter", identifier)
			}
		}
	}
	return source, nil
}

func runEmailPoll(ctx context.Context, identifier string, s *store.Store, getOAuthMgr func(context.Context, string) (*oauth.Manager, error), state *invocation) error {
	var src *store.Source
	var err error
	for i, account := range state.cfg.Accounts {
		if account.Enabled && account.PollInterval != 0 && strings.EqualFold(account.Email, identifier) {
			target := validatePollingTargets(ctx, state.cfg, s, state)[i]
			src, err = target.source, target.err
			break
		}
	}
	if src == nil && err == nil {
		src, err = pollingSource(ctx, identifier, s, state)
	}
	if err != nil {
		return err
	}
	var incomplete bool
	if src.SourceType == sourceTypeGmail {
		summary, syncErr := runScheduledGmailSyncWithProfile(ctx, src.Identifier, src, s, getOAuthMgr, state, true)
		err = syncErr
		if err == nil {
			incomplete, err = gmailAcquisitionIncomplete(ctx, s, src, summary)
		}
	} else {
		_, complete, syncErr := runScheduledIMAPSyncWithCoverage(ctx, src, s, state)
		err = syncErr
		incomplete = !complete
	}
	if scheduledSyncYielded(ctx) {
		return scheduledSyncYieldResult(ctx, err)
	}
	if gmail.IsQuotaDeferred(err) {
		incomplete = true
		err = nil
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

func gmailAcquisitionIncomplete(ctx context.Context, s *store.Store, src *store.Source, summary *gmail.SyncSummary) (bool, error) {
	debt, err := sync.New(nil, s, nil).HasPendingAcquisition(ctx, src.ID)
	if err != nil {
		return false, err
	}
	_, err = s.GetLatestCheckpointedSyncContext(ctx, src.ID)
	checkpoint := err == nil
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return false, err
	}
	return summary.Errors > 0 || debt || checkpoint, nil
}
