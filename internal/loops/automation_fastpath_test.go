package loops_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopsIdenticalWithAndWithoutAutomationColumns(t *testing.T) {
	for _, tc := range []struct {
		name, headers, attachment string
		flags                     uint32
	}{
		{name: "human"},
		{name: "newsletter", headers: "List-Unsubscribe: <mailto:leave@example.test>\r\n", flags: 8},
		{name: "auto-reply", headers: "Auto-Submitted: auto-replied\r\n", flags: 1},
		{name: "attachment", attachment: "report.pdf"},
		{name: "bot and sender bits preserve rules", flags: store.FlagBotSender | store.FlagSenderNoReply},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := storetest.New(t)
			id := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send the report?", SentAt: ago(3 * day), FileName: tc.attachment,
				Raw: "From: alex@example.test\r\nSubject: report\r\n" + tc.headers + "\r\nCan you send the report?"})
			before, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
			require.NoError(t, err)
			_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM loop_message_evidence`)
			require.NoError(t, err)
			require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, tc.flags))
			counting := &countingStore{Store: f.Store}
			after, err := loops.List(t.Context(), counting, loops.Options{}, now)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			if tc.attachment != "" {
				assert.Zero(t, counting.originals)
			}
		})
	}
}

func TestLoopsAutomationRetainsMissingAttachmentAndUnreadableFallback(t *testing.T) {
	for _, raw := range []string{
		"invalid MIME",
		"From: alex@example.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nCan you send it?\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\n\r\nreport\r\n--b--\r\n",
	} {
		t.Run(raw[:min(len(raw), 12)], func(t *testing.T) {
			f := storetest.New(t)
			id := f.PersistEmail(storetest.EmailSpec{From: alex, To: []string{me}, Body: "Can you send it?", Raw: raw, SentAt: ago(3 * day)})
			before, err := loops.List(t.Context(), f.Store, loops.Options{}, now)
			require.NoError(t, err)
			_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM loop_message_evidence`)
			require.NoError(t, err)
			require.NoError(t, f.Store.PutMessageAutomationContext(t.Context(), id, 0))
			counting := &countingStore{Store: f.Store}
			after, err := loops.List(t.Context(), counting, loops.Options{}, now)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Positive(t, counting.originals)
		})
	}
}
