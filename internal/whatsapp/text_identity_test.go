package whatsapp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextPersistedKeys(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: asters\n"), textOptions())
	require.NoError(t, err)
	// Fixed SHA-256 values independently calculated from byte-length-framed fields.
	var chat, message, sender string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT source_conversation_id FROM conversations").Scan(&chat))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT source_message_id FROM messages").Scan(&message))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT identifier_value FROM participant_identifiers WHERE identifier_type = 'whatsapp_txt'").Scan(&sender))
	assert.Equal(t, "whatsapp_txt:32c97f9e3c01ae63b595143bfee66978336626bbd70e0771d2f0d17082d5fcee", chat)
	assert.Equal(t, "whatsapp_txt:a66be5de53a84c868bbbb94b4f846627491f849c1e2cbd96109d3f973d9abcb3:0", message)
	assert.Equal(t, "f306fa6dc96fb4ed9e48408d7039d37d5d15dec20d5c8f6e324c3cfd818f8e28", sender)
}
