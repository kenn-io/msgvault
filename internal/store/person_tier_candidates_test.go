package store_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonTierCandidatesUseResolvedSendersNativeOwnershipAndMembershipFallback(t *testing.T) {
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
	misleading := f.EnsureParticipant("misleading@example.com", "Misleading", "example.com")
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	// Two inbound messages resolve the From envelope, even though sender_id
	// points at somebody else. Baked is_from_me is intentionally misleading.
	for i := range 2 {
		m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-in-%d", i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		m.SenderID = sql.NullInt64{Int64: misleading, Valid: true}
		id, err := f.Store.UpsertMessage(m)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
		require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_from_me=TRUE,identity_is_from_me=TRUE WHERE id=?`), id)
		require.NoError(t, err)
	}
	conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "candidate-native", "direct_chat", "Synthetic chat")
	require.NoError(t, err)
	require.NoError(t, f.Store.EnsureConversationParticipant(conv, peer, "member"))
	m := storetest.NewMessage(f.Source.ID, conv).WithSourceMessageID("candidate-native-out").WithSentAt(asOf.AddDate(0, 0, -2)).Build()
	nativeID, err := f.Store.UpsertMessage(m)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_is_from_me=TRUE WHERE id=?`), nativeID)
	require.NoError(t, err)
	before, err := f.Store.ListPersons()
	require.NoError(t, err)
	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(t, err)
	require.Len(t, page.Candidates, 1)
	got := page.Candidates[0]
	assert.Equal(t, peer, got.ParticipantID)
	assert.Equal(t, "participant_approximation", got.Basis)
	assert.Equal(t, "msgvault person promote "+strconv.FormatInt(peer, 10), got.SuggestedCommand)
	assert.Equal(t, 1, got.OutboundShort)
	assert.Equal(t, 2, got.InboundShort)
	assert.Equal(t, 3, got.ActiveDaysShort)
	after, err := f.Store.ListPersons()
	require.NoError(t, err)
	assert.Equal(t, before, after, "a candidate read never creates a profile")
}

func TestPersonTierCandidatesRejectBroadcastCoauthorsListsAndOwnerAbsent(t *testing.T) {
	for _, kind := range []string{"large_inbound", "large_outbound", "coauthor", "list", "observed", "meeting_outbound"} {
		t.Run(kind, func(t *testing.T) {
			f := storetest.New(t)
			owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
			peer := f.EnsureParticipant("peer@example.com", "Peer", "example.com")
			stranger := f.EnsureParticipant("stranger@example.com", "Stranger", "example.com")
			require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
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
				require.NoError(t, err)
				require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", from, nil))
				require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", to, nil))
			}
			page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 1)
			require.NoError(t, err)
			assert.Empty(t, page.Candidates)
			if kind == "list" {
				cfg := persontiers.DefaultConfig()
				cfg.ExcludeListMessages = false
				page, err = f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, cfg, "UTC", 25)
				require.NoError(t, err)
				require.Len(t, page.Candidates, 1)
				assert.Equal(t, peer, page.Candidates[0].ParticipantID)
			}
		})
	}
}

func TestPersonTierCandidatesCanonicalGroupsDropBoundMembersAndPaginate(t *testing.T) {
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	first := f.EnsureParticipant("first@example.com", "First", "example.com")
	alias := f.EnsureParticipant("alias@example.com", "Alias", "example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	for _, id := range []int64{first, alias} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET canonical_id='synthetic-canonical' WHERE id=?`), id)
		require.NoError(t, err)
	}
	for _, peer := range []int64{first, second} {
		for i := range 3 {
			from, to := owner, peer
			if i < 2 {
				from, to = peer, owner
			}
			m := f.NewMessage().WithSourceMessageID(fmt.Sprintf("candidate-group-%d-%d", peer, i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
			id, err := f.Store.UpsertMessage(m)
			require.NoError(t, err)
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
		}
	}
	var all []store.PersonTierCandidate
	cursor := int64(0)
	for {
		page, err := f.Store.PersonTierCandidatesContext(t.Context(), cursor, 1, asOf, persontiers.DefaultConfig(), "UTC", 25)
		require.NoError(t, err)
		all = append(all, page.Candidates...)
		if page.NextCursor == "" {
			break
		}
		next, err := strconv.ParseInt(page.NextCursor, 10, 64)
		require.NoError(t, err)
		require.Greater(t, next, cursor)
		cursor = next
	}
	require.Len(t, all, 2)
	assert.Equal(t, first, all[0].ParticipantID)
	assert.Equal(t, second, all[1].ParticipantID)
	_, _, err := f.Store.CreatePersonFromParticipant(alias)
	require.NoError(t, err)
	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, persontiers.DefaultConfig(), "UTC", 25)
	require.NoError(t, err)
	require.Len(t, page.Candidates, 1)
	assert.Equal(t, second, page.Candidates[0].ParticipantID)
}

func TestPersonTierCandidatesUseCalendarShortWindowAndHardExclusions(t *testing.T) {
	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	human := f.EnsureParticipant("human@example.com", "Human", "example.com")
	automated := f.EnsureParticipant("noreply@example.com", "Automated", "example.com")
	room := f.EnsureParticipant("room@rooms.example.com", "Room", "rooms.example.com")
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
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
			require.NoError(t, err)
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
		}
	}
	page, err := f.Store.PersonTierCandidatesContext(t.Context(), 0, 50, asOf, cfg, zone.String(), 25)
	require.NoError(t, err)
	require.Len(t, page.Candidates, 1)
	assert.Equal(t, human, page.Candidates[0].ParticipantID)
	assert.Equal(t, 1, page.Candidates[0].OutboundShort)
	assert.Equal(t, 2, page.Candidates[0].InboundShort)
	assert.Equal(t, 3, page.Candidates[0].ActiveDaysShort)
}
