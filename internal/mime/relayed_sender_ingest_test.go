package mime_test

import (
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMalformedRelaySenderFallsBackDuringImport(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("eml", "reader@example.test")
	require.NoError(t, err)
	for i, value := range []string{"alice\xff", "alice\x00bob"} {
		raw := []byte("From: GitHub <notifications@github.com>\r\nTo: reader@example.test\r\nX-GitHub-Sender: " + value + "\r\nSubject: Notice\r\n\r\nHello")
		require.NoError(t, importer.IngestRawMessage(t.Context(), st, source.ID, source.Identifier, "", nil, strconv.Itoa(i), strconv.Itoa(i), raw, time.Time{}, slog.Default()))
		var address string
		err = st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT p.email_address FROM messages m JOIN participants p ON p.id = m.sender_id WHERE m.source_message_id = ?`), strconv.Itoa(i)).Scan(&address)
		require.NoError(t, err)
		assert.Equal(t, "notifications@github.com", address)
	}
}
