//go:build pgvector

package cmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

func TestConversationPolicyCleanupPostgresUsesActualArchiveTransaction(t *testing.T) {
	f, _, ctx := documentVectorCommandFixture(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("requires scratch PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cfg := invocationFromContext(ctx).cfg
	cfg.Vector.Enabled = false
	f.Store.DB().SetMaxOpenConns(1)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	lanes, closeLanes, err := openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	require.True(t, lanes.nativeMissing)
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes))
	require.NoError(t, closeLanes())
	present, err := f.Store.ConversationPolicyNativeVectorsPresent(ctx)
	require.NoError(t, err)
	assert.False(t, present, "missing-schema cleanup creates no native tables")
	native, err := pgvector.Open(ctx, pgvector.Options{DB: f.Store.DB(), PolicyStore: f.Store, Dimension: 3})
	require.NoError(t, err)
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Full))
	message := f.CreateMessage("pg-cleanup")
	generation, err := native.CreateGeneration(ctx, "synthetic", 3, "")
	require.NoError(t, err)
	require.NoError(t, native.Upsert(ctx, generation, []vector.Chunk{{MessageID: message, Vector: []float32{1, 0, 0}}}))
	require.NoError(t, f.Store.SetConversationPolicyOverride(ctx, f.Source.ID, "default-thread", conversationpolicy.Cold))
	lanes, closeLanes, err = openConversationPolicyCleanupLanes(ctx, f.Store)
	require.NoError(t, err)
	require.False(t, lanes.nativeMissing)
	require.NoError(t, reconcileConversationPolicyCleanup(ctx, f.Store, lanes))
	require.NoError(t, closeLanes())
	var count int
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&count))
	assert.Zero(t, count)
	_, pending, err := f.Store.ConversationPolicyCleanupPending(ctx)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestConversationPolicyRuntimePostgresCleanupWithoutVectorClient(t *testing.T) {
	st, cfg, ctx, room := policyRuntimeFixture(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires scratch PostgreSQL with pgvector")
	}
	now := time.Now().UTC()
	st.DB().SetMaxOpenConns(1)
	message, err := st.UpsertMessage(&store.Message{SourceID: 1, ConversationID: room, SourceMessageID: "native-own", MessageType: "beeper", IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	require.NoError(t, err)
	require.NoError(t, initializeConversationPolicy(ctx, st, cfg, now))
	native, err := pgvector.Open(ctx, pgvector.Options{DB: st.DB(), PolicyStore: st, Dimension: 3})
	require.NoError(t, err)
	defer func() { _ = native.Close() }()
	generation, err := native.CreateGeneration(ctx, "synthetic", 3, "")
	require.NoError(t, err)
	require.NoError(t, native.Upsert(ctx, generation, []vector.Chunk{{MessageID: message, Vector: []float32{1, 0, 0}}}))
	ticks := make(chan time.Time)
	observed := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runConversationPolicyRuntime(ctx, st, currentConversationPolicySettingsSource(invocationFromContext(ctx)), ticks, func(err error) { observed <- err })
	}()
	ticks <- now.Add(24*time.Hour + time.Minute)
	select {
	case err := <-observed:
		require.NoError(t, err)
	case <-time.After(120 * time.Second):
		require.FailNow(t, "policy pass did not finish")
	}
	cancel()
	<-done
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&count))
	assert.Zero(t, count)
	_, pending, err := st.ConversationPolicyCleanupPending(t.Context())
	require.NoError(t, err)
	assert.False(t, pending)
}
