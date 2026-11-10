package whatsapp

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextImportYouIsScopedLabel(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - You: fictional garden note"), textOptions())
	require.NoError(t, err)
	var sender sql.NullInt64
	var fromMe bool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT sender_id,is_from_me FROM messages").Scan(&sender, &fromMe))
	require.True(t, sender.Valid, "a literal You label is still sender evidence, not a senderless notice")
	assert.False(t, fromMe, "a display label cannot establish owner direction")
	var kind, value string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT identifier_type,identifier_value FROM participant_identifiers WHERE participant_id = ?", sender.Int64).Scan(&kind, &value))
	assert.Equal(t, "whatsapp_txt", kind)
	assert.NotEqual(t, textOptions().Phone, value)
}
