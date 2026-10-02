package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetDocumentProviderConsentTime reads the timestamp of the exact recorded
// profile consent. Profile enablement and retirement are reported separately.
func (s *Store) GetDocumentProviderConsentTime(ctx context.Context, profileID, fingerprint string) (*time.Time, error) {
	var consentedAt time.Time
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`SELECT consented_at FROM document_provider_consents WHERE profile_id = ? AND profile_fingerprint = ?`), profileID, fingerprint).Scan(&consentedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // No exact consent has been recorded.
	}
	if err != nil {
		return nil, fmt.Errorf("read exact document consent timestamp: %w", err)
	}
	return &consentedAt, nil
}
