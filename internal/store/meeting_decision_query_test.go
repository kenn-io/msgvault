package store

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMeetingRecordDecisionQueriesStayBounded(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var ids []int64
	for i := range 40 {
		_, id := projectionFixture(t, st, fmt.Sprintf("query-bound-%d", i), "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"query-bound_20270314T100000Z"}}`)
		ids = append(ids, id)
	}
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true, MaxStmtChars: 10000})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: ids[0]}})
	requirements.NoError(err)
	requirements.Equal(ids, record.MessageIDs)
	count := strings.Count(logs.String(), "FROM meeting_link_decisions d JOIN messages")
	requirements.Positive(count)
	requirements.LessOrEqual(count, 3)
}
