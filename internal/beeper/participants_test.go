package beeper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveUserPhoneRungDedupesAcrossSources(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// A native SMS/WhatsApp import already created this person by phone.
	existing, err := st.EnsureParticipantByPhone("+15550100001", "Alice Native", "whatsapp")
	require.NoError(t, err)

	pid, err := r.resolveUser(&User{
		ID:          "@15550100001:local-whatsapp.localhost",
		PhoneNumber: "+15550100001",
		FullName:    "Alice",
	})
	require.NoError(t, err)
	assert.Equal(t, existing, pid, "phone rung must dedupe with native imports")
}

func TestResolveUserEmailRung(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	existing, err := st.EnsureParticipant("bob@example.com", "Bob Mail", "example.com")
	require.NoError(t, err)

	pid, err := r.resolveUser(&User{
		ID:       "@linkedin_bob:beeper.local",
		Email:    "Bob@Example.com",
		FullName: "Bob",
	})
	require.NoError(t, err)
	assert.Equal(t, existing, pid, "email rung must lowercase and dedupe with mail archives")
}

func TestResolveUserIdentifierFallback(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	pid, err := r.resolveUser(&User{ID: "@signal_uuid:beeper.local", FullName: "Carol"})
	require.NoError(t, err)
	require.NotZero(t, pid)

	var identifierType, identifierValue string
	require.NoError(t, st.DB().QueryRow(
		`SELECT identifier_type, identifier_value FROM participant_identifiers WHERE participant_id = ?`, pid,
	).Scan(&identifierType, &identifierValue))
	assert.Equal(t, "beeper", identifierType)
	assert.Equal(t, "@signal_uuid:beeper.local", identifierValue)
}

func TestResolveUserLadderIsExclusive(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// A phone-resolvable user must resolve through the phone rung only: the
	// Matrix ID must never become its own identifier row (calling two rungs
	// would fork the person on later bare-ID lookups).
	pid, err := r.resolveUser(&User{
		ID:          "@15550100002:local-whatsapp.localhost",
		PhoneNumber: "+15550100002",
		Email:       "dave@example.com", // phone wins over email
		FullName:    "Dave",
	})
	require.NoError(t, err)
	require.NotZero(t, pid)

	var matrixIDRows int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_value = '@15550100002:local-whatsapp.localhost'`,
	).Scan(&matrixIDRows))
	assert.Zero(t, matrixIDRows)

	var phone string
	require.NoError(t, st.DB().QueryRow(
		`SELECT phone_number FROM participants WHERE id = ?`, pid,
	).Scan(&phone))
	assert.Equal(t, "+15550100002", phone)

	// The phone rung records the phone under the beeper identifier namespace.
	var phoneIdentRows int
	require.NoError(t, st.DB().QueryRow(
		`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id = ? AND identifier_type = 'beeper' AND identifier_value = '+15550100002'`, pid,
	).Scan(&phoneIdentRows))
	assert.Equal(t, 1, phoneIdentRows)
}

func TestResolveUserCache(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	u := &User{ID: "@x:beeper.local", FullName: "X"}
	first, err := r.resolveUser(u)
	require.NoError(t, err)
	second, err := r.resolveUser(u)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestResolveIDHitsSeededCache(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// Seeding from the chat participant list (phone rung)…
	seeded, err := r.resolveUser(&User{
		ID:          "@15550100003:local-whatsapp.localhost",
		PhoneNumber: "+15550100003",
		FullName:    "Eve",
	})
	require.NoError(t, err)

	// …means a later bare sender ID resolves to the same person, not a fork.
	pid, err := r.resolveID("@15550100003:local-whatsapp.localhost", "Eve")
	require.NoError(t, err)
	assert.Equal(t, seeded, pid)
}

func TestResolveIDEmpty(t *testing.T) {
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)
	pid, err := r.resolveID("", "nobody")
	require.NoError(t, err)
	assert.Zero(t, pid)
}
