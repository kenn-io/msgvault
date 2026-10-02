package store

import (
	"context"
	"errors"
	"fmt"
)

// HasAnyActiveLaneConsent is a readiness-only historical check. A grant for
// another policy never authorizes the current policy. Purpose is a closed set.
func (s *Store) HasAnyActiveLaneConsent(ctx context.Context, purpose string) (bool, error) {
	var query string
	switch purpose {
	case "person_inference":
		query = `SELECT EXISTS (SELECT 1 FROM person_inference_consents WHERE revoked_at IS NULL)`
	case "person_semantic":
		query = `SELECT EXISTS (SELECT 1 FROM person_semantic_embedding_consents WHERE revoked_at IS NULL)`
	case "document_embedding", "query_embedding":
		var active bool
		err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS (SELECT 1 FROM document_vector_consents WHERE purpose = ?)`), purpose).Scan(&active)
		if err != nil {
			return false, fmt.Errorf("read document vector readiness consent: %w", err)
		}
		return active, nil
	default:
		return false, errors.New("unsupported lane consent purpose")
	}
	var active bool
	if err := s.db.QueryRowContext(ctx, query).Scan(&active); err != nil {
		return false, fmt.Errorf("read lane readiness consent: %w", err)
	}
	return active, nil
}
