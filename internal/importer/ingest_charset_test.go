package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"mime/quotedprintable"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIngestRawMessage_DeclaredUTF8HTML(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	require.True(st.FTS5Available(), "body search must exercise the full-text index")
	source, err := st.GetOrCreateSource("mbox", "archive@example.org")
	require.NoError(err)

	// Enough German prose to trigger the erroneous Windows-1252 detection.
	const bodyHTML = "<html><body><p>Sehr geehrte Damen und Herren, ich wende mich wegen der Heizkörper an Sie.</p><div>Dies ist ein Hinweis zur Ablesung. Bitte senden Sie die Angaben im Laufe der kommenden Woche an die Verwaltung.</div><p>Mit freundlichen Grüßen</p></body></html>"
	var encoded bytes.Buffer
	writer := quotedprintable.NewWriter(&encoded)
	_, err = writer.Write([]byte(bodyHTML))
	require.NoError(err)
	require.NoError(writer.Close())
	raw := []byte("From: sender@example.org\r\nTo: recipient@example.org\r\n" +
		"Subject: Meter reading\r\nMessage-ID: <reading@example.org>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" + encoded.String())
	digest := sha256.Sum256(raw)
	err = IngestRawMessage(context.Background(), st, source.ID, "archive@example.org", "",
		nil, "reading-1", hex.EncodeToString(digest[:]), raw, time.Time{}, slog.Default())
	require.NoError(err)

	// The term occurs only in the body, so neither headers nor addresses can match.
	results, total, err := st.SearchMessages("Heizkörper", 0, 10)
	require.NoError(err)
	require.Equal(int64(1), total)
	require.Len(results, 1)
	message, err := st.GetMessage(results[0].ID)
	require.NoError(err)
	assert.Contains(message.Snippet, "Heizkörper")
}
