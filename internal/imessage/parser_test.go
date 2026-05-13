package imessage

import (
	"testing"
	"time"

	"howett.net/plist"
)

func TestAppleTimestampToTime(t *testing.T) {
	tests := []struct {
		name     string
		ts       int64
		wantYear int
		wantZero bool
	}{
		{
			name:     "zero returns zero time",
			ts:       0,
			wantZero: true,
		},
		{
			name:     "nanoseconds - 2024-01-01",
			ts:       725760000000000000, // 2024-01-01 00:00:00 UTC in Apple nanoseconds
			wantYear: 2024,
		},
		{
			name:     "seconds - 2024-01-01",
			ts:       725760000, // 2024-01-01 00:00:00 UTC in Apple seconds
			wantYear: 2024,
		},
		{
			name:     "nanoseconds - 2020-06-15",
			ts:       613872000000000000, // 2020-06-15 in Apple nanoseconds
			wantYear: 2020,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appleTimestampToTime(tt.ts)
			if tt.wantZero {
				if !got.IsZero() {
					t.Errorf("expected zero time, got %v", got)
				}
				return
			}
			if got.Year() != tt.wantYear {
				t.Errorf("expected year %d, got %d (time: %v)", tt.wantYear, got.Year(), got)
			}
		})
	}
}

func TestTimeToAppleTimestamp(t *testing.T) {
	// 2024-01-01 00:00:00 UTC
	tm := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// In seconds
	gotSec := timeToAppleTimestamp(tm, false)
	wantSec := int64(725760000) // 1704067200 - 978307200
	if gotSec != wantSec {
		t.Errorf("seconds: got %d, want %d", gotSec, wantSec)
	}

	// In nanoseconds
	gotNano := timeToAppleTimestamp(tm, true)
	wantNano := int64(725760000000000000)
	if gotNano != wantNano {
		t.Errorf("nanoseconds: got %d, want %d", gotNano, wantNano)
	}
}

func TestRoundTripTimestamp(t *testing.T) {
	original := time.Date(2024, 6, 15, 12, 30, 45, 0, time.UTC)

	// Round trip through nanoseconds
	appleNano := timeToAppleTimestamp(original, true)
	recovered := appleTimestampToTime(appleNano)
	if !recovered.Equal(original) {
		t.Errorf("nanosecond round trip: got %v, want %v", recovered, original)
	}

	// Round trip through seconds (loses sub-second precision)
	appleSec := timeToAppleTimestamp(original, false)
	recoveredSec := appleTimestampToTime(appleSec)
	expected := time.Date(2024, 6, 15, 12, 30, 45, 0, time.UTC)
	if !recoveredSec.Equal(expected) {
		t.Errorf("second round trip: got %v, want %v", recoveredSec, expected)
	}
}

func TestResolveHandle(t *testing.T) {
	tests := []struct {
		name            string
		handleID        string
		wantPhone       string
		wantEmail       string
		wantDisplayName string
	}{
		{
			name:      "email address",
			handleID:  "John@Example.com",
			wantEmail: "john@example.com",
		},
		{
			name:      "US phone with +1",
			handleID:  "+15551234567",
			wantPhone: "+15551234567",
		},
		{
			name:      "US phone 10 digits",
			handleID:  "5551234567",
			wantPhone: "+15551234567",
		},
		{
			name:      "US phone with formatting",
			handleID:  "(555) 123-4567",
			wantPhone: "+15551234567",
		},
		{
			name:      "US phone 11 digits with 1",
			handleID:  "15551234567",
			wantPhone: "+15551234567",
		},
		{
			name:      "international phone",
			handleID:  "+447911123456",
			wantPhone: "+447911123456",
		},
		{
			name: "empty string",
		},
		{
			name:            "short code (not a phone)",
			handleID:        "12345",
			wantDisplayName: "12345",
		},
		{
			name:            "handle with prefix falls to raw handle",
			handleID:        "p:+1555123",
			wantPhone:       "",
			wantDisplayName: "p:+1555123",
		},
		{
			name:            "system handle without digits",
			handleID:        "system",
			wantDisplayName: "system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPhone, gotEmail, gotDisplay := resolveHandle(tt.handleID)
			if gotPhone != tt.wantPhone {
				t.Errorf("phone: got %q, want %q", gotPhone, tt.wantPhone)
			}
			if gotEmail != tt.wantEmail {
				t.Errorf("email: got %q, want %q", gotEmail, tt.wantEmail)
			}
			if gotDisplay != tt.wantDisplayName {
				t.Errorf("displayName: got %q, want %q", gotDisplay, tt.wantDisplayName)
			}
		})
	}
}

// makeAttributedBodyBlob builds a minimal NSKeyedArchiver binary plist blob
// equivalent to an NSAttributedString with the given text.
func makeAttributedBodyBlob(text string) []byte {
	archive := struct {
		Archiver string               `plist:"$archiver"`
		Version  uint64               `plist:"$version"`
		Top      map[string]plist.UID `plist:"$top"`
		Objects  []interface{}        `plist:"$objects"`
	}{
		Archiver: "NSKeyedArchiver",
		Version:  100000,
		Top:      map[string]plist.UID{"root": 1},
		Objects: []interface{}{
			"$null",
			map[string]interface{}{
				"$class":    plist.UID(3),
				"NS.string": plist.UID(2),
			},
			text,
			map[string]interface{}{
				"$classname": "NSAttributedString",
				"$classes":   []string{"NSAttributedString", "NSObject"},
			},
		},
	}
	data, err := plist.Marshal(archive, plist.BinaryFormat)
	if err != nil {
		panic("makeAttributedBodyBlob: " + err.Error())
	}
	return data
}

// makeStreamtypedBlob builds an NSArchiver "streamtyped" blob containing
// an NSAttributedString with the given text. Mirrors the format produced
// by macOS Ventura+/Sequoia chat.db.
func makeStreamtypedBlob(text string) []byte {
	// Header
	header := []byte("\x04\x0bstreamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84")
	// Class name "NSAttributedString"
	className := []byte("\x12NSAttributedString")
	// Parent class
	parent := []byte("\x00\x84\x84\x08NSObject\x00\x85\x92\x84\x84\x84\x08NSString\x01\x94")
	// Text length prefix + marker
	marker := []byte("\x84\x01+")

	var buf []byte
	buf = append(buf, header...)
	buf = append(buf, className...)
	buf = append(buf, parent...)
	buf = append(buf, marker...)

	// Length encoding
	textBytes := []byte(text)
	n := len(textBytes)
	if n < 128 {
		buf = append(buf, byte(n))
	} else {
		// Multi-byte little-endian length
		nBytes := 0
		tmp := n
		for tmp > 0 {
			nBytes++
			tmp >>= 8
		}
		buf = append(buf, 0x80|byte(nBytes))
		for i := 0; i < nBytes; i++ {
			buf = append(buf, byte(n>>(8*i)))
		}
	}

	buf = append(buf, textBytes...)
	return buf
}

// makeRealStreamtypedBlob builds a blob matching the actual macOS Sequoia
// format where the multi-byte length has extra framing bytes (0x81, 0x92, 0x00)
// between the marker and the text. The 0x92 byte is > 0x20, which the
// original parser couldn't skip.
func makeRealStreamtypedBlob(text string) []byte {
	// This matches the real format seen in chat.db:
	// \x84\x01+ \x81 \x92 \x00 <text> \x86 ...
	var buf []byte
	buf = append(buf, "\x04\x0bstreamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84"...)
	buf = append(buf, "\x12NSAttributedString"...)
	buf = append(buf, "\x00\x84\x84\x08NSObject\x00\x85\x92\x84\x84\x84\x08NSString\x01\x94"...)
	buf = append(buf, "\x84\x01+"...) // marker
	// Multi-byte length prefix with actual 0x92 framing byte (> 0x20)
	n := len(text)
	buf = append(buf, 0x81, byte(n), 0x92, 0x00)
	buf = append(buf, text...)
	buf = append(buf, 0x86) // terminator
	return buf
}

func TestExtractAttributedBodyText(t *testing.T) {
	// Build a test string > 127 bytes to exercise multi-byte length encoding.
	longText := "This message is longer than one hundred and twenty-seven bytes " +
		"to exercise the multi-byte length encoding path in streamtyped format parsing. " +
		"Extra padding here."

	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{"nil blob", nil, ""},
		{"empty blob", []byte{}, ""},
		{"invalid plist", []byte("not a plist"), ""},
		{"plain ASCII message", makeAttributedBodyBlob("Hello from iMessage"), "Hello from iMessage"},
		{"unicode and emoji", makeAttributedBodyBlob("Hey! \xf0\x9f\x98\x8a"), "Hey! \xf0\x9f\x98\x8a"},
		{"multiline", makeAttributedBodyBlob("Line one\nLine two"), "Line one\nLine two"},
		{"streamtyped short", makeStreamtypedBlob("Hello world"), "Hello world"},
		{"streamtyped long", makeStreamtypedBlob("This is a longer message that tests multi-byte length encoding and should work correctly"), "This is a longer message that tests multi-byte length encoding and should work correctly"},
		{"streamtyped multi-byte length >127", makeStreamtypedBlob(longText), longText},
		{"streamtyped real format", makeRealStreamtypedBlob("I am learning Go"), "I am learning Go"},
		{"streamtyped real format 50 bytes", makeRealStreamtypedBlob("Yeah, we should catch up soon! How about Thursday?"), "Yeah, we should catch up soon! How about Thursday?"},
		{"streamtyped real format 100 bytes", makeRealStreamtypedBlob("This is exactly one hundred bytes of text for testing the mid-range length encoding in streamtyped!!"), "This is exactly one hundred bytes of text for testing the mid-range length encoding in streamtyped!!"},
		{"streamtyped real format long", makeRealStreamtypedBlob(longText), longText},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAttributedBodyText(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSnippet(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short text", "hello", 100, "hello"},
		{"long text", "hello world this is a long message", 10, "hello worl"},
		{"empty", "", 100, ""},
		{"multiline", "line1\nline2\nline3", 100, "line1 line2 line3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snippet(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
