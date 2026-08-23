package activity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
)

func TestClassifyDirectionRolesAndEvidence(t *testing.T) {
	tests := []struct {
		name          string
		candidate     store.ActivityCandidate
		maxDirect     int
		wantRefKind   store.ActivityRefKind
		wantChannel   store.ActivityChannel
		wantDirection store.ActivityDirection
		wantOwner     string
		wantPersons   []store.ActivityEventPerson
	}{
		{
			name: "inbound email credits only the sender",
			candidate: store.ActivityCandidate{
				MessageID:        1,
				SourceID:         7,
				MessageType:      "email",
				ConversationType: "email_thread",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 10, PersonID: new(int64(100)), RecipientType: "from"},
					{ParticipantID: 11, RecipientType: "to", IsOwner: true, OwnerAddress: "me@example.com"},
					{ParticipantID: 12, PersonID: new(int64(101)), RecipientType: "cc"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelEmail,
			wantDirection: store.DirectionInbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 100, Role: store.RoleSender, Evidence: store.EvidenceDirect},
				{PersonID: 101, Role: store.RoleAddressed, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			name: "outbound email treats to cc and bcc alike below threshold",
			candidate: store.ActivityCandidate{
				MessageID:        2,
				SourceID:         7,
				MessageType:      "email",
				ConversationType: "email_thread",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 11, RecipientType: "from", IsOwner: true, OwnerAddress: "me@example.com"},
					{ParticipantID: 12, PersonID: new(int64(101)), RecipientType: "to"},
					{ParticipantID: 13, PersonID: new(int64(102)), RecipientType: "cc"},
					{ParticipantID: 14, PersonID: new(int64(103)), RecipientType: "bcc"},
				},
			},
			maxDirect:     3,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelEmail,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 101, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
				{PersonID: 102, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
				{PersonID: 103, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
			},
		},
		{
			name: "outbound broadcast above threshold is co-presence",
			candidate: store.ActivityCandidate{
				MessageID:        3,
				MessageType:      "chat",
				ConversationType: "group_chat",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 11, RecipientType: "from", IsOwner: true},
					{ParticipantID: 12, PersonID: new(int64(101)), RecipientType: "member"},
					{ParticipantID: 13, PersonID: new(int64(102)), RecipientType: "member"},
				},
			},
			maxDirect:     1,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionOutbound,
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 101, Role: store.RoleMember, Evidence: store.EvidenceCoPresence},
				{PersonID: 102, Role: store.RoleMember, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			name: "observed chat is co-presence",
			candidate: store.ActivityCandidate{
				MessageID:        4,
				MessageType:      "chat",
				ConversationType: "direct_chat",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 20, PersonID: new(int64(200)), RecipientType: "from"},
					{ParticipantID: 21, PersonID: new(int64(201)), RecipientType: "member"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionObserved,
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 200, Role: store.RoleSender, Evidence: store.EvidenceCoPresence},
				{PersonID: 201, Role: store.RoleMember, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			name: "owner-organized meeting credits attendees",
			candidate: store.ActivityCandidate{
				MessageID:   5,
				SourceID:    8,
				MessageType: "calendar_event",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 30, RecipientType: "from", IsOwner: true, OwnerAddress: "owner@example.com"},
					{ParticipantID: 31, PersonID: new(int64(300)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMeeting,
			wantChannel:   store.ChannelMeeting,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "owner@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 300, Role: store.RoleAttendee, Evidence: store.EvidenceDirect},
			},
		},
		{
			name: "counterpart-organized meeting credits only organizer",
			candidate: store.ActivityCandidate{
				MessageID:   6,
				SourceID:    8,
				MessageType: "calendar_event",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 30, PersonID: new(int64(300)), RecipientType: "from"},
					{ParticipantID: 31, RecipientType: "to", IsOwner: true, OwnerAddress: "owner@example.com"},
					{ParticipantID: 32, PersonID: new(int64(301)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMeeting,
			wantChannel:   store.ChannelMeeting,
			wantDirection: store.DirectionInbound,
			wantOwner:     "owner@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 300, Role: store.RoleOrganizer, Evidence: store.EvidenceDirect},
				{PersonID: 301, Role: store.RoleAttendee, Evidence: store.EvidenceCoPresence},
			},
		},
		{
			// Two distinct authors co-sign one message. The owner is the
			// second From counterpart, so participant-ID order must not
			// classify the message as inbound off the first author.
			name: "outbound when any co-author is the owner",
			candidate: store.ActivityCandidate{
				MessageID:        11,
				SourceID:         9,
				MessageType:      "email",
				ConversationType: "email_thread",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 40, PersonID: new(int64(400)), RecipientType: "from"},
					{ParticipantID: 41, RecipientType: "from", IsOwner: true, OwnerAddress: "me@example.com"},
					{ParticipantID: 42, PersonID: new(int64(401)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelEmail,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 400, Role: store.RoleSender, Evidence: store.EvidenceDirect},
				{PersonID: 401, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
			},
		},
		{
			// The owner authored nothing here: another person's co-authored
			// message stays inbound only because ownership appears among the
			// non-author counterparts.
			name: "inbound requires ownership outside the author set",
			candidate: store.ActivityCandidate{
				MessageID:        12,
				SourceID:         9,
				MessageType:      "email",
				ConversationType: "email_thread",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 50, PersonID: new(int64(500)), RecipientType: "from"},
					{ParticipantID: 51, PersonID: new(int64(501)), RecipientType: "from"},
					{ParticipantID: 52, RecipientType: "to", IsOwner: true, OwnerAddress: "me@example.com"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelEmail,
			wantDirection: store.DirectionInbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 500, Role: store.RoleSender, Evidence: store.EvidenceDirect},
				{PersonID: 501, Role: store.RoleSender, Evidence: store.EvidenceDirect},
			},
		},
		{
			// Imported meeting transcripts (Granola, Circleback, generic
			// meeting import) store meeting_transcript in a 'meeting'
			// conversation — they are meeting evidence with organizer and
			// attendee roles, not channel-other messages.
			name: "imported meeting transcript classifies as a meeting",
			candidate: store.ActivityCandidate{
				MessageID:        17,
				SourceID:         8,
				MessageType:      "meeting_transcript",
				ConversationType: "meeting",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 30, RecipientType: "from", IsOwner: true, OwnerAddress: "owner@example.com"},
					{ParticipantID: 31, PersonID: new(int64(300)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMeeting,
			wantChannel:   store.ChannelMeeting,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "owner@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 300, Role: store.RoleAttendee, Evidence: store.EvidenceDirect},
			},
		},
		{
			// A source-native outgoing message may have no resolved author
			// counterpart at all (sender participant unknown). The source's
			// own ownership flag is authoritative for direction.
			name: "source-native ownership is outbound without a resolved author",
			candidate: store.ActivityCandidate{
				MessageID:        13,
				SourceID:         9,
				MessageType:      "chat",
				ConversationType: "direct_chat",
				SourceIsFromMe:   true,
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 60, PersonID: new(int64(600)), RecipientType: "to"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 600, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
			},
		},
		{
			// A source-native chat sender is also a conversation member. Only
			// the 'from' row carries source-derived ownership, so the member
			// row must not count toward the broadcast threshold (maxDirect 1
			// with one real counterpart stays direct) or emit contact
			// evidence between the owner and their own person.
			name: "source-native sender's member row is not a counterpart",
			candidate: store.ActivityCandidate{
				MessageID:        14,
				SourceID:         9,
				MessageType:      "chat",
				ConversationType: "group_chat",
				SourceIsFromMe:   true,
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 70, PersonID: new(int64(700)), RecipientType: "from", IsOwner: true},
					{ParticipantID: 70, PersonID: new(int64(700)), RecipientType: "member"},
					{ParticipantID: 71, PersonID: new(int64(701)), RecipientType: "member"},
				},
			},
			maxDirect:     1,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 701, Role: store.RoleMember, Evidence: store.EvidenceDirect},
			},
		},
		{
			// The owner's person can be reachable through a second participant
			// (an unconfirmed alias bound to the same curated person). Person
			// links are owner-relative, so that row is not contact evidence
			// either.
			name: "owner person linked via second participant is not a counterpart",
			candidate: store.ActivityCandidate{
				MessageID:        15,
				SourceID:         9,
				MessageType:      "chat",
				ConversationType: "group_chat",
				Counterparts: []store.ActivityCounterpart{
					{ParticipantID: 80, PersonID: new(int64(800)), RecipientType: "from", IsOwner: true, OwnerAddress: "me@example.com"},
					{ParticipantID: 81, PersonID: new(int64(800)), RecipientType: "member"},
					{ParticipantID: 82, PersonID: new(int64(801)), RecipientType: "member"},
				},
			},
			maxDirect:     25,
			wantRefKind:   store.RefKindMessage,
			wantChannel:   store.ChannelChat,
			wantDirection: store.DirectionOutbound,
			wantOwner:     "me@example.com",
			wantPersons: []store.ActivityEventPerson{
				{PersonID: 801, Role: store.RoleMember, Evidence: store.EvidenceDirect},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			got := store.ClassifyActivityCandidate(test.candidate, test.maxDirect)
			assert.Equal(test.wantRefKind, got.RefKind)
			assert.Equal(test.wantChannel, got.Channel)
			assert.Equal(test.wantDirection, got.Direction)
			assert.Equal(test.wantOwner, got.OwnerAddress)
			assert.Equal(test.wantPersons, got.Persons)
			if test.wantDirection == store.DirectionObserved {
				assert.Nil(got.OwnerSourceID)
			} else {
				requireSourceID := test.candidate.SourceID
				if requireSourceID == 0 {
					assert.Nil(got.OwnerSourceID)
				} else if assert.NotNil(got.OwnerSourceID) {
					assert.Equal(requireSourceID, *got.OwnerSourceID)
				}
			}
		})
	}
}
func TestClassifyKeepsOneStrongestDeterministicLinkPerPerson(t *testing.T) {
	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageID:        7,
		MessageType:      "email",
		ConversationType: "email_thread",
		Counterparts: []store.ActivityCounterpart{
			{ParticipantID: 40, PersonID: new(int64(400)), RecipientType: "from"},
			{ParticipantID: 41, RecipientType: "to", IsOwner: true},
			{ParticipantID: 42, PersonID: new(int64(400)), RecipientType: "cc"},
			{ParticipantID: 43, PersonID: new(int64(401)), RecipientType: "member"},
			{ParticipantID: 44, PersonID: new(int64(401)), RecipientType: "to"},
			{ParticipantID: 45, PersonID: new(int64(999)), RecipientType: "to", IsOwner: true},
		},
	}, 25)

	assert.Equal(t, []store.ActivityEventPerson{
		{PersonID: 400, Role: store.RoleSender, Evidence: store.EvidenceDirect},
		{PersonID: 401, Role: store.RoleAddressed, Evidence: store.EvidenceCoPresence},
	}, got.Persons)
}

func TestClassifyDefaultsInvalidThresholdAndOtherChannel(t *testing.T) {
	counterparts := []store.ActivityCounterpart{
		{ParticipantID: 1, RecipientType: "from", IsOwner: true},
	}
	for id := int64(2); id <= DefaultMaxDirectCounterparts+1; id++ {
		counterparts = append(counterparts, store.ActivityCounterpart{
			ParticipantID: id,
			PersonID:      new(id),
			RecipientType: "to",
		})
	}

	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageID:        8,
		MessageType:      "unknown",
		ConversationType: "unknown",
		Counterparts:     counterparts,
	}, 0)

	assert.Equal(t, store.ChannelOther, got.Channel)
	assert.Len(t, got.Persons, DefaultMaxDirectCounterparts)
	for _, link := range got.Persons {
		assert.Equal(t, store.EvidenceDirect, link.Evidence)
	}
}

func TestClassifyDoesNotReverseOnDuplicateSenderAliases(t *testing.T) {
	sender := int64(500)
	recipient := int64(501)

	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageID:        9,
		MessageType:      "email",
		ConversationType: "email_thread",
		Counterparts: []store.ActivityCounterpart{
			// One merged participant can have multiple immutable envelope
			// aliases. The duplicate rows must not let iteration order turn
			// an outbound message into an inbound one.
			{ParticipantID: sender, PersonID: &sender, RecipientType: "from"},
			{ParticipantID: sender, PersonID: &sender, RecipientType: "from", IsOwner: true, OwnerAddress: "me@example.test"},
			{ParticipantID: recipient, PersonID: &recipient, RecipientType: "to"},
		},
	}, 25)

	assert.Equal(t, store.DirectionOutbound, got.Direction)
	assert.Equal(t, "me@example.test", got.OwnerAddress)
}

// TestClassifyCountsAliasParticipantsOncePerPerson pins person-relative
// audience counting: several alias participants bound to one curated person
// are one counterpart, so an alias-heavy direct message stays below the
// broadcast threshold and keeps direct evidence.
func TestClassifyCountsAliasParticipantsOncePerPerson(t *testing.T) {
	contact := int64(700)
	other := int64(701)

	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageID:        16,
		SourceID:         9,
		MessageType:      "email",
		ConversationType: "email_thread",
		Counterparts: []store.ActivityCounterpart{
			{ParticipantID: 90, RecipientType: "from", IsOwner: true, OwnerAddress: "me@example.com"},
			// Three alias participants of the same curated person.
			{ParticipantID: 91, PersonID: &contact, RecipientType: "to"},
			{ParticipantID: 92, PersonID: &contact, RecipientType: "to"},
			{ParticipantID: 93, PersonID: &contact, RecipientType: "cc"},
			{ParticipantID: 94, PersonID: &other, RecipientType: "to"},
		},
	}, 2)

	assert.Equal(t, store.DirectionOutbound, got.Direction)
	assert.Equal(t, []store.ActivityEventPerson{
		{PersonID: contact, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
		{PersonID: other, Role: store.RoleAddressed, Evidence: store.EvidenceDirect},
	}, got.Persons,
		"two persons at the threshold stay direct despite four participant rows")
}

func TestClassifyCountsDuplicateAliasesOnceForBroadcasts(t *testing.T) {
	sender := int64(510)
	first := int64(511)
	second := int64(512)

	got := store.ClassifyActivityCandidate(store.ActivityCandidate{
		MessageID:        10,
		MessageType:      "email",
		ConversationType: "email_thread",
		Counterparts: []store.ActivityCounterpart{
			{ParticipantID: sender, PersonID: &sender, RecipientType: "from", IsOwner: true},
			{ParticipantID: first, PersonID: &first, RecipientType: "to"},
			{ParticipantID: first, PersonID: &first, RecipientType: "to"},
			{ParticipantID: second, PersonID: &second, RecipientType: "to"},
		},
	}, 2)

	assert.Equal(t, store.DirectionOutbound, got.Direction)
	assert.Len(t, got.Persons, 2)
	for _, person := range got.Persons {
		assert.Equal(t, store.EvidenceDirect, person.Evidence,
			"two distinct recipients at the threshold are still direct")
	}
}
