package telegram

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// beeperObservationFor records the provider-identity observation a Beeper
// bridge writes for one Telegram user on pid, under key.
func beeperObservationFor(
	t *testing.T, st *store.Store, pid int64, key, slug string,
) *store.RecordContactObservationResult {
	t.Helper()
	slugPtr := slug
	sourceRef := slug + ":bridge"
	observedAt := time.Now().UTC()
	result, err := st.RecordContactObservationContext(context.Background(), pid, store.ParticipantContactObservationInput{
		AddressKind:    store.ContactAddressProviderIdentity,
		ServiceSlug:    &slugPtr,
		ProviderUserID: &key,
		OriginalValue:  key,
		ObservedAt:     &observedAt,
		Envelope: store.ValueEnvelopeInput{
			Source:    store.ProvenanceArchiveObservation,
			SourceRef: &sourceRef,
		},
	})
	require.NoError(t, err)
	return result
}

// telegramSenderParticipantID returns the participant the import created
// for one numeric Telegram sender.
func telegramSenderParticipantID(t *testing.T, st *store.Store, nativeID string) int64 {
	t.Helper()
	pid, hasPhone, err := st.ParticipantByIdentifier("telegram", nativeID)
	require.NoError(t, err)
	require.NotZero(t, pid, "sender participant exists")
	require.False(t, hasPhone)
	return pid
}

func importFullFixture(t *testing.T, st *store.Store) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "full"))
	require.NoError(t, err)
	_, err = Import(context.Background(), st, ImportOptions{
		RootDir: dir, AttachmentsDir: t.TempDir(),
	})
	require.NoError(t, err)
}

// stableCandidatePairState returns the stable-provider candidate state for
// one participant pair, in either endpoint order.
func stableCandidatePairState(t *testing.T, st *store.Store, a, b int64) string {
	t.Helper()
	var state string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT state FROM identity_match_candidates
		WHERE basis = 'stable_provider_id'
		  AND ((left_id = ? AND right_id = ?) OR (left_id = ? AND right_id = ?))`),
		a, b, b, a).Scan(&state))
	return state
}

// TestExportAndBeeperObservationLinkToOneParticipant pins that the shared
// Telegram provider key links the export's participant and a Beeper bridge
// participant into one accepted identity in either arrival order.
func TestExportAndBeeperObservationLinkToOneParticipant(t *testing.T) {
	t.Run("bridge first", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		assert := assert.New(t)
		require := require.New(t)
		alice := "7000000000000555555"

		// The Beeper side first: a bridge participant carrying the shared key.
		bridgePID, err := st.EnsureParticipantByIdentifier("beeper", "beeper:7:acct-7:22:@alice:beeper.local", "Alice Example")
		require.NoError(err)
		beeperObservationFor(t, st, bridgePID, beeperidentity.TelegramStableProviderKey(alice), "telegram")

		importFullFixture(t, st)
		exportPID := telegramSenderParticipantID(t, st, alice)

		assert.Equal("accepted",
			stableCandidatePairState(t, st, bridgePID, exportPID),
			"shared key auto-links export and bridge observations")

		// Both participants carry the same provider key now.
		var keys int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(DISTINCT participant_id) FROM participant_contact_observations
			WHERE provider_user_id = ?`), beeperidentity.TelegramStableProviderKey(alice)).Scan(&keys))
		assert.Equal(2, keys, "both participants observed under the shared key")
	})

	t.Run("export first", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		assert := assert.New(t)
		require := require.New(t)
		alice := "7000000000000555555"

		importFullFixture(t, st)
		exportPID := telegramSenderParticipantID(t, st, alice)

		// The bridge side arrives after the export: the shared matcher entry
		// point links it the same way.
		bridgePID, err := st.EnsureParticipantByIdentifier("beeper", "beeper:7:acct-7:22:@alice:beeper.local", "Alice Example")
		require.NoError(err)
		result := beeperObservationFor(t, st, bridgePID, beeperidentity.TelegramStableProviderKey(alice), "telegram")
		require.NoError(beeper.MatchProviderIdentity(context.Background(), st, bridgePID, result))
		assert.Equal("accepted",
			stableCandidatePairState(t, st, bridgePID, exportPID),
			"both import orders accept the same link")
	})
}

func TestDifferentNetworkSameNumericIDDoesNotLink(t *testing.T) {
	st := testutil.NewTestStore(t)
	alice := "7000000000000555555"

	// A Discord participant carrying an account-scoped key whose native ID
	// happens to equal Alice's Telegram ID.
	discordPID, err := st.EnsureParticipantByIdentifier("discord", "discord:7:acct-7:22:7000000000000555555", "Alice Elsewhere")
	require.NoError(t, err)
	discordKey := "provider:7:discord:7:account:7:acct-7:19:7000000000000555555"
	beeperObservationFor(t, st, discordPID, discordKey, "discord")

	importFullFixture(t, st)
	exportPID := telegramSenderParticipantID(t, st, alice)

	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidates
		WHERE basis = 'stable_provider_id'
		  AND ((left_id = ? AND right_id = ?) OR (left_id = ? AND right_id = ?))`),
		discordPID, exportPID, exportPID, discordPID).Scan(&n))
	assert.Zero(t, n, "a different network's same numeric ID never links")
}

// TestChannelAndChatSendersResolveToNamespacedParticipants pins the
// documented from_id prefixes beyond "user": channel-authored posts and
// anonymous group admins resolve as Telegram-namespaced senders, while the
// shared provider-identity observation stays restricted to numeric user IDs
// — no bridge can corroborate a channel or group identity.
func TestChannelAndChatSendersResolveToNamespacedParticipants(t *testing.T) {
	st := testutil.NewTestStore(t)
	assert := assert.New(t)
	require := require.New(t)
	importFullFixture(t, st)

	// Both prefixed senders exist as participants under their full raw ID;
	// a bare channel ID could collide with an unrelated user's numeric ID.
	for _, identifier := range []string{"channel7000000000000777777", "chat-1009999999999"} {
		var n int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM participant_identifiers
			WHERE identifier_type = 'telegram' AND identifier_value = ?`),
			identifier).Scan(&n))
		assert.Equal(1, n, "sender %s is a participant", identifier)
	}

	// Both messages carry the sender and a From recipient.
	for _, msgID := range []string{"-1009999999999:202", "-1009999999999:206"} {
		var senderNull int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM messages m
			JOIN sources s ON s.id = m.source_id
			WHERE s.source_type = 'telegram' AND m.source_message_id = ?
			  AND m.sender_id IS NULL`), msgID).Scan(&senderNull))
		assert.Zero(senderNull, "%s has a sender participant", msgID)

		var fromCount int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*) FROM message_recipients r
			JOIN messages m ON m.id = r.message_id
			WHERE m.source_message_id = ? AND r.recipient_type = 'from'`), msgID).Scan(&fromCount))
		assert.Equal(1, fromCount, "%s has a From recipient", msgID)
	}

	// No provider observations exist for the non-user senders.
	var observations int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM participant_contact_observations o
		JOIN participant_identifiers i ON i.participant_id = o.participant_id
		WHERE i.identifier_type = 'telegram'
		  AND i.identifier_value IN ('channel7000000000000777777', 'chat-1009999999999')`,
	).Scan(&observations))
	assert.Zero(observations, "channel/chat senders get no provider observations")
}
