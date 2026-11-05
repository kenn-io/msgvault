package mime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type legacyCSVShape struct {
	payload   string
	bodyType  string
	header    string
	extraPNG  bool
	twoCSVs   bool
	strayLine string
}

func legacyCSVFixture(withStrayLine bool, shape legacyCSVShape) []byte {
	stray := ""
	if withStrayLine {
		stray = shape.strayLine
		if stray == "" {
			stray = "--------------040908010102080206010104"
		}
		stray += "\r\n"
	}
	if shape.bodyType == "" {
		shape.bodyType = "text/html"
	}
	if shape.payload == "" {
		shape.payload = "first,last\r\nSample,Person"
	}
	csv := "--related\r\n" + stray +
		"Content-Type: text/csv; name=example.csv\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment; filename=example.csv\r\n" + shape.header +
		"\r\n" + shape.payload + "\r\n"
	if shape.twoCSVs {
		csv += csv
	}
	extra := ""
	if shape.extraPNG {
		extra = "--related\r\nContent-Type: image/png\r\n" +
			"Content-Transfer-Encoding: base64\r\n" +
			"Content-Disposition: attachment; filename=example.png\r\n\r\niVBORw0KGgo=\r\n"
	}
	return []byte("From: sender@example.org\r\nTo: recipient@example.org\r\n" +
		"Subject: recovery fixture\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=related\r\n\r\n" +
		"--related\r\nContent-Type: " + shape.bodyType + "; charset=utf-8\r\n\r\n<p>Example body</p>\r\n" +
		csv + extra + "--related--\r\n")
}

func TestLegacyCSVRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := legacyCSVFixture(true, legacyCSVShape{})
	original := bytes.Clone(raw)
	_, err := Parse(raw)
	require.ErrorContains(err, "malformed MIME header initial line")
	msg, err := ParseWithRecovery(raw, "unused fallback")
	require.NoError(err)
	assert.Equal("<p>Example body</p>", strings.TrimSpace(msg.BodyHTML))
	require.Len(msg.Attachments, 1)
	assert.Equal("text/csv", msg.Attachments[0].ContentType)
	assert.Equal([]byte("first,last\r\nSample,Person"), msg.Attachments[0].Content)
	assert.Equal(original, raw)
	_, fingerprints, err := ParseWithRecoveryAndPartFingerprints(raw, "")
	require.NoError(err)
	assert.Empty(fingerprints, "repaired framing is not evidence of original framing")
	assert.Equal(original, raw)
}

func TestLegacyCSVRecoveryMatchesValidMail(t *testing.T) {
	for name, shape := range map[string]legacyCSVShape{
		"long QP row":    {payload: "first,last\r\n" + strings.Repeat("x", 5000) + "=2CPerson"},
		"PNG attachment": {extraPNG: true},
		"plain body":     {bodyType: "text/plain"},
		"extra header":   {header: "Content-Description: exported contacts\r\n"},
		"folded header":  {header: "Content-Description: exported\r\n\tcontacts\r\n"},
		"two CSV parts":  {twoCSVs: true},
		"leading space":  {strayLine: " --------------040908010102080206010104"},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			expected, err := Parse(legacyCSVFixture(false, shape))
			require.NoError(err)
			raw := legacyCSVFixture(true, shape)
			original := bytes.Clone(raw)
			recovered, err := ParseWithRecovery(raw, "unused fallback")
			require.NoError(err)
			assert.Equal(expected, recovered)
			assert.Equal(original, raw)
		})
	}
}

func TestLegacyCSVRecoveryUnknownDelimiter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := legacyCSVFixture(true, legacyCSVShape{strayLine: "--unrecognized"})
	original := bytes.Clone(raw)
	_, strictErr := Parse(raw)
	require.Error(strictErr)
	msg, err := ParseWithRecovery(raw, "unused fallback")
	require.EqualError(err, strictErr.Error())
	assert.Equal("recovery fixture", msg.Subject)
	assert.Contains(msg.BodyText, "[MIME parsing failed:")
	assert.Empty(msg.BodyHTML)
	assert.Empty(msg.Attachments)
	assert.Equal(original, raw)
}
