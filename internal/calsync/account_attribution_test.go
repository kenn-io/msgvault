package calsync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func calendarAccountIDs(t *testing.T, st *store.Store) []int64 {
	t.Helper()
	results, _, err := st.SearchMessagesQuery(search.Parse("account:"+testAccount), 0, 100)
	require.NoError(t, err)
	ids := make([]int64, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ID)
	}
	return ids
}

// simulateLegacyEvents returns events to the state an older msgvault left
// them in: no account attribution.
func simulateLegacyEvents(t *testing.T, st *store.Store, src *store.Source) {
	t.Helper()
	_, err := st.DB().Exec(st.Rebind(
		`UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id = ?`), src.ID)
	require.NoError(t, err)
}

func TestIncrementalHealsCalendarAccountAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner", Primary: true}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "One"), timedEvent("e2", "Two")}}
	m.FullSyncToken["primary"] = "T1"
	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err)
	require.Len(calendarAccountIDs(t, st), 2, "new events derive at ingest")

	src := primarySource(t, st)
	simulateLegacyEvents(t, st, src)
	assert.Empty(calendarAccountIDs(t, st))

	m.IncEvents["T1"] = [][]gcal.Event{{}}
	m.IncNextToken["T1"] = "T2"
	_, err = s.Incremental(context.Background())
	require.NoError(err)
	assert.Len(calendarAccountIDs(t, st), 2, "the next incremental sync heals archived events")
}
