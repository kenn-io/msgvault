package beeper

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestResolveUserPhoneRungDedupesAcrossSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// A native SMS/WhatsApp import already created this person by phone.
	existing, err := st.EnsureParticipantByPhone("+15550100001", "Alice Native", "whatsapp")
	require.NoError(err)

	pid, err := r.resolveUser(&User{
		ID:          "@15550100001:local-whatsapp.localhost",
		PhoneNumber: "+15550100001",
		FullName:    "Alice",
	})
	require.NoError(err)
	assert.Equal(existing, pid, "phone rung must dedupe with native imports")
}

func TestResolveUserEmailRung(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	existing, err := st.EnsureParticipant("bob@example.com", "Bob Mail", "example.com")
	require.NoError(err)

	pid, err := r.resolveUser(&User{
		ID:       "@linkedin_bob:beeper.local",
		Email:    "Bob@Example.com",
		FullName: "Bob",
	})
	require.NoError(err)
	assert.Equal(existing, pid, "email rung must lowercase and dedupe with mail archives")
}

func TestResolveUserIdentifierFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	pid, err := r.resolveUser(&User{ID: "@signal_uuid:beeper.local", FullName: "Carol"})
	require.NoError(err)
	require.NotZero(pid)

	var identifierType, identifierValue string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT identifier_type, identifier_value FROM participant_identifiers WHERE participant_id = ?`), pid,
	).Scan(&identifierType, &identifierValue))
	assert.Equal("beeper", identifierType)
	assert.Equal("@signal_uuid:beeper.local", identifierValue)
}

func TestResolveUserLadderIsExclusive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// A phone-resolvable user resolves through the phone rung (phone wins
	// over email), and the Beeper user ID is persisted as an identifier of
	// that same participant so later runs cannot fork the person.
	pid, err := r.resolveUser(&User{
		ID:          "@15550100002:local-whatsapp.localhost",
		PhoneNumber: "+15550100002",
		Email:       "dave@example.com", // phone wins over email
		FullName:    "Dave",
	})
	require.NoError(err)
	require.NotZero(pid)

	var matrixIDOwner int64
	require.NoError(st.DB().QueryRow(
		`SELECT participant_id FROM participant_identifiers WHERE identifier_value = '@15550100002:local-whatsapp.localhost'`,
	).Scan(&matrixIDOwner))
	assert.Equal(pid, matrixIDOwner, "the Beeper user ID must map to the phone participant, not a fork")

	var phone string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT phone_number FROM participants WHERE id = ?`), pid,
	).Scan(&phone))
	assert.Equal("+15550100002", phone)

	// The phone rung records the phone under the beeper identifier namespace.
	var phoneIdentRows int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id = ? AND identifier_type = 'beeper' AND identifier_value = '+15550100002'`), pid,
	).Scan(&phoneIdentRows))
	assert.Equal(1, phoneIdentRows)
}

func TestResolveUserCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	u := &User{ID: "@x:beeper.local", FullName: "X"}
	first, err := r.resolveUser(u)
	require.NoError(err)
	second, err := r.resolveUser(u)
	require.NoError(err)
	assert.Equal(first, second)
}

func TestResolveIDHitsSeededCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// Seeding from the chat participant list (phone rung)…
	seeded, err := r.resolveUser(&User{
		ID:          "@15550100003:local-whatsapp.localhost",
		PhoneNumber: "+15550100003",
		FullName:    "Eve",
	})
	require.NoError(err)

	// …means a later bare sender ID resolves to the same person, not a fork.
	pid, err := r.resolveID("@15550100003:local-whatsapp.localhost", "Eve")
	require.NoError(err)
	assert.Equal(seeded, pid)
}

func TestResolveUserUpgradesBareIDFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// A departed member is first seen as a bare sender ID (fallback rung)…
	weak, err := r.resolveID("@15550100004:local-whatsapp.localhost", "Fay")
	require.NoError(err)
	require.NotZero(weak)

	// …but the same user appears later in another chat's participant list
	// with a phone number. The richer metadata must win (deduping across
	// archives), not be blocked by the cached fallback.
	native, err := st.EnsureParticipantByPhone("+15550100004", "Fay Native", "imessage")
	require.NoError(err)

	rich, err := r.resolveUser(&User{
		ID:          "@15550100004:local-whatsapp.localhost",
		PhoneNumber: "+15550100004",
		FullName:    "Fay",
	})
	require.NoError(err)
	assert.Equal(native, rich, "phone metadata must dedupe with the native archive")
	assert.NotEqual(weak, rich)

	// Subsequent bare-ID lookups now resolve to the upgraded participant.
	again, err := r.resolveID("@15550100004:local-whatsapp.localhost", "Fay")
	require.NoError(err)
	assert.Equal(rich, again)
}

func TestRichResolutionPersistsAcrossRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Run 1 sees the user with a phone number in a chat's member list.
	r1 := newParticipantResolver(st)
	rich, err := r1.resolveUser(&User{
		ID:          "@15550100005:local-whatsapp.localhost",
		PhoneNumber: "+15550100005",
		FullName:    "Gil",
	})
	require.NoError(err)

	// Run 2 (fresh cache) sees the same user only as a bare sender ID (they
	// left the group): the persisted identifier must unify, not fork.
	r2 := newParticipantResolver(st)
	pid, err := r2.resolveID("@15550100005:local-whatsapp.localhost", "Gil")
	require.NoError(err)
	assert.Equal(rich, pid, "bare-ID resolution in a later run must find the rich participant")

	// And the reverse order: a weak participant from run 1 is re-pointed
	// when run 2 learns the phone number.
	r3 := newParticipantResolver(st)
	weak, err := r3.resolveID("@15550100006:beeper.local", "Hana")
	require.NoError(err)
	r4 := newParticipantResolver(st)
	native, err := st.EnsureParticipantByPhone("+15550100006", "Hana Native", "imessage")
	require.NoError(err)
	upgraded, err := r4.resolveUser(&User{
		ID:          "@15550100006:beeper.local",
		PhoneNumber: "+15550100006",
		FullName:    "Hana",
	})
	require.NoError(err)
	assert.Equal(native, upgraded)
	assert.NotEqual(weak, upgraded)
	r5 := newParticipantResolver(st)
	pid, err = r5.resolveID("@15550100006:beeper.local", "Hana")
	require.NoError(err)
	assert.Equal(native, pid, "the identifier row must be re-pointed at the rich participant")
}

func TestRichUpgradeMergesWeakHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Run 1: the user is only seen as a bare sender ID; messages, a reaction,
	// a mention, and membership are written under the weak participant.
	r1 := newParticipantResolver(st)
	weak, err := r1.resolveID("@15550100007:beeper.local", "Ida")
	require.NoError(err)

	src, err := st.GetOrCreateSource("beeper", "merge-test")
	require.NoError(err)
	convID, err := st.EnsureConversationWithType(src.ID, "!merge:beeper.local", "direct_chat", "Merge")
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(convID, weak, "member"))
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID, SourceMessageID: "mm1",
		MessageType: "beeper", SentAt: sql.NullTime{Time: time.Now(), Valid: true},
		SenderID: sql.NullInt64{Int64: weak, Valid: true},
	})
	require.NoError(err)
	require.NoError(st.UpsertReaction(msgID, weak, "emoji", "👍", time.Now()))
	require.NoError(st.ReplaceMessageRecipients(msgID, "mention", []int64{weak}, []string{""}))

	// Run 2: the same user appears with a phone number. The weak history must
	// fold into the rich participant, not stay split.
	r2 := newParticipantResolver(st)
	rich, err := r2.resolveUser(&User{
		ID:          "@15550100007:beeper.local",
		PhoneNumber: "+15550100007",
		FullName:    "Ida",
	})
	require.NoError(err)
	require.NotEqual(weak, rich)

	var senderID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), msgID).Scan(&senderID))
	assert.Equal(rich, senderID, "messages must move to the rich participant")

	var reactionOwner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT participant_id FROM reactions WHERE message_id = ?`), msgID).Scan(&reactionOwner))
	assert.Equal(rich, reactionOwner)

	var mentionOwner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'mention'`), msgID).Scan(&mentionOwner))
	assert.Equal(rich, mentionOwner)

	var memberOwner int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT participant_id FROM conversation_participants WHERE conversation_id = ?`), convID).Scan(&memberOwner))
	assert.Equal(rich, memberOwner)

	var weakRows int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participants WHERE id = ?`), weak).Scan(&weakRows))
	assert.Zero(weakRows, "the weak participant row must be deleted after the merge")
}

func TestPhoneUpgradesEmailResolution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)

	// First seen with only an email (e.g. a LinkedIn chat)…
	emailPID, err := r.resolveUser(&User{
		ID:       "@15550100008:beeper.local",
		Email:    "kim@example.com",
		FullName: "Kim",
	})
	require.NoError(err)

	// …later seen with an E.164 phone. Phone outranks email in the ladder:
	// the cached email resolution must not block deduping with the native
	// SMS archive, and the email participant's history folds into it.
	native, err := st.EnsureParticipantByPhone("+15550100008", "Kim Native", "imessage")
	require.NoError(err)
	phonePID, err := r.resolveUser(&User{
		ID:          "@15550100008:beeper.local",
		PhoneNumber: "+15550100008",
		Email:       "kim@example.com",
		FullName:    "Kim",
	})
	require.NoError(err)
	assert.Equal(native, phonePID, "phone metadata must dedupe with the native archive")
	assert.NotEqual(emailPID, phonePID)

	// The merge preserved the email and its analytics domain on the surviving
	// participant.
	var email, domain string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COALESCE(email_address, ''), COALESCE(domain, '') FROM participants WHERE id = ?`), phonePID).
		Scan(&email, &domain))
	assert.Equal("kim@example.com", email)
	assert.Equal("example.com", domain)

	// Cache and identifier now both point at the phone participant.
	again, err := r.resolveUser(&User{ID: "@15550100008:beeper.local", Email: "kim@example.com"})
	require.NoError(err)
	assert.Equal(phonePID, again)
	pid, err := r.resolveID("@15550100008:beeper.local", "Kim")
	require.NoError(err)
	assert.Equal(phonePID, pid)
}

func TestResolveIDEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	r := newParticipantResolver(st)
	pid, err := r.resolveID("", "nobody")
	require.NoError(err)
	assert.Zero(pid)
}

func TestPhoneUpgradesEmailResolutionAcrossRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Run 1 sees the user with only an email; the Beeper ID lands on the
	// email participant.
	r1 := newParticipantResolver(st)
	emailPID, err := r1.resolveUser(&User{
		ID:       "@15550100013:beeper.local",
		Email:    "mia@example.com",
		FullName: "Mia",
	})
	require.NoError(err)

	// Run 2 (fresh cache) sees a phone: the email participant is the same
	// Beeper user with weaker metadata — its history must fold into the
	// phone-deduped participant, not stay split.
	r2 := newParticipantResolver(st)
	phonePID, err := r2.resolveUser(&User{
		ID:          "@15550100013:beeper.local",
		PhoneNumber: "+15550100013",
		FullName:    "Mia",
	})
	require.NoError(err)
	require.NotEqual(emailPID, phonePID)

	var gone int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participants WHERE id = ?`), emailPID).Scan(&gone))
	assert.Zero(gone, "the email participant must be merged away")
	var email string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COALESCE(email_address, '') FROM participants WHERE id = ?`), phonePID).Scan(&email))
	assert.Equal("mia@example.com", email, "the merge preserves the email")

	// And the reverse across runs: an email-only sighting must not downgrade
	// a phone-resolved identity.
	r3 := newParticipantResolver(st)
	again, err := r3.resolveUser(&User{
		ID:       "@15550100013:beeper.local",
		Email:    "mia@example.com",
		FullName: "Mia",
	})
	require.NoError(err)
	assert.Equal(phonePID, again, "email-only sightings keep the phone participant")
}

func TestRichUpgradeDoesNotAbsorbContactOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// The Beeper user ID is already owned by a participant with its own
	// contact metadata (e.g. the person's old number from another archive).
	owner, err := st.EnsureParticipantByPhone("+15550100011", "Lea Old", "imessage")
	require.NoError(err)
	require.NoError(st.SetParticipantIdentifier(owner, "beeper", "@lea:beeper.local"))

	// Resolving the same Beeper ID with a different phone must re-point the
	// identifier, but never destructively merge a contact-bearing row.
	r := newParticipantResolver(st)
	pid, err := r.resolveUser(&User{
		ID:          "@lea:beeper.local",
		PhoneNumber: "+15550100012",
		FullName:    "Lea",
	})
	require.NoError(err)
	require.NotEqual(owner, pid)

	var oldPhone string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COALESCE(phone_number, '') FROM participants WHERE id = ?`), owner).Scan(&oldPhone))
	assert.Equal("+15550100011", oldPhone, "the previous owner must survive un-merged")

	again, err := r.resolveID("@lea:beeper.local", "Lea")
	require.NoError(err)
	assert.Equal(pid, again, "the identifier now points at the new resolution")
}
