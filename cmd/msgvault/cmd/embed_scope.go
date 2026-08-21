package cmd

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/opserr"
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
		resolved, err := resolveEmbedAccountList(s, cfg.Vector.Embed.Scope.Accounts, true)
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
		accountIDs, err := resolveEmbedAccountList(s, embedAccounts, false)
		if err != nil {
			return nil, fmt.Errorf("--account: %w", err)
		}
		ids = append(ids, accountIDs...)
	}
	for _, name := range embedCollections {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, errors.New("--collection: collection name is required")
		}
		scope, err := ResolveCollectionFlag(s, name)
		if err != nil {
			return nil, fmt.Errorf("--collection: %w", err)
		}
		collectionIDs := scope.SourceIDs()
		if len(collectionIDs) == 0 {
			return nil, fmt.Errorf("--collection %q has no accounts; refusing to widen the embedding scope", name)
		}
		ids = append(ids, collectionIDs...)
	}
	if len(ids) == 0 {
		return nil, errors.New("explicit embedding scope matched no accounts; refusing to widen the embedding scope")
	}
	return ids, nil
}

// resolveEmbedAccountList resolves embedding account selectors. Unlike
// collection-management inputs, durable embedding configuration must never
// accept numeric source IDs: SQLite may reuse an ID after an account is
// removed.
//
// requireIdentifier restricts matches to the account's CANONICAL identifier
// (rejecting display-name matches) and must be set for durable
// configuration: the drift detection that guards the privacy boundary
// compares resolved source IDs, and a display name is not a stable
// identity — a removed source's recycled ID plus a same-named replacement
// account would re-resolve identically and silently send the replacement
// account's text to the embedding endpoint. One-run --account flags may
// stay permissive (the operator is present and the resolution is not
// re-evaluated over time).
func resolveEmbedAccountList(s *store.Store, accounts []string, requireIdentifier bool) ([]int64, error) {
	var ids []int64
	for _, input := range accounts {
		input = strings.TrimSpace(input)
		if input == "" {
			return nil, opserr.Invalid(errors.New("account identifier is required"))
		}
		scope, err := ResolveAccountFlag(s, input)
		if err != nil {
			return nil, err
		}
		// A nil Source means the calendar-only path matched, which looks up
		// by account email (an identifier), so only a resolved primary
		// source can carry a display-name match.
		if requireIdentifier && scope.Source != nil && !store.EqualIdentifier(scope.Source.Identifier, input) {
			return nil, opserr.Invalid(fmt.Errorf(
				"account %q matches by display name; durable embedding scope requires the account identifier %q",
				input, scope.Source.Identifier))
		}
		accountIDs := scope.SourceIDs()
		if len(accountIDs) == 0 {
			return nil, opserr.Invalid(fmt.Errorf("account %q matched no sources", input))
		}
		ids = append(ids, accountIDs...)
	}
	if len(ids) == 0 {
		return nil, opserr.Invalid(errors.New("no account identifiers configured"))
	}
	return ids, nil
}

// configuredEmbedBuildScope resolves the durable account configuration anew.
// It is used by each daemon job run (and the search preflight's drift check)
// to detect source-ID reuse before the cached worker can send any message
// content to the embedding endpoint. A DETERMINISTIC resolution failure — a
// configured account removed, ambiguous, or malformed — is wrapped with
// vector.ErrScopeUnresolvable so callers latch vector search stale instead
// of retrying forever against cached source IDs; transient failures (a busy
// database) pass through unwrapped for retry.
func configuredEmbedBuildScope(s *store.Store) (vector.BuildScope, error) {
	messageTypes := cfg.Vector.Embed.Scope.MessageTypes
	if len(cfg.Vector.Embed.Scope.Accounts) == 0 {
		return vector.NewBuildScope(messageTypes, nil), nil
	}
	ids, err := resolveEmbedAccountList(s, cfg.Vector.Embed.Scope.Accounts, true)
	if err != nil {
		if kind := opserr.KindOf(err); kind == opserr.KindInvalid || kind == opserr.KindNotFound {
			return vector.BuildScope{}, fmt.Errorf("%w: [vector.embed.scope] accounts: %w", vector.ErrScopeUnresolvable, err)
		}
		return vector.BuildScope{}, fmt.Errorf("[vector.embed.scope] accounts: %w", err)
	}
	return vector.NewBuildScope(messageTypes, ids), nil
}

// ensureEmbedScopeResolved resolves the configured embed-scope accounts for
// embeddings management commands (list/activate) that compute coverage or
// compare generation fingerprints against short-lived stores of their own.
// A no-op when no accounts are configured.
//
// It mutates the package-global cfg, so it may only run in short-lived
// single-goroutine CLI processes. Daemon code paths (HTTP handlers, the
// background vector init) must use resolvedVectorConfig instead.
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

// resolvedVectorConfig returns a copy of the vector config with the
// configured [vector.embed.scope] accounts resolved into Scope.SourceIDs,
// leaving the shared package-global cfg untouched. This is the resolution
// path for the daemon, where concurrent HTTP handler goroutines and the
// background vector init would otherwise race on the global scope field.
func resolvedVectorConfig(s *store.Store) (vector.Config, error) {
	vecCfg := cfg.Vector
	// Each scope resolves only for its enabled lane: a stale account in a
	// disabled lane's scope must not block the enabled lane from starting.
	if vecCfg.Enabled && len(vecCfg.Embed.Scope.Accounts) > 0 {
		ids, err := resolveEmbedAccountList(s, vecCfg.Embed.Scope.Accounts, true)
		if err != nil {
			return vector.Config{}, fmt.Errorf("[vector.embed.scope] accounts: %w", err)
		}
		vecCfg.Embed.Scope.SourceIDs = vector.NewBuildScope(nil, ids).SourceIDs
	}
	if vecCfg.Multimodal.Enabled && len(vecCfg.Multimodal.Scope.Accounts) > 0 {
		ids, err := resolveEmbedAccountList(s, vecCfg.Multimodal.Scope.Accounts, true)
		if err != nil {
			return vector.Config{}, fmt.Errorf("[vector.multimodal.scope] accounts: %w", err)
		}
		vecCfg.Multimodal.Scope.SourceIDs = vector.NewBuildScope(nil, ids).SourceIDs
	}
	return vecCfg, nil
}

// openResolvedVectorConfig is resolvedVectorConfig for callers without an
// open store, such as the daemon's CLI-plan HTTP handlers.
func openResolvedVectorConfig() (vector.Config, error) {
	if len(cfg.Vector.Embed.Scope.Accounts) == 0 && len(cfg.Vector.Multimodal.Scope.Accounts) == 0 {
		return cfg.Vector, nil
	}
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return vector.Config{}, fmt.Errorf("open main db for embed scope resolution: %w", err)
	}
	defer func() { _ = s.Close() }()
	return resolvedVectorConfig(s)
}
