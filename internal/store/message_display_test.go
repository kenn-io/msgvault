package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/archivemarker"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMessageDisplayCarriesHiddenTextAndEncrypted(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	hidden := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Create(t, f.Store)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET hidden_text_present=TRUE WHERE id=?`), hidden)
	requirements.NoError(err)
	matrix, err := f.Store.GetOrCreateSource("matrix", "matrix-test")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(matrix.ID, "matrix-thread", "Matrix")
	requirements.NoError(err)
	m := f.NewMessage().Build()
	m.SourceID = matrix.ID
	m.ConversationID = conv
	m.MessageType = "matrix"
	m.Snippet = sql.NullString{String: archivemarker.EncryptedMessagePlaceholder, Valid: true}
	encrypted, err := f.Store.UpsertMessage(m)
	requirements.NoError(err)
	// The production Matrix writer persists the complete short marker as snippet.
	// Display hydration must work without a message body.
	got, err := f.Store.MessageDisplayContext(t.Context(), []int64{hidden, encrypted})
	requirements.NoError(err)
	assertions.True(got[hidden].HiddenText)
	assertions.NotNil(got[hidden].OccurredAt)
	assertions.True(got[encrypted].Encrypted)
	assertions.Nil(got[encrypted].OccurredAt)
	got, err = f.Store.MessageDisplayContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), []int64{hidden, encrypted})
	requirements.NoError(err)
	assertions.Empty(got)
}

func TestOwnerAnchorMessageDisplayAllowsSourceDeletionOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	sourceDeleted := f.NewMessage().WithSentAt(at).WithSnippet("Source deleted question").Create(t, f.Store)
	localDeleted := f.NewMessage().WithSentAt(at.Add(time.Minute)).WithSnippet("Locally deleted question").Create(t, f.Store)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), at, sourceDeleted)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=? WHERE id=?`), at, localDeleted)
	requirements.NoError(err)

	regular, err := f.Store.MessageDisplayContext(t.Context(), []int64{sourceDeleted, localDeleted})
	requirements.NoError(err)
	assertions.Empty(regular)

	ownerAnchor, err := f.Store.MessageDisplayForOwnerAnchorContext(t.Context(), []int64{sourceDeleted, localDeleted})
	requirements.NoError(err)
	assertions.Contains(ownerAnchor, sourceDeleted)
	assertions.NotContains(ownerAnchor, localDeleted)

	restricted, err := f.Store.MessageDisplayForOwnerAnchorContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), []int64{sourceDeleted})
	requirements.NoError(err)
	assertions.Empty(restricted)
}
