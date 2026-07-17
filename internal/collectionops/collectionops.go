package collectionops

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

var (
	// ErrAccountsRequired reports a missing collection --accounts value.
	ErrAccountsRequired = errors.New("--accounts is required")
	// ErrNoValidAccounts reports an account list with no usable entries.
	ErrNoValidAccounts = errors.New("no valid accounts in --accounts")
	// ErrNameRequired reports a missing collection name.
	ErrNameRequired = errors.New("collection name is required")
)

// AccountResolverStore is the source/collection lookup surface needed to
// resolve user-supplied account tokens.
type AccountResolverStore interface {
	GetSourcesByIdentifierOrDisplayName(query string) ([]*store.Source, error)
	GetSourcesByTypeAndAccount(sourceType, accountEmail string) ([]*store.Source, error)
	GetCollectionByName(name string) (*store.CollectionWithSources, error)
}

// Store is the collection and source surface needed by collection operations.
type Store interface {
	AccountResolverStore
	GetSourceByID(id int64) (*store.Source, error)
	CreateCollection(name, description string, sourceIDs []int64) (*store.Collection, error)
	AddSourcesToCollection(name string, sourceIDs []int64) error
	RemoveSourcesFromCollection(name string, sourceIDs []int64) error
	DeleteCollection(name string) error
}

// CreateRequest creates a collection from account identifiers.
type CreateRequest struct {
	Name     string   `json:"name"`
	Accounts []string `json:"accounts"`
}

// SourcesRequest adds or removes account identifiers from a collection.
type SourcesRequest struct {
	Accounts []string `json:"accounts"`
}

// MutationResult is the CLI-facing result for collection mutations.
type MutationResult struct {
	Name        string `json:"name"`
	SourceCount int    `json:"source_count,omitempty"`
}

// ParseAccountsFlag parses a comma-separated --accounts flag value.
func ParseAccountsFlag(raw string) ([]string, error) {
	if raw == "" {
		return nil, opserr.Invalid(ErrAccountsRequired)
	}
	parts := strings.Split(raw, ",")
	accounts := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			accounts = append(accounts, part)
		}
	}
	if len(accounts) == 0 {
		return nil, opserr.Invalid(ErrNoValidAccounts)
	}
	return accounts, nil
}

// Create creates a collection after resolving account identifiers to sources.
func Create(st Store, req CreateRequest) (MutationResult, error) {
	if strings.TrimSpace(req.Name) == "" {
		return MutationResult{}, opserr.Invalid(ErrNameRequired)
	}
	sourceIDs, err := ResolveAccountList(st, req.Accounts)
	if err != nil {
		return MutationResult{}, err
	}
	coll, err := st.CreateCollection(req.Name, "", sourceIDs)
	if err != nil {
		return MutationResult{}, storeMutationError(err)
	}
	return MutationResult{Name: coll.Name, SourceCount: len(sourceIDs)}, nil
}

// AddSources adds resolved account sources to a collection.
func AddSources(st Store, name string, req SourcesRequest) (MutationResult, error) {
	if strings.TrimSpace(name) == "" {
		return MutationResult{}, opserr.Invalid(ErrNameRequired)
	}
	sourceIDs, err := ResolveAccountList(st, req.Accounts)
	if err != nil {
		return MutationResult{}, err
	}
	if err := st.AddSourcesToCollection(name, sourceIDs); err != nil {
		return MutationResult{}, storeMutationError(err)
	}
	return MutationResult{Name: name, SourceCount: len(sourceIDs)}, nil
}

// RemoveSources removes resolved account sources from a collection.
func RemoveSources(st Store, name string, req SourcesRequest) (MutationResult, error) {
	if strings.TrimSpace(name) == "" {
		return MutationResult{}, opserr.Invalid(ErrNameRequired)
	}
	sourceIDs, err := ResolveAccountList(st, req.Accounts)
	if err != nil {
		return MutationResult{}, err
	}
	if err := st.RemoveSourcesFromCollection(name, sourceIDs); err != nil {
		return MutationResult{}, storeMutationError(err)
	}
	return MutationResult{Name: name, SourceCount: len(sourceIDs)}, nil
}

// Delete deletes a collection without deleting member sources or messages.
func Delete(st Store, name string) (MutationResult, error) {
	if strings.TrimSpace(name) == "" {
		return MutationResult{}, opserr.Invalid(ErrNameRequired)
	}
	if err := st.DeleteCollection(name); err != nil {
		return MutationResult{}, storeMutationError(err)
	}
	return MutationResult{Name: name}, nil
}

// ResolveAccountList resolves account tokens or source IDs to source IDs.
func ResolveAccountList(st Store, accounts []string) ([]int64, error) {
	var ids []int64
	for _, raw := range accounts {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if p[0] >= '0' && p[0] <= '9' {
			if id, err := strconv.ParseInt(p, 10, 64); err == nil {
				_, lookupErr := st.GetSourceByID(id)
				switch {
				case lookupErr == nil:
					ids = append(ids, id)
					continue
				case errors.Is(lookupErr, store.ErrSourceNotFound):
				default:
					return nil, opserr.Internal(fmt.Errorf("get source %d: %w", id, lookupErr))
				}
			}
		}
		scope, err := ResolveAccount(st, p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, scope.SourceIDs()...)
	}
	if len(ids) == 0 {
		return nil, opserr.Invalid(ErrNoValidAccounts)
	}
	return ids, nil
}

// Scope is a resolved account source scope.
type Scope struct {
	Input               string
	Source              *store.Source
	AdditionalSourceIDs []int64
}

// SourceIDs returns the source IDs represented by the scope.
func (s Scope) SourceIDs() []int64 {
	switch {
	case s.Source != nil:
		ids := make([]int64, 0, 1+len(s.AdditionalSourceIDs))
		ids = append(ids, s.Source.ID)
		for _, id := range s.AdditionalSourceIDs {
			if id != s.Source.ID {
				ids = append(ids, id)
			}
		}
		return ids
	case len(s.AdditionalSourceIDs) > 0:
		return append([]int64(nil), s.AdditionalSourceIDs...)
	}
	return nil
}

// ResolveAccount resolves an account token to its primary and related sources.
func ResolveAccount(st AccountResolverStore, input string) (Scope, error) {
	scope := Scope{Input: input}
	if input == "" {
		return scope, nil
	}

	sources, err := st.GetSourcesByIdentifierOrDisplayName(input)
	if err != nil {
		return scope, opserr.Internal(fmt.Errorf("look up source for %q: %w", input, err))
	}
	calendarSources, err := st.GetSourcesByTypeAndAccount(gcal.SourceType, input)
	if err != nil {
		return scope, opserr.Internal(fmt.Errorf("look up calendar sources for %q: %w", input, err))
	}
	if len(sources) > 1 {
		names := make([]string, 0, len(sources))
		for _, src := range sources {
			names = append(names, fmt.Sprintf(
				"%s (%s, id=%d)",
				src.Identifier, src.SourceType, src.ID,
			))
		}
		return scope, opserr.Invalid(fmt.Errorf(
			"ambiguous account %q matches multiple sources: %v",
			input, names,
		))
	}
	if len(sources) == 1 {
		scope.Source = sources[0]
		if sources[0].SourceType != gcal.SourceType &&
			!store.EqualIdentifier(sources[0].Identifier, input) {
			resolvedCalendarSources, err := st.GetSourcesByTypeAndAccount(
				gcal.SourceType,
				sources[0].Identifier,
			)
			if err != nil {
				return scope, opserr.Internal(fmt.Errorf(
					"look up calendar sources for %q: %w",
					sources[0].Identifier,
					err,
				))
			}
			calendarSources = appendUniqueSources(calendarSources, resolvedCalendarSources)
		}
		scope.AdditionalSourceIDs = sourceIDsExcept(calendarSources, sources[0].ID)
		return scope, nil
	}
	if len(calendarSources) > 0 {
		scope.AdditionalSourceIDs = sourceIDsExcept(calendarSources, 0)
		return scope, nil
	}

	_, collErr := st.GetCollectionByName(input)
	switch {
	case collErr == nil:
		return scope, opserr.Invalid(fmt.Errorf(
			"%q is a collection, not an account; use --collection %s",
			input,
			input,
		))
	case errors.Is(collErr, store.ErrCollectionNotFound):
	default:
		return scope, opserr.Internal(fmt.Errorf("look up collection %q: %w", input, collErr))
	}

	return scope, opserr.NotFound(fmt.Errorf(
		"no account found for %q (try 'msgvault list-accounts')",
		input,
	))
}

// CollectionScope is a resolved collection source scope.
type CollectionScope struct {
	Input      string
	Collection *store.CollectionWithSources
}

// SourceIDs returns the source IDs represented by the collection scope.
func (s CollectionScope) SourceIDs() []int64 {
	if s.Collection == nil {
		return nil
	}
	return append([]int64(nil), s.Collection.SourceIDs...)
}

// ResolveCollection resolves a collection token and rejects account tokens with
// a hint to use account scope instead.
func ResolveCollection(st AccountResolverStore, input string) (CollectionScope, error) {
	scope := CollectionScope{Input: input}
	if input == "" {
		return scope, nil
	}

	coll, err := st.GetCollectionByName(input)
	switch {
	case err == nil:
		scope.Collection = coll
		return scope, nil
	case errors.Is(err, store.ErrCollectionNotFound):
	default:
		return scope, opserr.Internal(fmt.Errorf("look up collection %q: %w", input, err))
	}

	sources, sourceErr := st.GetSourcesByIdentifierOrDisplayName(input)
	if sourceErr != nil {
		return scope, opserr.Internal(fmt.Errorf("look up source for %q: %w", input, sourceErr))
	}
	if len(sources) >= 1 {
		return scope, opserr.Invalid(fmt.Errorf(
			"%q is an account, not a collection; use --account %s", input, input),
		)
	}

	return scope, opserr.NotFound(fmt.Errorf(
		"no collection named %q (try 'msgvault collection list')", input),
	)
}

func appendUniqueSources(dst []*store.Source, srcs []*store.Source) []*store.Source {
	seen := make(map[int64]struct{}, len(dst)+len(srcs))
	for _, src := range dst {
		if src == nil {
			continue
		}
		seen[src.ID] = struct{}{}
	}
	for _, src := range srcs {
		if src == nil {
			continue
		}
		if _, ok := seen[src.ID]; ok {
			continue
		}
		seen[src.ID] = struct{}{}
		dst = append(dst, src)
	}
	return dst
}

func sourceIDsExcept(sources []*store.Source, exclude int64) []int64 {
	ids := make([]int64, 0, len(sources))
	seen := map[int64]struct{}{}
	if exclude != 0 {
		seen[exclude] = struct{}{}
	}
	for _, src := range sources {
		if src == nil {
			continue
		}
		if _, ok := seen[src.ID]; ok {
			continue
		}
		seen[src.ID] = struct{}{}
		ids = append(ids, src.ID)
	}
	return ids
}

func storeMutationError(err error) error {
	switch {
	case errors.Is(err, store.ErrCollectionNotFound):
		return opserr.NotFound(err)
	case errors.Is(err, store.ErrCollectionExists), errors.Is(err, store.ErrCollectionImmutable):
		return opserr.Invalid(err)
	default:
		return opserr.Internal(err)
	}
}
