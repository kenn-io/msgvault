package deletion

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/query"
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

// SourceMessageIDs returns target provider IDs in their existing order.
func SourceMessageIDs(targets []query.DeletionTarget) []string {
	ids := make([]string, len(targets))
	for i := range targets {
		ids[i] = targets[i].SourceMessageID
	}
	return ids
}
