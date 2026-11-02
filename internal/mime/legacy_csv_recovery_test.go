package mime

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recoveryCSV = "first,last\r\nSample,Person\r\n"

func legacyCSVFixture(missingSeparators bool) []byte {
	separator := "\r\n"
	pdf := ""
	if missingSeparators {
		separator = ""
		pdf = "--related\r\nContent-Type: application/pdf\r\n" + recoveryCSV
	}
	return []byte("From: sender@example.org\r\nTo: recipient@example.org\r\n" +
		"Subject: recovery fixture\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/related; boundary=related\r\n\r\n" +
		"--related\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Example body</p>\r\n" + pdf +
		"--related\r\n--------------040908010102080206010104\r\n" +
		"Content-Type: text/csv; name=example.csv\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"Content-Disposition: attachment; filename=example.csv\r\n" + separator + recoveryCSV +
		"--related--\r\n")
}

func TestLegacyCSVRecovery(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_separators_%t", missing), func(t *testing.T) {
			raw := legacyCSVFixture(missing)
			original := bytes.Clone(raw)
			_, err := Parse(raw)
			require.Error(t, err, "strict parsing must retain its existing behavior")
			msg, err := ParseWithRecovery(raw, "unused fallback")
			require.NoError(t, err)
			assert.Equal(t, "recovery fixture", msg.Subject)
			assert.Equal(t, "<p>Example body</p>", strings.TrimSpace(msg.BodyHTML))
			require.Len(t, msg.Attachments, 1)
			assert.Equal(t, "text/csv", msg.Attachments[0].ContentType)
			assert.Equal(t, strings.TrimSpace(recoveryCSV), strings.TrimSpace(string(msg.Attachments[0].Content)))
			assert.Contains(t, msg.Errors, legacyCSVRecoveryNotice)
			assert.Equal(t, original, raw)
			_, fingerprints, err := ParseWithRecoveryAndPartFingerprints(raw, "")
			require.NoError(t, err)
			assert.Empty(t, fingerprints, "repaired framing is not evidence of original framing")
		})
	}
}

func TestLegacyCSVRecoveryRejectsAmbiguousInputs(t *testing.T) {
	base := string(legacyCSVFixture(false))
	cases := map[string]string{
		"unknown delimiter":     strings.ReplaceAll(base, "--------------040908010102080206010104", "--unrecognized"),
		"empty CSV":             strings.Replace(base, recoveryCSV, "\r\n", 1),
		"missing HTML":          strings.Replace(base, "Content-Type: text/html", "Content-Type: text/plain", 1),
		"undeclared parent":     strings.Replace(base, "boundary=related", "boundary=another", 1),
		"different PDF data":    strings.Replace(string(legacyCSVFixture(true)), recoveryCSV, "first,last\r\nOther,Data\r\n", 1),
		"additional attachment": strings.Replace(base, "--related--", "--related\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=second.bin\r\n\r\nextra\r\n--related--", 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Nil(t, recoverLegacyCSV([]byte(raw)))
		})
	}
	_, err := ParseWithRecovery([]byte(cases["empty CSV"]), "")
	assert.Error(t, err, "unsupported malformed messages retain the placeholder recovery path")
}

func TestLegacyCSVRecoveryDoesNotChangeValidMail(t *testing.T) {
	valid := bytes.ReplaceAll(legacyCSVFixture(false), []byte("--------------040908010102080206010104\r\n"), nil)
	cases := [][]byte{
		valid,
		[]byte("From: sender@example.org\r\nSubject: plain\r\n\r\nOrdinary body\r\n"),
		[]byte("Subject: signature in plain text\r\n\r\n" + string(legacyCSVFixture(false))),
	}
	for i, raw := range cases {
		t.Run(fmt.Sprintf("valid_%d", i), func(t *testing.T) {
			original := bytes.Clone(raw)
			strict, err := Parse(raw)
			require.NoError(t, err)
			recovered, err := ParseWithRecovery(raw, "fallback")
			require.NoError(t, err)
			assert.Equal(t, strict, recovered)
			assert.Equal(t, original, raw)
		})
	}
}
