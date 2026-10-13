package mime

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hasApplePlaceholder(parts []PartFingerprint) bool {
	return slices.ContainsFunc(parts, func(part PartFingerprint) bool {
		return part.HasAppleContentLength
	})
}

func TestPartFingerprints_AppleContentLengthHeaders(t *testing.T) {
	marker := "X-Apple-Content-Length: 12"
	attachment := "Content-Type: application/octet-stream\n" +
		"Content-Disposition: attachment; filename=notes.bin\n"
	outer := "Content-Type: multipart/mixed; boundary=outer\n\n"
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"root", marker + "\n\n", true},
		{"empty value", "x-apple-content-length:\n\n", true},
		{"folded malformed value", "X-aPpLe-CoNtEnT-lEnGtH:\n\tunknown\n\n", true},
		{"attachment", outer + "--outer\n" + attachment + marker + "\n\n\n--outer--\n", true},
		{"nested multipart", outer + "--outer\nContent-Type: multipart/mixed; boundary=inner\n\n" +
			"--inner\n" + attachment + marker + "\n\n\n--inner--\n--outer--\n", true},
		{"subject", "Subject: " + marker + "\n\n", false},
		{"body", "Content-Type: text/plain\n\n" + marker, false},
		{"preamble", outer + marker + "\n--outer\nContent-Type: text/plain\n\nBody\n--outer--\n", false},
		{"epilogue", outer + "--outer\nContent-Type: text/plain\n\nBody\n--outer--\n" + marker, false},
		{"encoded attachment", outer + "--outer\n" + attachment + "Content-Transfer-Encoding: base64\n\n" +
			base64.StdEncoding.EncodeToString([]byte(marker)) + "\n--outer--\n", false},
		{"similar header", "X-Apple-Content-Length-Example: 12\n\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, newline := range []string{"\n", "\r\n"} {
				raw := []byte(strings.ReplaceAll(tc.raw, "\n", newline))
				_, parts, err := ParseWithRecoveryAndPartFingerprints(raw, "")
				require.NoError(t, err)
				require.NotEmpty(t, parts)
				assert.Equal(t, tc.want, hasApplePlaceholder(parts))
			}
		})
	}
}

func TestPartFingerprints_UnavailableAfterFatalError(t *testing.T) {
	parsed, parts, err := ParseWithRecoveryAndPartFingerprints([]byte("Message-ID: <recovered@example.test>\n"+
		"X-Apple-Content-Length: 12\n"+
		"Content-Type: multipart mixed; boundary=outer\n\n--outer--\n"), "")
	require.Error(t, err)
	assert.Equal(t, "<recovered@example.test>", parsed.MessageID)
	assert.Empty(t, parts, "recovery cannot inspect the full MIME tree")
}

func TestPartFingerprints_AppleContentLengthWithNonfatalDiagnostic(t *testing.T) {
	parsed, parts, err := ParseWithRecoveryAndPartFingerprints([]byte("Content-Type: multipart/mixed; boundary=outer\n\n"+
		"--outer\nContent-Type: text plain\nX-Apple-Content-Length: 12\n\n\n--outer--\n"), "")
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Errors, "fixture must exercise tolerated MIME diagnostics")
	assert.True(t, hasApplePlaceholder(parts))
}

// Header placement supplies the oracle; payload bytes cannot create a header.
func FuzzPartFingerprints_AppleContentLength(f *testing.F) {
	f.Add(uint32(0), false, false, false, []byte("body"))
	f.Add(^uint32(0), true, true, true, []byte("body"))
	f.Fuzz(func(t *testing.T, mask uint32, inHeader, nested, crlf bool, content []byte) {
		header := []byte("x-apple-content-length")
		for i, ch := range header {
			if mask&(1<<i) != 0 && ch >= 'a' && ch <= 'z' {
				header[i] = ch - ('a' - 'A')
			}
		}
		content = content[:min(len(content), 4096)]
		marker := string(header) + ": 12\n"
		raw := "Content-Type: text/plain\n"
		if inHeader {
			raw += marker
		}
		raw += "\n" + marker + base64.StdEncoding.EncodeToString(content) + "\n"
		if nested {
			raw = "Content-Type: multipart/mixed; boundary=outer\n\n--outer\n" + raw + "--outer--\n"
		}
		if crlf {
			raw = strings.ReplaceAll(raw, "\n", "\r\n")
		}
		_, parts, err := ParseWithRecoveryAndPartFingerprints([]byte(raw), "")
		require.NoError(t, err)
		require.NotEmpty(t, parts)
		assert.Equal(t, inHeader, hasApplePlaceholder(parts))
	})
}
