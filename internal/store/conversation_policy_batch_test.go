package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
)

func newConversationPolicyBatchTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	return st
}

func TestConfigureConversationPolicyBatchesLargeRoomInvalidation(t *testing.T) {
	require := require.New(t)
	st := newConversationPolicyBatchTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("slack", "large-policy-bootstrap")
	require.NoError(err)
	require.NoError(st.withConversationPolicyWrite(ctx, func(tx *loggedTx) error {
		var attribution string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN key='identity_revision' THEN value END),'0') || ':' || COALESCE(MAX(CASE WHEN key='account_identity_revision' THEN value END),'0') FROM archive_metadata`).Scan(&attribution); err != nil {
			return err
		}
		fingerprint, err := st.conversationPolicySelfFingerprintTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := st.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_attribution_revision", attribution); err != nil {
			return err
		}
		return st.setConversationPolicyMetadataTx(ctx, tx, "conversation_policy_self_attribution_fingerprint", fingerprint)
	}))

	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = tx.Rollback() }()
	conversations, err := tx.PrepareContext(ctx, st.Rebind(`INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type,title,participant_count,metadata) VALUES(?,?,?,?,?,?,?)`))
	require.NoError(err)
	defer func() { _ = conversations.Close() }()
	policies, err := tx.PrepareContext(ctx, `INSERT INTO conversation_policy(conversation_id,effective_tier) VALUES(?,'full')`)
	require.NoError(err)
	defer func() { _ = policies.Close() }()
	for id := int64(1); id <= 11_000; id++ {
		_, err = conversations.ExecContext(ctx, id, source.ID, fmt.Sprintf("bootstrap-room-%d", id), "channel", "Synthetic room", 0, "{}")
		require.NoError(err)
		_, err = policies.ExecContext(ctx, id)
		require.NoError(err)
	}
	require.NoError(policies.Close())
	require.NoError(conversations.Close())
	require.NoError(tx.Commit())

	result, err := st.ConfigureConversationPolicy(ctx, conversationpolicy.RecommendedSettings(), time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(result.ChangedConversationIDs, 11_000)
}

func TestRefreshConversationPolicyBatchesLargeRoomSet(t *testing.T) {
	st := newConversationPolicyBatchTestStore(t)
	ids := make([]int64, 33_000)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	result, err := st.RefreshConversationPolicy(t.Context(), ids, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Empty(t, result.ChangedConversationIDs)
}
