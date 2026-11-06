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
	for _, tc := range []struct {
		name        string
		primary     string
		alias       string
		lookup      string
		aliasLookup string
		primaryKey  string
		aliasKey    string
	}{
		{"ASCII", "bob@example.com", "Alias@Example.com", "bob@example.com", "alias@example.com", "bob@example.com", "alias@example.com"},
		{"non-ASCII", "Élodie@example.com", "Álias@example.com", "Élodie@EXAMPLE.com", "Álias@EXAMPLE.com", "élodie@example.com", "álias@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			alice, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
			require.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(alice)
			require.NoError(err)
			participant, err := st.EnsureParticipant(tc.primary, "Example", "example.com")
			require.NoError(err)
			_, err = st.db.Exec(`INSERT INTO participant_identifiers (participant_id,identifier_type,identifier_value,is_primary) VALUES (?,'email',?,FALSE)`, participant, tc.alias)
			require.NoError(err)
			got, err := st.ParticipantsByEmailContext(t.Context(), []string{" ALICE@Example.com ", " " + tc.lookup + " ", tc.aliasLookup, "missing@example.com", ""})
			require.NoError(err)
			assert.Equal(map[string]AddressParticipant{
				"alice@example.com": {ParticipantID: alice, PersonID: person.ID},
				tc.primaryKey:       {ParticipantID: participant},
				tc.aliasKey:         {ParticipantID: participant},
			}, got)
			if tc.name == "non-ASCII" {
				for _, address := range []string{tc.lookup, tc.aliasLookup} {
					id, err := st.EmailParticipantContext(t.Context(), address)
					require.NoError(err)
					assert.Equal(participant, id)
				}
			}
		})
	}
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
	t.Run("visibility after merge", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		data, note := projectionFixture(t, st, "historical", "twenty_json", `{"schema_version":1,"recording":{}}`)
		participant, err := st.EnsureParticipant("attendee@example.org", "Attendee", "example.org")
		require.NoError(err)
		require.NoError(st.ReplaceMessageRecipients(note, "attendee", []int64{participant}, nil))
		into, err := st.GetOrCreateSource("meeting_import", "destination")
		require.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: data.Message.SourceID, IntoSourceID: into.ID})
		require.NoError(err)
		ctx := sourcescope.WithSourceIDs(t.Context(), []int64{into.ID})
		got, err := st.ParticipantsByEmailContext(ctx, []string{"attendee@example.org"})
		require.NoError(err)
		assert.Equal(map[string]AddressParticipant{"attendee@example.org": {ParticipantID: participant}}, got)
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
