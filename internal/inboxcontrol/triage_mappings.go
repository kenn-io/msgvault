package inboxcontrol

import (
	"context"
	"maps"
	"math"

	"go.kenn.io/msgvault/internal/emailtags"
)

// TriageMappingStore retains source-bound owner configuration independently of
// provider observations. Replacing or clearing it advances a durable revision.
type TriageMappingStore interface {
	InboxTriageMappings(ctx context.Context, source SourceIdentity) (map[string]string, int64, error)
	ReplaceInboxTriageMappings(ctx context.Context, source SourceIdentity, entries map[string]string, expectedRevision int64, principal Principal) (int64, error)
}

// ValidateTriageMappingEntries checks configuration shape, not native existence.
// Native existence is verified by UpdateTriageMappings under the source lease.
func ValidateTriageMappingEntries(source SourceIdentity, entries map[string]string) error {
	if err := source.Validate(); err != nil {
		return err
	}
	for category, tag := range entries {
		if !validTriageCategory(category) {
			return ErrInvalid
		}
		if _, err := emailtags.Normalize(emailtags.Change{Add: []string{tag}}, source.SourceType == sourceTypeIMAP); err != nil {
			return ErrInvalid
		}
	}
	return nil
}

func validTriageCategory(category string) bool {
	switch category {
	case "todo", "reply-needed", "watch", "delegated", "finished", "uncertain":
		return true
	default:
		return false
	}
}

// ValidateTriageMappingUpdate takes the authenticated principal, never an
// owner flag from a configuration request. Zero creates the first revision.
func ValidateTriageMappingUpdate(source SourceIdentity, entries map[string]string, expectedRevision int64, principal Principal) error {
	if !principal.Owner || !validIdentity(principal.ID) {
		return ErrDenied
	}
	if expectedRevision < 0 || expectedRevision == math.MaxInt64 {
		return ErrInvalid
	}
	return ValidateTriageMappingEntries(source, entries)
}

// UpdateTriageMappings changes only owner configuration. Catalog lookup is
// read-only; delegates cannot provision tags or replace this configuration.
func (s *Service) UpdateTriageMappings(ctx context.Context, source SourceIdentity, entries map[string]string, expectedRevision int64, principal Principal, acquireWrite func(context.Context) (func(), error)) (int64, error) {
	if err := ValidateTriageMappingUpdate(source, entries, expectedRevision, principal); err != nil {
		return 0, err
	}
	store, ok := s.Ledger.(TriageMappingStore)
	if !ok || s.Resolve == nil {
		return 0, ErrUnavailable
	}
	request := Request{Operation: OpGetCapabilities, Source: &source, DryRun: true, NativeTagCatalog: true}
	if err := s.authorize(ctx, principal, request); err != nil {
		return 0, err
	}
	done, err := s.acquire(ctx, source.SourceID, acquireWrite)
	if err != nil {
		return 0, err
	}
	defer done()
	if err := s.authorize(ctx, principal, request); err != nil {
		return 0, err
	}
	provider, err := s.Resolve(ctx, request)
	if err != nil {
		return 0, safePreflightError(err)
	}
	if provider == nil {
		return 0, ErrUnavailable
	}
	defer closeProvider(provider)
	catalog, ok := provider.(TagCatalogProvider)
	if !ok {
		return 0, ErrUnavailable
	}
	tags, err := catalog.TagCatalog(ctx, source)
	if err != nil {
		return 0, safePreflightError(err)
	}
	// Preserve native spelling, including case-insensitive IMAP keyword matches.
	canonical := maps.Clone(entries)
	if canonical == nil {
		canonical = map[string]string{}
	}
	for category, requested := range entries {
		matches := 0
		for _, tag := range tags {
			if emailtags.Contains([]string{tag.ID}, requested, source.SourceType == sourceTypeIMAP) {
				canonical[category] = tag.ID
				matches++
			}
		}
		if matches != 1 {
			return 0, ErrDenied
		}
	}
	if err := s.authorize(ctx, principal, request); err != nil {
		return 0, err
	}
	return store.ReplaceInboxTriageMappings(ctx, source, canonical, expectedRevision, principal)
}
