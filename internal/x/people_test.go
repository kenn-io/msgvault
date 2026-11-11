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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	_testRequire.NoError(err)
	first := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	record := func(id, username string, at time.Time) int64 {
		t.Helper()
		pid, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: id, Username: username, Name: "Example Author"}, at)
		require.NoError(t, err)
		return pid
	}
	alice := record("1001", "alice", first)
	_testRequire.Positive(alice)
	person, _, err := st.CreatePersonFromParticipantWithDisplayNameContext(t.Context(), alice, new("Curated Name"))
	_testRequire.NoError(err)
	_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameFormatted, Formatted: new("Curated Name"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	_testRequire.NoError(err)
	_testAssert.Equal(alice, record("1001", "alicia", first.Add(time.Hour)))
	other := record("1002", "alice", first.Add(2*time.Hour))
	_testAssert.NotEqual(alice, other)
	_testAssert.Equal(alice, record("1001", "alice", first))
	_testAssert.Equal(alice, record("1001", "alice", first.Add(time.Hour))) // Equal-time conflict cannot overwrite current evidence.
	// A repeated current label must move its freshness forward too.
	record("1001", "alicia", first.Add(4*time.Hour))
	record("1001", "stale_handle", first.Add(3*time.Hour))
	observations, err := st.ListParticipantObservationsContext(t.Context(), alice, true)
	_testRequire.NoError(err)
	var usernames []string
	for _, o := range observations {
		_testAssert.Equal(new("x"), o.ServiceSlug)
		_testAssert.Nil(o.ScopeKind)
		_testAssert.Nil(o.ScopeValue)
		_testAssert.Equal(new("provider:1:x:7:service:0::4:1001"), o.ProviderUserID)
		if o.AddressKind == store.ContactAddressUsername {
			usernames = append(usernames, o.NormalizedValue)
			_testRequire.NotNil(o.ObservedAt)
			_testAssert.Equal(first.Add(4*time.Hour), o.ObservedAt.UTC())
		}
	}
	_testAssert.Equal([]string{"alicia"}, usernames)
	points, err := st.FindObservationsByAddressContext(t.Context(), store.ContactPointQuery{AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"), NormalizedValue: "alice"})
	_testRequire.NoError(err)
	_testRequire.Len(points, 1)
	_testAssert.Equal(other, points[0].ParticipantID)
	names, err := st.ListPersonNamesContext(t.Context(), person.ID, true)
	_testRequire.NoError(err)
	_testRequire.NotEmpty(names)
	var curated bool
	for _, n := range names {
		if n.Envelope.Source == store.ProvenanceUser && n.Formatted != nil && *n.Formatted == "Curated Name" {
			curated = true
		}
	}
	_testAssert.True(curated)
	var service string
	_testRequire.NoError(st.DB().QueryRow(st.Rebind(`SELECT cs.slug FROM participant_identifiers pi JOIN communication_services cs ON cs.id=pi.service_id WHERE pi.identifier_type='x_user_id' AND pi.identifier_value=?`), "1001").Scan(&service))
	_testAssert.Equal("x", service)
}

func TestXAuthorObservationFailureRollsBackRenameAndParticipant(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	_testRequire.NoError(err)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pid, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "current"}, now)
	_testRequire.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION x_observation_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.original_value = 'fail_handle' THEN RAISE EXCEPTION 'synthetic observation failure'; END IF; RETURN NEW; END $$`)
		_testRequire.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER x_observation_failure BEFORE INSERT ON participant_contact_observations FOR EACH ROW EXECUTE FUNCTION x_observation_failure()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER x_observation_failure BEFORE INSERT ON participant_contact_observations WHEN NEW.original_value = 'fail_handle' BEGIN SELECT RAISE(FAIL,'synthetic observation failure'); END`)
	}
	_testRequire.NoError(err)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "fail_handle"}, now.Add(time.Hour))
	_testRequire.Error(err)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1002", Username: "fail_handle"}, now.Add(time.Hour))
	_testRequire.Error(err)
	observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
	_testRequire.NoError(err)
	var current []string
	for _, o := range observations {
		if o.AddressKind == store.ContactAddressUsername {
			current = append(current, o.NormalizedValue)
		}
	}
	_testAssert.Equal([]string{"current"}, current)
	var count int
	_testRequire.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1002'`).Scan(&count))
	_testAssert.Zero(count)
}

func TestXMissingAuthorExpansionKeepsNumericIdentity(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	_testRequire.NoError(err)
	pid, err := RecordAuthor(t.Context(), st, source.ID, User{ID: "1001"}, time.Now())
	_testRequire.NoError(err)
	_testAssert.Positive(pid)
	observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
	_testRequire.NoError(err)
	_testRequire.Len(observations, 1)
	_testAssert.Equal(store.ContactAddressProviderIdentity, observations[0].AddressKind)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{Username: "bare_handle"}, time.Now())
	_testRequire.Error(err)
	protected, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1002", Username: "private_handle", Protected: true}, time.Now())
	_testRequire.NoError(err)
	observations, err = st.ListParticipantObservationsContext(t.Context(), protected, true)
	_testRequire.NoError(err)
	_testRequire.Len(observations, 1)
	_testAssert.Equal(store.ContactAddressProviderIdentity, observations[0].AddressKind)
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "9000")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pid, err := RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "public_handle"}, at)
	_testRequire.NoError(err)
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "private_handle", Protected: true}, at.Add(3*time.Hour))
	_testRequire.NoError(err)
	for _, older := range []time.Time{at.Add(time.Hour), at.Add(3 * time.Hour)} {
		_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "public_handle"}, older)
		_testRequire.NoError(err)
	}
	observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
	_testRequire.NoError(err)
	for _, o := range observations {
		_testAssert.NotEqual(store.ContactAddressUsername, o.AddressKind)
	}
	_, err = RecordAuthor(t.Context(), st, source.ID, User{ProtectedKnown: true, ID: "1001", Username: "restored"}, at.Add(4*time.Hour))
	_testRequire.NoError(err)
	observations, err = st.ListParticipantObservationsContext(t.Context(), pid, true)
	_testRequire.NoError(err)
	var handles []string
	for _, o := range observations {
		if o.AddressKind == store.ContactAddressUsername {
			handles = append(handles, o.NormalizedValue)
		}
	}
	_testAssert.Equal([]string{"restored"}, handles)
}
