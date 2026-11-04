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
			assert := assert.New(t)
			require := require.New(t)
			raw := legacyCSVFixture(missing)
			original := bytes.Clone(raw)
			_, err := Parse(raw)
			require.Error(err, "strict parsing must retain its existing behavior")
			msg, err := ParseWithRecovery(raw, "unused fallback")
			require.NoError(err)
			assert.Equal("recovery fixture", msg.Subject)
			assert.Equal("<p>Example body</p>", strings.TrimSpace(msg.BodyHTML))
			require.Len(msg.Attachments, 1)
			assert.Equal("text/csv", msg.Attachments[0].ContentType)
			assert.Equal(strings.TrimSpace(recoveryCSV), strings.TrimSpace(string(msg.Attachments[0].Content)))
			assert.Contains(msg.Errors, legacyCSVRecoveryNotice)
			assert.Equal(original, raw)
			_, fingerprints, err := ParseWithRecoveryAndPartFingerprints(raw, "")
			require.NoError(err)
			assert.Empty(fingerprints, "repaired framing is not evidence of original framing")
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

func TestLegacyCSVRecoveryPreservesAdditionalHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		raw    []byte
		header string
	}{
		"additional header": {legacyCSVFixture(false), "Content-Description: exported contacts\r\n"},
		"folded header":     {legacyCSVFixture(false), "Content-Description: exported\r\n\tcontacts\r\n"},
		"multiple headers":  {legacyCSVFixture(false), "Content-ID: <export@example.org>\r\nX-Export-Version: 1\r\n"},
		"malformed sibling": {legacyCSVFixture(true), "Content-Description: exported contacts\r\n\r\n"},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			raw := bytes.Replace(tc.raw, []byte("filename=example.csv\r\n"), []byte("filename=example.csv\r\n"+tc.header), 1)
			original := bytes.Clone(raw)
			strict, err := Parse(bytes.Replace(raw, []byte("--------------040908010102080206010104\r\n"), nil, 1))
			require.NoError(err)
			require.Len(strict.Attachments, 1)
			expected := []byte("first,last\r\nSample,Person")
			assert.Equal(expected, strict.Attachments[0].Content)
			recovered, err := ParseWithRecovery(raw, "fallback")
			require.NoError(err)
			require.Len(recovered.Attachments, 1)
			assert.Equal(expected, recovered.Attachments[0].Content)
			assert.Equal(strict.Attachments, recovered.Attachments)
			assert.Equal(original, raw)
		})
	}
}

func TestLegacyCSVRecoveryRejectsAmbiguousSeparatorRepairs(t *testing.T) {
	for name, raw := range map[string][]byte{
		"additional header": bytes.Replace(legacyCSVFixture(false), []byte("filename=example.csv\r\n\r\n"), []byte("filename=example.csv\r\nContent-Description: exported contacts\r\n"), 1),
		"folded header":     bytes.Replace(legacyCSVFixture(false), []byte("filename=example.csv\r\n\r\n"), []byte("filename=example.csv\r\n\tcreation-date=unknown\r\n"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			original := bytes.Clone(raw)
			assert.Nil(t, recoverLegacyCSV(raw))
			_, err := ParseWithRecovery(raw, "fallback")
			require.Error(t, err)
			assert.Equal(t, original, raw)
		})
	}
}

func TestLegacyCSVRecoveryRejectsDifferentPDFWithValidCSVHeaders(t *testing.T) {
	for name, header := range map[string]string{
		"separator only":    "",
		"additional header": "Content-Description: exported contacts\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			raw := bytes.Replace(legacyCSVFixture(true), []byte(recoveryCSV), []byte("first,last\r\nOther,Data\r\n"), 1)
			raw = bytes.Replace(raw, []byte("filename=example.csv\r\n"), []byte("filename=example.csv\r\n"+header+"\r\n"), 1)
			original := bytes.Clone(raw)

			assert.Nil(recoverLegacyCSV(raw))
			assert.Equal(original, raw)
		})
	}
}

func TestLegacyCSVRecoveryPreservesNonHeaderPayload(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_separators_%t", missing), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			payload := []byte("Exported on: example\r\n" + recoveryCSV)
			raw := bytes.ReplaceAll(legacyCSVFixture(missing), []byte(recoveryCSV), payload)
			original := bytes.Clone(raw)
			msg, err := ParseWithRecovery(raw, "fallback")
			require.NoError(err)
			require.Len(msg.Attachments, 1)
			assert.Equal(bytes.TrimSuffix(payload, []byte("\r\n")), msg.Attachments[0].Content)
			assert.Equal(original, raw)
		})
	}
}

func TestLegacyCSVRecoveryRejectsDifferentBoundaryLikePayloads(t *testing.T) {
	for name, prefix := range map[string]string{
		"inline":      "A--related",
		"line prefix": "--related",
	} {
		for ending, newline := range map[string]string{"CRLF": "\r\n", "LF": "\n"} {
			t.Run(name+"/"+ending, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				pdf := []byte("first,last\r\n" + prefix + "Y\r\n")
				csv := []byte("first,last\r\n" + prefix + "X\r\n")
				raw := bytes.Replace(legacyCSVFixture(true), []byte(recoveryCSV), pdf, 1)
				raw = bytes.Replace(raw, []byte(recoveryCSV), csv, 1)
				raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte(newline))
				original := bytes.Clone(raw)

				assert.Nil(recoverLegacyCSV(raw))
				_, err := ParseWithRecovery(raw, "fallback")
				require.Error(err)
				assert.Equal(original, raw)
			})
		}
	}
}

func TestLegacyCSVRecoveryPreservesBoundaryLikePayloads(t *testing.T) {
	for name, line := range map[string]string{
		"inline":              "A--relatedX",
		"line prefix":         "--relatedX",
		"trailing blank line": "A--relatedX\r\n",
	} {
		for ending, newline := range map[string]string{"CRLF": "\r\n", "LF": "\n"} {
			t.Run(name+"/"+ending, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				payload := []byte("first,last\r\n" + line + "\r\n")
				raw := bytes.ReplaceAll(legacyCSVFixture(true), []byte(recoveryCSV), payload)
				raw = bytes.Replace(raw, []byte("--related--\r\n"), []byte("--related-- \t\r\n"), 1)
				raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte(newline))
				original := bytes.Clone(raw)
				expected := bytes.TrimSuffix(bytes.ReplaceAll(payload, []byte("\r\n"), []byte(newline)), []byte(newline))

				msg, err := ParseWithRecovery(raw, "fallback")
				require.NoError(err)
				require.Len(msg.Attachments, 1)
				assert.Equal(expected, msg.Attachments[0].Content)
				assert.Contains(msg.Errors, legacyCSVRecoveryNotice)
				assert.Equal(original, raw)
			})
		}
	}
}

func TestLegacyCSVRecoveryRejectsAmbiguousClosingBoundaryPayloads(t *testing.T) {
	for _, different := range []bool{false, true} {
		for ending, newline := range map[string]string{"CRLF": "\r\n", "LF": "\n"} {
			t.Run(fmt.Sprintf("different_payloads_%t/%s", different, ending), func(t *testing.T) {
				assert := assert.New(t)
				csv := []byte("first,last\r\n--related--X\r\n")
				pdf := csv
				if different {
					pdf = []byte("first,last\r\n--related--Y\r\n")
				}
				raw := bytes.Replace(legacyCSVFixture(true), []byte(recoveryCSV), pdf, 1)
				raw = bytes.Replace(raw, []byte(recoveryCSV), csv, 1)
				raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte(newline))
				original := bytes.Clone(raw)

				assert.Nil(recoverLegacyCSV(raw))
				assert.Equal(original, raw)
			})
		}
	}
}

func TestLegacyCSVRecoveryRejectsClosingBoundaryPayloadWithoutPDF(t *testing.T) {
	for name, marker := range map[string]string{
		"closing prefix":          "--related--X",
		"inline closing prefix":   "A--related--X",
		"delimiter whitespace":    "--related suffix",
		"inline whitespace":       "A--related suffix",
		"later inline whitespace": "A--relatedX B--related suffix",
	} {
		for _, missing := range []bool{false, true} {
			for ending, newline := range map[string]string{"CRLF": "\r\n", "LF": "\n"} {
				t.Run(fmt.Sprintf("%s/missing_separator_%t/%s", name, missing, ending), func(t *testing.T) {
					assert := assert.New(t)
					payload := []byte("first,last\r\n" + marker + "\r\nSample,Person\r\n")
					raw := bytes.Replace(legacyCSVFixture(false), []byte(recoveryCSV), payload, 1)
					if missing {
						raw = bytes.Replace(raw, []byte("filename=example.csv\r\n\r\n"), []byte("filename=example.csv\r\n"), 1)
					}
					raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte(newline))
					original := bytes.Clone(raw)

					assert.Nil(recoverLegacyCSV(raw))
					assert.Equal(original, raw)
				})
			}
		}
	}
}

func TestLegacyCSVRecoveryRejectsDifferentTrailingBlankLines(t *testing.T) {
	for ending, newline := range map[string]string{"CRLF": "\r\n", "LF": "\n"} {
		t.Run(ending, func(t *testing.T) {
			assert := assert.New(t)
			raw := bytes.Replace(legacyCSVFixture(true), []byte(recoveryCSV), []byte(recoveryCSV+"\r\n"), 1)
			raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte(newline))
			original := bytes.Clone(raw)

			assert.Nil(recoverLegacyCSV(raw))
			assert.Equal(original, raw)
		})
	}
}

func TestLegacyCSVRecoveryPreservesExtraCSVNewline(t *testing.T) {
	for _, extra := range []int{1, 2} {
		for ending, newline := range map[string]string{"CRLF": "\r\n", "LF": "\n"} {
			t.Run(fmt.Sprintf("extra_newlines_%d/%s", extra, ending), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				fixture := string(legacyCSVFixture(true))
				csvStart := strings.LastIndex(fixture, recoveryCSV)
				require.NotEqual(-1, csvStart)
				raw := []byte(fixture[:csvStart] + recoveryCSV + strings.Repeat("\r\n", extra) + fixture[csvStart+len(recoveryCSV):])
				raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte(newline))
				original := bytes.Clone(raw)

				if extra == 2 {
					assert.Nil(recoverLegacyCSV(raw))
					assert.Equal(original, raw)
					return
				}
				msg, err := ParseWithRecovery(raw, "fallback")
				require.NoError(err)
				require.Len(msg.Attachments, 1)
				assert.Equal([]byte(strings.ReplaceAll(recoveryCSV, "\r\n", newline)), msg.Attachments[0].Content)
				assert.Contains(msg.Errors, legacyCSVRecoveryNotice)
				assert.Equal(original, raw)
			})
		}
	}
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
			assert := assert.New(t)
			require := require.New(t)
			original := bytes.Clone(raw)
			strict, err := Parse(raw)
			require.NoError(err)
			recovered, err := ParseWithRecovery(raw, "fallback")
			require.NoError(err)
			assert.Equal(strict, recovered)
			assert.Equal(original, raw)
		})
	}
}
