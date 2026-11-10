package loops_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopsIdenticalWithAndWithoutAutomationColumns(t *testing.T) {
	for _, tc := range []struct {
		name, headers string
		flags         uint32
		calendar      bool
	}{
		{name: "human"},
		{name: "auto submitted", headers: "Auto-Submitted: auto-replied\r\n", flags: store.FlagAutoSubmitted},
		{name: "bulk", headers: "Precedence: bulk\r\n", flags: store.FlagPrecedenceBulk},
		{name: "list id", headers: "List-Id: Updates <updates.example.test>\r\n", flags: store.FlagListID},
		{name: "unsubscribe", headers: "List-Unsubscribe: <mailto:leave@example.test>\r\n", flags: store.FlagListUnsubscribe},
		{name: "calendar", flags: store.FlagCalendarPart, calendar: true},
		{name: "reply to", headers: "Reply-To: noreply@example.test\r\n", flags: store.FlagReplyToNoReply},
		{name: "bot and sender bits preserve rules", flags: store.FlagBotSender | store.FlagSenderNoReply},
	} {
		for _, attachment := range []bool{false, true} {
			for _, stale := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/file=%t/stale=%t", tc.name, attachment, stale), func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					f := storetest.New(t)
					raw := "From: alex@example.test\r\nSubject: report\r\n" + tc.headers
					partType := "text/plain"
					if tc.calendar {
						partType = "text/calendar"
					}
					filename := ""
					if attachment {
						filename = "report.pdf"
						raw += "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: " + partType + "\r\n\r\nCan you send the report?\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\n\r\nreport\r\n--b--\r\n"
					} else {
						raw += "Content-Type: " + partType + "\r\n\r\nCan you send the report?"
					}
					id := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the report?", SentAt: ago(3 * day), FileName: filename, Raw: raw})
					before, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
					require.NoError(err)
					_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM loop_message_evidence`)
					require.NoError(err)
					require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, tc.flags))
					if stale {
						// Deliberately contradictory stale bits prove the fallback ignores them.
						_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?`), tc.flags^store.FlagAutoSubmitted, store.AutomationRuleVersion+1, id)
						require.NoError(err)
					}
					counting := &countingStore{Store: f.Store}
					after, err := loops.List(t.Context(), counting, loops.Options{}, now)
					require.NoError(err)
					assert.Equal(before, after)
					if attachment && !stale {
						assert.Zero(counting.originals)
					} else {
						assert.Positive(counting.originals)
					}
					if attachment {
						assert.Positive(counting.automationReads)
					} else {
						assert.Zero(counting.automationReads)
					}
				})
			}
		}
	}
}

func TestLoopsAutomationRetainsMissingAttachmentAndUnreadableFallback(t *testing.T) {
	for _, raw := range []string{
		"invalid MIME",
		"From: alex@example.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nCan you send it?\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\n\r\nreport\r\n--b--\r\n",
	} {
		t.Run(raw[:min(len(raw), 12)], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			id := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send it?", Raw: raw, SentAt: ago(3 * day)})
			before, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
			require.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM loop_message_evidence`)
			require.NoError(err)
			require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, 0))
			counting := &countingStore{Store: f.Store}
			after, err := loops.List(t.Context(), counting, loops.Options{}, now)
			require.NoError(err)
			assert.Equal(before, after)
			assert.Positive(counting.originals)
		})
	}
}

func TestLoopsAutomationUnknownMalformedOriginalWithAttachment(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send it?", Raw: "invalid MIME", FileName: "report.pdf", SentAt: ago(3 * day)})
	counting := &countingStore{Store: f.Store}
	result, err := loops.List(t.Context(), counting, loops.Options{}, now)
	require.NoError(t, err)
	assert.Positive(result.SkippedUnparseable)
	assert.Positive(counting.originals)
	assert.Positive(counting.automationReads)
}
