package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationPolicyManualTransactionSnapshotAndSemanticReads(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person := newPersonFactLedgerStore(t)
	s.DB().SetMaxOpenConns(1)
	conn, err := s.DB().Conn(t.Context())
	requirements.NoError(err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(t.Context(), s.dialect.BeginWriteSQL())
	requirements.NoError(err)
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	tx := &loggedTx{manualConn: conn, rebind: s.Rebind}
	_, err = tx.ExecContext(t.Context(), `UPDATE persons SET display_name=? WHERE id=?`, "Uncommitted synthetic name", person)
	requirements.NoError(err)
	ctx := context.WithValue(t.Context(), readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx})
	var name string
	requirements.NoError(ReadDBContext(ctx, s.DB()).QueryRowContext(ctx, s.Rebind(`SELECT display_name FROM persons WHERE id=?`), person).Scan(&name))
	assertions.Equal("Uncommitted synthetic name", name)
	doc, err := s.loadPersonSemanticDocumentLoggedTx(ctx, tx, person)
	requirements.NoError(err)
	assertions.Contains(doc.Text, "Uncommitted synthetic name")
	_, err = conn.ExecContext(t.Context(), "ROLLBACK")
	requirements.NoError(err)
	requirements.NoError(conn.Close())
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT display_name FROM persons WHERE id=?`), person).Scan(&name))
	assertions.Equal("Alice", name)
}

func TestConversationPolicySerializedRemovalReconcilesFactProjection(t *testing.T) {
	s, person, source, _, _ := policyFactFixture(t)
	s.DB().SetMaxOpenConns(1)
	require.Len(t, policyFactValues(t, s, person), 1)
	_, _, err := s.RemoveSourceSerialized(t.Context(), source)
	require.NoError(t, err)
	assert.Empty(t, policyFactValues(t, s, person))
}
