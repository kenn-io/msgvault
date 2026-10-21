package query

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func TestDuckDBScannerSearchRequiresFullParticipantValueSupport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.Open(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchemaContext(t.Context()))
	source, err := st.GetOrCreateSource("slack", "synthetic-scanner-policy")
	require.NoError(err)
	support, err := st.EnsureConversation(source.ID, "support", "")
	require.NoError(err)
	target, err := st.EnsureConversation(source.ID, "target", "")
	require.NoError(err)
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "support", "full"))
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "target", "full"))
	shared, err := st.ResolveConversationParticipantContext(t.Context(), support, store.ConversationParticipantIdentity{IdentifierType: "slack", IdentifierValue: "synthetic:shared", Email: "cold-only@hidden.test", DisplayName: "Cold Only Contact"})
	require.NoError(err)
	_, err = st.ResolveConversationParticipantContext(t.Context(), target, store.ConversationParticipantIdentity{IdentifierType: "slack", IdentifierValue: "synthetic:shared"})
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: target, SourceMessageID: "full-message", MessageType: "slack", Subject: sql.NullString{String: "Visible subject", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(err)
	for _, role := range []string{"from", "to"} {
		require.NoError(st.ReplaceMessageRecipientsContext(t.Context(), id, role, []int64{shared.ParticipantID}, []string{""}))
	}
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "support", "cold"))
	engine, err := NewDuckDBEngine("", path, nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(engine.Close()) })
	if !engine.hasSQLite() {
		t.Skip("DuckDB sqlite_scanner extension unavailable")
	}
	require.Nil(engine.sqliteEngine, "exercise scanner rather than SQLite delegation")
	for _, text := range []string{"from:cold-only@hidden.test", "to:cold-only@hidden.test"} {
		rows, err := engine.Search(t.Context(), search.Parse(text), 10, 0)
		require.NoError(err)
		assert.Empty(rows, "Cold-only values must not select Full messages: %s", text)
	}
	rows, err := engine.Search(t.Context(), search.Parse("subject:Visible"), 10, 0)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Empty(rows[0].FromEmail)
	assert.Empty(rows[0].FromName)
	require.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "support", "full"))
	rows, err = engine.Search(t.Context(), search.Parse("from:cold-only@hidden.test"), 10, 0)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("cold-only@hidden.test", rows[0].FromEmail)
	assert.Equal("Cold Only Contact", rows[0].FromName)
}
