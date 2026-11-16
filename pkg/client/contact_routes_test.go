package client

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedParticipantAddressPermitsEmptyMetadata(t *testing.T) {
	require := require.New(t)
	identifier := generated.ParticipantAddress{
		ParticipantID: 7,
		Type:          "email",
		Value:         "person@example.org",
		IdentityKey:   `["email","","","person@example.org"]`,
	}
	require.NoError(identifier.Validate())
}

func TestGeneratedPersonMessagingRoutesAcceptsEmptyCanonicalAliasReason(t *testing.T) {
	page := generated.PersonMessagingRoutesPage{
		AliasReason: "",
		CheckedAt:   time.Now(),
		ContactPoints: generated.ContactPointPage{
			Items: []generated.PersonContactPoint{},
		},
		Freshness: "archive_only",
		Observations: generated.ContactObservationPage{
			Items: []generated.ParticipantContactObservation{},
		},
		PersonUID:    "person-canonical",
		RequestedUID: "person-canonical",
		Routes: generated.MessagingRoutePage{
			Items: []generated.MessagingRoute{},
		},
		UnreviewedSuggestions: generated.IdentityRouteSuggestionPage{
			Items: []generated.IdentityRouteSuggestion{},
		},
		Warnings: []string{},
	}

	require.NoError(t, page.Validate())
}

func TestGeneratedMessagingRoutesAcceptEmptyDirectAndUnresolvedFields(t *testing.T) {
	direct := generated.MessagingRoute{
		AccountID:            "account-a",
		BoundParticipantIds:  []int64{7},
		ConversationID:       1,
		ConversationType:     "direct_chat",
		MemberChatIds:        []string{},
		MembershipComplete:   true,
		MergedIntoChatID:     "",
		MissingMemberChatIds: []string{},
		Network:              "whatsapp",
		NetworkLabel:         "",
		ProviderChatID:       "",
		Reasons:              []string{},
		SourceID:             1,
		SourceType:           "beeper",
		Status:               generated.ArchiveVerified,
	}
	require.NoError(t, direct.Validate())

	unresolved := direct
	unresolved.Network = ""
	unresolved.MergedIntoChatID = ""
	unresolved.Reasons = []string{"network_unverified"}
	unresolved.Status = generated.Unresolved
	require.NoError(t, unresolved.Validate())
}

func TestGeneratedContactCandidatesAcceptEmptySavedDisplayName(t *testing.T) {
	page := generated.ContactCandidatePage{
		Candidates: []generated.ContactCandidate{{
			PersonID:    7,
			PersonUID:   "person-7",
			DisplayName: "",
			Revision:    2,
			MatchKinds:  []string{"bound_observed_name"},
		}},
		IdentityRevision: 3,
	}

	require.NoError(t, page.Validate())
}

func TestGeneratedLoopListAcceptsUnnamedCounterpartyAndEmptySummary(t *testing.T) {
	asked := time.Date(2026, time.October, 20, 9, 0, 0, 0, time.UTC)
	page := generated.LoopListResponse{
		Items: []generated.LoopItem{{
			AnchorMessageID:    4521,
			QuestionMessageIds: []int64{4521},
			OwedSinceAt:        asked,
			LastActivityAt:     asked,
			Kind:               generated.ReplyOwed,
			Channel:            generated.Chat,
			Counterparty:       generated.LoopCounterparty{ParticipantID: 7},
		}},
	}

	require.NoError(t, page.Validate())
}
