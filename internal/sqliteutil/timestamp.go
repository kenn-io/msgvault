package sqliteutil

import (
	"encoding/binary"
	"time"
)

const TimestampKeyFunction = "msgvault_timestamp_key"

// timestampKey returns an exact sortable instant, or SQL NULL for absent,
// invalid and zero timestamps, matching the Store's nullable timestamp reader.
// Separate seconds and nanoseconds retain dates outside UnixNano's range.
func timestampKey(value any) []byte {
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		return nil
	}
	parsed := ParseTime(text)
	if parsed.IsZero() {
		return nil
	}
	return TimestampKey(parsed)
}

// TimestampKey encodes a typed query instant, including the zero-time bound.
// Stored timestamp parsing separately treats zero as unavailable.
func TimestampKey(instant time.Time) []byte {
	key := make([]byte, 12)
	// Flipping the sign bit makes unsigned byte order match signed seconds.
	binary.BigEndian.PutUint64(key, uint64(instant.Unix())^(uint64(1)<<63)) // #nosec G115 -- intentional two's-complement conversion for signed ordering.
	binary.BigEndian.PutUint32(key[8:], uint32(instant.Nanosecond()))       // #nosec G115 -- time.Time.Nanosecond is in [0, 999999999].
	return key
}

// ParseTime parses a datetime string from SQLite into time.Time.
// Uses the same comprehensive format list as dbTimeLayouts in sync.go.
func ParseTime(s string) time.Time {
	// Same formats as dbTimeLayouts - order matters: more specific first
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07:00", // space-separated with fractional seconds and TZ
		"2006-01-02T15:04:05.999999999-07:00", // T-separated with fractional seconds and TZ
		"2006-01-02 15:04:05.999999999",       // space-separated with fractional seconds
		"2006-01-02T15:04:05.999999999",       // T-separated with fractional seconds
		"2006-01-02 15:04:05",                 // SQLite datetime('now') format
		"2006-01-02T15:04:05",                 // T-separated basic
		"2006-01-02 15:04",                    // space-separated without seconds
		"2006-01-02T15:04",                    // T-separated without seconds
		"2006-01-02",                          // date only
		time.RFC3339,                          // e.g., "2006-01-02T15:04:05Z"
		time.RFC3339Nano,                      // e.g., "2006-01-02T15:04:05.999999999Z07:00"
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
