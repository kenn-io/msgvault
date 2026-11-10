package agentdemo_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
	"go.kenn.io/msgvault/internal/search"
)

func TestPhaseOneDoesNotDeclareDeletion(t *testing.T) {
	st, m := archive(t, 1)
	assert.NotContains(t, m.Expect, "deleted_at_source")
	require.NotZero(t, m.Messages["deleted_at_source"])
	assert.Zero(t, count(t, st, "SELECT count(*) FROM messages WHERE deleted_from_source_at IS NOT NULL"))
}

func TestPatFirstCorrespondenceIsFromOwner(t *testing.T) {
	st, m := archive(t, 1)
	var id int64
	var own bool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT m.id,m.is_from_me FROM messages m WHERE m.sender_id=? OR EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.participant_id=?) ORDER BY m.sent_at,m.id LIMIT 1`, m.Participants["pat"].ID, m.Participants["pat"].ID).Scan(&id, &own))
	assert.Equal(t, m.Messages["pat_quote_outbound"], id)
	assert.True(t, own)
}

func TestSharedAttachmentVisibleInSearch(t *testing.T) {
	st, m := archive(t, 1)
	ids, total, err := st.SearchMessageIDsQueryContext(t.Context(), search.ParseOR("has:attachment"), 50)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	for _, h := range m.Attachments["contract_v2"].MessageHandles {
		assert.Contains(t, ids, m.Messages[h], h)
		var has bool
		var n int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT has_attachments,attachment_count FROM messages WHERE id=?", m.Messages[h]).Scan(&has, &n))
		assert.True(t, has, h)
		assert.Equal(t, 1, n, h)
	}
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
			dir := t.TempDir()
			m := &agentdemo.Manifest{Scenario: tc.scenario, ScenarioVersion: tc.version}
			require.NoError(t, m.Write(dir))
			got, err := agentdemo.Load(filepath.Clean(dir))
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, m, got)
		})
	}
}
func TestEmailSubjectsAreReadable(t *testing.T) {
	st, m := archive(t, 1)
	for h, want := range map[string]string{"jordan_contract_question": "Signed contract for the summer project", "sam_trip_own_1": "Trip itinerary and lodging", "background_01_1": "Project archive notes 01"} {
		var subject string
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT subject FROM messages WHERE id=?", m.Messages[h]).Scan(&subject))
		assert.Equal(t, want, subject, h)
	}
	require.Contains(t, m.Participants, "background")
	assert.Zero(t, count(t, st, "SELECT count(*) FROM person_participants WHERE participant_id=?", m.Participants["background"].ID))
	assert.Equal(t, 181, count(t, st, `SELECT count(*) FROM messages m WHERE sender_id=? OR EXISTS (SELECT 1 FROM message_recipients r WHERE r.message_id=m.id AND r.participant_id=?)`, m.Participants["background"].ID, m.Participants["background"].ID))
}
