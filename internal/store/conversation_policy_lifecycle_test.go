package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicySerializedRemovalWaitsForPublication(t *testing.T) {
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "locked@example.test")
	requirements.NoError(err)
	var release func()
	if s.IsPostgreSQL() {
		tx, err := s.DB().BeginTx(t.Context(), nil)
		requirements.NoError(err)
		requirements.NoError(s.LockPostgresConversationPolicyTx(t.Context(), tx, store.ConversationPolicyShared))
		release = func() { _ = tx.Rollback() }
	} else {
		var seq int
		var name, path string
		requirements.NoError(s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
		other := policyFenceStore(t, path)
		release, err = other.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
		requirements.NoError(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the held policy read fence blocks serialized source removal
	defer cancel()
	_, _, err = s.RemoveSourceSerialized(ctx, src.ID)
	requirements.Error(err)
	release()
	_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
	requirements.NoError(err)
}
