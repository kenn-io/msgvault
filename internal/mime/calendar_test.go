package mime

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalendarMIMEParts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	const calendar = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:example@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw := "From: host@example.com\r\nSubject: Example invite\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n--a\r\nContent-Type: text/plain\r\n\r\nExample body\r\n--a\r\nContent-Type: text/calendar; method=CANCEL\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(calendar)) + "\r\n--a--\r\n--m\r\nContent-Type: application/octet-stream; name=ignored.ics\r\n\r\n" + calendar + "\r\n--m\r\nContent-Type: message/rfc822\r\n\r\nContent-Type: text/calendar\r\n\r\n" + calendar + "\r\n--m--\r\n"
	msg, err := Parse([]byte(raw))
	requirements.NoError(err)
	requirements.Len(msg.CalendarParts, 1)
	assertions.Equal(calendar, string(msg.CalendarParts[0].Content))
	assertions.Equal("CANCEL", msg.CalendarParts[0].Method)
	msg, err = Parse([]byte("Content-Type: text/calendar\r\n\r\n" + calendar))
	requirements.NoError(err)
	requirements.Len(msg.CalendarParts, 1)
	assertions.Equal(calendar, string(msg.CalendarParts[0].Content))
}
