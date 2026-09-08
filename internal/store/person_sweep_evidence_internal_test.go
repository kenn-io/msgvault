package store

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

// TestPersonSweepHistoricalCandidatesBoundUsesTheJournalMessageIndex asks the
// SQLite planner about the exact bounded candidate statement the store runs.
// Without an index on (person_id, message_id) each correlated bound subquery
// scans the person's whole journal prefix for every live message, which is
// quadratic on a real archive; the plan must search the covering index.
func TestPersonSweepHistoricalCandidatesBoundUsesTheJournalMessageIndex(t *testing.T) {
	// openTestStore is always SQLite, which is the dialect EXPLAIN QUERY PLAN
	// speaks and the one where the unindexed bound was measured as quadratic.
	requirements := require.New(t)
	checks := assert.New(t)
	st := openTestStore(t)
	participantID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	requirements.NoError(err)

	query, args, err := st.personSweepHistoricalCandidatesQuery(t.Context(),
		peoplesweep.HistoricalCandidateRequest{PersonID: person.ID, Limit: 160, ThroughSequence: 42})
	requirements.NoError(err)
	rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &notUsed, &detail))
		plan = append(plan, detail)
	}
	requirements.NoError(rows.Err())

	joined := strings.Join(plan, "\n")
	boundSteps := 0
	for _, step := range plan {
		if !strings.Contains(step, " bound ") {
			continue
		}
		boundSteps++
		checks.Contains(step, "idx_person_sweep_changes_person_message",
			"the journal bound must be answered by the (person_id, message_id) index:\n%s", joined)
	}
	checks.Equal(2, boundSteps, "both bound subqueries appear in the plan:\n%s", joined)
}
