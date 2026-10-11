package query_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAccountOperatorsOnLiveEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	alias, err := st.EnsureParticipant("alias@example.org", "", "example.org")
	require.NoError(err)
	persist := func(key, raw string, to ...int64) int64 {
		data := &store.MessagePersistData{
			Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email"},
			RawMIME: []byte(raw),
		}
		if len(to) > 0 {
			data.Recipients = []store.RecipientSet{{Type: "to", ParticipantIDs: to, DisplayNames: []string{""}, EmailAddresses: []string{"alias@example.org"}}}
		}
		id, err := st.PersistMessageContext(t.Context(), data)
		require.NoError(err)
		return id
	}
	work := persist("work", "X-Delivered-To: work@example.org\r\n\r\nbody")
	toAlias := persist("alias", "To: alias@example.org\r\n\r\nbody", alias)

	engine := query.NewSQLiteEngine(st.DB())
	if st.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(st.DB(), query.PostgreSQLQueryDialect{})
	}
	run := func(q string) ([]int64, int64) {
		parsed := search.Parse(q)
		require.NoError(parsed.Err())
		rows, err := engine.Search(t.Context(), parsed, 100, 0)
		require.NoError(err)
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		slices.Sort(ids)
		count, err := engine.SearchFastCount(t.Context(), parsed, query.MessageFilter{})
		require.NoError(err)
		return ids, count
	}

	ids, count := run("received:work@example.org")
	assert.Equal([]int64{work}, ids)
	assert.Equal(int64(1), count)
	ids, count = run("account:work@example.org")
	assert.Equal([]int64{work}, ids)
	assert.Equal(int64(1), count)
	toBefore, _ := run("to:alias@example.org")
	assert.Equal([]int64{toAlias}, toBefore)

	require.NoError(st.AddAccountIdentity(f.Source.ID, "alias@example.org", "manual"))
	ids, count = run("received:work@example.org received:alias@example.org")
	assert.Equal([]int64{work, toAlias}, ids, "repeated values are OR'd")
	assert.Equal(int64(2), count)
	ids, count = run("account:alias@example.org received:work@example.org")
	assert.Empty(ids, "the two operators are AND'd")
	assert.Zero(count)
	toAfter, _ := run("to:alias@example.org")
	assert.Equal(toBefore, toAfter, "to: results never depend on attribution")

	// account: also covers calendar events, so aggregates must not fall back
	// to the email-only default.
	event, err := st.UpsertMessage(&store.Message{
		SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "event", MessageType: "calendar_event",
		SentAt: sql.NullTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET account_address = 'work@example.org', account_path = 'calendar' WHERE id = ?`), event)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET sent_at = ? WHERE id = ?`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), work)
	require.NoError(err)
	total := func(rows []query.AggregateRow) int64 {
		var n int64
		for _, row := range rows {
			n += row.Count
		}
		return n
	}
	opts := query.AggregateOptions{SearchQuery: "account:work@example.org", TimeGranularity: query.TimeMonth}
	rows, err := engine.Aggregate(t.Context(), query.ViewTime, opts)
	require.NoError(err)
	assert.Equal(int64(2), total(rows), "Aggregate counts the calendar event")
	rows, err = engine.SubAggregate(t.Context(), query.MessageFilter{}, query.ViewTime, opts)
	require.NoError(err)
	assert.Equal(int64(2), total(rows), "SubAggregate counts the calendar event")
}
