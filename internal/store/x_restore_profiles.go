package store

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/beeperidentity"
)

// Current privacy authority contains numeric identities and observation times,
// never usernames or labels. It survives restoring a snapshot with no posts by
// the protected author and can be attached to an existing restored X source.
func (s *Store) xProtectedAuthorRecordTx(ctx context.Context, tx *loggedTx, hasSources bool) ([]XRemovalRecordAuthor, error) {
	ids := make(map[string]bool)
	metadata, err := tx.QueryContext(ctx, `SELECT key FROM archive_metadata WHERE key LIKE ?`, xProtectedAuthorKeyPrefix+"%")
	if err != nil {
		return nil, err
	}
	for metadata.Next() {
		var key string
		if err := metadata.Scan(&key); err != nil {
			_ = metadata.Close()
			return nil, err
		}
		id := strings.TrimPrefix(key, xProtectedAuthorKeyPrefix)
		if !validXAuthorID(id) {
			_ = metadata.Close()
			return nil, errors.New("invalid X author privacy authority")
		}
		ids[id] = true
		if len(ids) > XMaxRemovalRecordBytes/128 {
			_ = metadata.Close()
			return nil, errors.New("x author removal record exceeds capacity")
		}
	}
	if err := errors.Join(metadata.Err(), metadata.Close()); err != nil {
		return nil, err
	}
	if hasSources {
		rows, err := tx.QueryContext(ctx, `SELECT pi.identifier_value,o.provider_user_id
 FROM participant_contact_observations o JOIN participant_identifiers pi
 ON pi.participant_id=o.participant_id AND pi.identifier_type='x_user_id'
 WHERE o.source_id IN (SELECT id FROM sources WHERE source_type='x')
 AND o.source='archive_observation' AND o.address_kind='provider_identity'
 AND o.active_until IS NULL AND o.superseded_at IS NULL
 AND (',' || COALESCE(o.type_tokens,'') || ',') LIKE '%,x_author_protected,%'
 GROUP BY pi.identifier_value,o.provider_user_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, anchor string
			if err := rows.Scan(&id, &anchor); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if validXAuthorID(id) && anchor == beeperidentity.ProviderIdentityKey("x", "service", "", id) {
				ids[id] = true
			}
			if len(ids) > XMaxRemovalRecordBytes/128 {
				_ = rows.Close()
				return nil, errors.New("x author removal record exceeds capacity")
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	out := []XRemovalRecordAuthor{}
	for id := range ids {
		protectedAt, _, err := s.xAuthorProtectionTimesAcrossSourcesTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if protectedAt.Valid {
			out = append(out, XRemovalRecordAuthor{UserID: id, ProtectedAt: protectedAt.Time.UTC()})
		}
	}
	slices.SortFunc(out, func(a, b XRemovalRecordAuthor) int { return compareXPostIDs(a.UserID, b.UserID) })
	return out, nil
}
