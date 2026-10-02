package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDurableAgentGrants(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	first := agentgrant.NewPersistentRegistry(st)
	id, secret, _, err := first.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}})
	requirements.NoError(err)
	var stored string
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT record FROM agent_grants WHERE id=?"), id).Scan(&stored))
	assertions.NotContains(stored, secret)
	var digest string
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT secret_hash FROM agent_grants WHERE id=?"), id).Scan(&digest))
	assertions.Len(digest, 64)
	first.Close()
	if !st.IsPostgreSQL() {
		var seq int
		var name, path string
		requirements.NoError(st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
		requirements.NoError(st.Close())
		st, err = store.Open(path)
		requirements.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
	}
	restarted := agentgrant.NewPersistentRegistry(st)
	grant, ok := restarted.Lookup(secret)
	requirements.True(ok)
	assertions.Equal(id, grant.ID)
	concurrent := agentgrant.NewPersistentRegistry(st)
	requirements.True(restarted.Revoke(id))
	_, ok = concurrent.Lookup(secret)
	assertions.False(ok, "revocation must affect the next request in every registry")
	_, expired, _, err := restarted.IssueExpires(context.Background(), "expires", []agentgrant.Permission{agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Now().Add(time.Hour))
	requirements.NoError(err)
	rows, err := st.ListAgentGrants(context.Background())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	rows[0].Grant.ExpiresAt = time.Now().Add(-time.Second)
	requirements.NoError(st.DeleteAgentGrant(context.Background(), rows[0].Grant.ID))
	requirements.NoError(st.SaveAgentGrant(context.Background(), rows[0]))
	_, ok = restarted.Lookup(expired)
	assertions.False(ok)
	listed, err := restarted.ListContext(t.Context())
	requirements.NoError(err)
	requirements.Len(listed, 1, "the owner must be able to discover and revoke expired grants")
	assertions.Equal(rows[0].Grant.ID, listed[0].ID)
	assertions.True(rows[0].Grant.ExpiresAt.Equal(listed[0].ExpiresAt))
	requirements.NoError(restarted.RevokeContext(t.Context(), listed[0].ID))
	listed, err = restarted.ListContext(t.Context())
	requirements.NoError(err)
	assertions.Empty(listed)
	_, err = st.DB().Exec("DROP TABLE agent_grants")
	requirements.NoError(err)
	_, err = restarted.ListContext(t.Context())
	requirements.Error(err, "storage failure must not appear as an empty grant list")
	failedID, failedSecret, _, err := restarted.Issue("unavailable", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}})
	require.ErrorIs(t, err, agentgrant.ErrPersistence)
	assertions.Empty(failedID)
	assertions.Empty(failedSecret)
}
