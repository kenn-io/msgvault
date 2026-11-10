package whatsapp

import (
	"strings"
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
	assert.Equal(t, "whatsapp_txt:c1d5a41dfc788949ed4e97d80a573a539d90e2a93fcc9251a6a3d14725f734fb:0", message)
	assert.Equal(t, "f306fa6dc96fb4ed9e48408d7039d37d5d15dec20d5c8f6e324c3cfd818f8e28", sender)
}

func TestTextPayloadKeyIgnoresClassification(t *testing.T) {
	payload := "Mira:  asters: blue\n\nbeside the gate"
	rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - "+payload), "dmy", "UTC", textLimitsDefault)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "message", rows[0].Kind)
	notice := rows[0]
	notice.Kind = "notice"
	notice.Sender = ""
	notice.Body = payload
	assert.Equal(t, textMessageKey("garden", rows[0], 0), textMessageKey("garden", notice, 0))
}
