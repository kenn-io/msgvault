package emlx

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestParse_ValidEmlxWithPlist(t *testing.T) {
	require := requirepkg.New(t)
	mime := "From: alice@example.com\r\nSubject: Hello\r\n\r\nBody\r\n"
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>date-sent</key>
	<real>252460800</real>
	<key>flags</key>
	<integer>8590195713</integer>
	<key>original-mailbox</key>
	<string>imap://user@example.com/INBOX</string>
</dict>
</plist>`

	data := fmt.Sprintf("%d\n%s%s", len(mime), mime, plist)

	msg, err := Parse([]byte(data))
	require.NoError(err, "Parse")
	require.Equal(mime, string(msg.Raw))

	// date-sent 252460800 seconds from Apple epoch (2001-01-01)
	// = 2009-01-01 00:00:00 UTC
	wantDate := time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
	require.True(msg.PlistDate.Equal(wantDate), "PlistDate = %v, want %v", msg.PlistDate, wantDate)
	require.Equal(8590195713, msg.Flags)
	require.Equal("imap://user@example.com/INBOX", msg.OrigMailbox)
}

func TestParse_ValidEmlxNoPlist(t *testing.T) {
	mime := "From: alice@example.com\r\nSubject: Hello\r\n\r\nBody\r\n"
	data := fmt.Sprintf("%d\n%s", len(mime), mime)

	msg, err := Parse([]byte(data))
	requirepkg.NoError(t, err, "Parse")
	requirepkg.Equal(t, mime, string(msg.Raw))
	requirepkg.True(t, msg.PlistDate.IsZero(), "PlistDate = %v, want zero", msg.PlistDate)
}

func TestParse_ByteCountMismatch(t *testing.T) {
	// Byte count is larger than available data.
	data := "9999\nshort"
	_, err := Parse([]byte(data))
	requirepkg.Error(t, err, "expected error for byte count mismatch")
}

func TestParse_NonNumericByteCount(t *testing.T) {
	data := "abc\nFrom: test\r\n\r\n"
	_, err := Parse([]byte(data))
	requirepkg.Error(t, err, "expected error for non-numeric byte count")
}

func TestParse_ZeroByteCount(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict></dict></plist>`
	data := "0\n" + plist

	msg, err := Parse([]byte(data))
	requirepkg.NoError(t, err, "Parse")
	requirepkg.Empty(t, msg.Raw)
}

func TestParse_EmptyFile(t *testing.T) {
	_, err := Parse([]byte{})
	requirepkg.Error(t, err, "expected error for empty file")
}

func TestParse_NoNewline(t *testing.T) {
	_, err := Parse([]byte("42"))
	requirepkg.Error(t, err, "expected error for missing newline")
}

func TestParse_NegativeByteCount(t *testing.T) {
	data := "-1\nstuff"
	_, err := Parse([]byte(data))
	requirepkg.Error(t, err, "expected error for negative byte count")
}

func TestParse_PlistWithIntegerDateSent(t *testing.T) {
	mime := "From: test@example.com\r\n\r\n"
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>date-sent</key>
	<integer>252460800</integer>
</dict>
</plist>`
	data := fmt.Sprintf("%d\n%s%s", len(mime), mime, plist)

	msg, err := Parse([]byte(data))
	requirepkg.NoError(t, err, "Parse")
	wantDate := time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
	requirepkg.True(t, msg.PlistDate.Equal(wantDate), "PlistDate = %v, want %v", msg.PlistDate, wantDate)
}

func TestParse_MalformedPlist(t *testing.T) {
	mime := "From: test@example.com\r\n\r\n"
	// Garbage after MIME — not valid XML.
	data := fmt.Sprintf("%d\n%sNOT XML AT ALL", len(mime), mime)

	msg, err := Parse([]byte(data))
	requirepkg.NoError(t, err, "should succeed with best-effort plist")
	assertpkg.Equal(t, mime, string(msg.Raw))
	assertpkg.True(t, msg.PlistDate.IsZero(), "PlistDate should be zero for malformed plist")
}

func TestParseFile(t *testing.T) {
	mime := "From: alice@example.com\r\nSubject: Test\r\n\r\nHi\r\n"
	data := fmt.Sprintf("%d\n%s", len(mime), mime)

	dir := t.TempDir()
	path := filepath.Join(dir, "1234.emlx")
	requirepkg.NoError(t, os.WriteFile(path, []byte(data), 0600), "write")

	msg, err := ParseFile(path)
	requirepkg.NoError(t, err, "ParseFile")
	requirepkg.Equal(t, mime, string(msg.Raw))
}

func TestParseFile_NotFound(t *testing.T) {
	_, err := ParseFile("/nonexistent/12345.emlx")
	requirepkg.Error(t, err, "expected error for missing file")
}

func TestParse_ExtremeByteCount(t *testing.T) {
	// A declared byte count near MaxInt64 must return an error, not panic.
	data := "9223372036854775807\nshort"
	_, err := Parse([]byte(data))
	requirepkg.Error(t, err, "expected error for extreme byte count")
}

func TestParse_WhitespaceAroundByteCount(t *testing.T) {
	mime := "From: test@example.com\r\n\r\n"
	// Byte count with leading/trailing spaces.
	data := fmt.Sprintf("  %d  \n%s", len(mime), mime)

	msg, err := Parse([]byte(data))
	requirepkg.NoError(t, err, "Parse")
	requirepkg.Equal(t, mime, string(msg.Raw))
}
