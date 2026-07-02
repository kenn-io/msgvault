package cmd

import (
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

// Scope is the result of resolving a user-supplied --account or
// --collection flag against the store.
type Scope struct {
	Input               string
	Source              *store.Source
	Collection          *store.CollectionWithSources
	AdditionalSourceIDs []int64
}

// IsEmpty reports whether the scope resolved to nothing.
func (s Scope) IsEmpty() bool {
	return s.Source == nil && s.Collection == nil && len(s.AdditionalSourceIDs) == 0
}

// IsCollection reports whether the scope refers to a collection.
func (s Scope) IsCollection() bool {
	return s.Collection != nil
}

// SourceIDs returns the source IDs that this scope expands to.
func (s Scope) SourceIDs() []int64 {
	switch {
	case s.Collection != nil:
		return append([]int64(nil), s.Collection.SourceIDs...)
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

// DisplayName returns a human-readable label for the scope.
func (s Scope) DisplayName() string {
	switch {
	case s.Collection != nil:
		return s.Collection.Name
	case s.Source != nil:
		return s.Source.Identifier
	case len(s.AdditionalSourceIDs) > 0:
		return s.Input
	}
	return ""
}

// ResolveAccountFlag resolves the value of an --account flag.
// It rejects collection names with a hint to use --collection.
func ResolveAccountFlag(st *store.Store, input string) (Scope, error) {
	scope, err := collectionops.ResolveAccount(st, input)
	return scopeFromResolvedAccount(scope), err
}

// ResolveEmailAccountFlag resolves an account for commands that operate only
// on email-like source rows and must not expand into Calendar sources.
func ResolveEmailAccountFlag(st *store.Store, input string) (Scope, error) {
	return resolveEmailAccountFlag(st, input)
}

func scopeFromResolvedAccount(scope collectionops.Scope) Scope {
	return Scope{
		Input:               scope.Input,
		Source:              scope.Source,
		AdditionalSourceIDs: scope.AdditionalSourceIDs,
	}
}

func scopeFromResolvedCollection(scope collectionops.CollectionScope) Scope {
	return Scope{
		Input:      scope.Input,
		Collection: scope.Collection,
	}
}

func resolveEmailAccountFlag(st *store.Store, input string) (Scope, error) {
	scope := Scope{Input: input}
	if input == "" {
		return scope, nil
	}

	// Try source resolution first.
	sources, err := st.GetSourcesByIdentifierOrDisplayName(input)
	if err != nil {
		return scope, opserr.Internal(fmt.Errorf("look up source for %q: %w", input, err))
	}
	sources = filterSources(sources, emailAccountSource)
	if len(sources) > 1 {
		names := make([]string, 0, len(sources))
		for _, s := range sources {
			names = append(names, fmt.Sprintf(
				"%s (%s, id=%d)",
				s.Identifier, s.SourceType, s.ID,
			))
		}
		return scope, opserr.Invalid(fmt.Errorf(
			"ambiguous account %q matches multiple sources: %v",
			input, names,
		))
	}
	if len(sources) == 1 {
		scope.Source = sources[0]
		return scope, nil
	}

	// No source match — check whether a collection exists with this name and
	// reject with a helpful hint.
	_, cerr := st.GetCollectionByName(input)
	switch {
	case cerr == nil:
		return scope, opserr.Invalid(fmt.Errorf(
			"%q is a collection, not an account; use --collection %s",
			input, input,
		))
	case errors.Is(cerr, store.ErrCollectionNotFound):
		// Neither a source nor a collection.
	default:
		return scope, opserr.Internal(fmt.Errorf("look up collection %q: %w", input, cerr))
	}

	return scope, opserr.NotFound(fmt.Errorf(
		"no account found for %q (try 'msgvault list-accounts')",
		input,
	))
}

func filterSources(sources []*store.Source, keep func(*store.Source) bool) []*store.Source {
	filtered := sources[:0]
	for _, src := range sources {
		if keep(src) {
			filtered = append(filtered, src)
		}
	}
	return filtered
}

func emailAccountSource(src *store.Source) bool {
	return src != nil && src.SourceType != sourceTypeCalendar
}

// ResolveCollectionFlag resolves the value of a --collection flag.
// It rejects account identifiers with a hint to use --account.
func ResolveCollectionFlag(st *store.Store, input string) (Scope, error) {
	scope, err := collectionops.ResolveCollection(st, input)
	return scopeFromResolvedCollection(scope), err
}
