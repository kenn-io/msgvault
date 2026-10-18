package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopAutomationSignalsPersistenceAtomicity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	signals := store.AutomationSignalsFromMIME(mime.AutomationSignals{AutoSubmitted: true, PrecedenceBulk: true, ListUnsubscribe: true, CalendarPart: true, ReplyToNoReply: true})
	message := storetest.NewMessage(f.Source.ID, f.ConvID).WithSourceMessageID("signals").Build()
	_, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "body", Valid: true}, AutomationSignals: &signals, LabelIDs: []int64{999999}})
	requirements.Error(err)
	assertions.Zero(loopCount(t, f.Store, "message_automation_signals"))
	id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, AutomationSignals: &signals})
	requirements.NoError(err)
	var auto, bulk, unsubscribe, calendar, reply bool
	var version int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT auto_submitted,precedence_bulk,list_unsubscribe,calendar_part,reply_to_noreply,parser_version FROM message_automation_signals WHERE message_id=?"), id).Scan(&auto, &bulk, &unsubscribe, &calendar, &reply, &version))
	assertions.True(auto)
	assertions.True(bulk)
	assertions.True(unsubscribe)
	assertions.True(calendar)
	assertions.True(reply)
	assertions.Equal(1, version)
	replacement := store.AutomationSignalsFromMIME(mime.AutomationSignals{})
	_, err = f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, AutomationSignals: &replacement, LabelIDs: []int64{999999}})
	requirements.Error(err)
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT auto_submitted FROM message_automation_signals WHERE message_id=?"), id).Scan(&auto))
	assertions.True(auto, "failed replacement preserves committed evidence")
}
