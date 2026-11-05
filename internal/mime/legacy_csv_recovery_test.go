package mime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestLegacyCSVRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := testemail.LegacyCSV(true, testemail.LegacyCSVOptions{})
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

func TestLegacyCSVRecoveryPreservesPayloadSignature(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	payload := "first,last\r\n\"MIME example:\r\n" +
		"--example\r\n--------------040908010102080206010104\r\n" +
		"Content-Type: text/csv; name=example.csv\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment; filename=example.csv\r\n\r\n" +
		"Sample,Person\",example"
	raw := testemail.LegacyCSV(true, testemail.LegacyCSVOptions{Payload: strings.ReplaceAll(payload, "=", "=3D")})
	original := bytes.Clone(raw)
	msg, err := ParseWithRecovery(raw, "")
	require.NoError(err)
	require.Len(msg.Attachments, 1)
	assert.Equal([]byte(payload), msg.Attachments[0].Content)
	assert.Equal(original, raw)
}

func TestLegacyCSVRecoveryMatchesValidMail(t *testing.T) {
	for name, shape := range map[string]testemail.LegacyCSVOptions{
		"long QP row":    {Payload: "first,last\r\n" + strings.Repeat("x", 5000) + "=2CPerson"},
		"PNG attachment": {ExtraPNG: true},
		"plain body":     {BodyType: "text/plain"},
		"extra header":   {Header: "Content-Description: exported contacts\r\n"},
		"folded header":  {Header: "Content-Description: exported\r\n\tcontacts\r\n"},
		"leading space":  {StrayLine: " --------------040908010102080206010104"},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			expected, err := Parse(testemail.LegacyCSV(false, shape))
			require.NoError(err)
			raw := testemail.LegacyCSV(true, shape)
			original := bytes.Clone(raw)
			recovered, err := ParseWithRecovery(raw, "unused fallback")
			require.NoError(err)
			assert.Equal(expected, recovered)
			assert.Equal(original, raw)
		})
	}
}

func TestLegacyCSVRecoveryFallsBack(t *testing.T) {
	for name, shape := range map[string]testemail.LegacyCSVOptions{
		"unknown delimiter":       {StrayLine: "--unrecognized"},
		"two malformed CSV parts": {TwoCSVs: true},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			raw := testemail.LegacyCSV(true, shape)
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
		})
	}
}
