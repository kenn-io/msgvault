package imessage

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/textimport"
	"go.kenn.io/msgvault/internal/textutil"
	"howett.net/plist"
)

// appleEpochOffset is the number of seconds between Unix epoch (1970-01-01)
// and Apple/Core Data epoch (2001-01-01).
const appleEpochOffset int64 = 978307200

// appleTimestampToTime converts an Apple epoch timestamp to time.Time.
// macOS High Sierra+ stores dates as nanoseconds since Apple epoch;
// older versions use seconds. We detect the format by checking magnitude.
// Sentinel values and dates outside years 1 through 9999 have no date.
func appleTimestampToTime(ts int64) time.Time {
	if ts == 0 || ts == math.MinInt64 || ts == math.MaxInt64 {
		return time.Time{}
	}
	// Values > 1e12 are nanoseconds (1e12 ns = ~16 minutes from epoch,
	// while 1e12 seconds from epoch = year ~33700).
	sec, nsec := ts, int64(0)
	if ts > 1_000_000_000_000 || ts < -1_000_000_000_000 {
		sec = ts / 1_000_000_000
		nsec = ts % 1_000_000_000
	}
	// Apple seconds for 0001-01-01 and 9999-12-31 23:59:59 UTC.
	// Check before adding the epoch offset so invalid values cannot overflow.
	if sec < -63_113_904_000 || sec > 252_423_993_599 {
		return time.Time{}
	}
	return time.Unix(sec+appleEpochOffset, nsec).UTC()
}

// timeToAppleTimestamp converts a time.Time to an Apple epoch timestamp.
// If useNano is true, returns nanoseconds; otherwise returns seconds.
func timeToAppleTimestamp(t time.Time, useNano bool) int64 {
	appleSec := t.Unix() - appleEpochOffset
	if useNano {
		return appleSec*1_000_000_000 + int64(t.Nanosecond())
	}
	return appleSec
}

// resolveHandle classifies an iMessage handle ID as a phone number,
// email address, or raw identifier (e.g. system handles).
//
// displayName is only set for non-phone, non-email handles (e.g. Apple
// system identifiers). chat.db carries no contact names — UI layers
// fall back to phone/email when display_name is empty.
func resolveHandle(handleID string) (phone, email, displayName string) {
	if handleID == "" {
		return "", "", ""
	}
	normalized, err := textimport.NormalizePhone(handleID)
	if err == nil {
		return normalized, "", ""
	}
	if strings.Contains(handleID, "@") {
		return "", strings.ToLower(handleID), ""
	}
	return "", "", handleID
}

// extractAttributedBodyText extracts the plain text string from chat.db's
// attributedBody column. This column uses one of two serialization formats:
//
//   - NSArchiver "streamtyped" — legacy format, header starts with
//     \x04\x0bstreamtyped. The text is embedded as an NSString with a
//     length prefix after the class hierarchy.
//   - NSKeyedArchiver binary plist — starts with "bplist". The text lives
//     at $objects[rootObj["NS.string"]].
//
// macOS Ventura+ / iOS 16+ stopped populating the plain-text "text"
// column for most iMessages; the content lives exclusively here.
func extractAttributedBodyText(data []byte) string {
	text, _ := extractAttributedBodyTextWithStatus(data)
	return text
}

// extractAttributedBodyTextWithStatus reports whether the blob was recognized
// and decoded. An empty decoded string is different from a malformed blob.
func extractAttributedBodyTextWithStatus(data []byte) (string, bool) {
	if len(data) == 0 {
		return "", false
	}

	// Try streamtyped format first (most common on modern macOS)
	if bytes.HasPrefix(data, []byte("\x04\x0bstreamtyped")) {
		return extractStreamtypedText(data)
	}

	// Try NSKeyedArchiver binary plist
	if bytes.HasPrefix(data, []byte("bplist")) {
		return extractKeyedArchiverText(data)
	}

	return "", false
}

// extractStreamtypedText extracts text from NSArchiver streamtyped format.
// The format embeds an NSString with the text content. We scan for the
// NSString class marker, read the byte length that follows it, and extract
// exactly that many UTF-8 bytes.
//
// The length after the marker (\x84\x01+) uses typedstream integer encoding:
//   - 0x00-0x7F: the byte itself is the length
//   - 0x81: a 2-byte little-endian length follows
//   - 0x82: a 4-byte little-endian length follows
func extractStreamtypedText(data []byte) (string, bool) {
	marker := []byte("\x84\x01+")
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return "", false
	}
	pos := idx + len(marker)
	if pos >= len(data) {
		return "", false
	}

	var textLen int
	switch b := data[pos]; {
	case b&0x80 == 0:
		textLen = int(b)
		pos++
	case b == 0x81 && len(data)-pos > 2:
		textLen = int(binary.LittleEndian.Uint16(data[pos+1:]))
		pos += 3
	case b == 0x82 && len(data)-pos > 4:
		textLen = int(binary.LittleEndian.Uint32(data[pos+1:]))
		pos += 5
	default:
		return "", false
	}

	if textLen > len(data)-pos {
		return "", false
	}
	text := string(data[pos : pos+textLen])
	if !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}

// extractKeyedArchiverText extracts text from NSKeyedArchiver binary plist.
func extractKeyedArchiverText(data []byte) (string, bool) {
	var archive struct {
		Top     map[string]plist.UID `plist:"$top"`
		Objects []any                `plist:"$objects"`
	}
	if _, err := plist.Unmarshal(data, &archive); err != nil {
		return "", false
	}

	rootUID, ok := archive.Top["root"]
	if !ok || int(rootUID) >= len(archive.Objects) { //nolint:gosec // index check immediately gates uint64->int
		return "", false
	}

	rootObj, ok := archive.Objects[rootUID].(map[string]any)
	if !ok {
		return "", false
	}

	nsStringUID, ok := rootObj["NS.string"].(plist.UID)
	if !ok {
		return "", false
	}
	if int(nsStringUID) >= len(archive.Objects) { //nolint:gosec // index check gates uint64->int
		return "", false
	}

	text, ok := archive.Objects[nsStringUID].(string)
	if !ok {
		return "", false
	}
	return text, true
}

// snippet returns the first n characters of s, suitable for message preview.
func snippet(s string, maxLen int) string {
	return textutil.PrefixRunes(strings.Join(strings.Fields(s), " "), maxLen)
}
