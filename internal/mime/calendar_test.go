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
	// Malformed forwarded mail with a boundary must not expose embedded invites.
	raw := "From: host@example.com\r\nSubject: Example invite\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n--a\r\nContent-Type: text/plain\r\n\r\nExample body\r\n--a\r\nContent-Type: text/calendar; method=CANCEL\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(calendar)) + "\r\n--a--\r\n--m\r\nContent-Type: application/octet-stream; name=invite.ics\r\n\r\n" + calendar + "\r\n--m\r\nContent-Type: message/rfc822; boundary=f\r\n\r\n--f\r\nContent-Type: text/calendar\r\n\r\n" + calendar + "\r\n--f--\r\n--m--\r\n"
	msg, err := Parse([]byte(raw))
	requirements.NoError(err)
	requirements.Len(msg.CalendarParts, 2)
	assertions.Equal(calendar, string(msg.CalendarParts[0].Content))
	assertions.Equal("CANCEL", msg.CalendarParts[0].Method)
	assertions.Equal(calendar, string(msg.CalendarParts[1].Content))
	assertions.Empty(msg.CalendarParts[1].Method)
	msg, err = Parse([]byte("Content-Type: text/calendar\r\n\r\n" + calendar))
	requirements.NoError(err)
	requirements.Len(msg.CalendarParts, 1)
	assertions.Equal(calendar, string(msg.CalendarParts[0].Content))

	t.Run("percent encoded", func(t *testing.T) {
		raw := "Content-Type: application/octet-stream\r\nContent-Disposition: attachment;\r\n filename*=UTF-8''R%C3%A9union%2Eics\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(calendar))
		msg, err := Parse([]byte(raw))
		require.NoError(t, err)
		require.Len(t, msg.CalendarParts, 1)
		assert.Equal(t, calendar, string(msg.CalendarParts[0].Content))
	})

	t.Run("non-calendar attachment", func(t *testing.T) {
		msg, err := Parse([]byte("Content-Type: application/octet-stream; name=notes.txt\r\n\r\nnotes"))
		require.NoError(t, err)
		assert.Empty(t, msg.CalendarParts)
	})

	t.Run("application ics", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		const calendar = "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"
		raw := "Content-Type: application/ics\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(calendar))
		msg, err := Parse([]byte(raw))
		requirements.NoError(err)
		requirements.Len(msg.CalendarParts, 1)
		assertions.Equal(calendar, string(msg.CalendarParts[0].Content))
	})
}
