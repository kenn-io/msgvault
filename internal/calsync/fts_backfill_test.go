package calsync

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/search"
)

func TestCalendarSenderSurvivesPostgresFTSBackfill(t *testing.T) {
	require := require.New(t)
	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Meeting")}}
	m.FullSyncToken["primary"] = "T1"
	s, st := newSyncer(t, m, Options{})
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL indexing")
	}
	_, err := s.Full(t.Context())
	require.NoError(err)
	query := &search.Query{TextTerms: []string{testAccount}}
	before, _, err := st.SearchMessageIDsQueryContext(t.Context(), query, 10)
	require.NoError(err)
	require.Len(before, 1)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET indexing_version = 2, search_fts_fields = NULL WHERE id = $1`, before[0])
	require.NoError(err)
	_, err = st.BackfillFTS(nil)
	require.NoError(err)
	after, _, err := st.SearchMessageIDsQueryContext(t.Context(), query, 10)
	require.NoError(err)
	require.Equal(before, after)
}
