package cmd

import (
	"errors"
	"fmt"

	"github.com/wesm/msgvault/internal/store"
)

// Scope is the result of resolving a user-supplied --account or
// --collection flag against the store.
type Scope struct {
	Input      string
	Source     *store.Source
	Collection *store.CollectionWithSources
}

// IsEmpty reports whether the scope resolved to nothing.
func (s Scope) IsEmpty() bool {
	return s.Source == nil && s.Collection == nil
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
		return []int64{s.Source.ID}
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
	}
	return ""
}

// ResolveAccountFlag resolves the value of an --account flag.
// It rejects collection names with a hint to use --collection.
func ResolveAccountFlag(st *store.Store, input string) (Scope, error) {
	scope := Scope{Input: input}
	if input == "" {
		return scope, nil
	}

	// Try source resolution first.
	sources, err := st.GetSourcesByIdentifierOrDisplayName(input)
	if err != nil {
		return scope, fmt.Errorf("look up source for %q: %w", input, err)
	}
	if len(sources) > 1 {
		names := make([]string, 0, len(sources))
		for _, s := range sources {
			names = append(names, fmt.Sprintf(
				"%s (%s, id=%d)",
				s.Identifier, s.SourceType, s.ID,
			))
		}
		return scope, fmt.Errorf(
			"ambiguous account %q matches multiple sources: %v",
			input, names,
		)
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
		return scope, fmt.Errorf(
			"%q is a collection, not an account; use --collection %s",
			input, input,
		)
	case errors.Is(cerr, store.ErrCollectionNotFound):
		// Neither a source nor a collection.
	default:
		return scope, fmt.Errorf("look up collection %q: %w", input, cerr)
	}

	return scope, fmt.Errorf(
		"no account found for %q (try 'msgvault list-accounts')",
		input,
	)
}

// ResolveCollectionFlag resolves the value of a --collection flag.
// It rejects account identifiers with a hint to use --account.
func ResolveCollectionFlag(st *store.Store, input string) (Scope, error) {
	scope := Scope{Input: input}
	if input == "" {
		return scope, nil
	}

	// Try collection resolution first.
	coll, err := st.GetCollectionByName(input)
	switch {
	case err == nil:
		scope.Collection = coll
		return scope, nil
	case errors.Is(err, store.ErrCollectionNotFound):
		// Fall through to source check.
	default:
		return scope, fmt.Errorf("look up collection %q: %w", input, err)
	}

	// No collection found — check whether any source matches and reject with a hint.
	sources, serr := st.GetSourcesByIdentifierOrDisplayName(input)
	if serr != nil {
		return scope, fmt.Errorf("look up source for %q: %w", input, serr)
	}
	if len(sources) >= 1 {
		return scope, fmt.Errorf(
			"%q is an account, not a collection; use --account %s",
			input, input,
		)
	}

	return scope, fmt.Errorf(
		"no collection named %q (try 'msgvault collection list')",
		input,
	)
}
