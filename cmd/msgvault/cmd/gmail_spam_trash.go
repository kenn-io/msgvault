package cmd

import (
	"context"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// gmailSkipsSpamTrash reports whether an [[accounts]] entry that resolves to
// src sets gmail_skip_spam_trash. Entries resolve the way scheduled sync
// resolves them: by identifier, display name, or alias, ignoring case.
func gmailSkipsSpamTrash(ctx context.Context, cfg *config.Config, s *store.Store, src *store.Source) (bool, error) {
	for _, account := range cfg.Accounts {
		if !account.GmailSkipSpamTrash {
			continue
		}
		matches, _, err := findScheduledSyncSources(ctx, s, account.Email)
		if err != nil {
			return false, fmt.Errorf("resolve account %q: %w", account.Email, err)
		}
		if slices.ContainsFunc(matches, func(match *store.Source) bool { return match.ID == src.ID }) {
			return true, nil
		}
	}
	return false, nil
}
