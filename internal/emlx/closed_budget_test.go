package emlx

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func TestEmlxAttachmentReplacementFitsExactBudget(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		for _, tc := range []struct {
			name    string
			body    string
			ending  string
			content []byte
			encoded string
		}{
			{"closed", "\n", "--b--\n", bytes.Repeat([]byte("x"), 60), strings.Repeat("eHh4", 19) + "\neHh4\n"},
			{"EOF", "\n", "", bytes.Repeat([]byte("x"), 60), strings.Repeat("eHh4", 19) + "\neHh4"},
			{"empty", "", "--b--\n", nil, "\n"},
		} {
			t.Run(fmt.Sprintf("%s/newline=%q", tc.name, nl), func(t *testing.T) {
				r, a := require.New(t), assert.New(t)
				prefix := "Content-Type: multipart/mixed; boundary=b\n\n" +
					"--b\nContent-Type: application/octet-stream\n" +
					"Content-Disposition: attachment; filename=part.bin\n"
				original := []byte(strings.ReplaceAll(prefix+fmt.Sprintf("X-Apple-Content-Length: %d\n\n", len(tc.content))+tc.body+tc.ending, "\n", nl))
				want := []byte(strings.ReplaceAll(prefix+"Content-Transfer-Encoding: base64\n\n"+tc.encoded+tc.ending, "\n", nl))
				if tc.ending == "" {
					want = append(want, strings.TrimSuffix(nl, "\n")...)
				}
				path := writePartial(t, t.TempDir(), 7, string(original), map[string][]byte{"1/part.bin": tc.content})
				limit := int64(len(want))

				parsed, err := ParseFile(path, limit)
				r.NoError(err)
				a.Equal(string(want), string(parsed.Raw))
				a.Equal(1, parsed.RestoredAttachments)
				merged, err := MergeAttachmentsFromFile(t.Context(), original, original, path, limit)
				r.NoError(err)
				a.Equal(string(want), string(merged.Raw))
				a.Equal(1, merged.ChangedParts)
				a.False(merged.Incomplete)
				if len(tc.content) > 0 {
					message, err := mime.ParseWithRecovery(merged.Raw, "")
					r.NoError(err)
					r.Len(message.Attachments, 1)
					a.Equal(tc.content, message.Attachments[0].Content)
				}

				tooSmall, err := ParseFile(path, limit-1)
				r.NoError(err)
				a.Equal(string(original), string(tooSmall.Raw), "one byte short must retain the placeholder")
				a.Zero(tooSmall.RestoredAttachments)
			})
		}
	}
}
