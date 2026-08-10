package cmd

import (
	"fmt"

	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// resolveEmbedScopeSourceIDs folds the account dimension of the embedding
// build scope into cfg.Vector.Embed.Scope.SourceIDs, so that every consumer
// of the resolved config — generation fingerprints, coverage counts, the
// embed worker's scan, and the vector backends' activation gates — sees the
// same account restriction.
//
// The --account/--collection flags, when present, REPLACE the configured
// [vector.embed.scope] accounts for the run (explicit operator intent);
// configured message_types always apply, since there is no CLI override for
// them. Unknown identifiers are a hard error: a silently skipped account
// would quietly widen the embedded corpus beyond what the operator asked
// for.
func resolveEmbedScopeSourceIDs(s *store.Store) error {
	var ids []int64
	switch {
	case len(embedAccounts) > 0 || len(embedCollections) > 0:
		resolved, err := resolveEmbedScopeFlags(s)
		if err != nil {
			return err
		}
		ids = resolved
	case len(cfg.Vector.Embed.Scope.Accounts) > 0:
		resolved, err := collectionops.ResolveAccountList(s, cfg.Vector.Embed.Scope.Accounts)
		if err != nil {
			return fmt.Errorf("[vector.embed.scope] accounts: %w", err)
		}
		ids = resolved
	default:
		return nil
	}
	// Normalize through NewBuildScope so overlapping accounts/collections
	// dedupe and the stored scope matches what the fingerprint derives.
	cfg.Vector.Embed.Scope.SourceIDs = vector.NewBuildScope(nil, ids).SourceIDs
	return nil
}

// resolveEmbedScopeFlags resolves the --account and --collection flag values
// to their union of source IDs.
func resolveEmbedScopeFlags(s *store.Store) ([]int64, error) {
	var ids []int64
	if len(embedAccounts) > 0 {
		accountIDs, err := collectionops.ResolveAccountList(s, embedAccounts)
		if err != nil {
			return nil, fmt.Errorf("--account: %w", err)
		}
		ids = append(ids, accountIDs...)
	}
	for _, name := range embedCollections {
		scope, err := ResolveCollectionFlag(s, name)
		if err != nil {
			return nil, fmt.Errorf("--collection: %w", err)
		}
		ids = append(ids, scope.SourceIDs()...)
	}
	return ids, nil
}

// ensureEmbedScopeResolved resolves the configured embed-scope accounts for
// embeddings management commands (list/activate) that compute coverage or
// compare generation fingerprints against short-lived stores of their own.
// A no-op when no accounts are configured.
func ensureEmbedScopeResolved() error {
	if len(cfg.Vector.Embed.Scope.Accounts) == 0 {
		return nil
	}
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open main db for embed scope resolution: %w", err)
	}
	defer func() { _ = s.Close() }()
	return resolveEmbedScopeSourceIDs(s)
}
