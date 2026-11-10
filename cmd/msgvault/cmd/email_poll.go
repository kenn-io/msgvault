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

func validatePollingTargets(ctx context.Context, accounts []config.AccountSchedule, s *store.Store, state *invocation) map[int]pollingTarget {
	targets := make(map[int]pollingTarget)
	seen, names := make(map[int64]bool), make(map[string]bool)
	catalog := pollingCatalog(ctx, accounts, s, state)
	for i, account := range accounts {
		key := strings.ToLower(account.Email)
		if names[key] {
			targets[i] = pollingTarget{err: fmt.Errorf("duplicate polling account %q", account.Email)}
			continue
		}
		names[key] = true
		source, err := catalog[i].source, catalog[i].err
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

func pollingCatalog(ctx context.Context, accounts []config.AccountSchedule, s *store.Store, state *invocation) map[int]pollingTarget {
	result := make(map[int]pollingTarget)
	rows, err := s.ListSourcesContext(ctx, "")
	index := make(map[string][]*store.Source)
	for _, row := range rows {
		keys := map[string]bool{strings.ToLower(row.Identifier): true}
		if row.DisplayName.Valid {
			keys[strings.ToLower(row.DisplayName.String)] = true
		}
		if row.Alias != "" {
			keys[strings.ToLower(row.Alias)] = true
		}
		for key := range keys {
			index[key] = append(index[key], row)
		}
	}
	for i, account := range accounts {
		if err != nil {
			result[i] = pollingTarget{err: fmt.Errorf("look up polling source: %w", err)}
			continue
		}
		source, matchErr := pollingSourceFromRows(account.Email, index[strings.ToLower(account.Email)], state)
		result[i] = pollingTarget{source: source, err: matchErr}
	}
	return result
}

func pollingSourceFromRows(identifier string, rows []*store.Source, state *invocation) (*store.Source, error) {
	var exact, named []*store.Source
	for _, row := range rows {
		if row.MergedIntoSourceID != 0 || (!strings.EqualFold(row.Identifier, identifier) && row.SourceType != sourceTypeGmail && row.SourceType != sourceTypeIMAP) {
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
	if len(candidates) == 0 || (len(candidates) == 1 && candidates[0].SourceType != sourceTypeGmail && candidates[0].SourceType != sourceTypeIMAP) {
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
	target := pollingTarget{err: fmt.Errorf("polling account %q is not configured", identifier)}
	for i, account := range state.pollAccounts {
		if account.Email == identifier {
			target = validatePollingTargets(ctx, state.pollAccounts, s, state)[i]
			break
		}
	}
	if target.err != nil {
		return target.err
	}
	src := target.source
	var err error
	var incomplete bool
	if src.SourceType == sourceTypeGmail {
		summary, syncErr := runScheduledGmailSyncWithProfile(ctx, src.Identifier, src, s, getOAuthMgr, state, true)
		err = syncErr
		if err == nil {
			incomplete, err = gmailAcquisitionIncomplete(ctx, s, src, summary)
		}
	} else {
		_, complete, syncErr := runScheduledIMAPSync(ctx, src, s, state)
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
