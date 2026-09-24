package deletion

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrNoDeletionTargets        = errors.New("selection has no deletion targets")
	ErrIncompleteDeletionSource = errors.New("deletion target has incomplete source metadata")
	ErrMultipleDeletionSources  = errors.New("deletion targets span multiple sources")
)

// SourceReferenceForTargets returns the one source shared by every target.
func SourceReferenceForTargets(targets []query.DeletionTarget) (SourceReference, error) {
	if len(targets) == 0 {
		return SourceReference{}, ErrNoDeletionTargets
	}
	var source SourceReference
	for i, target := range targets {
		if target.SourceID <= 0 || strings.TrimSpace(target.SourceType) == "" || strings.TrimSpace(target.SourceIdentifier) == "" {
			return SourceReference{}, fmt.Errorf("%w: message %d", ErrIncompleteDeletionSource, target.MessageID)
		}
		if i == 0 {
			source = SourceReference{ID: target.SourceID, Type: target.SourceType, Identifier: target.SourceIdentifier}
			continue
		}
		if target.SourceID != source.ID || target.SourceType != source.Type || target.SourceIdentifier != source.Identifier {
			return SourceReference{}, ErrMultipleDeletionSources
		}
	}
	return source, nil
}

// SourceCatalog supplies the source rows used to resolve a durable deletion
// reference. The ID is a local snapshot; the identifier and effective type
// are the portable identity.
type SourceCatalog interface {
	GetSourceByID(id int64) (*store.Source, error)
	GetSourcesByIdentifier(identifier string) ([]*store.Source, error)
}

// ResolveSourceReference resolves a source reference without changing the
// raw source row. A matching local snapshot wins; otherwise the portable
// identity must select exactly one source.
func ResolveSourceReference(catalog SourceCatalog, ref SourceReference) (*store.Source, error) {
	effectiveType := store.EffectiveSourceType(ref.Type)
	if ref.ID > 0 {
		source, err := catalog.GetSourceByID(ref.ID)
		switch {
		case err == nil && source != nil &&
			store.EffectiveSourceType(source.SourceType) == effectiveType &&
			source.Identifier == ref.Identifier:
			return source, nil
		case err != nil && !errors.Is(err, store.ErrSourceNotFound):
			return nil, fmt.Errorf("resolve source snapshot %d: %w", ref.ID, err)
		}
	}

	sources, err := catalog.GetSourcesByIdentifier(ref.Identifier)
	if err != nil {
		return nil, fmt.Errorf("resolve source identifier %q: %w", ref.Identifier, err)
	}
	var matches []*store.Source
	for _, source := range sources {
		if source != nil && store.EffectiveSourceType(source.SourceType) == effectiveType {
			matches = append(matches, source)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("source %s/%s: %w", ref.Type, ref.Identifier, store.ErrSourceNotFound)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("source %s/%s is ambiguous: %d matching sources", ref.Type, ref.Identifier, len(matches))
	}
}

// SourceMessageIDs returns target provider IDs in their existing order.
func SourceMessageIDs(targets []query.DeletionTarget) []string {
	ids := make([]string, len(targets))
	for i := range targets {
		ids[i] = targets[i].SourceMessageID
	}
	return ids
}
