package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPostgreSQLConversationPolicyMaintenanceLocks(t *testing.T) {
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	if !s.IsPostgreSQL() {
		t.Skip("PostgreSQL scratch database not configured")
	}
	tx, err := s.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	requirements.NoError(s.LockPostgresConversationPolicyTx(t.Context(), tx, store.ConversationPolicyShared))
	other, err := s.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = other.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the first SQL transaction holds the actual shared policy lock
	defer cancel()
	requirements.Error(s.LockPostgresConversationPolicyTx(ctx, other, store.ConversationPolicyExclusive))
	requirements.NoError(tx.Rollback())
	_ = other.Rollback()
	// Exercise the real archive-maintenance transaction, not a copied table list.
	requirements.NoError(s.WithExclusiveLock(t.Context(), func() error {
		for _, table := range []string{"conversation_policy"} {
			var locked bool
			err := s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE relation=CAST(? AS regclass) AND mode='ExclusiveLock' AND granted)`), table).Scan(&locked)
			if err != nil {
				return err
			}
			assert.True(t, locked, "maintenance must lock %s", table)
		}
		return nil
	}))
}
