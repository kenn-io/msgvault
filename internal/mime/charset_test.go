package mime

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	stdmime "mime"
	"mime/quotedprintable"
	"strings"
	"testing"

	"github.com/jhillyerd/enmime/v2"
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

func charsetHeaders() string {
	return "From: " + stdmime.QEncoding.Encode("utf-8", "Example Grüße") + " <sender@example.org>\r\n" +
		"To: recipient@example.org\r\nSubject: " + stdmime.QEncoding.Encode("utf-8", "Zählerablesung") + "\r\nMIME-Version: 1.0\r\n"
}

// The German prose makes the HTML detector prefer Windows-1252 over valid UTF-8.
// Short fragments do not reproduce the reported corruption.
func TestParse_DeclaredUTF8Bodies(t *testing.T) {
	for _, charset := range []string{"utf-8", "UTF-8", "UtF8", "unicode-1-1-utf-8", "utf8mb4"} {
		for _, transfer := range []string{"quoted-printable", "base64", "8bit"} {
			t.Run(charset+"/"+transfer, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				msg := mustParse(t, []byte(charsetHeaders()+charsetPart(t, charset, transfer, charsetHTML)))
				assert.Equal(charsetHTML, msg.BodyHTML)
				assert.Equal(charsetText, msg.BodyText)
				assert.Equal(charsetText, msg.GetBodyText())
				assert.Equal("Zählerablesung", msg.Subject)
				require.Len(msg.From, 1)
				assert.Equal("Example Grüße", msg.From[0].Name)
				assert.NotContains(strings.Join(msg.Errors, "\n"), enmime.ErrorCharsetDeclaration)
				assert.Contains(strings.Join(msg.Errors, "\n"), enmime.ErrorPlainTextFromHTML)
			})
		}
	}
}

func TestParse_DeclaredUTF8NestedBodyPreservesAttachments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	body := charsetPart(t, "utf-8", "quoted-printable", charsetHTML)
	raw := []byte(charsetHeaders() + "Content-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPreferred plain body.\r\n" +
		"--alt\r\n" + body + "\r\n--alt--\r\n" +
		"--outer\r\nContent-Disposition: attachment; filename=explicit.html\r\n" + body + "\r\n" +
		"--outer\r\nContent-Disposition: inline; filename=inline.html\r\nContent-ID: <file@example.org>\r\n" + body + "\r\n--outer--\r\n")
	baseline, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	require.NoError(err)
	msg := mustParse(t, raw)
	assert.Equal("Preferred plain body.", msg.BodyText)
	assert.Equal(charsetHTML, msg.BodyHTML)
	parts := baseline.Attachments
	parts = append(parts, baseline.Inlines...)
	require.Len(parts, 2)
	require.Len(msg.Attachments, 2)
	for i, part := range parts {
		got := msg.Attachments[i]
		assert.Equal(part.FileName, got.Filename)
		assert.Equal(part.Content, got.Content)
		assert.Equal(len(part.Content), got.Size)
		digest := sha256.Sum256(part.Content)
		assert.Equal(hex.EncodeToString(digest[:]), got.ContentHash)
	}
	// The attachments still report their original detector decisions.
	assert.Contains(strings.Join(msg.Errors, "\n"), enmime.ErrorCharsetDeclaration)
}

func TestParse_CharsetRecoveryControls(t *testing.T) {
	for _, tc := range []struct{ name, charset, body, want string }{
		{"invalid UTF-8 declaration", "utf-8", strings.ReplaceAll(strings.ReplaceAll(charsetHTML, "ö", "\xf6"), "Grüßen", "Gr\xfc\xdfen"), charsetHTML},
		{"declared Latin-1", "iso-8859-1", strings.ReplaceAll(strings.ReplaceAll(charsetHTML, "ö", "\xf6"), "Grüßen", "Gr\xfc\xdfen"), charsetHTML},
		{"undeclared charset", "", "<p>Café</p>", "<p>Café</p>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part := charsetPart(t, tc.charset, "quoted-printable", tc.body)
			if tc.charset == "" {
				part = strings.Replace(part, "; charset=", "", 1)
			}
			msg := mustParse(t, []byte(charsetHeaders()+part))
			assert.Equal(t, tc.want, msg.BodyHTML)
			if tc.want == charsetHTML {
				assert.Equal(t, charsetText, msg.BodyText)
			} else {
				assert.Equal(t, "Café", msg.BodyText)
			}
		})
	}
}

func TestRestoreDeclaredUTF8PreservesOtherWarnings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	raw := []byte(charsetHeaders() + charsetPart(t, "utf-8", "quoted-printable", charsetHTML))
	root, err := envelopeParser.ReadParts(bytes.NewReader(raw))
	require.NoError(err)
	require.Equal("windows-1252", root.Charset)
	warning := &enmime.Error{Name: enmime.ErrorMalformedHeader, Detail: "synthetic unrelated warning"}
	root.Errors = append(root.Errors, warning)
	restoreDeclaredUTF8(raw, root)
	assert.Equal(charsetHTML, string(root.Content))
	assert.Contains(root.Errors, warning)
	for _, problem := range root.Errors {
		assert.NotEqual(enmime.ErrorCharsetDeclaration, problem.Name)
	}
}

func TestIsUTF8Charset(t *testing.T) {
	for _, charset := range []string{"utf-8", "UTF-8", "uTf8", "unicode-1-1-utf-8", "UTF8MB4"} {
		assert.True(t, isUTF8Charset(charset), charset)
	}
	for _, charset := range []string{"", "windows-1252", "iso-8859-1", "utf-16"} {
		assert.False(t, isUTF8Charset(charset), charset)
	}
}
