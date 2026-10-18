package store_test

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func compressedLoopRaw(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	_, err := w.Write(raw)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return out.Bytes()
}
func backfillMessage(t *testing.T, f *storetest.Fixture, id int64, raw []byte, compression string, encrypted int) {
	t.Helper()
	override := ""
	if f.Store.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	loopExec(t, f.Store, "INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type)"+override+" VALUES(?,?,?,?,'email')", id, f.Source.ID, f.ConvID, fmt.Sprintf("raw-%d", id))
	if raw != nil {
		loopExec(t, f.Store, "INSERT INTO message_raw(message_id,raw_data,raw_format,compression,encryption_version) VALUES(?,?,'mime',?,?)", id, raw, compression, encrypted)
	}
}
func TestLoopBackfillAtomicStartedCursorIncludesZeroAndNegative(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	raw := compressedLoopRaw(t, []byte("From: peer@example.test\r\nTo: owner@example.test\r\nAuto-Submitted: auto-generated\r\nSubject: A notice\r\n\r\nGenerated text"))
	for _, id := range []int64{-1, 0, 1} {
		encrypted := 0
		if id == 0 {
			encrypted = 1
		}
		backfillMessage(t, f, id, raw, "zlib", encrypted)
	}
	loopExec(t, f.Store, "UPDATE loop_derivation_state SET last_run_at=?", now)
	if f.Store.IsPostgreSQL() {
		loopExec(t, f.Store, `CREATE FUNCTION fail_loop_backfill() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected cursor failure'; END $$`)
		loopExec(t, f.Store, `CREATE TRIGGER fail_loop_backfill BEFORE INSERT ON archive_metadata FOR EACH ROW WHEN (NEW.key='loops_signals_backfill_started') EXECUTE FUNCTION fail_loop_backfill()`)
	} else {
		loopExec(t, f.Store, `CREATE TRIGGER fail_loop_backfill BEFORE INSERT ON archive_metadata WHEN NEW.key='loops_signals_backfill_started' BEGIN SELECT RAISE(ABORT,'injected cursor failure'); END`)
	}
	_, err := f.Store.BackfillLoopSignalsContext(t.Context(), 1, now)
	requirements.Error(err)
	assertions.Zero(loopCount(t, f.Store, "message_automation_signals"))
	assertions.Zero(loopCount(t, f.Store, "loop_conversation_queue"))
	state, err := f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	assertions.Zero(state.SignalsBackfillLastMessageID)
	assertions.False(state.SignalsBackfillComplete)
	if f.Store.IsPostgreSQL() {
		loopExec(t, f.Store, "DROP TRIGGER fail_loop_backfill ON archive_metadata")
	} else {
		loopExec(t, f.Store, "DROP TRIGGER fail_loop_backfill")
	}
	for i, want := range []int64{1, 0, -1} {
		result, err := f.Store.BackfillLoopSignalsContext(t.Context(), 1, now)
		requirements.NoError(err)
		assertions.Equal(1, result.Scanned)
		if want == 0 {
			assertions.Zero(result.Parsed)
			assertions.Equal(1, result.Encrypted)
		} else {
			assertions.Equal(1, result.Parsed)
		}
		state, err = f.Store.LoopDerivationStateContext(t.Context())
		requirements.NoError(err)
		assertions.Equal(want, state.SignalsBackfillLastMessageID)
		assertions.Equal(i == 2, state.SignalsBackfillComplete)
	}
	assertions.Equal(2, loopCount(t, f.Store, "message_automation_signals"))
	rows, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal("signals", rows[0].Reason)
	assertions.Nil(rows[0].FeedBound)
	result, err := f.Store.BackfillLoopSignalsContext(t.Context(), 1, now)
	requirements.NoError(err)
	assertions.Zero(result.Scanned)
}
func TestLoopBackfillBoundsDecodeAndSafeSkippedEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	calendar := []byte("From: peer@example.test\r\nTo: owner@example.test\r\nSubject: Meeting\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n--outer\r\nContent-Type: multipart/alternative; boundary=inner\r\n\r\n--inner\r\nContent-Type: text/plain\r\n\r\nMeeting text\r\n--inner\r\nContent-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n--inner--\r\n--outer--\r\n")
	valid := compressedLoopRaw(t, calendar)
	backfillMessage(t, f, 1, valid, "zlib", 0)
	backfillMessage(t, f, 2, valid, "zlib", 1)
	backfillMessage(t, f, 3, valid, "brotli", 0)
	backfillMessage(t, f, 4, nil, "", 0)
	backfillMessage(t, f, 5, []byte("not a zlib stream"), "zlib", 0)
	backfillMessage(t, f, 6, valid[:len(valid)/2], "zlib", 0)
	backfillMessage(t, f, 7, bytes.Repeat([]byte("x"), store.LoopSignalsMaxRawBytes+2), "zlib", 0)
	backfillMessage(t, f, 8, compressedLoopRaw(t, bytes.Repeat([]byte("x"), store.LoopSignalsMaxRawBytes+2)), "zlib", 0)
	backfillMessage(t, f, 9, calendar, "", 0)
	result, err := f.Store.BackfillLoopSignalsContext(t.Context(), 20, now)
	requirements.NoError(err)
	assertions.Equal(9, result.Scanned)
	assertions.Equal(1, result.Parsed)
	assertions.Equal(1, result.Encrypted)
	assertions.Equal(2, result.Unsupported)
	assertions.Equal(1, result.Missing)
	assertions.Equal(2, result.Invalid)
	assertions.Equal(2, result.TooLarge)
	var calendarPart bool
	requirements.NoError(f.Store.DB().QueryRow("SELECT calendar_part FROM message_automation_signals WHERE message_id=1").Scan(&calendarPart))
	assertions.True(calendarPart)
	assertions.Equal(1, loopCount(t, f.Store, "message_automation_signals"))
	assertions.LessOrEqual(result.MaxStoredBytesRead, int64(store.LoopSignalsMaxRawBytes+1))
	assertions.LessOrEqual(result.MaxDecodedBytesRead, int64(store.LoopSignalsMaxRawBytes+1))
}
