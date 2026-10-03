package mcp

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestCardDAVConfirmationPreview(t *testing.T) {
	for _, tt := range []struct {
		name, version, fields, want string
	}{
		{"photo", "4.0", "PHOTO:data:;base64,QUJD", "PHOTO:[inline PHOTO, 17 encoded bytes]"},
		{"data URI leading space", "4.0", "PHOTO: data:;base64,QUJD", "PHOTO:[inline PHOTO, 18 encoded bytes]"},
		{"data URI leading tab", "4.0", "LOGO:\tDaTa:;base64,QUJD", "LOGO:[inline LOGO, 18 encoded bytes]"},
		{"logo", "4.0", "LOGO:data:;base64,QUJD", "LOGO:[inline LOGO, 17 encoded bytes]"},
		{"sound", "4.0", "SOUND:data:;base64,QUJD", "SOUND:[inline SOUND, 17 encoded bytes]"},
		{"key", "4.0", "KEY:data:;base64,QUJD", "KEY:[inline KEY, 17 encoded bytes]"},
		{"folded data URI", "4.0", "PHOTO:data:;base64,QU\r\n JD", "PHOTO:[inline PHOTO, 17 encoded bytes]"},
		{"group case and quoted parameter", "4.0", `item1.photo;TYPE="x:a;b":DaTa:;base64,QUJD`, `item1.photo;TYPE="x:a;b":[inline PHOTO, 17 encoded bytes]`},
		{"repeated properties", "4.0", "PHOTO:data:,a\r\nPHOTO:data:,bb", "PHOTO:[inline PHOTO, 7 encoded bytes]\r\nPHOTO:[inline PHOTO, 8 encoded bytes]"},
		{"legacy b", "3.0", "PHOTO;ENCODING=b:QUJD", "PHOTO;ENCODING=b:[inline PHOTO, 4 encoded bytes]"},
		{"legacy BASE64", "2.1", "KEY;ENCODING=BASE64:QUJD", "KEY;ENCODING=BASE64:[inline KEY, 4 encoded bytes]"},
		{"legacy bare BASE64", "2.1", "PHOTO;BASE64:QUJD", "PHOTO;BASE64:[inline PHOTO, 4 encoded bytes]"},
		{"legacy bare b", "2.1", "LOGO;b:QUJD", "LOGO;b:[inline LOGO, 4 encoded bytes]"},
		{"encoding whitespace", "3.0", `SOUND;ENCODING=" Base64 ":QUJD`, `SOUND;ENCODING=" Base64 ":[inline SOUND, 4 encoded bytes]`},
		{"named TYPE is not encoding", "4.0", "PHOTO;TYPE=BASE64:https://example.com/photo.png", "PHOTO;TYPE=BASE64:https://example.com/photo.png"},
		{"preserved bare encoding", "4.0", "KEY;BaSe64:QUJD", "KEY;BaSe64:[inline KEY, 4 encoded bytes]"},
		{"legacy quoted case", "3.0", `SOUND;encoding="Base64":QUJD`, `SOUND;encoding="Base64":[inline SOUND, 4 encoded bytes]`},
		{"empty data URI", "4.0", "PHOTO:data:,", "PHOTO:[inline PHOTO, 6 encoded bytes]"},
		{"empty legacy media", "3.0", "LOGO;ENCODING=b:", "LOGO;ENCODING=b:[inline LOGO, 0 encoded bytes]"},
		{"remote media", "4.0", "PHOTO:https://example.com/photo.png\r\nLOGO:https://example.com/logo.png\r\nSOUND:cid:sound\r\nKEY:https://example.com/key.asc", "PHOTO:https://example.com/photo.png\r\nLOGO:https://example.com/logo.png\r\nSOUND:cid:sound\r\nKEY:https://example.com/key.asc"},
		{"ordinary text", "4.0", "NOTE:data:;base64,QUJD\r\nTITLE:工程師", "NOTE:data:;base64,QUJD\r\nTITLE:工程師"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := "BEGIN:VCARD\r\nVERSION:" + tt.version + "\r\nFN:Example Contact\r\n" + tt.fields + "\r\nEMAIL:contact@example.com\r\nEND:VCARD\r\n"
			want := "BEGIN:VCARD\r\nVERSION:" + tt.version + "\r\nFN:Example Contact\r\n" + tt.want + "\r\nEMAIL:contact@example.com\r\nEND:VCARD\r\n"
			// Unfold the display's physical lines to compare ordered logical
			// fields, including parameters and unmodified contact values.
			got := strings.ReplaceAll(cardDAVConfirmationPreview(body), "\r\n ", "")
			assert.Equal(t, want, got)
		})
	}
}

func TestCardDAVConfirmationPreviewUnavailable(t *testing.T) {
	valid := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Example Contact\r\nEND:VCARD\r\n"
	for _, tt := range []struct{ name, body string }{
		{"empty", ""},
		{"malformed", "BEGIN:VCARD\r\nPHOTO:data:;base64,QUJD\r\n"},
		{"multiple cards", valid + valid},
		{"encoder rejection", "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Example\x00Contact\r\nEND:VCARD\r\n"},
		{"line limit", "BEGIN:VCARD\r\nPHOTO:data:," + strings.Repeat("x", vcard.DefaultMaxPhysicalLineBytes) + "\r\nEND:VCARD\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, "Contact preview unavailable.", cardDAVConfirmationPreview(tt.body))
		})
	}
}

func FuzzCardDAVConfirmationPreviewInlineMedia(f *testing.F) {
	f.Add([]byte{0, 1, 2}, uint8(0))
	f.Add([]byte{}, uint8(1))
	f.Add([]byte{255}, uint8(2))
	f.Add([]byte("synthetic media"), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, variant uint8) {
		property := []string{"PHOTO", "LOGO", "SOUND", "KEY"}[variant%4]
		value := "data:;base64," + base64.StdEncoding.EncodeToString(data)
		body := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Example Contact\r\n" + property + ":" + value + "\r\nEMAIL:contact@example.com\r\nEND:VCARD\r\n"
		preview := cardDAVConfirmationPreview(body)
		if len(property)+1+len(value) > vcard.DefaultMaxLogicalLineBytes {
			assert.Equal(t, "Contact preview unavailable.", preview)
			return
		}
		assert.NotContains(t, preview, "data:;base64,")
		assert.Contains(t, preview, "[inline "+property+",")
		assert.Contains(t, preview, "EMAIL:contact@example.com")
	})
}
