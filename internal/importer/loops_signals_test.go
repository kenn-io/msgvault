package importer_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLoopAutomationSignalsImporter(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("mbox", "owner@example.test")
	requirements.NoError(err)
	raw := []byte("From: peer@example.test\r\nTo: owner@example.test\r\nSubject: automation\r\nAuto-Submitted: auto-replied\r\nPrecedence: bulk\r\nList-Unsubscribe: <mailto:unsubscribe@example.test>\r\nReply-To: no-reply@example.test\r\nContent-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")
	requirements.NoError(importer.IngestRawMessage(t.Context(), st, source.ID, "owner@example.test", "", nil, "signals", "signals", raw, time.Now().UTC(), slog.Default()))
	var auto, bulk, unsubscribe, calendar, reply bool
	var version int
	requirements.NoError(st.DB().QueryRow("SELECT auto_submitted,precedence_bulk,list_unsubscribe,calendar_part,reply_to_noreply,parser_version FROM message_automation_signals").Scan(&auto, &bulk, &unsubscribe, &calendar, &reply, &version))
	assertions.True(auto)
	assertions.True(bulk)
	assertions.True(unsubscribe)
	assertions.True(calendar)
	assertions.True(reply)
	assertions.Equal(1, version)
}
