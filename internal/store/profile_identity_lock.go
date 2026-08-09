package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// lockProfileIdentityKeyTxContext serializes a check-then-insert for one
// logical profile-identity key. PostgreSQL row locks cannot lock an absent
// row, and its ordinary unique indexes treat NULL values as distinct. A
// transaction-scoped advisory lock closes that gap without changing the
// duplicate-tolerant API contract. SQLite has a single writer, so taking the
// existing identity-mutation write lock before the read provides the same
// ordering there.
func (s *Store) lockProfileIdentityKeyTxContext(
	ctx context.Context,
	tx *loggedTx,
	namespace string,
	parts ...any,
) error {
	if !s.IsPostgreSQL() {
		return s.lockIdentityMutationTxContext(ctx, tx)
	}

	var key strings.Builder
	key.WriteString(namespace)
	for _, part := range parts {
		rendered := fmt.Sprintf("%v", part)
		partType := fmt.Sprintf("%T", part)
		key.WriteByte('|')
		key.WriteString(partType)
		key.WriteByte(':')
		key.WriteString(strconv.Itoa(len(rendered)))
		key.WriteByte(':')
		key.WriteString(rendered)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended(CAST(? AS TEXT), 0))`,
		key.String(),
	); err != nil {
		return fmt.Errorf("lock %s identity key: %w", namespace, err)
	}
	return nil
}
