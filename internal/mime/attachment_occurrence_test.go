package mime

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseInlineBinaryOccurrence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contentType, key string
		nested, single         bool
	}{
		{name: "multipart", contentType: "application/octet-stream", key: "mime:2"},
		{name: "nested", contentType: "application/octet-stream", nested: true, key: "mime:2.1"},
		{name: "single root", contentType: "application/octet-stream", single: true, key: "mime:0"},
		{name: "malformed media type", contentType: "invalid value", key: "mime:2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := []byte("inline bytes")
			raw := attachmentOccurrenceMIME(payload, 1, tc.contentType, true, tc.nested, tc.single)
			msg, err := Parse(raw)
			require.NoError(t, err)
			require.Len(t, msg.Attachments, 1, "one physical MIME part")
			assert.Equal(t, Attachment{
				Filename: "part-0.bin", ContentType: "application/octet-stream",
				ContentID: "part-0", Disposition: "inline", PartKey: tc.key,
				Size: len(payload), ContentHash: fmt.Sprintf("%x", sha256.Sum256(payload)),
				Content: payload, IsInline: true,
			}, msg.Attachments[0])
		})
	}
}

// The fixture defines the physical occurrences independently of the parser.
// Repeated bytes still occupy distinct keys, for both inline and standalone parts.
func FuzzParseAttachmentOccurrences(f *testing.F) {
	f.Add([]byte("inline bytes"), uint8(0))
	f.Add([]byte{}, uint8(7))
	f.Add([]byte{0, 255, '\r', '\n'}, uint8(15))
	f.Fuzz(func(t *testing.T, payload []byte, flags uint8) {
		// Bound materialization, while retaining arbitrary binary payloads.
		if len(payload) > 64*1024 {
			payload = payload[:64*1024]
		}
		count := 1 + int(flags&3)
		inline, nested := flags&4 == 0, flags&8 != 0
		raw := attachmentOccurrenceMIME(payload, count, "application/octet-stream", inline, nested, false)
		msg, err := Parse(raw)
		require.NoError(t, err)
		require.Len(t, msg.Attachments, count)
		for i, attachment := range msg.Attachments {
			key := fmt.Sprintf("mime:%d", i+2)
			if nested {
				key = fmt.Sprintf("mime:2.%d", i+1)
			}
			disposition := "attachment"
			if inline {
				disposition = "inline"
			}
			assert.Equal(t, Attachment{
				Filename: fmt.Sprintf("part-%d.bin", i), ContentType: "application/octet-stream",
				ContentID: fmt.Sprintf("part-%d", i), Disposition: disposition, PartKey: key,
				Size: len(payload), ContentHash: fmt.Sprintf("%x", sha256.Sum256(payload)),
				Content: payload, IsInline: inline,
			}, attachment)
		}
	})
}

func TestDistinctAttachmentsRetainsUnkeyedOccurrences(t *testing.T) {
	t.Parallel()
	parts := []Attachment{
		{Filename: "first", PartKey: "mime:2", ContentHash: "same"},
		{Filename: "duplicate listing", PartKey: "mime:2", ContentHash: "same"},
		{Filename: "second physical part", PartKey: "mime:3", ContentHash: "same"},
		{Filename: "unkeyed first", ContentHash: "same"},
		{Filename: "unkeyed second", ContentHash: "same"},
	}
	assert.Equal(t, []Attachment{parts[0], parts[2], parts[3], parts[4]}, DistinctAttachments(parts))
}

func attachmentOccurrenceMIME(payload []byte, count int, contentType string, inline, nested, single bool) []byte {
	var raw strings.Builder
	raw.WriteString("From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: occurrence evidence\r\nMIME-Version: 1.0\r\n")
	if !single {
		raw.WriteString("Content-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
		if nested {
			raw.WriteString("--outer\r\nContent-Type: multipart/related; boundary=inner\r\n\r\n")
		}
	}
	for i := range count {
		if !single {
			if nested {
				raw.WriteString("--inner\r\n")
			} else {
				raw.WriteString("--outer\r\n")
			}
		}
		disposition := "attachment"
		if inline {
			disposition = "inline"
		}
		fmt.Fprintf(&raw, "Content-Type: %s\r\nContent-Disposition: %s; filename=part-%d.bin\r\nContent-ID: <part-%d>\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			contentType, disposition, i, i, base64.StdEncoding.EncodeToString(payload))
	}
	if !single {
		if nested {
			raw.WriteString("--inner--\r\n")
		}
		raw.WriteString("--outer--\r\n")
	}
	return []byte(raw.String())
}
