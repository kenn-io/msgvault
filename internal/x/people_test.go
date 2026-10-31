package x

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXAuthorRenameDoesNotTransferRecycledHandle(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	record := func(id, username string, at time.Time) int64 {
		t.Helper()
		pid, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: id, Username: username, Name: "Example Author"}, at)
		require.NoError(t, err)
		return pid
	}
	alice := record("1001", "alice", first)
	require.Positive(t, alice)
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), alice, new("Curated Name"))
	require.NoError(t, err)
	_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameFormatted, Formatted: new("Curated Name"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	assert.Equal(t, alice, record("1001", "alicia", first.Add(time.Hour)))
	other := record("1002", "alice", first.Add(2*time.Hour))
	assert.NotEqual(t, alice, other)
	assert.Equal(t, alice, record("1001", "alice", first))
	assert.Equal(t, alice, record("1001", "alice", first.Add(time.Hour))) // Equal-time conflict cannot overwrite current evidence.
	// A repeated current label must move its freshness forward too.
	record("1001", "alicia", first.Add(4*time.Hour))
	record("1001", "stale_handle", first.Add(3*time.Hour))
	observations, err := st.ListParticipantObservationsContext(t.Context(), alice, true)
	require.NoError(t, err)
	var usernames []string
	for _, o := range observations {
		assert.Equal(t, new("x"), o.ServiceSlug)
		assert.Nil(t, o.ScopeKind)
		assert.Nil(t, o.ScopeValue)
		assert.Equal(t, new("provider:1:x:7:service:0::4:1001"), o.ProviderUserID)
		if o.AddressKind == store.ContactAddressUsername {
			usernames = append(usernames, o.NormalizedValue)
			require.NotNil(t, o.ObservedAt)
			assert.Equal(t, first.Add(4*time.Hour), o.ObservedAt.UTC())
		}
	}
	assert.Equal(t, []string{"alicia"}, usernames)
	points, err := st.FindObservationsByAddressContext(t.Context(), store.ContactPointQuery{AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"), NormalizedValue: "alice"})
	require.NoError(t, err)
	require.Len(t, points, 1)
	assert.Equal(t, other, points[0].ParticipantID)
	names, err := st.ListPersonNamesContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	require.NotEmpty(t, names)
	var curated bool
	for _, n := range names {
		if n.Envelope.Source == store.ProvenanceUser && n.Formatted != nil && *n.Formatted == "Curated Name" {
			curated = true
		}
	}
	assert.True(t, curated)
	var service string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT cs.slug FROM participant_identifiers pi JOIN communication_services cs ON cs.id=pi.service_id WHERE pi.identifier_type='x_user_id' AND pi.identifier_value=?`), "1001").Scan(&service))
	assert.Equal(t, "x", service)
}

func TestXAuthorObservationFailureRollsBackRenameAndParticipant(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pid, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "current"}, now)
	require.NoError(t, err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION x_observation_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.original_value = 'fail_handle' THEN RAISE EXCEPTION 'synthetic observation failure'; END IF; RETURN NEW; END $$`)
		require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER x_observation_failure BEFORE INSERT ON participant_contact_observations FOR EACH ROW EXECUTE FUNCTION x_observation_failure()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER x_observation_failure BEFORE INSERT ON participant_contact_observations WHEN NEW.original_value = 'fail_handle' BEGIN SELECT RAISE(FAIL,'synthetic observation failure'); END`)
	}
	require.NoError(t, err)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "fail_handle"}, now.Add(time.Hour))
	require.Error(t, err)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1002", Username: "fail_handle"}, now.Add(time.Hour))
	require.Error(t, err)
	observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
	require.NoError(t, err)
	var current []string
	for _, o := range observations {
		if o.AddressKind == store.ContactAddressUsername {
			current = append(current, o.NormalizedValue)
		}
	}
	assert.Equal(t, []string{"current"}, current)
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1002'`).Scan(&count))
	assert.Zero(t, count)
}

func TestXMissingAuthorExpansionKeepsNumericIdentity(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	pid, err := RecordAuthor(t.Context(), st, source.ID, User{ID: "1001"}, time.Now())
	require.NoError(t, err)
	assert.Positive(t, pid)
	observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, store.ContactAddressProviderIdentity, observations[0].AddressKind)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{Username: "bare_handle"}, time.Now())
	require.Error(t, err)
	protected, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1002", Username: "private_handle", Protected: true}, time.Now())
	require.NoError(t, err)
	observations, err = st.ListParticipantObservationsContext(t.Context(), protected, true)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, store.ContactAddressProviderIdentity, observations[0].AddressKind)
}

func TestXAuthorRejectsInvalidIdentity(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	for _, id := range []string{"", "0", "01", "-1", "１２", strings.Repeat("9", 129)} {
		_, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: id, Username: "example"}, time.Now())
		require.Error(t, err)
	}
}

func TestXAuthorProtectedEvidenceBlocksStaleNames(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pid, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "public_handle"}, at)
	require.NoError(t, err)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "private_handle", Protected: true}, at.Add(3*time.Hour))
	require.NoError(t, err)
	for _, older := range []time.Time{at.Add(time.Hour), at.Add(3 * time.Hour)} {
		_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "public_handle"}, older)
		require.NoError(t, err)
	}
	observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
	require.NoError(t, err)
	for _, o := range observations {
		assert.NotEqual(t, store.ContactAddressUsername, o.AddressKind)
	}
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "restored"}, at.Add(4*time.Hour))
	require.NoError(t, err)
	observations, err = st.ListParticipantObservationsContext(t.Context(), pid, true)
	require.NoError(t, err)
	var handles []string
	for _, o := range observations {
		if o.AddressKind == store.ContactAddressUsername {
			handles = append(handles, o.NormalizedValue)
		}
	}
	assert.Equal(t, []string{"restored"}, handles)
}
