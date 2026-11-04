package mime

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNULProjections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw, err := os.ReadFile("testdata/nul.eml")
	require.NoError(err)
	original := bytes.Clone(raw)
	msg, fingerprints, err := ParseWithRecoveryAndPartFingerprints(raw, "")
	require.NoError(err)
	assert.Equal("Report�tail", msg.Subject)
	require.Len(msg.From, 1)
	assert.Equal("Sender�Name", msg.From[0].Name)
	assert.Equal("sender@example.org", msg.From[0].Email)
	assert.Equal("beforetoken�aftertoken", msg.GetBodyText())
	assert.Equal("<p>htmlbefore�htmlafter</p>", msg.BodyHTML)
	require.Len(msg.Attachments, 1)
	attachment := msg.Attachments[0]
	assert.Equal("file�name.bin", attachment.Filename)
	assert.Equal([]byte{0, 255, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}, attachment.Content)
	assert.Equal(fmt.Sprintf("%x", sha256.Sum256(attachment.Content)), attachment.ContentHash)
	assert.Equal(original, raw)
	require.NotEmpty(fingerprints)
	// Equal display projections must not make different originals fingerprint alike.
	visible := bytes.ReplaceAll(raw, []byte("YmVmb3JldG9rZW4AYWZ0ZXJ0b2tlbg=="), []byte("YmVmb3JldG9rZW7vv71hZnRlcnRva2Vu"))
	other, otherFingerprints, err := ParseWithRecoveryAndPartFingerprints(visible, "")
	require.NoError(err)
	assert.Equal(msg.BodyText, other.BodyText)
	assert.NotEqual(fingerprints, otherFingerprints)
}

func TestParseWithRecoveryNUL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, subject := range []string{"Subject: =?UTF-8?Q?Report=00tail?=\r\n", ""} {
		raw := []byte(subject + "From: =?UTF-8?Q?Sender=00Name?= <sender@example.org>\r\nContent-Type: multipart/mixed\r\n\r\nbody")
		msg, err := ParseWithRecovery(raw, "Report\x00tail")
		require.Error(err, "normalization must not turn a malformed envelope into a successful parse")
		assert.Equal("Report�tail", msg.Subject)
		require.Len(msg.From, 1)
		assert.Equal("Sender�Name", msg.From[0].Name)
		assert.NotContains(msg.BodyText, "\x00")
	}
}

func TestParseNULBodyEncodings(t *testing.T) {
	const text = "\x00alpha\x00\x00omega\x00 � \\u0000\tend"
	const want = "�alpha��omega� � \\u0000\tend"
	for _, contentType := range []string{"text/plain", "text/html"} {
		for _, encoding := range []string{"base64", "quoted-printable"} {
			t.Run(contentType+"/"+encoding, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				body := base64.StdEncoding.EncodeToString([]byte(text))
				if encoding == "quoted-printable" {
					body = "=00alpha=00=00omega=00 =EF=BF=BD \\u0000=09end"
				}
				raw := []byte("MIME-Version: 1.0\r\nContent-Type: " + contentType + "; charset=utf-8\r\nContent-Transfer-Encoding: " + encoding + "\r\n\r\n" + body)
				msg, err := Parse(raw)
				require.NoError(err)
				if contentType == "text/plain" {
					assert.Equal(want, msg.BodyText)
				} else {
					assert.Equal(want, msg.BodyHTML)
				}
			})
		}
	}
}

func TestParseNULContentIDAndGroups(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw, err := os.ReadFile("testdata/nul.eml")
	require.NoError(err)
	raw = bytes.ReplaceAll(raw, []byte("Content-Type: application/octet-stream"),
		[]byte("Content-ID: <bad\x00id@example.org>\r\nContent-Type: application/octet-stream"))
	msg, err := Parse(raw)
	require.NoError(err)
	require.Len(msg.Attachments, 1)
	msg.OmitNULIdentities()
	assert.Empty(msg.Attachments[0].ContentID)
	groups := ParseGoogleGroupsHeaders([]byte("X-Google-Groups: group\r\nX-Gmail-Labels: =?UTF-8?Q?Label=00tail?=\r\n\r\n"), "")
	assert.Equal([]string{"group", "Label�tail"}, groups.Labels)
	groups = ParseGoogleGroupsHeaders([]byte("X-Google-Groups: bad\x00group\r\n\r\n"), "")
	assert.Empty(groups.Group, "damaged group identity must not be fabricated")
}

func TestNULIdentityHeaders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, bad := range []string{"bad\x00id", "bad�id"} {
		raw := []byte("Message-ID: <" + bad + "@example.org>\r\n" +
			"In-Reply-To: <" + bad + "@example.org>\r\n" +
			"References: <" + bad + "@example.org> <good@example.org>\r\n" +
			"List-ID: <" + bad + ".example.org>\r\n\r\nbody")
		msg, err := ParseWithRecovery(raw, "")
		require.NoError(err)
		mid, reply, refs := ParseThreadingHeaders(raw)
		msg.OmitNULIdentities()
		if strings.ContainsRune(bad, 0) {
			assert.Equal(bad+"@example.org", mid, "historical extraction retains exact evidence")
			assert.Equal(mid, reply)
			assert.Equal([]string{mid, "good@example.org"}, refs)
			assert.Empty(msg.MessageID)
			assert.Empty(msg.InReplyTo)
			assert.Empty(msg.ListID)
			assert.Equal([]string{"good@example.org"}, msg.References)
		} else {
			assert.NotEmpty(msg.MessageID, "literal replacement characters are not NUL")
			assert.NotEmpty(mid)
		}
	}
	// A broken address token must not yield a valid-looking suffix identity.
	msg, err := Parse([]byte("From: bad\x00sender@example.org, good@example.org\r\n\r\nbody"))
	require.NoError(err)
	assert.Equal([]Address{{Email: "good@example.org", Domain: "example.org"}}, msg.From)
}
