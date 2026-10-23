package mime

import (
	"encoding/base64"
	"math/rand/v2"
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
		{"list id", "List-Id: Updates <updates.example.test>\n", AutomationSignals{ListID: true}},
		{"bulk", "Precedence: BuLk\n", AutomationSignals{PrecedenceBulk: true}},
		{"list", "Precedence: list\n", AutomationSignals{PrecedenceBulk: true}},
		{"junk", "Precedence: junk\n", AutomationSignals{PrecedenceBulk: true}},
		{"normal priority", "Precedence: normal\n", AutomationSignals{}},
		{"unsubscribe", "List-Unsubscribe: <mailto:leave@example.test>\n", AutomationSignals{ListUnsubscribe: true}},
		{"empty unsubscribe", "List-Unsubscribe:\n", AutomationSignals{ListUnsubscribe: true}},
		{"reply address", "Reply-To: Alex Example <alex@example.test>\n", AutomationSignals{}},
		{"no reply", "Reply-To: Example <NO-REPLY+case@example.test>\n", AutomationSignals{ReplyToNoReply: true}},
		{"multiple reply addresses with explicit no reply", "Reply-To: alex@example.test, noreply@example.test\n", AutomationSignals{ReplyToNoReply: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "From: alex@example.test\nSubject: Ordinary message\n" + tt.headers + "\nIs this available?\n"
			msg, err := Parse([]byte(raw))
			require.NoError(t, err)
			assert.Equal(t, tt.want, msg.AutomationSignals())
		})
	}
}

func TestParseAutomationNestedCalendar(t *testing.T) {
	storedMIME := func(parts string) string {
		return "From: alex@example.test\r\nTo: me@example.test\r\nSubject: Planning\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=outer\r\n\r\n" + parts + "--outer--\r\n"
	}

	payload := make([]byte, 256<<10)
	_, err := rand.NewChaCha8([32]byte{2}).Read(payload)
	require.NoError(t, err)
	const text = "--outer\r\nContent-Type: text/plain\r\n\r\nCan you join?\r\n"
	const calendar = "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"
	attachment := "--outer\r\nContent-Type: application/pdf\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(payload) + "\r\n"
	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{"a calendar part after a large attachment, nested in an alternative",
			storedMIME(attachment + "--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n" +
				"--inner\r\nContent-Type: text/plain\r\n\r\nCan you join?\r\n--inner\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\n" + calendar + "--inner--\r\n"), true},
		{"a body line that quotes a calendar header",
			storedMIME("--outer\r\nContent-Type: text/plain\r\n\r\nCan you join? The invite source starts with\r\nContent-Type: text/calendar\r\n"), false},
		{"no calendar part", storedMIME(text + attachment), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse([]byte(tt.raw))
			require.NoError(t, err)
			assert.Equal(t, AutomationSignals{CalendarPart: tt.want}, msg.AutomationSignals())
			assert.Equal(t, tt.want, msg.HasCalendarPart)
		})
	}
}

func TestAutoSubmittedKeywordSyntax(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  bool
	}{
		{"no", false},
		{"NO", false},
		{"no (manually submitted)", false},
		{"no; x-origin=manual", false},
		{"(manual (nested)) NO (owner); x-origin=manual", false},
		{`(escaped \) comment) no`, false},
		{"auto-replied", true},
		{"auto-replied (out of office); x-origin=vacation", true},
		{"(generated) auto-generated; x-origin=cron", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			raw := "From: alex@example.test\nAuto-Submitted: " + tt.value + "\n\nCan you review?"
			msg, err := Parse([]byte(raw))
			requirements.NoError(err)
			requirements.NotNil(msg)
			assertions.Equal(tt.value, msg.AutoSubmitted)
			assertions.Equal(tt.want, msg.AutomationSignals().AutoSubmitted)
		})
	}
}

func TestIsNoReplyAddress(t *testing.T) {
	for _, tt := range []struct {
		address string
		want    bool
	}{
		{"alex@example.test", false},
		{"alex+questions@example.test", false},
		{"no-reply@example.test", true},
		{"do-not-reply+case@example.test", true},
		{"service.noreply@example.test", true},
		{"service.donotreply@example.test", true},
		{"info@example.test", false},
		{"support@example.test", false},
		{"hello@example.test", false},
		{"team@example.test", false},
		{"alex@no-reply.example.test", false},
		{"noreply", false},
		{"", false},
	} {
		t.Run(tt.address, func(t *testing.T) {
			assert.Equal(t, tt.want, IsNoReplyAddress(tt.address))
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
