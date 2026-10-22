package mime

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/quotedprintable"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const charsetHTML = "<html><body><p>Sehr geehrte Damen und Herren, ich wende mich wegen der Heizkörper an Sie.</p><div>Dies ist ein Hinweis zur Ablesung. Bitte senden Sie die Angaben im Laufe der kommenden Woche an die Verwaltung.</div><p>Mit freundlichen Grüßen</p></body></html>"
const charsetText = "Sehr geehrte Damen und Herren, ich wende mich wegen der Heizkörper an Sie.\n\nDies ist ein Hinweis zur Ablesung. Bitte senden Sie die Angaben im Laufe der kommenden Woche an die Verwaltung.\n\nMit freundlichen Grüßen"

func charsetPart(t *testing.T, charset, transfer, body string) string {
	t.Helper()
	switch transfer {
	case "quoted-printable":
		var buf bytes.Buffer
		w := quotedprintable.NewWriter(&buf)
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		body = buf.String()
	case "base64":
		body = base64.StdEncoding.EncodeToString([]byte(body))
	}
	return fmt.Sprintf("Content-Type: text/html; charset=%s\r\nContent-Transfer-Encoding: %s\r\n\r\n%s", charset, transfer, body)
}

// The German prose makes the charset detector prefer Windows-1252 over valid UTF-8.
// Short fragments do not reproduce the reported corruption.
func TestParse_DeclaredUTF8Bodies(t *testing.T) {
	for _, tc := range []struct{ kind, charset, transfer string }{
		{"html", "utf-8", "quoted-printable"},
		{"html", "UTF8", "base64"},
		{"html", "unicode-1-1-utf-8", "quoted-printable"},
		{"plain", "utf-8", ""},
		{"named", "utf-8", "quoted-printable"},
		{"inline", "utf-8", "quoted-printable"},
	} {
		t.Run(tc.kind+"/"+tc.charset+"/"+tc.transfer, func(t *testing.T) {
			assert := assert.New(t)

			part := charsetPart(t, tc.charset, tc.transfer, charsetHTML)
			wantHTML := charsetHTML
			switch tc.kind {
			case "plain":
				part = "Content-Type: text/plain; charset=utf-8\r\n\r\n" + charsetText
				wantHTML = ""
			case "named":
				part = strings.Replace(part, "charset=utf-8", "charset=utf-8; name=body.html", 1)
			case "inline":
				part = "Content-Disposition: inline; filename=body.html\r\n" + part
			}
			msg := mustParse(t, []byte(part))
			assert.Equal(wantHTML, msg.BodyHTML)
			assert.Equal(charsetText, msg.BodyText)
		})
	}
}

func TestParse_DeclaredUTF8NestedBodyPreservesAttachments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	body := charsetPart(t, "utf-8", "quoted-printable", charsetHTML)
	raw := []byte("Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPreferred plain body.\r\n" +
		"--alt\r\n" + body + "\r\n--alt--\r\n" +
		"--outer\r\nContent-Disposition: attachment; filename=explicit.html\r\n" + body + "\r\n" +
		"--outer\r\nContent-Disposition: inline; filename=inline.html\r\nContent-ID: <file@example.org>\r\n" + body + "\r\n--outer--\r\n")
	msg := mustParse(t, raw)
	assert.Equal("Preferred plain body.", msg.BodyText)
	assert.Equal(charsetHTML, msg.BodyHTML)
	require.Len(msg.Attachments, 2)
	for i, filename := range []string{"explicit.html", "inline.html"} {
		got := msg.Attachments[i]
		assert.Equal(filename, got.Filename)
		assert.Equal([]byte(charsetHTML), got.Content)
	}
}

func TestParse_DeclaredUTF8WithMalformedAttachment(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\n" + charsetPart(t, "utf-8", "quoted-printable", charsetHTML) + "\r\n" +
		"--outer\r\nContent-Type: text/plain; charset=utf-7\r\nContent-Disposition: attachment; filename=broken.txt\r\n\r\n" + charsetText + " +invalid!\r\n--outer--\r\n")
	assert := assert.New(t)
	msg := mustParse(t, raw)
	assert.Equal(charsetHTML, msg.BodyHTML)
	assert.Equal(charsetText, msg.BodyText)
	require.Len(t, msg.Attachments, 1)
	assert.Equal("broken.txt", msg.Attachments[0].Filename)
}

func TestParse_InvalidDeclaredUTF8StillRecovered(t *testing.T) {
	body := strings.ReplaceAll(strings.ReplaceAll(charsetHTML, "ö", "\xf6"), "Grüßen", "Gr\xfc\xdfen")
	msg := mustParse(t, []byte(charsetPart(t, "utf-8", "quoted-printable", body)))
	assert.Equal(t, charsetHTML, msg.BodyHTML)
	assert.Equal(t, charsetText, msg.BodyText)
}
