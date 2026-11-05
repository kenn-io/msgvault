package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestParticipantsByEmailMapsSavedAndUnsaved(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(alice)
	require.NoError(err)
	bob, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err)
	_, err = st.db.Exec(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,is_primary) VALUES (?,'email','Alias@Example.com',FALSE)`, bob)
	require.NoError(err)
	got, err := st.ParticipantsByEmailContext(t.Context(), []string{" ALICE@Example.com ", "bob@example.com", "alias@example.com", "missing@example.com", ""})
	require.NoError(err)
	assert.Equal(map[string]AddressParticipant{
		"alice@example.com": {ParticipantID: alice, PersonID: person.ID},
		"bob@example.com":   {ParticipantID: bob},
		"alias@example.com": {ParticipantID: bob},
	}, got)
	for address, participant := range got {
		id, err := st.EmailParticipantContext(t.Context(), address)
		require.NoError(err)
		assert.Equal(participant.ParticipantID, id)
	}
	// Ambiguous aliases must not pick a participant by row order.
	_, err = st.db.Exec(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,is_primary) VALUES (?,'apple_id','bob@example.com',FALSE)`, alice)
	require.NoError(err)
	got, err = st.ParticipantsByEmailContext(t.Context(), []string{"bob@example.com"})
	require.NoError(err)
	assert.Empty(got)
	id, err := st.EmailParticipantContext(t.Context(), "BOB@example.com")
	require.NoError(err)
	assert.Zero(id)
}

func TestEmailParticipantNonASCIIAddress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	participant, err := st.EnsureParticipant("Élodie@example.com", "Example", "example.com")
	require.NoError(err)
	_, err = st.db.Exec(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,is_primary) VALUES (?,'email','Álias@example.com',FALSE)`, participant)
	require.NoError(err)
	for _, address := range []string{"Élodie@EXAMPLE.com", "Álias@EXAMPLE.com"} {
		id, err := st.EmailParticipantContext(t.Context(), address)
		require.NoError(err)
		assert.Equal(participant, id)
	}
	got, err := st.ParticipantsByEmailContext(t.Context(), []string{" Élodie@EXAMPLE.com ", "Álias@EXAMPLE.com"})
	require.NoError(err)
	assert.Equal(map[string]AddressParticipant{
		"élodie@example.com": {ParticipantID: participant},
		"álias@example.com":  {ParticipantID: participant},
	}, got)
}

func TestParticipantsByEmailRespectsSourceScope(t *testing.T) {
	t.Run("visibility", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
		require.NoError(err)
		_, err = st.EnsureParticipant("bob@example.com", "Bob", "example.com")
		require.NoError(err)
		data, note := projectionFixture(t, st, "visible", "meeting_json", `{"action_items":[]}`)
		require.NoError(st.ReplaceMessageRecipients(note, "attendee", []int64{alice}, nil))
		ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID})
		got, err := st.ParticipantsByEmailContext(ctx, []string{"alice@example.com", "bob@example.com"})
		require.NoError(err)
		assert.Equal(map[string]AddressParticipant{"alice@example.com": {ParticipantID: alice}}, got)
	})
	t.Run("ambiguity", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		visible, err := st.EnsureParticipant("visible@example.com", "Visible", "example.com")
		require.NoError(err)
		hidden, err := st.EnsureParticipant("hidden@example.com", "Hidden", "example.com")
		require.NoError(err)
		for i, id := range []int64{visible, hidden} {
			_, err = st.db.Exec(`INSERT INTO participant_identifiers(participant_id,identifier_type,identifier_value,is_primary) VALUES (?,?,'Alias@Example.com',FALSE)`, id, []string{"email", "apple_id"}[i])
			require.NoError(err)
		}
		data, note := projectionFixture(t, st, "visible-note", "plaud_json", `{}`)
		require.NoError(st.ReplaceMessageRecipients(note, "attendee", []int64{visible}, nil))
		ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID})
		got, err := st.ParticipantsByEmailContext(ctx, []string{"alias@example.com"})
		require.NoError(err)
		assert.Equal(map[string]AddressParticipant{"alias@example.com": {ParticipantID: visible}}, got)
		got, err = st.ParticipantsByEmailContext(t.Context(), []string{"alias@example.com"})
		require.NoError(err)
		assert.Empty(got)
	})
}

func TestMeetingActionItemCountsByTranscript(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, one := projectionFixture(t, st, "one", "meeting_json", `{"action_items":[{"title":"Send brief"},{"title":"Book room","status":"done"}]}`)
	_, empty := projectionFixture(t, st, "empty", "meeting_json", `{"action_items":[]}`)
	_, deleted := projectionFixture(t, st, "deleted", "meeting_json", meetingProjectionRaw)
	_, err := st.db.Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, deleted)
	require.NoError(err)
	eventData, event := calendarFixture(t, st, "event", singleLinkRaw("event", "meeting@example.org"))
	got, err := st.MeetingActionItemCountsContext(t.Context(), []int64{one, one, empty, deleted, event, 999999})
	require.NoError(err)
	assert.Equal(map[int64]int{one: 2, empty: 0}, got)
	scoped, err := st.MeetingActionItemCountsContext(sourcescope.WithSourceIDs(t.Context(), []int64{eventData.Message.SourceID}), []int64{one})
	require.NoError(err)
	assert.Empty(scoped)
	scoped, err = st.MeetingActionItemCountsContext(sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID}), []int64{one, empty})
	require.NoError(err)
	assert.Equal(map[int64]int{one: 2, empty: 0}, scoped)
}

func TestParticipantsByEmailBatchReadBound(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var addresses []string
	want := map[string]AddressParticipant{}
	for i := range 20 {
		address := fmt.Sprintf("attendee-%d@example.com", i)
		id, err := st.EnsureParticipant(address, "Attendee", "example.com")
		require.NoError(err)
		addresses = append(addresses, address)
		want[address] = AddressParticipant{ParticipantID: id}
	}
	reads := 0
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(context.Context, string, []driver.NamedValue) error { reads++; return nil }})
	got, err := observed.ParticipantsByEmailContext(t.Context(), addresses)
	require.NoError(err)
	assert.Equal(want, got)
	assert.Equal(1, reads)
	for i := range 500 {
		addresses = append(addresses, fmt.Sprintf("unknown-%d@example.com", i))
	}
	reads = 0
	got, err = observed.ParticipantsByEmailContext(t.Context(), addresses)
	require.NoError(err)
	assert.Equal(want, got)
	assert.Equal(2, reads)
}
