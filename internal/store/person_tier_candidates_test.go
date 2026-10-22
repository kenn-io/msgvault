package store_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonTierCandidatesLargeLinkedGroupRespectsSQLiteBindLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))

	const memberCount = 500
	const domain = "large.example.test"
	root := f.EnsureParticipant("root@"+domain, "Root", domain)
	for i := 1; i < memberCount; i++ {
		f.EnsureParticipant(fmt.Sprintf("alias-%03d@%s", i, domain), fmt.Sprintf("Alias %d", i), domain)
	}
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO participant_links(participant_a,participant_b)
		SELECT ?,id FROM participants WHERE id>? AND domain=?`), root, root, domain)
	require.NoError(err)

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, outbound := range []bool{false, false, true} {
		from, to := root, owner
		if outbound {
			from, to = owner, root
		}
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("large-group-%d", i)).
			WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
	}

	if !f.Store.IsPostgreSQL() {
		f.Store.DB().SetMaxOpenConns(1)
		conn, err := f.Store.DB().Conn(t.Context())
		require.NoError(err)
		err = conn.Raw(func(driverConn any) error {
			sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
			require.True(ok, "test store uses SQLite")
			sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
			return nil
		})
		require.NoError(err)
		require.NoError(conn.Close())
	}

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(root, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
}

func TestPersonTierCandidatesUseResolvedSendersNativeOwnershipAndMembershipFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	misleading := f.EnsureParticipant("misleading@example.com", "Misleading", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// Two inbound messages resolve the From envelope, even though sender_id
	// points at somebody else. Baked is_from_me is intentionally misleading.
	for i := range 2 {
		m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-in-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		m.SenderID = sql.NullInt64{Int64: misleading, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_from_me=TRUE,identity_is_from_me=TRUE WHERE id=?`), id)
		require.NoError(err)
	}
	conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "candidate-native", "direct_chat", "Synthetic chat")
	require.NoError(err)
	require.NoError(f.Store.EnsureConversationParticipant(conv, peer, "member"))
	m := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID("candidate-native-out").WithSentAt(asOf.AddDate(0, 0, -2)).Build()
	nativeID, err := f.Store.UpsertMessage(m)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_is_from_me=TRUE WHERE id=?`), nativeID)
	require.NoError(err)
	before, err := f.Store.ListPersons()
	require.NoError(err)
	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	got := page.Candidates[0]
	assert.Equal(peer, got.ParticipantID)
	assert.Equal("participant_approximation", got.Basis)
	assert.Equal("msgvault person promote "+strconv.FormatInt(peer, 10), got.SuggestedCommand)
	assert.Equal(1, got.OutboundShort)
	assert.Equal(2, got.InboundShort)
	assert.Equal(3, got.ActiveDaysShort)
	after, err := f.Store.ListPersons()
	require.NoError(err)
	assert.Equal(before, after, "a candidate read never creates a profile")
}

func TestPersonTierCandidatesRejectBroadcastCoauthorsListsAndOwnerAbsent(t *testing.T) {
	for _, kind := range []string{"large_inbound", "large_outbound", "coauthor", "list", "observed", "meeting_outbound"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
			peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
			stranger := f.EnsureParticipant("stranger@example.com", "Stranger", "example.com")
			require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
			asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
			for i := range 3 {
				outbound := i == 2
				from, to := []int64{peer}, []int64{owner}
				if outbound {
					from, to = []int64{owner}, []int64{peer}
				}
				if kind == "large_inbound" && !outbound {
					to = append(to, stranger)
				}
				if kind == "large_outbound" && outbound {
					to = append(to, stranger)
				}
				if kind == "coauthor" && outbound {
					from = append(from, peer)
					to = []int64{stranger}
				}
				if kind == "observed" && !outbound {
					to = []int64{stranger}
				}
				m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-reject-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
				if kind == "meeting_outbound" && outbound {
					m.MessageType = "calendar_event"
				}
				if kind == "list" {
					m.ListID = sql.NullString{String: "synthetic.example.com", Valid: true}
				}
				id, err := f.Store.UpsertMessage(m)
				require.NoError(err)
				require.NoError(f.Store.ReplaceMessageRecipients(id, "from", from, nil))
				require.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
			}
			maxDirect := 1
			if kind == "coauthor" || kind == "observed" {
				maxDirect = 25
			}
			page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", maxDirect)
			require.NoError(err)
			assert.Empty(page.Candidates)
			if kind == "list" {
				cfg := persontiers.DefaultConfig()
				cfg.ExcludeListMessages = false
				page, err = f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, cfg, "UTC", 25)
				require.NoError(err)
				require.Len(page.Candidates, 1)
				assert.Equal(peer, page.Candidates[0].ParticipantID)
			}
		})
	}
}

func TestPersonTierCandidatesUseFullChatMembershipForAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	extra := f.EnsureParticipant("extra@example.com", "Extra", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-large-group", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, peer, extra} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, outbound := range []bool{false, false, true} {
		from, to := peer, owner
		if outbound {
			from, to = owner, peer
		}
		message := storetest.NewMessage(f.Source.ID, conversation).
			WithSourceMessageID(fmt.Sprintf("tier-chat-audience-%d", i)).
			WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
		// The envelope names only one recipient. The conversation roster still
		// has two non-owner participants and exceeds this direct-audience limit.
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
	}

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	assert.Empty(page.Candidates)
}

func TestPersonTierCandidatesLinkedAliasesRespectChatRosterLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	alias := f.EnsureParticipant("peer-alias@example.com", "Peer alias", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	_, err := f.Store.LinkParticipants(peer, alias)
	require.NoError(err)

	group, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-peer-group", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, peer, alias} {
		require.NoError(f.Store.EnsureConversationParticipant(group, participant, "member"))
	}
	direct, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-peer-direct", "direct_chat", "Synthetic chat")
	require.NoError(err)
	for _, participant := range []int64{owner, peer} {
		require.NoError(f.Store.EnsureConversationParticipant(direct, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	addTierCandidateAudienceMessage(t, f, group, "tier-linked-chat-in-1", asOf, peer, []int64{owner})
	addTierCandidateAudienceMessage(t, f, group, "tier-linked-chat-in-2", asOf.AddDate(0, 0, -1), alias, []int64{owner})
	addTierCandidateAudienceMessage(t, f, direct, "tier-linked-chat-out", asOf.AddDate(0, 0, -2), owner, []int64{peer})

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	assert.Empty(page.Candidates)
}

func TestPersonTierCandidatesLinkedAliasesRespectEnvelopeLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	alias := f.EnsureParticipant("peer-alias@example.com", "Peer alias", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	_, err := f.Store.LinkParticipants(peer, alias)
	require.NoError(err)
	email, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-peer-email", "email", "Synthetic email")
	require.NoError(err)

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	addTierCandidateAudienceMessage(t, f, email, "tier-linked-email-in-1", asOf, peer, []int64{owner})
	addTierCandidateAudienceMessage(t, f, email, "tier-linked-email-in-2", asOf.AddDate(0, 0, -1), alias, []int64{owner})
	addTierCandidateAudienceMessage(t, f, email, "tier-linked-email-out", asOf.AddDate(0, 0, -2), owner, []int64{peer, alias})

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	assert.Empty(page.Candidates)
}

func TestPersonTierCandidatesMergedPersonAliasesRespectChatRosterLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	contact := f.EnsureParticipant("contact@example.com", "Contact", "example.com")
	contactAlias := f.EnsureParticipant("contact-alias@example.com", "Contact alias", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	mergeTierTestParticipants(t, f.Store, contact, contactAlias, "tier-candidate-merged-chat-contact")

	group, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-merged-contact-group", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, peer, contact, contactAlias} {
		require.NoError(f.Store.EnsureConversationParticipant(group, participant, "member"))
	}
	direct, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-merged-contact-direct", "direct_chat", "Synthetic chat")
	require.NoError(err)
	for _, participant := range []int64{owner, peer} {
		require.NoError(f.Store.EnsureConversationParticipant(direct, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	addTierCandidateAudienceMessage(t, f, group, "tier-merged-chat-in-1", asOf, peer, []int64{owner})
	addTierCandidateAudienceMessage(t, f, group, "tier-merged-chat-in-2", asOf.AddDate(0, 0, -1), peer, []int64{owner})
	addTierCandidateAudienceMessage(t, f, direct, "tier-merged-chat-out", asOf.AddDate(0, 0, -2), owner, []int64{peer})

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 2)
	require.NoError(err)
	assert.Empty(page.Candidates)
}

func TestPersonTierCandidatesMergedPersonAliasesRespectEnvelopeLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	contact := f.EnsureParticipant("contact@example.com", "Contact", "example.com")
	contactAlias := f.EnsureParticipant("contact-alias@example.com", "Contact alias", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	mergeTierTestParticipants(t, f.Store, contact, contactAlias, "tier-candidate-merged-email-contact")
	email, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-merged-contact-email", "email", "Synthetic email")
	require.NoError(err)

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	addTierCandidateAudienceMessage(t, f, email, "tier-merged-email-in-1", asOf, peer, []int64{owner, contact, contactAlias})
	addTierCandidateAudienceMessage(t, f, email, "tier-merged-email-in-2", asOf.AddDate(0, 0, -1), peer, []int64{owner, contact, contactAlias})
	addTierCandidateAudienceMessage(t, f, email, "tier-merged-email-out", asOf.AddDate(0, 0, -2), owner, []int64{peer})

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 2)
	require.NoError(err)
	assert.Empty(page.Candidates)
}

func addTierCandidateAudienceMessage(
	t *testing.T,
	f *storetest.Fixture,
	conversationID int64,
	sourceMessageID string,
	sentAt time.Time,
	from int64,
	to []int64,
) int64 {
	t.Helper()
	require := require.New(t)
	message := storetest.NewMessage(f.Source.ID, conversationID).
		WithSourceMessageID(sourceMessageID).WithSentAt(sentAt).Build()
	message.SenderID = sql.NullInt64{Int64: from, Valid: true}
	id, err := f.Store.UpsertMessage(message)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
	return id
}

func TestPersonTierCandidatesExcludeLinkedOwnerAliasesFromChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias, err := f.Store.EnsureParticipantByIdentifier("beeper", "synthetic-owner-id", "Owner alias")
	require.NoError(err)
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "synthetic-owner-id", "manual"))
	_, err = f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-owner-candidate", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, ownerAlias, peer} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, outbound := range []bool{false, false, true} {
		from, to := peer, ownerAlias
		if outbound {
			from, to = ownerAlias, peer
		}
		message := storetest.NewMessage(f.Source.ID, conversation).
			WithSourceMessageID(fmt.Sprintf("tier-linked-owner-candidate-%d", i)).
			WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
	}

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(peer, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
	assert.Equal(3, page.Candidates[0].ActiveDaysShort)
}

func TestPersonTierCandidatesExcludeMergedSourceOwnerFromChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	ownerSurvivor := f.EnsureParticipant("merged-owner@example.com", "Merged owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner-alias@example.com", "manual"))

	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-merged-source-owner-candidate", "group_chat", "Synthetic group")
	require.NoError(err)
	for _, participant := range []int64{owner, ownerAlias, peer} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	inboundMessageIDs := make([]int64, 0, 2)
	for i := range 2 {
		messageID := addTierCandidateAudienceMessage(
			t, f, conversation, fmt.Sprintf("tier-merged-source-owner-in-%d", i), asOf.AddDate(0, 0, -i), peer, []int64{owner},
		)
		inboundMessageIDs = append(inboundMessageIDs, messageID)
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			UPDATE message_recipients SET email_address = ?
			WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`),
			"owner@example.com", messageID, owner)
		require.NoError(err)
	}
	addTierCandidateAudienceMessage(
		t, f, conversation, "tier-merged-source-owner-out", asOf.AddDate(0, 0, -2), ownerAlias, []int64{peer},
	)
	require.NoError(f.Store.MergeParticipants(owner, ownerSurvivor))

	var archivedOwnerAddress string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		SELECT email_address FROM message_recipients
		WHERE message_id = ? AND participant_id = ? AND recipient_type = 'to'`), inboundMessageIDs[0], ownerSurvivor).
		Scan(&archivedOwnerAddress))
	require.Equal("owner@example.com", archivedOwnerAddress)

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(peer, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
	assert.Equal(3, page.Candidates[0].ActiveDaysShort)
}

func TestPersonTierCandidatesExcludeLinkedOwnerAliasesFromEnvelopeAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	_, err := f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-linked-owner-envelope", "email", "Synthetic email")
	require.NoError(err)

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for i, outbound := range []bool{false, false, true} {
		from, to := peer, []int64{owner, ownerAlias}
		if outbound {
			from, to = owner, []int64{peer}
		}
		message := storetest.NewMessage(f.Source.ID, conversation).
			WithSourceMessageID(fmt.Sprintf("tier-linked-owner-envelope-%d", i)).
			WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
	}

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(peer, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
	assert.Equal(3, page.Candidates[0].ActiveDaysShort)
}

func TestPersonTierCandidatesExcludeResolvedNativeOwnerFromChatAudience(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	ownerAlias := f.EnsureParticipant("owner-alias@example.com", "Owner alias", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	_, err := f.Store.LinkParticipants(owner, ownerAlias)
	require.NoError(err)

	// The native source identifies its sender as the owner without an account
	// identity row for this source. The roster still includes that owner.
	conversation, err := f.Store.EnsureConversationWithType(f.Source.ID, "tier-native-owner", "direct_chat", "Synthetic chat")
	require.NoError(err)
	for _, participant := range []int64{owner, ownerAlias, peer} {
		require.NoError(f.Store.EnsureConversationParticipant(conversation, participant, "member"))
	}
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	outbound := storetest.NewMessage(f.Source.ID, conversation).
		WithSourceMessageID("tier-native-owner-outbound").
		WithSentAt(asOf.AddDate(0, 0, -1)).Build()
	outbound.SenderID = sql.NullInt64{Int64: owner, Valid: true}
	outboundID, err := f.Store.UpsertMessage(outbound)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageRecipients(outboundID, "from", []int64{owner}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(outboundID, "to", []int64{peer}, nil))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_is_from_me=TRUE WHERE id=?`), outboundID)
	require.NoError(err)

	// A second source supplies ordinary owner-recognized inbound activity so
	// the peer meets the promotion floor without adding an identity to the
	// source whose chat audience is under test.
	otherSource, err := f.Store.GetOrCreateSource("gmail", "other@example.invalid")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(otherSource.ID, "owner@example.com", "manual"))
	otherConversation, err := f.Store.EnsureConversationWithType(otherSource.ID, "tier-native-owner-inbound", "direct_chat", "Synthetic chat")
	require.NoError(err)
	for _, participant := range []int64{owner, peer} {
		require.NoError(f.Store.EnsureConversationParticipant(otherConversation, participant, "member"))
	}
	for i := 2; i <= 3; i++ {
		inbound := storetest.NewMessage(otherSource.ID, otherConversation).
			WithSourceMessageID(fmt.Sprintf("tier-native-owner-inbound-%d", i)).
			WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		inbound.SenderID = sql.NullInt64{Int64: peer, Valid: true}
		inboundID, err := f.Store.UpsertMessage(inbound)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(inboundID, "from", []int64{peer}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(inboundID, "to", []int64{owner}, nil))
	}

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(peer, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
}

func TestPersonTierCandidatesCombineActivityAcrossLinkedAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	first := f.EnsureParticipant("first-alias@example.com", "First alias", "example.com")
	second := f.EnsureParticipant("second-alias@example.com", "Second alias", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	_, err := f.Store.LinkParticipants(first, second)
	require.NoError(err)

	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	activity := []struct {
		at   time.Time
		from int64
		to   int64
	}{
		{at: asOf.AddDate(0, 0, -2), from: first, to: owner},
		{at: asOf.AddDate(0, 0, -1), from: owner, to: second},
		{at: asOf, from: first, to: owner},
	}
	for i, event := range activity {
		m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-linked-%d", i)).WithSentAt(event.at).Build()
		id, err := f.Store.UpsertMessage(m)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{event.from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{event.to}, nil))
	}

	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(first, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
	assert.Equal(3, page.Candidates[0].ActiveDaysShort)
	_, _, err = f.Store.CreatePersonFromParticipant(second)
	require.NoError(err)
	page, err = f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(err)
	assert.Empty(page.Candidates)
}

func TestPersonTierCandidatesUnlinkedCanonicalAliasesSurvivePromotion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	first := f.EnsureParticipant("first@example.com", "First", "example.com")
	alias := f.EnsureParticipant("alias@example.com", "Alias", "example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	for _, id := range []int64{first, alias} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET canonical_id='synthetic-canonical' WHERE id=?`), id)
		require.NoError(err)
	}
	for _, peer := range []int64{first, second} {
		for i := range 3 {
			from, to := owner, peer
			if i < 2 {
				from, to = peer, owner
			}
			m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-group-%d-%d", peer, i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
			id, err := f.Store.UpsertMessage(m)
			require.NoError(err)
			require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
			require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
		}
	}
	var all []store.PersonTierCandidate
	cursor := int64(0)
	for {
		page, err := f.Store.PersonTierCandidatesContext(t.Context(), cursor, 1, asOf, persontiers.DefaultConfig(), "UTC", 25)
		require.NoError(err)
		all = append(all, page.Candidates...)
		if page.NextCursor == "" {
			break
		}
		next, err := strconv.ParseInt(page.NextCursor, 10, 64)
		require.NoError(err)
		require.Greater(next, cursor)
		cursor = next
	}
	require.Len(all, 2)
	assert.Equal(first, all[0].ParticipantID)
	assert.Equal(second, all[1].ParticipantID)
	person, created, err := f.Store.CreatePersonFromParticipant(alias)
	require.NoError(err)
	require.True(created)
	assert.Equal([]int64{alias}, person.ParticipantIDs)
	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(err)
	require.Len(page.Candidates, 2)
	assert.Equal(first, page.Candidates[0].ParticipantID)
	assert.Equal(second, page.Candidates[1].ParticipantID)
}

func TestPersonTierCandidatesUseCalendarShortWindowAndHardExclusions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	human := f.EnsureParticipant("human@example.com", "Human", "example.com")
	automated := f.EnsureParticipant("noreply@example.com", "Automated", "example.com")
	room := f.EnsureParticipant("room@rooms.example.com", "Room", "rooms.example.com")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(err)
	asOf := time.Date(2026, 11, 2, 0, 0, 0, 0, zone)
	cfg := persontiers.DefaultConfig()
	cfg.ShortWindowDays = 3
	cfg.ExcludeDomainPatterns = []string{"rooms.example.com"}
	for _, peer := range []int64{human, automated, room} {
		for i, days := range []int{-3, -2, -1, 0, 1} {
			from, to := peer, owner
			if days == -2 {
				from, to = owner, peer
			}
			at := asOf.AddDate(0, 0, days).Add(23*time.Hour + 30*time.Minute)
			m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-window-%d-%d", peer, i)).WithSentAt(at).Build()
			id, err := f.Store.UpsertMessage(m)
			require.NoError(err)
			require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
			require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
		}
	}
	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, cfg, zone.String(), 25)
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(human, page.Candidates[0].ParticipantID)
	assert.Equal(1, page.Candidates[0].OutboundShort)
	assert.Equal(2, page.Candidates[0].InboundShort)
	assert.Equal(3, page.Candidates[0].ActiveDaysShort)
}
