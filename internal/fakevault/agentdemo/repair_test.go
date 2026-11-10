package agentdemo_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
)

func TestPatFirstCorrespondenceIsFromOwner(t *testing.T) {
	st, m := archive(t, 1)
	var id int64
	var own bool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT m.id,m.is_from_me FROM messages m WHERE m.sender_id=? OR EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.participant_id=?) ORDER BY m.sent_at,m.id LIMIT 1`, m.Participants["pat"].ID, m.Participants["pat"].ID).Scan(&id, &own))
	assert.Equal(t, m.Messages["pat_quote_outbound"], id)
	assert.True(t, own)
}

func TestLoadValidatesScenario(t *testing.T) {
	for _, tc := range []struct {
		name, scenario string
		version        int
		errorText      string
	}{
		{"supported", agentdemo.Scenario, agentdemo.ScenarioVersion, ""},
		{"wrong scenario", "other", agentdemo.ScenarioVersion, "scenario"},
		{"future version", agentdemo.Scenario, agentdemo.ScenarioVersion + 1, "version"},
		{"missing version", agentdemo.Scenario, 0, "version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			m := &agentdemo.Manifest{Scenario: tc.scenario, ScenarioVersion: tc.version}
			require.NoError(m.Write(dir))
			got, err := agentdemo.Load(filepath.Clean(dir))
			if tc.errorText != "" {
				require.ErrorContains(err, tc.errorText)
				return
			}
			require.NoError(err)
			assert.Equal(t, m, got)
		})
	}
}
func TestEmailFixtureShapeAndRepresentativeSubjects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, m := archive(t, 1)
	for h, want := range map[string]string{"jordan_contract_question": "Signed contract for the summer project", "sam_trip_own_1": "Trip itinerary and lodging", "background_01_1": "Project archive notes 01"} {
		var subject string
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT subject FROM messages WHERE id=?", m.Messages[h]).Scan(&subject))
		assert.Equal(want, subject, h)
	}
	require.Contains(m.Participants, "background")
	// Background volume is a fixture contract independent of subject wording.
	assert.Equal(181, count(t, st, `SELECT count(*) FROM messages m WHERE sender_id=? OR EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.participant_id=?)`, m.Participants["background"].ID, m.Participants["background"].ID))
	// Imported mail retains raw records; Discord has no Phase 1 chapter.
	assert.Zero(count(t, st, "SELECT count(*) FROM messages m WHERE message_type='email' AND NOT EXISTS (SELECT 1 FROM message_raw r WHERE r.message_id=m.id)"))
	assert.Zero(count(t, st, "SELECT count(*) FROM messages WHERE source_id=?", m.Sources["discord"].ID))
}
