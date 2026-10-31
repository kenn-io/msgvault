package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/archivemarker"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

func TestMessageDisplayCarriesHiddenTextAndEncrypted(t *testing.T) {
	f := storetest.New(t)
	hidden := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Create(t, f.Store)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET hidden_text_present=TRUE WHERE id=?`), hidden)
	require.NoError(t, err)
	matrix, err := f.Store.GetOrCreateSource("matrix", "matrix-test")
	require.NoError(t, err)
	conv, err := f.Store.EnsureConversation(matrix.ID, "matrix-thread", "Matrix")
	require.NoError(t, err)
	m := f.NewMessage().Build()
	m.SourceID = matrix.ID
	m.ConversationID = conv
	m.MessageType = "matrix"
	m.Snippet = sql.NullString{String: archivemarker.EncryptedMessagePlaceholder, Valid: true}
	encrypted, err := f.Store.UpsertMessage(m)
	require.NoError(t, err)
	// The production Matrix writer persists the complete short marker as snippet.
	// Display hydration must work without a message body.
	got, err := f.Store.MessageDisplayContext(t.Context(), []int64{hidden, encrypted})
	require.NoError(t, err)
	assert.True(t, got[hidden].HiddenText)
	assert.NotNil(t, got[hidden].OccurredAt)
	assert.True(t, got[encrypted].Encrypted)
	assert.Nil(t, got[encrypted].OccurredAt)
	got, err = f.Store.MessageDisplayContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), []int64{hidden, encrypted})
	require.NoError(t, err)
	assert.Empty(t, got)
}
