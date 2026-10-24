package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestListObservationsOrderingAndSensitiveFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	first := insertObservationRow(t, st, person, 1, personobs.LevelExplicit, "[PERSON] runs")
	second := insertObservationRow(t, st, person, 2, personobs.LevelExplicit, "[PERSON] swims")
	sensitive := insertObservationRow(t, st, person, 3, personobs.LevelExplicit, "[PERSON] has a mortgage")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET pinned=TRUE WHERE id=?`), first)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET sensitive=TRUE WHERE id=?`), sensitive)
	require.NoError(err)
	page, err := st.ListPersonObservations(t.Context(), person, personobs.Filter{})
	require.NoError(err)
	require.Len(page.Items, 2)
	assert.Equal(2, page.Total)
	assert.Equal([]int64{first, second}, observationIDs(page.Items))
	page, err = st.ListPersonObservations(t.Context(), person, personobs.Filter{IncludeSensitive: true, Limit: 1, Offset: 1})
	require.NoError(err)
	assert.Equal(3, page.Total)
	assert.Equal([]int64{sensitive}, observationIDs(page.Items))
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := past.AddDate(1, 0, 0)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET valid_from=? WHERE id=?`), future, second)
	require.NoError(err)
	page, err = st.ListPersonObservations(t.Context(), person, personobs.Filter{AsOf: &past})
	require.NoError(err)
	assert.Equal([]int64{first}, observationIDs(page.Items))
	begins := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)
	local := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("west", -8*60*60))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET valid_from=? WHERE id=?`), begins, second)
	require.NoError(err)
	page, err = st.ListPersonObservations(t.Context(), person, personobs.Filter{AsOf: &local})
	require.NoError(err)
	assert.Equal([]int64{first, second}, observationIDs(page.Items))
}
func observationIDs(rows []personobs.Observation) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}
func TestListObservationsAfterSequenceKeyset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	highID := insertObservationRow(t, st, person, 30, personobs.LevelExplicit, "[PERSON] runs")
	lowSequence := insertObservationRow(t, st, person, 10, personobs.LevelExplicit, "[PERSON] swims")
	insertObservationRow(t, st, person, 20, personobs.LevelDeductive, "[PERSON] exercises")
	rows, err := st.ListPersonObservationsAfterSequence(t.Context(), person, personobs.LevelExplicit, 0, 1)
	require.NoError(err)
	assert.Equal([]int64{lowSequence}, observationIDs(rows))
	rows, err = st.ListPersonObservationsAfterSequence(t.Context(), person, personobs.LevelExplicit, 10, 10)
	require.NoError(err)
	assert.Equal([]int64{highID}, observationIDs(rows))
}
func linkObservationEvidence(t *testing.T, st *store.Store, person, id int64, key string, message int64) int64 {
	t.Helper()
	evidence := insertPersonFactSchemaEvidence(t, st, person, key)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_observation_evidence(observation_id,evidence_id,quote_start,quote_end,quote_sha256,message_id,conversation_id,conversation_kind,authored_by_person,author_is_owner,event_time)
 VALUES(?,?,0,7,?,?,1,'direct_chat',TRUE,FALSE,CURRENT_TIMESTAMP)`), id, evidence, strings.Repeat("a", 64), message)
	require.NoError(t, err)
	return evidence
}
func TestGetObservationDetailSupportedCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	id := insertObservationRow(t, st, person, 1, personobs.LevelExplicit, "[PERSON] runs")
	linkObservationEvidence(t, st, person, id, "first", 1)
	excluded := linkObservationEvidence(t, st, person, id, "excluded", 2)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observation_evidence SET exclusion_reason='draft' WHERE evidence_id=?`), excluded)
	require.NoError(err)
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	assert.Equal(1, detail.SupportedCount)
	assert.Len(detail.Evidence, 2)
}
func TestObservationSupportFollowsGenerationChronology(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	id := insertObservationRow(t, st, person, 1, personobs.LevelExplicit, "[PERSON] runs")
	evidence := linkObservationEvidence(t, st, person, id, "evidence", 1)
	newer := insertPersonFactSchemaGeneration(t, st, person, "newer")
	older := insertPersonFactSchemaGeneration(t, st, person, "older")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_generations SET resolved_at=? WHERE id=?`), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), newer)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_generations SET resolved_at=? WHERE id=?`), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), older)
	require.NoError(err)
	for _, event := range []struct {
		generation int64
		supported  bool
		reason     string
	}{{newer, false, "source-deleted"}, {older, true, "source-reimported"}} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_fact_evidence_status_events(person_id,generation_id,evidence_id,evidence_key,source_version,supported,reason) VALUES(?,?,?,'evidence','source-v1',?,?)`), person, event.generation, evidence, event.supported, event.reason)
		require.NoError(err)
	}
	detail, err := st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	assert.Zero(detail.SupportedCount)
	// Same resolved time breaks ties by event id.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_fact_generations SET resolved_at=? WHERE id=?`), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), older)
	require.NoError(err)
	detail, err = st.GetPersonObservation(t.Context(), id)
	require.NoError(err)
	assert.Equal(1, detail.SupportedCount)
}
func TestPersonObservationLayoutCounts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := personFactSchemaPerson(t, st, "alice@example.com")
	id := insertObservationRow(t, st, person, 1, personobs.LevelExplicit, "[PERSON] runs")
	derived := insertObservationRow(t, st, person, 2, personobs.LevelDeductive, "[PERSON] exercises")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET pinned=TRUE,sensitive=TRUE WHERE id=?`), id)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_observations SET premise_changed_at=CURRENT_TIMESTAMP WHERE id=?`), derived)
	require.NoError(err)
	layout, err := st.PersonObservationLayout(t.Context(), person)
	require.NoError(err)
	assert.Equal(2, layout.Total)
	assert.Equal(1, layout.Pinned)
	assert.Equal(1, layout.Sensitive)
	assert.Equal(1, layout.NeedsRecheck)
	assert.Equal(1, layout.ByLevel[personobs.LevelExplicit])
	assert.Equal(2, layout.ByStatus[personobs.StatusActive])
	rows, err := st.ListObservationsNeedingRecheck(t.Context(), person, personobs.LevelDeductive, 10)
	require.NoError(err)
	assert.Equal([]int64{derived}, observationIDs(rows))
}
