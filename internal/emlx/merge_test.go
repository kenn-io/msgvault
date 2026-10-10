package emlx

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func TestEmlxUnclosedMergeFitsExactBudget(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	content := bytes.Repeat([]byte("x"), 60)
	prefix := "Content-Type: multipart/mixed; boundary=b\n\n" +
		"--b\nContent-Type: text/plain\n\nbody\n" +
		"--b\nContent-Type: application/octet-stream\nContent-Disposition: attachment; filename=final.bin\n"
	original := []byte(prefix + "X-Apple-Content-Length: 60\n\n")
	encoded := base64.StdEncoding.EncodeToString(content)
	want := []byte(prefix + "Content-Transfer-Encoding: base64\n\n" + encoded[:76] + "\n" + encoded[76:])
	r.Greater(len(want), len(original), "the final replacement must grow")
	path := writePartial(t, t.TempDir(), 7, string(original), map[string][]byte{"2/final.bin": content})
	parsed, err := ParseFile(path, 128<<20)
	r.NoError(err)
	r.Equal(want, parsed.Raw, "fixture is independently encoded and reaches the real restorer")
	merged, err := MergeAttachmentsFromFile(t.Context(), original, original, path, int64(len(want)), nil)
	r.NoError(err)
	a.Equal(want, merged.Raw, "an EOF part has no final LF to charge")
	a.Equal(1, merged.ChangedParts)
	a.False(merged.Incomplete)
}

func TestEmlxMergeBudgetPreservesArchived(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	original := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: application/octet-stream\r\nX-Apple-Content-Length: 3\r\n\r\n\r\n--b\r\nContent-Type: application/octet-stream\r\nX-Apple-Content-Length: 3\r\n\r\n\r\n--b--\r\n")
	old := bytes.Replace(original, []byte("X-Apple-Content-Length: 3\r\n\r\n\r\n"), []byte("Content-Transfer-Encoding: base64\r\n\r\nb2xk\r\n"), 1)
	newRaw := append([]byte(nil), original...)
	at := bytes.LastIndex(newRaw, []byte("X-Apple-Content-Length: 3\r\n\r\n\r\n"))
	newRaw = append(append(append([]byte(nil), newRaw[:at]...), []byte("Content-Transfer-Encoding: base64\r\n\r\nbmV3\r\n")...), newRaw[at+len("X-Apple-Content-Length: 3\r\n\r\n\r\n"):]...)
	parts := []RestorationPart{{Key: "1", State: RestorationMissing}, {Key: "2", State: RestorationSupplied}}
	limited, err := MergeAttachments(original, newRaw, old, parts, int64(len(old)), nil)
	r.NoError(err)
	a.True(limited.Incomplete)
	a.Equal(old, limited.Raw)
	merged, err := MergeAttachments(original, newRaw, old, parts, int64(len(old)+100), nil)
	r.NoError(err)
	a.False(merged.Incomplete)
	a.Contains(string(merged.Raw), "b2xk")
	a.Contains(string(merged.Raw), "bmV3")
	lowered, err := MergeAttachments(original, original, merged.Raw, []RestorationPart{{Key: "1", State: RestorationMissing}, {Key: "2", State: RestorationMissing}}, 1, nil)
	r.NoError(err)
	a.Equal(merged.Raw, lowered.Raw)
}

func TestEmlxMergePartIdentityAndAmbiguousLayout(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	original := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=same.bin\r\nX-Apple-Content-Length: 3\r\n\r\n\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=same.bin\r\nX-Apple-Content-Length: 3\r\n\r\n\r\n--b--\r\n")
	old := bytes.Replace(original, []byte("X-Apple-Content-Length: 3\r\n\r\n\r\n"), []byte("Content-Transfer-Encoding: base64\r\n\r\nb2xk\r\n"), 1)
	candidate := bytes.ReplaceAll(original, []byte("X-Apple-Content-Length: 3\r\n\r\n\r\n"), []byte("Content-Transfer-Encoding: base64\r\n\r\nbmV3\r\n"))
	merged, err := MergeAttachments(original, candidate, old, []RestorationPart{{Key: "1", State: RestorationMissing}, {Key: "2", State: RestorationSupplied}}, 4096, nil)
	r.NoError(err)
	parsed, err := mime.Parse(merged.Raw)
	r.NoError(err)
	r.Len(parsed.Attachments, 2)
	a.Equal([]byte("old"), parsed.Attachments[0].Content)
	a.Equal([]byte("new"), parsed.Attachments[1].Content)
	broken := bytes.Replace(old, []byte("--b--"), []byte("--different--"), 1)
	rejected, err := MergeAttachments(original, candidate, broken, []RestorationPart{{Key: "2", State: RestorationSupplied}}, 4096, nil)
	r.Error(err)
	a.Equal(broken, rejected.Raw)
}

// Splitting is byte preserving even when a parser-recoverable message reaches
// EOF instead of a closing delimiter, or a delimiter has trailing MIME LWSP.
func FuzzEmlxSplitPreservesRaw(f *testing.F) {
	f.Add([]byte("synthetic body"), uint8(0), false)
	f.Add([]byte("synthetic body"), uint8(1), true)
	f.Add([]byte("synthetic body"), uint8(2), false)
	f.Fuzz(func(t *testing.T, body []byte, ending uint8, crlf bool) {
		body = body[:min(len(body), 8192)]
		nl := "\n"
		if crlf {
			nl = "\r\n"
		}
		raw := append([]byte("Content-Type: multipart/mixed; boundary=b"+nl+nl+"--b"+nl+"Content-Type: text/plain"+nl+nl), body...)
		switch ending % 3 {
		case 1:
			raw = append(raw, []byte(nl+"--b--"+nl)...)
		case 2:
			raw = append(raw, []byte(nl+"--b-- \t"+nl)...)
		}
		parts, err := splitParts(raw)
		require.NoError(t, err)
		assert.Equal(t, raw, bytes.Join(parts, nil))
	})
}
