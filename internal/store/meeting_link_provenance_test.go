package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingProjectionGranolaProvenanceUpgrade(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	_, id := projectionFixture(t, st, "provenance", "granola_json", `{"calendar_event":{"organiser":"host@example.com","invitees":[{"email":"guest@example.org"}]}}`)
	_, err := st.db.Exec(`UPDATE meeting_details SET projection_version = 1, content_json = ? WHERE message_id = ?`, `{"source_participants":[{"email":"guest@example.org","role":"to"}]}`, id)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE applied_migrations SET version = 1 WHERE name = ?`, migrationMeetingProjectionV1)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	content, _ := readProjection(t, st, id)
	requirements.Len(content.SourceParticipants, 2)
	assert.Equal(t, "invited", content.SourceParticipants[1].Provenance)
}
