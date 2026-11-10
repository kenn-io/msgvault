package whatsapp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextPersistedKeys(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	_, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: asters\n"), textOptions())
	require.NoError(err)
	// Fixed SHA-256 values independently calculated from byte-length-framed fields.
	var chat, message, sender string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_conversation_id FROM conversations").Scan(&chat))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_message_id FROM messages").Scan(&message))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT identifier_value FROM participant_identifiers WHERE identifier_type = 'whatsapp_txt'").Scan(&sender))
	assert.Equal("whatsapp_txt:32c97f9e3c01ae63b595143bfee66978336626bbd70e0771d2f0d17082d5fcee", chat)
	assert.Equal("whatsapp_txt:c1d5a41dfc788949ed4e97d80a573a539d90e2a93fcc9251a6a3d14725f734fb:0", message)
	assert.Equal("f306fa6dc96fb4ed9e48408d7039d37d5d15dec20d5c8f6e324c3cfd818f8e28", sender)
}

func TestTextPayloadKeyIgnoresClassification(t *testing.T) {
	for _, tc := range []struct{ payload, kind string }{
		{"Mira:  asters: blue\n\nbeside the gate", "message"},
		{`Mira changed the subject to "Asters: blue"`, "notice"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			rows, _, err := parseText(t.Context(), strings.NewReader("13/6/26, 10:00 - "+tc.payload), "dmy", "UTC", textLimitsDefault)
			require.NoError(err)
			require.Len(rows, 1)
			require.Equal(tc.kind, rows[0].Kind)
			want := "whatsapp_txt:" + textHash("garden", rows[0].WallTime, rows[0].Precision, tc.payload) + ":0"
			assert.Equal(want, textMessageKey("garden", rows[0], 0))
		})
	}
}
