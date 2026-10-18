package mime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAutomationSignals(t *testing.T) {
	for _, tt := range []struct {
		name, headers string
		want          AutomationSignals
	}{
		{"ordinary", "", AutomationSignals{}},
		{"explicit human", "Auto-Submitted: no\n", AutomationSignals{}},
		{"case insensitive no", "Auto-Submitted: NO\n", AutomationSignals{}},
		{"auto reply", "Auto-Submitted: auto-replied\n", AutomationSignals{AutoSubmitted: true}},
		{"bulk", "Precedence: BuLk\n", AutomationSignals{PrecedenceBulk: true}},
		{"list", "Precedence: list\n", AutomationSignals{PrecedenceBulk: true}},
		{"junk", "Precedence: junk\n", AutomationSignals{PrecedenceBulk: true}},
		{"normal priority", "Precedence: normal\n", AutomationSignals{}},
		{"unsubscribe", "List-Unsubscribe: <mailto:leave@example.test>\n", AutomationSignals{ListUnsubscribe: true}},
		{"empty unsubscribe", "List-Unsubscribe:\n", AutomationSignals{ListUnsubscribe: true}},
		{"reply address", "Reply-To: Alex Example <alex@example.test>\n", AutomationSignals{}},
		{"no reply", "Reply-To: Example <NO-REPLY+case@example.test>\n", AutomationSignals{ReplyToNoReply: true}},
		{"multiple reply addresses with explicit no reply", "Reply-To: alex@example.test, noreply@example.test\n", AutomationSignals{ReplyToNoReply: true}},
		{"support reply address is monitored", "Reply-To: support@example.test\n", AutomationSignals{}},
		{"hello reply address is monitored", "Reply-To: hello@example.test\n", AutomationSignals{}},
		{"notifications reply address is monitored", "Reply-To: notifications@example.test\n", AutomationSignals{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, newline := range []string{"\n", "\r\n"} {
				raw := "From: alex@example.test\nSubject: Ordinary message\n" + tt.headers + "\nIs this available?\n"
				msg, err := Parse([]byte(strings.ReplaceAll(raw, "\n", newline)))
				require.NoError(t, err)
				assert.Equal(t, tt.want, msg.AutomationSignals())
			}
		})
	}
}

func TestParseAutomationNestedCalendar(t *testing.T) {
	raw := "MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=outer\n\n" +
		"--outer\nContent-Type: text/plain\n\nMeeting details\n" +
		"--outer\nContent-Type: multipart/alternative; boundary=inner\n\n" +
		"--inner\nContent-Type: text/plain\n\nMeeting details\n" +
		"--inner\nContent-Type: text/calendar; charset=utf-8\n\nBEGIN:VCALENDAR\nEND:VCALENDAR\n" +
		"--inner--\n--outer--\n"
	msg, err := Parse([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, AutomationSignals{CalendarPart: true}, msg.AutomationSignals())
	assert.True(t, msg.HasCalendarPart)
}

func TestParseAutomationRecovery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := []byte("From: alex@example.test\nAuto-Submitted: auto-generated\nPrecedence: list\n" +
		"List-Unsubscribe: <mailto:leave@example.test>\nReply-To: noreply@example.test\n" +
		"Content-Type: multipart/mixed\n\nMissing boundary")
	msg, err := ParseWithRecovery(raw, "Recovered message")
	requirements.Error(err)
	requirements.NotNil(msg)
	assertions.Equal(AutomationSignals{
		AutoSubmitted: true, PrecedenceBulk: true, ListUnsubscribe: true, ReplyToNoReply: true,
	}, msg.AutomationSignals())
	assertions.Equal("auto-generated", msg.AutoSubmitted)
	assertions.Equal("list", msg.Precedence)
}

func TestAutoSubmittedKeywordSyntax(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{"no (manually submitted)", false},
		{"no; x-origin=manual", false},
		{"(manual (nested)) NO (owner); x-origin=manual", false},
		{`(escaped \) comment) no`, false},
		{"auto-replied (out of office); x-origin=vacation", true},
		{"(generated) auto-generated; x-origin=cron", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			for _, recovery := range []bool{false, true} {
				raw := "From: alex@example.test\nAuto-Submitted: " + tt.value + "\n"
				if recovery {
					raw += "Content-Type: multipart/mixed\n"
				}
				raw += "\nCan you review?"
				var msg *Message
				var err error
				if recovery {
					msg, err = ParseWithRecovery([]byte(raw), "Recovered")
					requirements.Error(err)
				} else {
					msg, err = Parse([]byte(raw))
					requirements.NoError(err)
				}
				requirements.NotNil(msg)
				assertions.Equal(tt.value, msg.AutoSubmitted)
				assertions.Equal(tt.want, msg.AutomationSignals().AutoSubmitted)
			}
		})
	}
}

func TestAutomatedAddress(t *testing.T) {
	for _, tt := range []struct {
		address string
		want    bool
	}{
		{"alex@example.test", false},
		{"alex+questions@example.test", false},
		{"no-reply@example.test", true},
		{"do-not-reply+case@example.test", true},
		{"service.noreply@example.test", true},
		{"notifications@example.test", true},
		{"mailer-daemon@example.test", true},
		{"info@example.test", true},
		{"alex@no-reply.example.test", false},
		{"noreply", false},
		{"", false},
	} {
		t.Run(tt.address, func(t *testing.T) {
			assert.Equal(t, tt.want, IsAutomatedAddress(tt.address))
		})
	}
}

// MIME content must not masquerade as top-level automation evidence.
// This catches accidentally inspecting body text or nested message headers.
func FuzzParseAutomationBodyIsolation(f *testing.F) {
	f.Add("Auto-Submitted: auto-replied\nList-Unsubscribe: yes\n")
	f.Add("Content-Type: text/calendar\nReply-To: noreply@example.test\n")
	f.Add("Is this available?\r\n")
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 4096 {
			t.Skip()
		}
		msg, err := Parse([]byte("From: alex@example.test\nContent-Type: text/plain; charset=utf-8\n\n" + body))
		require.NoError(t, err)
		assert.Equal(t, AutomationSignals{}, msg.AutomationSignals())
	})
}

func FuzzAutomationHeaderSemantics(f *testing.F) {
	f.Add(uint8(0), "A question", "Can you review?")
	f.Add(uint8(3), "An update", "For your information")
	f.Fuzz(func(t *testing.T, flags uint8, subject, body string) {
		if len(subject)+len(body) > 4096 {
			t.Skip()
		}
		subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.ToValidUTF8(subject, "?"))
		positive := flags&1 != 0
		headers := "From: alex@example.test\nSubject: " + subject + "\n"
		if positive {
			headers += "Auto-Submitted: (notice) auto-generated; x-origin=test\nPrecedence: bulk\nList-Unsubscribe: <mailto:leave@example.test>\nReply-To: noreply@example.test\n"
		} else {
			headers += "Auto-Submitted: (human) no; x-origin=manual\nPrecedence: normal\nReply-To: alex@example.test\n"
		}
		lines := strings.Split(headers, "\n")
		for i, line := range lines {
			if flags&2 != 0 {
				name, value, ok := strings.Cut(line, ":")
				if ok {
					lines[i] = strings.ToUpper(name) + ":" + value
				}
			}
		}
		headers = strings.Join(lines, "\n")
		newline := "\n"
		if flags&4 != 0 {
			newline = "\r\n"
		}
		for _, recovery := range []bool{false, true} {
			header := headers
			if recovery {
				header += "Content-Type: multipart/mixed\n"
			}
			raw := []byte(strings.ReplaceAll(header+"\n", "\n", newline) + body)
			var msg *Message
			var err error
			if recovery {
				msg, err = ParseWithRecovery(raw, "Recovered")
				require.Error(t, err)
			} else {
				msg, err = Parse(raw)
				require.NoError(t, err)
			}
			require.NotNil(t, msg)
			assert.Equal(t, AutomationSignals{AutoSubmitted: positive, PrecedenceBulk: positive, ListUnsubscribe: positive, ReplyToNoReply: positive}, msg.AutomationSignals())
		}
	})
}
