package loops_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopsIdenticalWithAndWithoutAutomationColumns(t *testing.T) {
	for _, tc := range []struct {
		name, headers          string
		calendar, noFile, stale bool
	}{
		{name: "human"},
		{name: "auto submitted", headers: "Auto-Submitted: auto-replied\r\n"},
		{name: "bulk", headers: "Precedence: bulk\r\n"},
		{name: "list id", headers: "List-Id: Updates <updates.example.test>\r\n"},
		{name: "damaged list identity", headers: "List-Id: Updates <updates\x00.example.test>\r\n"},
		{name: "unsubscribe", headers: "List-Unsubscribe: <mailto:leave@example.test>\r\n"},
		{name: "calendar", calendar: true},
		{name: "reply to", headers: "Reply-To: noreply@example.test\r\n"},
		{name: "stale", stale: true},
		{name: "no file", noFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			raw := "From: alex@example.test\r\nSubject: report\r\n" + tc.headers
			partType := "text/plain"
			if tc.calendar {
				partType = "text/calendar"
			}
			filename := ""
			if !tc.noFile {
				filename = "report.pdf"
			}
			raw += "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: " + partType + "\r\n\r\nCan you send the report?\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\n\r\nreport\r\n--b--\r\n"
			parsed, err := mime.Parse([]byte(raw))
			require.NoError(err)
			flags := parsed.AutomationFlags()
			require.NotNil(flags)
			id := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the report?", SentAt: ago(3 * day), FileName: filename, Raw: raw})
			before, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
			require.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM loop_message_evidence`)
			require.NoError(err)
			require.NoError(f.Store.PutMessageAutomationContext(t.Context(), id, *flags))
			if tc.stale {
				// Contradictory stale bits prove the fallback ignores them.
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET automation_flags = ?, automation_rule_version = ? WHERE id = ?`), *flags^store.FlagAutoSubmitted, store.AutomationRuleVersion+1, id)
				require.NoError(err)
			}
			counting := &countingStore{Store: f.Store}
			after, err := loops.List(t.Context(), counting, loops.Options{}, now)
			require.NoError(err)
			assert.Equal(before, after)
			if !tc.noFile && !tc.stale {
				assert.Zero(counting.originals)
			} else {
				assert.Positive(counting.originals)
			}
			if tc.noFile {
				assert.Zero(counting.automationReads)
			} else {
				assert.Positive(counting.automationReads)
			}
		})
	}
}

func TestLoopsAutomationUnknownMalformedOriginalWithAttachment(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send it?", Raw: "invalid MIME", FileName: "report.pdf", SentAt: ago(3 * day)})
	result, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
	require.NoError(t, err)
	assert.Positive(result.SkippedUnparseable)
}
