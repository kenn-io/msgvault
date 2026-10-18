package loops

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func detectorFixture() (ConversationView, time.Time) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	owner := LoopParticipant{ID: 1, Email: "owner@example.test", IsOwner: true}
	other := LoopParticipant{ID: 2, PersonID: 7, Email: "alex@example.test", DisplayName: "Alex Example"}
	return ConversationView{
		ConversationID: 77, SourceID: 3, SourceType: "gmail", ConversationType: "email_thread",
		Participants: []LoopParticipant{owner, other}, LastMessageAt: now.Add(-48 * time.Hour),
		Messages: []LoopMessage{{ID: 10, OccurredAt: now.Add(-48 * time.Hour), Direction: Inbound,
			Sender: other, Recipients: []LoopRecipient{{LoopParticipant: owner, Type: "to"}}, Subject: "Review request"}},
		BodyText: func(int64) (string, error) { return "Can you check?", nil },
	}, now
}

func TestDetectInboundGraceAndNudge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	for _, age := range []time.Duration{3 * time.Hour, 48 * time.Hour} {
		view.Messages[0].OccurredAt = now.Add(-age)
		view.LastMessageAt = now.Add(-time.Hour)
		nudge := view.Messages[0]
		nudge.ID = 11
		nudge.OccurredAt = view.LastMessageAt
		view.Messages = append(view.Messages[:1], nudge)
		got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
		requirements.NoError(got.Err)
		requirements.NotNil(got.Open)
		assertions.Equal(int64(10), got.Open.Anchor.ID)
		assertions.Equal(int64(11), got.Open.LastActivity.ID)
		assertions.Equal(now.Add(-age), got.Open.OwedSinceAt)
		assertions.Equal(age >= 24*time.Hour, got.Open.Status == "open")
		assertions.Equal([]int64{10, 11}, got.Open.TurnMessageIDs)
	}
}

func TestDetectForwardSettlement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	for _, direction := range []Direction{Outbound, Inbound, Outbound, Inbound} {
		m := view.Messages[0]
		m.ID = int64(10 + len(view.Messages))
		m.Direction = direction
		m.OccurredAt = m.OccurredAt.Add(time.Duration(len(view.Messages)) * time.Hour)
		if direction == Outbound {
			m.Sender = view.Participants[0]
			m.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
		}
		view.Messages = append(view.Messages, m)
	}
	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	assertions.Equal(int64(11), got.Close[0].MessageID)
	assertions.Equal(view.Messages[1].OccurredAt, got.Close[0].At)
	requirements.NotNil(got.Open)
	assertions.Equal(int64(14), got.Open.Anchor.ID)
}

func TestDetectUsesBottomPostedOwnerQuestion(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new obligation", true: "existing obligation"}[existing], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
			anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			view.Messages = []LoopMessage{anchor}
			view.BodyText = func(int64) (string, error) {
				return "On Monday, Alex wrote:\n> Can you send the report?\n\nCan you confirm the date?", nil
			}
			var live []LiveLoop
			if existing {
				live = []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: anchor.ID}}
			}

			got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Empty(got.Close)
			assertions.Empty(got.CoverageGaps)
			requirements.NotNil(got.Open)
			assertions.Equal(AwaitingReply, got.Open.Kind)
			assertions.Equal(anchor.ID, got.Open.Anchor.ID)
		})
	}
}

func TestDetectIgnoresQuestionInForwardedMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
	anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	view.Messages = []LoopMessage{anchor}
	view.BodyText = func(int64) (string, error) {
		return "FYI\n---------- Forwarded message ----------\nFrom: Alex <alex@example.test>\nSubject: Which day works?\n\nCan you meet Friday?", nil
	}

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Nil(got.Open)
}

func TestDetectRetainsDirectRequestAfterListQuestionAcknowledgement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	listQuestion := view.Messages[0]
	listQuestion.ID, listQuestion.ListID = 10, "list.example.test"
	listQuestion.OccurredAt = now.Add(-72 * time.Hour)
	acknowledgement := listQuestion
	acknowledgement.ID, acknowledgement.Direction, acknowledgement.Sender = 11, Outbound, view.Participants[0]
	acknowledgement.ListID = ""
	acknowledgement.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	acknowledgement.OccurredAt = now.Add(-48 * time.Hour)
	request := view.Messages[0]
	request.ID, request.OccurredAt = 12, now.Add(-24*time.Hour)
	view.Messages = []LoopMessage{listQuestion, acknowledgement, request}
	view.LastMessageAt = request.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		switch id {
		case listQuestion.ID:
			return "Could you review this newsletter?", nil
		case acknowledgement.ID:
			return "Thanks!", nil
		default:
			return "Can you send the report?", nil
		}
	}

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(request.ID, got.Open.Anchor.ID)
	assertions.Equal(view.Participants[1].ID, got.Open.Counterparty.ID)
}

func TestDetectBulkQuestionSurvivesAcknowledgementLookback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	ownerParticipation := view.Messages[0]
	ownerParticipation.ID, ownerParticipation.Direction, ownerParticipation.Sender = 9, Outbound, view.Participants[0]
	ownerParticipation.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	ownerParticipation.OccurredAt = now.Add(-72 * time.Hour)
	question := view.Messages[0]
	question.ID, question.OccurredAt = 10, now.Add(-48*time.Hour)
	question.Labels = []string{"CATEGORY_PROMOTIONS"}
	view.Messages = []LoopMessage{ownerParticipation, question}
	view.LastMessageAt = question.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		switch id {
		case ownerParticipation.ID:
			return "I sent the report.", nil
		default:
			return "Could you review this newsletter?", nil
		}
	}

	eligible := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(eligible.Err)
	requirements.NotNil(eligible.Open)
	assertions.Equal(ReplyOwed, eligible.Open.Kind)
	assertions.Equal(question.ID, eligible.Open.Anchor.ID)

	acknowledgement := ownerParticipation
	acknowledgement.ID, acknowledgement.OccurredAt = 11, now.Add(-24*time.Hour)
	view.Messages = append(view.Messages, acknowledgement)
	view.LastMessageAt = acknowledgement.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		switch id {
		case ownerParticipation.ID:
			return "I sent the report.", nil
		case question.ID:
			return "Could you review this newsletter?", nil
		default:
			return "Thanks!", nil
		}
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: question.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	assertions.Empty(got.CoverageGaps)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(question.ID, got.Open.Anchor.ID)
}

func TestDetectGoogleVoiceDirectTextConversation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	view.SourceType, view.ConversationType = "google_voice", "direct_chat"
	view.Messages[0].OccurredAt = now.Add(-48 * time.Hour)
	view.LastMessageAt = view.Messages[0].OccurredAt

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(view.Messages[0].ID, got.Open.Anchor.ID)
}

func TestDetectEligibility(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		fresh        bool
		change       func(*ConversationView, *Suppressions)
	}{
		{name: "list mail", reason: "withdrawn", change: func(v *ConversationView, _ *Suppressions) { v.Messages[0].ListID = "list.example.test" }},
		{name: "bulk label", fresh: true, change: func(v *ConversationView, _ *Suppressions) { v.Messages[0].Labels = []string{"CATEGORY_PROMOTIONS"} }},
		{name: "bulk header", fresh: true, change: func(v *ConversationView, _ *Suppressions) {
			v.Messages[0].Signals = &mime.AutomationSignals{PrecedenceBulk: true}
		}},
		{name: "cc only", reason: "withdrawn", change: func(v *ConversationView, _ *Suppressions) { v.Messages[0].Recipients[0].Type = "cc" }},
		{name: "conversation mute", reason: "muted", change: func(v *ConversationView, s *Suppressions) { s.Conversations = []int64{v.ConversationID} }},
		{name: "sender mute", reason: "muted", change: func(v *ConversationView, s *Suppressions) { s.Participants = []int64{v.Messages[0].Sender.ID} }},
		{name: "person mute", reason: "muted", change: func(v *ConversationView, s *Suppressions) { s.Persons = []int64{v.Messages[0].Sender.PersonID} }},
		{name: "meeting", reason: "withdrawn", change: func(v *ConversationView, _ *Suppressions) { v.ConversationType = "meeting" }},
		{name: "nonmessage source", reason: "withdrawn", change: func(v *ConversationView, _ *Suppressions) { v.SourceType = "document" }},
		{name: "outside lookback", reason: "withdrawn", change: func(v *ConversationView, _ *Suppressions) {
			v.LastMessageAt = v.LastMessageAt.Add(-366 * 24 * time.Hour)
		}},
		{name: "self only", reason: "withdrawn", change: func(v *ConversationView, _ *Suppressions) { v.Participants = v.Participants[:1] }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			var supp Suppressions
			tt.change(&view, &supp)
			live := []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}
			if tt.fresh {
				live = nil
			}
			got := Detect(view, live, now, DefaultThresholds(), supp)
			requirements.NoError(got.Err)
			if tt.fresh {
				assertions.Empty(got.Close)
				assertions.Nil(got.Open)
				return
			}
			assertions.Nil(got.Open)
			requirements.Len(got.Close, 1)
			assertions.Equal(tt.reason, got.Close[0].Reason)
		})
	}
}

func TestDetectOutboundQuestion(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{"Can you review?", true}, {"Can you review https://example.test/proposal?", true},
		{"FYI, attached", false}, {"See https://example.test/?q=one", false},
	} {
		t.Run(tt.body, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			view.Messages[0].Direction = Outbound
			view.Messages[0].Sender = view.Participants[0]
			view.Messages[0].Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			view.Messages[0].OccurredAt = now.Add(-96 * time.Hour)
			view.BodyText = func(int64) (string, error) { return tt.body, nil }
			got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			if !tt.want {
				assertions.Nil(got.Open)
				return
			}
			requirements.NotNil(got.Open)
			assertions.Equal(AwaitingReply, got.Open.Kind)
			assertions.Equal("open", got.Open.Status)
			assertions.Equal(int64(2), got.Open.Counterparty.ID)
		})
	}
}

func TestDetectSkipsOutboundQuestionWithoutEligibleCounterparty(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	owner, other := view.Participants[0], view.Participants[1]
	selfQuestion := view.Messages[0]
	selfQuestion.ID, selfQuestion.Direction, selfQuestion.Sender = 10, Outbound, owner
	selfQuestion.Recipients = []LoopRecipient{{LoopParticipant: owner, Type: "to"}}
	selfQuestion.OccurredAt = now.Add(-96 * time.Hour)
	eligibleQuestion := selfQuestion
	eligibleQuestion.ID = 11
	eligibleQuestion.Recipients = []LoopRecipient{{LoopParticipant: other, Type: "to"}}
	eligibleQuestion.OccurredAt = now.Add(-48 * time.Hour)
	view.Messages = []LoopMessage{selfQuestion, eligibleQuestion}
	view.LastMessageAt = eligibleQuestion.OccurredAt
	view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.Equal(AwaitingReply, got.Open.Kind)
	assertions.Equal(eligibleQuestion.ID, got.Open.Anchor.ID)
	assertions.Equal(other.ID, got.Open.Counterparty.ID)
}

func TestDetectSkipsInboundMessageWithoutEligibleCounterparty(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	invalid := view.Messages[0]
	invalid.ID = 10
	invalid.Sender = LoopParticipant{Email: "unknown@example.test"}
	invalid.OccurredAt = now.Add(-48 * time.Hour)
	valid := view.Messages[0]
	valid.ID = 11
	valid.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = []LoopMessage{invalid, valid}
	view.LastMessageAt = valid.OccurredAt

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: valid.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(valid.ID, got.Open.Anchor.ID)
}

func TestDetectSkipsMutedCounterpartyWhenSelectingInboundAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	blair := LoopParticipant{ID: 3, PersonID: 8, Email: "blair@example.test", DisplayName: "Blair Example"}
	view.Participants = append(view.Participants, blair)
	older := view.Messages[0]
	older.ID, older.Sender = 10, blair
	older.OccurredAt = now.Add(-48 * time.Hour)
	newer := view.Messages[0]
	newer.ID = 11
	newer.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = []LoopMessage{older, newer}
	view.LastMessageAt = newer.OccurredAt

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: newer.ID}}, now, DefaultThresholds(), Suppressions{Participants: []int64{blair.ID}})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(newer.ID, got.Open.Anchor.ID)
	assertions.Equal(view.Participants[1].ID, got.Open.Counterparty.ID)
}

func TestDetectSkipsMutedCounterpartyWhenSelectingOutboundAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	blair := LoopParticipant{ID: 3, PersonID: 8, Email: "blair@example.test", DisplayName: "Blair Example"}
	view.Participants = append(view.Participants, blair)
	owner := view.Participants[0]
	older := view.Messages[0]
	older.ID, older.Direction, older.Sender = 10, Outbound, owner
	older.Recipients = []LoopRecipient{{LoopParticipant: blair, Type: "to"}}
	older.OccurredAt = now.Add(-120 * time.Hour)
	newer := older
	newer.ID = 11
	newer.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	newer.OccurredAt = now.Add(-96 * time.Hour)
	view.Messages = []LoopMessage{older, newer}
	view.LastMessageAt = newer.OccurredAt
	view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: newer.ID}}, now, DefaultThresholds(), Suppressions{Participants: []int64{blair.ID}})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(newer.ID, got.Open.Anchor.ID)
	assertions.Equal(view.Participants[1].ID, got.Open.Counterparty.ID)
}

func TestDetectOwnerMessagesWithAutomationLikeAddresses(t *testing.T) {
	for _, senderIsOwner := range []bool{true, false} {
		for _, address := range []string{"hello@example.test", "support@example.test"} {
			t.Run(fmt.Sprintf("%s/owner=%t/settles reply", address, senderIsOwner), func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				view, now := detectorFixture()
				owner := view.Participants[0]
				owner.Email = address
				view.Participants[0] = owner
				sender := owner
				sender.IsOwner = senderIsOwner
				answer := view.Messages[0]
				answer.ID, answer.Direction, answer.Sender = 11, Outbound, sender
				answer.OccurredAt = now.Add(-time.Hour)
				view.Messages = append(view.Messages, answer)
				view.LastMessageAt = answer.OccurredAt
				view.BodyText = func(id int64) (string, error) {
					if id == answer.ID {
						return "I sent the report.", nil
					}
					return "Can you review the report?", nil
				}

				got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
				requirements.NoError(got.Err)
				requirements.Len(got.Close, 1)
				assertions.Equal("turn_flip", got.Close[0].Reason)
				assertions.Equal(answer.ID, got.Close[0].MessageID)
			})

			t.Run(fmt.Sprintf("%s/owner=%t/outbound question", address, senderIsOwner), func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				view, now := detectorFixture()
				owner := view.Participants[0]
				owner.Email = address
				view.Participants[0] = owner
				sender := owner
				sender.IsOwner = senderIsOwner
				question := view.Messages[0]
				question.Direction, question.Sender = Outbound, sender
				question.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				question.OccurredAt = now.Add(-96 * time.Hour)
				view.Messages = []LoopMessage{question}
				view.LastMessageAt = question.OccurredAt
				view.BodyText = func(int64) (string, error) { return "Can you review the report?", nil }

				got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
				requirements.NoError(got.Err)
				requirements.NotNil(got.Open)
				assertions.Equal(AwaitingReply, got.Open.Kind)
				assertions.Equal(question.ID, got.Open.Anchor.ID)
			})
		}
	}
}

func TestDetectOutboundDraftDoesNotCreateAwaitingReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	draft := view.Messages[0]
	draft.Direction, draft.Sender, draft.Draft = Outbound, view.Participants[0], true
	draft.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	draft.OccurredAt = now.Add(-96 * time.Hour)
	view.Messages = []LoopMessage{draft}
	view.LastMessageAt = draft.OccurredAt
	view.BodyText = func(int64) (string, error) { return "Can you review the report?", nil }

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Nil(got.Open)
	assertions.Empty(got.Close)
}

func TestSettlementDoesNotCloseReplyOwedForOutboundDraft(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	draft := view.Messages[0]
	draft.ID, draft.Direction, draft.Sender, draft.Draft = 11, Outbound, view.Participants[0], true
	draft.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	draft.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = append(view.Messages, draft)
	view.LastMessageAt = draft.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == draft.ID {
			return "I sent the report.", nil
		}
		return "Can you review the report?", nil
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(int64(10), got.Open.Anchor.ID)
}

func TestDetectAcknowledgementKeepsQuestion(t *testing.T) {
	view, now := detectorFixture()
	ack := view.Messages[0]
	ack.ID, ack.Direction, ack.Sender = 11, Outbound, view.Participants[0]
	ack.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = append(view.Messages, ack)
	for _, body := range []string{
		"Thanks, Alex!",
		"Thanks, Alex Example!",
		"Thank you very much!",
		"Thanks a lot!",
		"Thanks so much!",
		"Many many thanks!",
	} {
		t.Run(body, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view.BodyText = func(id int64) (string, error) {
				if id == 10 {
					return "Can you check?", nil
				}
				return body, nil
			}
			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Empty(got.Close)
			requirements.NotNil(got.Open)
			assertions.Equal(ReplyOwed, got.Open.Kind)
		})
	}
	view.BodyText = func(id int64) (string, error) {
		if id == 10 {
			return "Can you check?", nil
		}
		return "Will do", nil
	}
	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	require.Len(t, got.Close, 1)
	assert.Equal(t, "turn_flip", got.Close[0].Reason)
	assert.Nil(t, got.Open)
}

func TestDetectAcknowledgementWithUserAttachmentSettlesReplyOwed(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	anchor.OccurredAt = now.Add(-48 * time.Hour)
	answer := anchor
	answer.ID, answer.Direction, answer.Sender = 11, Outbound, view.Participants[0]
	answer.OccurredAt = now.Add(-24 * time.Hour)
	answer.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	answer.HasUserAttachments = true
	view.Messages = []LoopMessage{anchor, answer}
	view.LastMessageAt = answer.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == answer.ID {
			return "Thanks!", nil
		}
		return "Could you send the report?", nil
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	assertions.Equal(answer.ID, got.Close[0].MessageID)
	assertions.Nil(got.Open)
}

func TestDetectAttachmentAnswerIsBoundaryForLaterAcknowledgement(t *testing.T) {
	for _, hasLiveLoop := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "existing FYI loop"}[hasLiveLoop], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			question := view.Messages[0]
			question.OccurredAt = now.Add(-96 * time.Hour)
			attachmentAnswer := question
			attachmentAnswer.ID, attachmentAnswer.Direction, attachmentAnswer.Sender = 11, Outbound, view.Participants[0]
			attachmentAnswer.OccurredAt = now.Add(-72 * time.Hour)
			attachmentAnswer.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			attachmentAnswer.HasUserAttachments = true
			fyi := question
			fyi.ID, fyi.OccurredAt = 12, now.Add(-48*time.Hour)
			finalAcknowledgement := attachmentAnswer
			finalAcknowledgement.ID, finalAcknowledgement.OccurredAt = 13, now.Add(-24*time.Hour)
			finalAcknowledgement.HasUserAttachments = false
			view.Messages = []LoopMessage{question, attachmentAnswer, fyi, finalAcknowledgement}
			view.LastMessageAt = finalAcknowledgement.OccurredAt
			view.BodyText = func(id int64) (string, error) {
				switch id {
				case question.ID:
					return "Could you send the report?", nil
				case attachmentAnswer.ID, finalAcknowledgement.ID:
					return "Thanks!", nil
				default:
					return "Here is the update.", nil
				}
			}

			var live []LiveLoop
			if hasLiveLoop {
				live = []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: fyi.ID}}
			}
			got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Empty(got.CoverageGaps)
			assertions.Nil(got.Open)
			if hasLiveLoop {
				requirements.Len(got.Close, 1)
				assertions.Equal("turn_flip", got.Close[0].Reason)
				assertions.Equal(finalAcknowledgement.ID, got.Close[0].MessageID)
			} else {
				assertions.Empty(got.Close)
			}
		})
	}
}

func TestDetectCCOnlyQuestionDoesNotNeutralizeAcknowledgement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	fyi := view.Messages[0]
	fyi.OccurredAt = now.Add(-72 * time.Hour)
	fyi.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "to"}}
	ccQuestion := fyi
	ccQuestion.ID, ccQuestion.OccurredAt = 11, now.Add(-48*time.Hour)
	ccQuestion.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "cc"}}
	acknowledgement := fyi
	acknowledgement.ID, acknowledgement.Direction, acknowledgement.Sender = 12, Outbound, view.Participants[0]
	acknowledgement.OccurredAt = now.Add(-24 * time.Hour)
	acknowledgement.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	view.Messages = []LoopMessage{fyi, ccQuestion, acknowledgement}
	view.LastMessageAt = acknowledgement.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == ccQuestion.ID {
			return "Could you review this flyer?", nil
		}
		if id == acknowledgement.ID {
			return "Thanks!", nil
		}
		return "Here is the update.", nil
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: fyi.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	assertions.Equal(acknowledgement.ID, got.Close[0].MessageID)
	assertions.Nil(got.Open)
}

func TestDetectQuestionEndingWithNameSettlesInboundAndOpensAwaitingReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	answer := view.Messages[0]
	answer.ID, answer.Direction, answer.Sender = 11, Outbound, view.Participants[0]
	answer.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	answer.OccurredAt = now.Add(-time.Hour)
	view.Messages = append(view.Messages, answer)
	view.LastMessageAt = answer.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == answer.ID {
			return "Okay, Alex?", nil
		}
		return "Can you check?", nil
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	assertions.Equal(answer.ID, got.Close[0].MessageID)
	requirements.NotNil(got.Open)
	assertions.Equal(AwaitingReply, got.Open.Kind)
	assertions.Equal(answer.ID, got.Open.Anchor.ID)
}

func TestDetectNameLikeAnswerSettlesQuestion(t *testing.T) {
	for _, body := range []string{"Thanks, sent!", "Thanks, no."} {
		t.Run(body, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			answer := view.Messages[0]
			answer.ID, answer.Direction, answer.Sender = 11, Outbound, view.Participants[0]
			answer.OccurredAt = now.Add(-24 * time.Hour)
			view.Messages = append(view.Messages, answer)
			view.LastMessageAt = answer.OccurredAt
			view.BodyText = func(id int64) (string, error) {
				if id == 10 {
					return "Can you check?", nil
				}
				return body, nil
			}

			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal("turn_flip", got.Close[0].Reason)
			assertions.Equal(int64(11), got.Close[0].MessageID)
		})
	}
}

func TestDetectAutomationIsNeutral(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, signal := range []mime.AutomationSignals{{AutoSubmitted: true}, {CalendarPart: true}, {ReplyToNoReply: true}} {
		view, now := detectorFixture()
		owner := view.Participants[0]
		owner.Email = "hello@example.test"
		view.Participants[0] = owner
		m := view.Messages[0]
		m.ID, m.Direction, m.Sender, m.Signals = 11, Outbound, owner, &signal
		view.Messages = append(view.Messages, m)
		got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
		requirements.NoError(got.Err)
		assertions.Empty(got.Close)
		requirements.NotNil(got.Open)
		assertions.Equal(int64(10), got.Open.Anchor.ID)
	}
}

func TestDetectBodyBudgetAndReadFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	base := view.Messages[0]
	view.Messages = nil
	for i := range 20 {
		m := base
		m.ID, m.Direction, m.Sender = int64(i+10), Outbound, view.Participants[0]
		m.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
		m.OccurredAt = now.Add(time.Duration(i-100) * time.Hour)
		view.Messages = append(view.Messages, m)
	}
	var reads int
	view.BodyText = func(int64) (string, error) { reads++; return "Can you check?", nil }
	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Equal(12, reads)
	requirements.NotNil(got.Open)
	assertions.Equal(int64(18), got.Open.Anchor.ID)
	assertions.True(got.Open.TruncatedRun)
	assertions.Len(got.Open.TurnMessageIDs, 20)
	view.BodyText = func(int64) (string, error) { return "", errors.New("body unavailable") }
	got = Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Nil(got.Open)
}

func TestDetectReportsProvenPreviousTurn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.False(got.Open.PreviousTurnKnown, "a bounded same-side window does not prove a new turn after terminal history")
	previous := view.Messages[0]
	previous.ID = 9
	previous.Direction = Outbound
	previous.Sender = view.Participants[0]
	previous.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	previous.OccurredAt = previous.OccurredAt.Add(-time.Hour)
	view.Messages = append([]LoopMessage{previous}, view.Messages...)
	got = Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.True(got.Open.PreviousTurnKnown, "an observed substantive opposite turn permits a new obligation")
}

func TestDetectUncertainAwaitingAnchorKeepsLive(t *testing.T) {
	for _, tc := range []struct {
		name           string
		messages       int
		anchorReadErr  bool
		newestQuestion bool
	}{
		{name: "old question exceeds body budget", messages: 20},
		{name: "newer question cannot replace unread old question", messages: 20, newestQuestion: true},
		{name: "anchor body unavailable", messages: 1, anchorReadErr: true},
		{name: "newer question cannot replace failed anchor body", messages: 2, anchorReadErr: true, newestQuestion: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			view, now := detectorFixture()
			base := view.Messages[0]
			view.Messages = nil
			for i := range tc.messages {
				message := base
				message.ID = int64(10 + i)
				message.Direction, message.Sender = Outbound, view.Participants[0]
				message.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				message.OccurredAt = now.Add(time.Duration(i-100) * time.Hour)
				view.Messages = append(view.Messages, message)
			}
			var reads int
			view.BodyText = func(id int64) (string, error) {
				reads++
				if id == 10 {
					if tc.anchorReadErr {
						return "", errors.New("body unavailable")
					}
					return "Can you review?", nil
				}
				if tc.newestQuestion && id == int64(9+tc.messages) {
					return "Any update?", nil
				}
				return "Following up on my earlier request.", nil
			}
			got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
			require.NoError(t, got.Err)
			assertions.Empty(got.Close, "an unread question is not evidence of withdrawal")
			assertions.Nil(got.Open, "uncertainty must not replace live evidence with a newer nudge")
			assertions.Equal([]int64{1}, got.CoverageGaps)
			assertions.LessOrEqual(reads, DefaultThresholds().RunMessageLimit)
		})
	}
}

func TestDetectUncertainAwaitingAnchorClosesWhenCounterpartyMuted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	view.Messages[0].Direction, view.Messages[0].Sender = Outbound, view.Participants[0]
	view.Messages[0].Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	view.BodyText = func(int64) (string, error) { return "", errors.New("body unavailable") }
	supp := Suppressions{Participants: []int64{view.Participants[1].ID}}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: 10}}, now, DefaultThresholds(), supp)
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("muted", got.Close[0].Reason)
	assertions.Nil(got.Open)
	assertions.Empty(got.CoverageGaps)
}

func TestDetectDoesNotReopenAnchorProvenAbsent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		direction Direction
	}{
		{name: "inbound", kind: ReplyOwed, direction: Inbound},
		{name: "outbound", kind: AwaitingReply, direction: Outbound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			anchor.Direction = tc.direction
			anchor.OccurredAt = now.Add(-96 * time.Hour)
			if tc.direction == Outbound {
				anchor.Sender = view.Participants[0]
				anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			}
			view.Messages = []LoopMessage{anchor}
			view.LastMessageAt = anchor.OccurredAt
			view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }
			view.AnchorExists = func(id int64) (bool, error) {
				assertions.Equal(anchor.ID, id)
				return false, nil
			}

			got := Detect(view, []LiveLoop{{ID: 1, Kind: tc.kind, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal("withdrawn", got.Close[0].Reason)
			assertions.Empty(got.CoverageGaps)
			assertions.Nil(got.Open)
		})
	}
}

func TestDetectSettlesAllAnchorsWithoutOrderDependence(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply owed first", true: "awaiting reply first"}[reverse], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			replyAnchor := view.Messages[0]
			replyAnchor.OccurredAt = now.Add(-48 * time.Hour)
			staleQuestion := replyAnchor
			staleQuestion.ID, staleQuestion.Direction, staleQuestion.Sender = 11, Outbound, view.Participants[0]
			staleQuestion.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			staleQuestion.OccurredAt = now.Add(-time.Hour)
			view.Messages = []LoopMessage{replyAnchor, staleQuestion}
			view.LastMessageAt = staleQuestion.OccurredAt
			view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }
			view.AnchorExists = func(id int64) (bool, error) { return id != staleQuestion.ID, nil }

			replyOwed := LiveLoop{ID: 1, Kind: ReplyOwed, AnchorMessageID: replyAnchor.ID}
			awaitingReply := LiveLoop{ID: 2, Kind: AwaitingReply, AnchorMessageID: staleQuestion.ID}
			live := []LiveLoop{replyOwed, awaitingReply}
			if reverse {
				live[0], live[1] = live[1], live[0]
			}
			got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal(awaitingReply.ID, got.Close[0].ID)
			assertions.Equal("withdrawn", got.Close[0].Reason)
			requirements.NotNil(got.Open)
			assertions.Equal(ReplyOwed, got.Open.Kind)
			assertions.Equal(replyAnchor.ID, got.Open.Anchor.ID)
		})
	}
}

func TestDetectExtendsEvidenceBeforeSettlingLiveLoops(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply owed first", true: "awaiting reply first"}[reverse], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			oldQuestion := view.Messages[0]
			oldQuestion.Direction, oldQuestion.Sender = Outbound, view.Participants[0]
			oldQuestion.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			oldQuestion.OccurredAt = now.Add(-120 * time.Hour)
			newQuestion := oldQuestion
			newQuestion.ID, newQuestion.Direction, newQuestion.Sender = 11, Inbound, view.Participants[1]
			newQuestion.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "to"}}
			newQuestion.OccurredAt = now.Add(-72 * time.Hour)
			replyAnchor := newQuestion
			replyAnchor.ID, replyAnchor.OccurredAt = 12, now.Add(-48*time.Hour)
			acknowledgement := oldQuestion
			acknowledgement.ID, acknowledgement.OccurredAt = 13, now.Add(-time.Hour)
			view.Messages = []LoopMessage{replyAnchor, acknowledgement}
			view.LastMessageAt = acknowledgement.OccurredAt
			view.BodyText = func(id int64) (string, error) {
				switch id {
				case oldQuestion.ID:
					return "Can you send the report?", nil
				case newQuestion.ID:
					return "Could you also confirm the date?", nil
				case replyAnchor.ID:
					return "Here is the report.", nil
				default:
					return "Thanks!", nil
				}
			}
			view.Extend = func() (ConversationView, error) {
				extended := view
				extended.Messages = []LoopMessage{oldQuestion, newQuestion, replyAnchor, acknowledgement}
				return extended, nil
			}

			replyOwed := LiveLoop{ID: 1, Kind: ReplyOwed, AnchorMessageID: replyAnchor.ID}
			awaitingReply := LiveLoop{ID: 2, Kind: AwaitingReply, AnchorMessageID: oldQuestion.ID}
			live := []LiveLoop{replyOwed, awaitingReply}
			if reverse {
				live[0], live[1] = live[1], live[0]
			}
			got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal(awaitingReply.ID, got.Close[0].ID)
			assertions.Equal("turn_flip", got.Close[0].Reason)
			assertions.Equal(newQuestion.ID, got.Close[0].MessageID)
		})
	}
}

func TestDetectSettlesLiveLoopsChronologicallyUnderBodyBudget(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply owed first", true: "awaiting reply first"}[reverse], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			owner, counterparty := view.Participants[0], view.Participants[1]
			question := view.Messages[0]
			question.ID, question.Direction, question.Sender = 10, Outbound, owner
			question.Recipients = []LoopRecipient{{LoopParticipant: counterparty, Type: "to"}}
			question.OccurredAt = now.Add(-96 * time.Hour)
			request := view.Messages[0]
			request.ID, request.OccurredAt = 11, now.Add(-72*time.Hour)
			request.Recipients = []LoopRecipient{{LoopParticipant: owner, Type: "to"}}
			acknowledgement := question
			acknowledgement.ID, acknowledgement.OccurredAt = 12, now.Add(-48*time.Hour)
			neutral := question
			neutral.ID, neutral.Direction, neutral.OccurredAt = 13, Observed, now.Add(-time.Hour)
			view.Messages = []LoopMessage{neutral}
			view.LastMessageAt = neutral.OccurredAt
			view.BodyText = func(id int64) (string, error) {
				switch id {
				case question.ID:
					return "Can you send the report?", nil
				case request.ID:
					return "Here is the report.", nil
				case acknowledgement.ID:
					return "Thanks!", nil
				default:
					return "Recent update.", nil
				}
			}
			view.Extend = func() (ConversationView, error) {
				extended := view
				extended.Messages = []LoopMessage{question, request, acknowledgement, neutral}
				return extended, nil
			}

			replyOwed := LiveLoop{ID: 1, Kind: ReplyOwed, AnchorMessageID: request.ID}
			awaitingReply := LiveLoop{ID: 2, Kind: AwaitingReply, AnchorMessageID: question.ID}
			live := []LiveLoop{replyOwed, awaitingReply}
			if reverse {
				live[0], live[1] = live[1], live[0]
			}
			cfg := DefaultThresholds()
			cfg.RunMessageLimit = 2

			got := Detect(view, live, now, cfg, Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal(awaitingReply.ID, got.Close[0].ID)
			assertions.Equal("turn_flip", got.Close[0].Reason)
			assertions.Equal(request.ID, got.Close[0].MessageID)
			assertions.Equal([]int64{replyOwed.ID}, got.CoverageGaps)
			assertions.Nil(got.Open)
		})
	}
}

func TestDetectUndatedSubstantiveMessagesAreIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, ConversationView, time.Time)
	}{
		{
			name: "inbound does not open",
			run: func(t *testing.T, view ConversationView, now time.Time) {
				t.Helper()
				view.Messages[0].OccurredAt = time.Time{}
				view.LastMessageAt = now.Add(-time.Hour)
				got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assert.Nil(t, got.Open)
				assert.Empty(t, got.Close)
			},
		},
		{
			name: "outbound question does not open",
			run: func(t *testing.T, view ConversationView, now time.Time) {
				t.Helper()
				question := view.Messages[0]
				question.Direction, question.Sender = Outbound, view.Participants[0]
				question.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				question.OccurredAt = time.Time{}
				view.Messages = []LoopMessage{question}
				view.LastMessageAt = now.Add(-time.Hour)
				got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assert.Nil(t, got.Open)
				assert.Empty(t, got.Close)
			},
		},
		{
			name: "undated reply preserves reply owed",
			run: func(t *testing.T, view ConversationView, now time.Time) {
				t.Helper()
				anchor := view.Messages[0]
				answer := anchor
				answer.ID, answer.Direction, answer.Sender = 11, Outbound, view.Participants[0]
				answer.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				answer.OccurredAt = time.Time{}
				view.Messages = []LoopMessage{anchor, answer}
				view.LastMessageAt = now.Add(-time.Hour)
				view.BodyText = func(int64) (string, error) { return "I will check.", nil }
				got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assert.Empty(t, got.Close)
				assert.Equal(t, []int64{1}, got.CoverageGaps)
				assert.Nil(t, got.Open)
			},
		},
		{
			name: "undated reply preserves awaiting reply",
			run: func(t *testing.T, view ConversationView, now time.Time) {
				t.Helper()
				anchor := view.Messages[0]
				anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
				anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				anchor.OccurredAt = now.Add(-96 * time.Hour)
				reply := view.Messages[0]
				reply.ID, reply.Direction = 11, Inbound
				reply.OccurredAt = time.Time{}
				view.Messages = []LoopMessage{anchor, reply}
				view.LastMessageAt = now.Add(-time.Hour)
				view.BodyText = func(id int64) (string, error) {
					if id == anchor.ID {
						return "Can you review this?", nil
					}
					return "I will check.", nil
				}
				got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assert.Empty(t, got.Close)
				assert.Equal(t, []int64{1}, got.CoverageGaps)
				assert.Nil(t, got.Open)
			},
		},
		{
			name: "undated live anchor remains uncertain",
			run: func(t *testing.T, view ConversationView, now time.Time) {
				t.Helper()
				view.Messages[0].OccurredAt = time.Time{}
				view.LastMessageAt = time.Time{}
				view.AnchorExists = func(int64) (bool, error) { return true, nil }
				got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: view.Messages[0].ID}}, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assert.Empty(t, got.Close)
				assert.Equal(t, []int64{1}, got.CoverageGaps)
				assert.Nil(t, got.Open)
			},
		},
		{
			name: "undated evidence prevents stale withdrawal",
			run: func(t *testing.T, view ConversationView, now time.Time) {
				t.Helper()
				view.Messages[0].OccurredAt = time.Time{}
				view.LastMessageAt = now.Add(-366 * 24 * time.Hour)
				view.AnchorExists = func(int64) (bool, error) { return true, nil }
				got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: view.Messages[0].ID}}, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assert.Empty(t, got.Close)
				assert.Equal(t, []int64{1}, got.CoverageGaps)
				assert.Nil(t, got.Open)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, now := detectorFixture()
			tc.run(t, view, now)
		})
	}
}

func TestDetectKeepsUnsettledOlderLoopWithoutRecentAnchor(t *testing.T) {
	for _, kind := range []string{ReplyOwed, AwaitingReply} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			recent := anchor
			anchor.ID, recent.ID = 10, 11
			recent.OccurredAt = now.Add(-24 * time.Hour)
			bodies := map[int64]string{}
			if kind == AwaitingReply {
				anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
				anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				recent = anchor
				recent.ID, recent.OccurredAt = 11, now.Add(-24*time.Hour)
				bodies[10] = "Can you review this?"
				bodies[11] = "Following up on this."
			} else {
				recent.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "cc"}}
			}
			view.Messages = []LoopMessage{recent}
			view.LastMessageAt = recent.OccurredAt
			view.BodyText = func(id int64) (string, error) { return bodies[id], nil }
			extended := view
			extended.Messages = []LoopMessage{anchor, recent}
			view.Extend = func() (ConversationView, error) { return extended, nil }

			got := Detect(view, []LiveLoop{{ID: 1, Kind: kind, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Empty(got.Close)
			assertions.Empty(got.CoverageGaps)
			assertions.Nil(got.Open)
		})
	}
}

func TestDetectDoesNotCloseLiveLoopWithCoverageGap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	recent := view.Messages[0]
	recent.ID = 11
	recent.OccurredAt = now.Add(-24 * time.Hour)
	recent.Signals = &mime.AutomationSignals{PrecedenceBulk: true}
	view.Messages = []LoopMessage{recent}
	view.LastMessageAt = recent.OccurredAt
	view.AnchorExists = func(int64) (bool, error) { return true, nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	assertions.Equal([]int64{1}, got.CoverageGaps)
	assertions.Nil(got.Open)
}

func TestDetectExtendedWindowPreservesLiveAnchorAndCounterparty(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	later := LoopParticipant{ID: 3, PersonID: 8, Email: "blair@example.test"}
	view.Participants = append(view.Participants, later)
	nudge := anchor
	nudge.ID, nudge.Sender = 11, later
	nudge.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = []LoopMessage{nudge}
	view.LastMessageAt = nudge.OccurredAt
	extended := ConversationView{Messages: []LoopMessage{anchor, nudge}}
	view.Extend = func() (ConversationView, error) { return extended, nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{Participants: []int64{later.ID}})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(anchor.ID, got.Open.Anchor.ID)
	assertions.Equal(anchor.Sender.ID, got.Open.Counterparty.ID)
	assertions.Equal(anchor.OccurredAt, got.Open.OwedSinceAt)
}

func TestDetectExtendedWindowRestoresConversationContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	view.ConversationType = "direct_chat"
	anchor := view.Messages[0]
	anchor.Direction, anchor.Sender, anchor.Recipients = Outbound, view.Participants[0], nil
	anchor.OccurredAt = now.Add(-96 * time.Hour)
	nudge := anchor
	nudge.ID, nudge.OccurredAt = 11, now.Add(-24*time.Hour)
	view.Messages = []LoopMessage{nudge}
	view.LastMessageAt = nudge.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == anchor.ID {
			return "Can you review this?", nil
		}
		return "Could you review this too?", nil
	}
	view.Extend = func() (ConversationView, error) { return ConversationView{Messages: []LoopMessage{anchor, nudge}}, nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(anchor.ID, got.Open.Anchor.ID)
	assertions.Equal(view.Participants[1].ID, got.Open.Counterparty.ID)
}

func TestDetectExtendedWindowUsesEarlierOwnerParticipationForBulkTurn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	anchor.OccurredAt = now.Add(-96 * time.Hour)
	reply := anchor
	reply.ID, reply.Direction, reply.Sender = 11, Outbound, view.Participants[0]
	reply.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	reply.OccurredAt = now.Add(-72 * time.Hour)
	bulk := anchor
	bulk.ID, bulk.OccurredAt = 12, now.Add(-48*time.Hour)
	bulk.Signals = &mime.AutomationSignals{PrecedenceBulk: true}
	current := bulk
	current.ID, current.OccurredAt = 13, now.Add(-24*time.Hour)
	view.Messages = []LoopMessage{bulk, current}
	view.LastMessageAt = current.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == reply.ID {
			return "I reviewed the request.", nil
		}
		return "Can you check?", nil
	}
	extended := view
	extended.Messages = []LoopMessage{anchor, reply, bulk, current}
	view.Extend = func() (ConversationView, error) { return extended, nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	requirements.NotNil(got.Open)
	assertions.Equal(bulk.ID, got.Open.Anchor.ID)
	assertions.Equal(anchor.Sender.ID, got.Open.Counterparty.ID)
}

func TestDetectExtendedNeutralWindowFindsNewQuestionAfterSettlement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	anchor.OccurredAt = now.Add(-96 * time.Hour)
	question := anchor
	question.ID, question.Direction, question.Sender = 11, Outbound, view.Participants[0]
	question.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	question.OccurredAt = now.Add(-24 * time.Hour)
	neutral := anchor
	neutral.ID, neutral.Direction = 12, Observed
	neutral.OccurredAt = now.Add(-time.Hour)
	view.Messages = []LoopMessage{neutral}
	view.LastMessageAt = neutral.OccurredAt
	view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }
	extended := view
	extended.Messages = []LoopMessage{anchor, question, neutral}
	view.Extend = func() (ConversationView, error) { return extended, nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	assertions.Equal(question.ID, got.Close[0].MessageID)
	requirements.NotNil(got.Open)
	assertions.Equal(AwaitingReply, got.Open.Kind)
	assertions.Equal(question.ID, got.Open.Anchor.ID)
}

func TestDetectWithdrawsExtendedLiveAnchorForTurnWideCCListMail(t *testing.T) {
	for _, extended := range []bool{false, true} {
		name := "full window"
		if extended {
			name = "extended window"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			anchor.OccurredAt = now.Add(-96 * time.Hour)
			listSender := LoopParticipant{ID: 3, Email: "blair@example.test"}
			view.Participants = append(view.Participants, listSender)
			newsletter := anchor
			newsletter.ID, newsletter.Sender = 11, listSender
			newsletter.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "cc"}}
			newsletter.ListID = "list.example.test"
			newsletter.OccurredAt = now.Add(-24 * time.Hour)
			view.LastMessageAt = newsletter.OccurredAt
			view.Messages = []LoopMessage{anchor, newsletter}
			if extended {
				view.Messages = []LoopMessage{newsletter}
				full := view
				full.Messages = []LoopMessage{anchor, newsletter}
				view.Extend = func() (ConversationView, error) { return full, nil }
			}

			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal("withdrawn", got.Close[0].Reason)
			assertions.Empty(got.CoverageGaps)
			assertions.Nil(got.Open)
		})
	}
}

func TestDetectReconsidersExtendedWindowAfterEditedAwaitingAnchorWithdrawn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	edited := view.Messages[0]
	edited.Direction, edited.Sender = Outbound, view.Participants[0]
	edited.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	edited.OccurredAt = now.Add(-96 * time.Hour)
	request := view.Messages[0]
	request.ID, request.OccurredAt = 11, now.Add(-48*time.Hour)
	neutral := edited
	neutral.ID, neutral.Direction, neutral.OccurredAt = 12, Observed, now.Add(-time.Hour)
	view.Messages = []LoopMessage{neutral}
	view.LastMessageAt = neutral.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == edited.ID {
			return "FYI only.", nil
		}
		return "", nil
	}
	extended := view
	extended.Messages = []LoopMessage{edited, request, neutral}
	view.Extend = func() (ConversationView, error) { return extended, nil }

	got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: edited.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("withdrawn", got.Close[0].Reason)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(request.ID, got.Open.Anchor.ID)
}

func TestDetectMarksUnreadableAwaitingQuestionRunIncomplete(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
	anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	anchor.OccurredAt = now.Add(-96 * time.Hour)
	newer := anchor
	newer.ID, newer.OccurredAt = 11, now.Add(-48*time.Hour)
	view.Messages = []LoopMessage{anchor, newer}
	view.LastMessageAt = newer.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == anchor.ID {
			return "", errors.New("body unavailable")
		}
		return "Any update?", nil
	}

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.Equal(newer.ID, got.Open.Anchor.ID)
	assertions.True(got.Open.TruncatedRun)
}

func TestDetectAcknowledgementAfterInboundNudgeKeepsQuestion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	question := view.Messages[0]
	nudge := question
	nudge.ID, nudge.OccurredAt = 11, now.Add(-48*time.Hour)
	ack := question
	ack.ID, ack.Direction, ack.Sender = 12, Outbound, view.Participants[0]
	ack.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = append(view.Messages, nudge, ack)
	view.LastMessageAt = ack.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		switch id {
		case 10:
			return "Can you review this?", nil
		case 11:
			return "Following up on the earlier note.", nil
		default:
			return "Thanks!", nil
		}
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(int64(10), got.Open.Anchor.ID)
}

func TestDetectListOrBulkMailDoesNotSplitAwaitingReplyTurn(t *testing.T) {
	for _, marker := range []struct {
		name string
		mark func(*LoopMessage)
	}{
		{name: "list id", mark: func(message *LoopMessage) { message.ListID = "list.example.test" }},
		{name: "promotion label", mark: func(message *LoopMessage) { message.Labels = []string{"CATEGORY_PROMOTIONS"} }},
		{name: "precedence bulk", mark: func(message *LoopMessage) {
			message.Signals = &mime.AutomationSignals{PrecedenceBulk: true}
		}},
		{name: "list unsubscribe", mark: func(message *LoopMessage) {
			message.Signals = &mime.AutomationSignals{ListUnsubscribe: true}
		}},
	} {
		for _, nudge := range []struct {
			name string
			body string
		}{
			{name: "statement nudge", body: "I sent the report earlier."},
			{name: "question nudge", body: "Could you review the updated report?"},
		} {
			for _, hasLive := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/live=%t", marker.name, nudge.name, hasLive)
				t.Run(name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					view, now := detectorFixture()
					anchor := view.Messages[0]
					anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
					anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
					anchor.OccurredAt = now.Add(-96 * time.Hour)

					mail := anchor
					mail.ID, mail.Direction, mail.Sender = 11, Inbound, view.Participants[1]
					mail.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "to"}}
					mail.OccurredAt = now.Add(-48 * time.Hour)
					marker.mark(&mail)

					latest := anchor
					latest.ID, latest.OccurredAt = 12, now.Add(-time.Hour)
					view.Messages = []LoopMessage{anchor, mail, latest}
					view.LastMessageAt = latest.OccurredAt
					view.BodyText = func(id int64) (string, error) {
						switch id {
						case anchor.ID:
							return "Can you review this?", nil
						case latest.ID:
							return nudge.body, nil
						default:
							return "Please review the newsletter.", nil
						}
					}

					var live []LiveLoop
					if hasLive {
						live = []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: anchor.ID}}
					}
					got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
					requirements.NoError(got.Err)
					assertions.Empty(got.Close)
					requirements.NotNil(got.Open)
					assertions.Equal(AwaitingReply, got.Open.Kind)
					assertions.Equal(anchor.ID, got.Open.Anchor.ID)
					assertions.Equal(anchor.OccurredAt, got.Open.OwedSinceAt)
					assertions.Equal("open", got.Open.Status)
				})
			}
		}
	}
}

func TestDetectLatestListIDDoesNotHideAwaitingReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	question := view.Messages[0]
	question.Direction, question.Sender = Outbound, view.Participants[0]
	question.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	question.OccurredAt = now.Add(-96 * time.Hour)
	mail := question
	mail.ID, mail.Direction, mail.Sender = 11, Inbound, view.Participants[1]
	mail.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "to"}}
	mail.OccurredAt = now.Add(-time.Hour)
	mail.ListID = "list.example.test"
	view.Messages = []LoopMessage{question, mail}
	view.LastMessageAt = mail.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == question.ID {
			return "Can you review this?", nil
		}
		return "Please review the newsletter.", nil
	}

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	requirements.NotNil(got.Open)
	assertions.Equal(AwaitingReply, got.Open.Kind)
	assertions.Equal(question.ID, got.Open.Anchor.ID)
	assertions.Equal(question.OccurredAt, got.Open.OwedSinceAt)
	assertions.Equal("open", got.Open.Status)
}

func TestDetectIneligibleTrailingListIDDoesNotWithdrawReplyOwed(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(*LoopMessage)
	}{
		{name: "deleted", mark: func(message *LoopMessage) { message.Deleted = true }},
		{name: "automated", mark: func(message *LoopMessage) {
			message.Signals = &mime.AutomationSignals{AutoSubmitted: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			anchor.OccurredAt = now.Add(-48 * time.Hour)
			listMail := anchor
			listMail.ID, listMail.OccurredAt = 11, now.Add(-time.Hour)
			listMail.ListID = "list.example.test"
			tc.mark(&listMail)
			view.Messages = []LoopMessage{anchor, listMail}
			view.LastMessageAt = listMail.OccurredAt
			view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }

			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Empty(got.Close)
			assertions.Empty(got.CoverageGaps)
			requirements.NotNil(got.Open)
			assertions.Equal(ReplyOwed, got.Open.Kind)
			assertions.Equal(anchor.ID, got.Open.Anchor.ID)
		})
	}
}

func TestDetectLatestBulkMailPreservesPendingOutboundQuestion(t *testing.T) {
	for _, marker := range []struct {
		name string
		mark func(*LoopMessage)
	}{
		{name: "promotion label", mark: func(message *LoopMessage) { message.Labels = []string{"CATEGORY_PROMOTIONS"} }},
		{name: "social label", mark: func(message *LoopMessage) { message.Labels = []string{"CATEGORY_SOCIAL"} }},
		{name: "forum label", mark: func(message *LoopMessage) { message.Labels = []string{"CATEGORY_FORUMS"} }},
		{name: "precedence bulk", mark: func(message *LoopMessage) {
			message.Signals = &mime.AutomationSignals{PrecedenceBulk: true}
		}},
		{name: "list unsubscribe", mark: func(message *LoopMessage) {
			message.Signals = &mime.AutomationSignals{ListUnsubscribe: true}
		}},
	} {
		for _, hasNudge := range []bool{false, true} {
			for _, hasLive := range []bool{false, true} {
				name := fmt.Sprintf("%s/nudge=%t/live=%t", marker.name, hasNudge, hasLive)
				t.Run(name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					view, now := detectorFixture()
					question := view.Messages[0]
					question.Direction, question.Sender = Outbound, view.Participants[0]
					question.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
					question.OccurredAt = now.Add(-96 * time.Hour)
					nudge := question
					nudge.ID, nudge.OccurredAt = 11, now.Add(-48*time.Hour)
					bulk := question
					bulk.ID, bulk.Direction, bulk.Sender = 12, Inbound, view.Participants[1]
					bulk.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "to"}}
					bulk.OccurredAt = now.Add(-time.Hour)
					marker.mark(&bulk)
					if hasNudge {
						view.Messages = []LoopMessage{question, nudge, bulk}
					} else {
						view.Messages = []LoopMessage{question, bulk}
					}
					view.LastMessageAt = bulk.OccurredAt
					view.BodyText = func(id int64) (string, error) {
						switch id {
						case question.ID:
							return "Can you review this?", nil
						case nudge.ID:
							return "I sent this earlier.", nil
						default:
							return "See the updates.", nil
						}
					}

					var live []LiveLoop
					if hasLive {
						live = []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: question.ID}}
					}
					got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
					requirements.NoError(got.Err)
					assertions.Empty(got.Close)
					assertions.Empty(got.CoverageGaps)
					requirements.NotNil(got.Open)
					assertions.Equal(AwaitingReply, got.Open.Kind)
					assertions.Equal(question.ID, got.Open.Anchor.ID)
					assertions.Equal(question.OccurredAt, got.Open.OwedSinceAt)
				})
			}
		}
	}
}

func TestDetectBulkQuestionDoesNotPromoteMutedOutboundQuestion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	muted := LoopParticipant{ID: 3, PersonID: 8, Email: "blair@example.test", DisplayName: "Blair Example"}
	view.Participants = append(view.Participants, muted)
	ownerQuestion := view.Messages[0]
	ownerQuestion.ID, ownerQuestion.Direction, ownerQuestion.Sender = 9, Outbound, view.Participants[0]
	ownerQuestion.OccurredAt = now.Add(-48 * time.Hour)
	ownerQuestion.Recipients = []LoopRecipient{{LoopParticipant: muted, Type: "to"}}
	bulkQuestion := view.Messages[0]
	bulkQuestion.ID, bulkQuestion.OccurredAt = 10, now.Add(-24*time.Hour)
	bulkQuestion.Labels = []string{"CATEGORY_PROMOTIONS"}
	view.Messages = []LoopMessage{ownerQuestion, bulkQuestion}
	view.LastMessageAt = bulkQuestion.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == ownerQuestion.ID {
			return "Could you review the report?", nil
		}
		return "Could you review this newsletter?", nil
	}
	suppressions := Suppressions{Participants: []int64{muted.ID}}

	got := Detect(view, nil, now, DefaultThresholds(), suppressions)
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(bulkQuestion.ID, got.Open.Anchor.ID)
	assertions.Equal(view.Participants[1].ID, got.Open.Counterparty.ID)

	again := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: bulkQuestion.ID}}, now, DefaultThresholds(), suppressions)
	requirements.NoError(again.Err)
	assertions.Empty(again.Close)
	requirements.NotNil(again.Open)
	assertions.Equal(ReplyOwed, again.Open.Kind)
	assertions.Equal(bulkQuestion.ID, again.Open.Anchor.ID)
}

func TestDetectBulkSuffixPreservesEarlierOutboundQuestion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nudgeBody string
		mixedList bool
	}{
		{name: "statement nudge after earlier bulk", nudgeBody: "I sent this earlier."},
		{name: "question nudge after earlier bulk", nudgeBody: "Could you review the revised copy?"},
		{name: "mixed list and bulk suffix", mixedList: true},
	} {
		for _, hasLive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/live=%t", tc.name, hasLive), func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				view, now := detectorFixture()
				question := view.Messages[0]
				question.Direction, question.Sender = Outbound, view.Participants[0]
				question.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				question.OccurredAt = now.Add(-120 * time.Hour)
				olderBulk := question
				olderBulk.ID, olderBulk.Direction, olderBulk.Sender = 11, Inbound, view.Participants[1]
				olderBulk.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "to"}}
				olderBulk.OccurredAt = now.Add(-96 * time.Hour)
				olderBulk.Labels = []string{"CATEGORY_PROMOTIONS"}
				latestBulk := olderBulk
				latestBulk.ID, latestBulk.OccurredAt = 14, now.Add(-time.Hour)
				var messages []LoopMessage
				if tc.mixedList {
					listMail := olderBulk
					listMail.ID, listMail.OccurredAt = 12, now.Add(-72*time.Hour)
					listMail.Labels = nil
					listMail.ListID = "list.example.test"
					messages = []LoopMessage{question, olderBulk, listMail, latestBulk}
				} else {
					nudge := question
					nudge.ID, nudge.OccurredAt = 12, now.Add(-72*time.Hour)
					nudgeBody := tc.nudgeBody
					if nudgeBody == "" {
						nudgeBody = "Newsletter update."
					}
					messages = []LoopMessage{question, olderBulk, nudge, latestBulk}
					view.BodyText = func(id int64) (string, error) {
						switch id {
						case question.ID:
							return "Can you review this?", nil
						case nudge.ID:
							return nudgeBody, nil
						default:
							return "Newsletter update.", nil
						}
					}
				}
				if tc.mixedList {
					view.BodyText = func(id int64) (string, error) {
						if id == question.ID {
							return "Can you review this?", nil
						}
						return "Newsletter update.", nil
					}
				}
				view.Messages = messages
				view.LastMessageAt = latestBulk.OccurredAt
				var live []LiveLoop
				if hasLive {
					live = []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: question.ID}}
				}
				got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
				requirements.NoError(got.Err)
				assertions.Empty(got.Close)
				assertions.Empty(got.CoverageGaps)
				requirements.NotNil(got.Open)
				assertions.Equal(AwaitingReply, got.Open.Kind)
				assertions.Equal(question.ID, got.Open.Anchor.ID)
				assertions.Equal(question.OccurredAt, got.Open.OwedSinceAt)
			})
		}
	}
}

func TestDetectHumanMatrixIdentifierEndingInBotRemainsSubstantive(t *testing.T) {
	for _, field := range []string{"display name", "canonical ID"} {
		t.Run(field, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			view.SourceType, view.ConversationType = "matrix", "direct_chat"
			sender := view.Participants[1]
			sender.Email, sender.Phone, sender.DisplayName, sender.CanonicalID = "", "", "", ""
			if field == "display name" {
				sender.DisplayName = "@talbot:example.test"
			} else {
				sender.CanonicalID = "@talbot:example.test"
			}
			view.Participants[1] = sender
			view.Messages[0].Sender = sender
			view.Messages[0].Recipients = nil

			got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.NotNil(got.Open)
			assertions.Equal(ReplyOwed, got.Open.Kind)
			assertions.Equal(view.Messages[0].ID, got.Open.Anchor.ID)
		})
	}
}

func TestDetectMatrixBotMarkerRemainsNeutral(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	view.SourceType, view.ConversationType = "matrix", "direct_chat"
	sender := view.Participants[1]
	sender.Email, sender.Phone, sender.DisplayName, sender.CanonicalID = "", "", "", "@release-bot:example.test"
	view.Participants[1] = sender
	view.Messages[0].Sender = sender
	view.Messages[0].Recipients = nil

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Nil(got.Open)
}

func TestDetectAcknowledgementIgnoresQuestionAnchorProvenAbsent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	fyi := view.Messages[0]
	fyi.OccurredAt = now.Add(-96 * time.Hour)
	missingQuestion := fyi
	missingQuestion.ID, missingQuestion.OccurredAt = 11, now.Add(-48*time.Hour)
	acknowledgement := fyi
	acknowledgement.ID, acknowledgement.Direction, acknowledgement.Sender = 12, Outbound, view.Participants[0]
	acknowledgement.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	acknowledgement.OccurredAt = now.Add(-time.Hour)
	view.Messages = []LoopMessage{fyi, missingQuestion, acknowledgement}
	view.LastMessageAt = acknowledgement.OccurredAt
	view.AnchorExists = func(id int64) (bool, error) {
		assertions.Equal(missingQuestion.ID, id)
		return false, nil
	}
	view.BodyText = func(id int64) (string, error) {
		switch id {
		case fyi.ID:
			return "Thanks for the update.", nil
		case missingQuestion.ID:
			return "Can you review this?", nil
		default:
			return "Thanks!", nil
		}
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: missingQuestion.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("withdrawn", got.Close[0].Reason)
	assertions.Nil(got.Open)
	assertions.Empty(got.CoverageGaps)
}

func TestDetectUnreadableAcknowledgementEvidenceMarksSpecTruncated(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	fyi := view.Messages[0]
	fyi.OccurredAt = now.Add(-48 * time.Hour)
	acknowledgement := fyi
	acknowledgement.ID, acknowledgement.Direction, acknowledgement.Sender = 12, Outbound, view.Participants[0]
	acknowledgement.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	acknowledgement.OccurredAt = now.Add(-time.Hour)
	view.Messages = []LoopMessage{fyi, acknowledgement}
	view.LastMessageAt = acknowledgement.OccurredAt
	view.BodyText = func(id int64) (string, error) {
		if id == fyi.ID {
			return "", errors.New("body unavailable")
		}
		return "Thanks!", nil
	}

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.Equal(ReplyOwed, got.Open.Kind)
	assertions.Equal(fyi.ID, got.Open.Anchor.ID)
	assertions.True(got.Open.TruncatedRun)
}

func TestDetectCounterpartyUsesObligationEvidence(t *testing.T) {
	t.Run("inbound anchor sender", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		view, now := detectorFixture()
		later := LoopParticipant{ID: 3, PersonID: 8, Email: "blair@example.test"}
		view.Participants = append(view.Participants, later)
		message := view.Messages[0]
		message.ID, message.Sender = 11, later
		message.OccurredAt = now.Add(-24 * time.Hour)
		view.Messages = append(view.Messages, message)
		view.LastMessageAt = message.OccurredAt

		got := Detect(view, nil, now, DefaultThresholds(), Suppressions{Persons: []int64{later.PersonID}})
		requirements.NoError(got.Err)
		requirements.NotNil(got.Open)
		assertions.Equal(int64(2), got.Open.Counterparty.ID)
		assertions.Empty(got.Close)
	})

	t.Run("outbound anchor recipients", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		view, now := detectorFixture()
		later := LoopParticipant{ID: 3, PersonID: 8, Email: "blair@example.test"}
		view.Participants = append(view.Participants, later)
		anchor := view.Messages[0]
		anchor.ID, anchor.Direction, anchor.Sender = 10, Outbound, view.Participants[0]
		anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
		anchor.OccurredAt = now.Add(-96 * time.Hour)
		fyi := anchor
		fyi.ID, fyi.Recipients = 11, []LoopRecipient{{LoopParticipant: later, Type: "to"}}
		fyi.OccurredAt = now.Add(-24 * time.Hour)
		view.Messages = []LoopMessage{anchor, fyi}
		view.LastMessageAt = fyi.OccurredAt
		view.BodyText = func(id int64) (string, error) {
			if id == 10 {
				return "Can you review this?", nil
			}
			return "FYI, another message.", nil
		}

		got := Detect(view, nil, now, DefaultThresholds(), Suppressions{Participants: []int64{later.ID}})
		requirements.NoError(got.Err)
		requirements.NotNil(got.Open)
		assertions.Equal(int64(2), got.Open.Counterparty.ID)
		assertions.Equal(int64(10), got.Open.Anchor.ID)
		assertions.Empty(got.Close)
	})
}

func TestDetectPrefersExplicitMentionOverGroupMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	view.SourceType, view.ConversationType = "slack", "group_chat"
	mentioned := LoopParticipant{ID: 9, PersonID: 18, Email: "blair@example.test", DisplayName: "Blair"}
	otherMention := LoopParticipant{ID: 11, PersonID: 22, Email: "casey@example.test", DisplayName: "Casey"}
	view.Participants = append(view.Participants, mentioned, otherMention)
	question := view.Messages[0]
	question.Direction, question.Sender = Outbound, view.Participants[0]
	question.Recipients = []LoopRecipient{
		{LoopParticipant: view.Participants[1], Type: "member"},
		{LoopParticipant: otherMention, Type: "mention"},
		{LoopParticipant: mentioned, Type: "mention"},
	}
	question.OccurredAt = now.Add(-96 * time.Hour)
	view.Messages = []LoopMessage{question}
	view.LastMessageAt = question.OccurredAt
	view.BodyText = func(int64) (string, error) { return "Can you review this?", nil }

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{Participants: []int64{view.Participants[1].ID}})
	requirements.NoError(got.Err)
	requirements.NotNil(got.Open)
	assertions.Equal(mentioned.ID, got.Open.Counterparty.ID)
	assertions.Empty(got.Close)
}

func TestDetectPreservesLiveBulkAnchorWhenPreviousTurnFallsOutsideWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	anchor := view.Messages[0]
	anchor.OccurredAt = now.Add(-48 * time.Hour)
	anchor.Signals = &mime.AutomationSignals{PrecedenceBulk: true}
	view.Messages = []LoopMessage{anchor}
	view.LastMessageAt = anchor.OccurredAt
	view.BodyText = func(int64) (string, error) { return "I can help with this.", nil }
	earlierOwnerTurn := anchor
	earlierOwnerTurn.ID, earlierOwnerTurn.Direction, earlierOwnerTurn.Sender = 9, Outbound, view.Participants[0]
	earlierOwnerTurn.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
	earlierOwnerTurn.OccurredAt = now.Add(-72 * time.Hour)
	var extensions int
	view.Extend = func() (ConversationView, error) {
		extensions++
		extended := view
		extended.Messages = []LoopMessage{earlierOwnerTurn, anchor}
		return extended, nil
	}

	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Empty(got.Close)
	assertions.Empty(got.CoverageGaps)
	requirements.NotNil(got.Open)
	assertions.Equal(anchor.ID, got.Open.Anchor.ID)
	assertions.False(got.Open.PreviousTurnKnown)
	assertions.Zero(extensions, "the live anchor remains inside the bounded window")
}

func TestDetectUsesLatestMessageForBulkAutomationSignals(t *testing.T) {
	for _, signal := range []struct {
		name  string
		value mime.AutomationSignals
	}{
		{name: "precedence bulk", value: mime.AutomationSignals{PrecedenceBulk: true}},
		{name: "list unsubscribe", value: mime.AutomationSignals{ListUnsubscribe: true}},
	} {
		t.Run(signal.name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				markOlder bool
				wantOpen  bool
			}{
				{name: "earlier message signal does not suppress latest clean message", markOlder: true, wantOpen: true},
				{name: "latest message signal suppresses without owner participation", wantOpen: false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					view, now := detectorFixture()
					older, newer := view.Messages[0], view.Messages[0]
					older.ID, newer.ID = 10, 11
					older.OccurredAt, newer.OccurredAt = now.Add(-48*time.Hour), now.Add(-24*time.Hour)
					if tc.markOlder {
						older.Signals = &signal.value
					} else {
						newer.Signals = &signal.value
					}
					view.Messages = []LoopMessage{older, newer}
					view.LastMessageAt = newer.OccurredAt

					got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
					requirements.NoError(got.Err)
					if !tc.wantOpen {
						assertions.Nil(got.Open)
						return
					}
					requirements.NotNil(got.Open)
					assertions.Equal(older.ID, got.Open.Anchor.ID)
				})
			}
		})
	}
}

func TestDetectAggregatesBulkLabelsAcrossInboundTurn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	older, newer := view.Messages[0], view.Messages[0]
	older.ID, newer.ID = 10, 11
	older.OccurredAt, newer.OccurredAt = now.Add(-48*time.Hour), now.Add(-24*time.Hour)
	older.Labels = []string{"CATEGORY_PROMOTIONS"}
	view.Messages = []LoopMessage{older, newer}
	view.LastMessageAt = newer.OccurredAt

	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	assertions.Nil(got.Open)
}

func TestDetectWithholdsListMailAndPreservesVerifiedBulkAnchor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mark     func(*LoopMessage)
		wantOpen bool
	}{
		{name: "bulk signal", wantOpen: true, mark: func(message *LoopMessage) {
			message.Signals = &mime.AutomationSignals{PrecedenceBulk: true}
		}},
		{name: "list mail", mark: func(message *LoopMessage) { message.ListID = "list.example.test" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			anchor.OccurredAt = now.Add(-48 * time.Hour)
			anchor.ListID = ""
			newer := anchor
			newer.ID = 11
			newer.OccurredAt = now.Add(-24 * time.Hour)
			tc.mark(&newer)
			view.Messages = []LoopMessage{anchor, newer}
			view.LastMessageAt = newer.OccurredAt

			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			if tc.wantOpen {
				assertions.Empty(got.Close)
				requirements.NotNil(got.Open)
				assertions.Equal(anchor.ID, got.Open.Anchor.ID)
				return
			}
			requirements.Len(got.Close, 1)
			assertions.Equal("withdrawn", got.Close[0].Reason)
			assertions.Nil(got.Open)
		})
	}
}

func TestDetectBotClassificationUsesCaseSensitiveSuffix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantOpen bool
	}{
		{name: "DeployBot", wantOpen: false},
		{name: "Talbot", wantOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			owner := view.Participants[0]
			bot := LoopParticipant{ID: 2, DisplayName: tc.name}
			view.SourceType, view.ConversationType = "slack", "direct_chat"
			view.Participants = []LoopParticipant{owner, bot}
			message := view.Messages[0]
			message.Sender = bot
			message.Recipients = nil
			view.Messages = []LoopMessage{message}
			view.BodyText = func(int64) (string, error) { return "Please review this.", nil }

			got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			if tc.wantOpen {
				requirements.NotNil(got.Open)
				assertions.Equal(ReplyOwed, got.Open.Kind)
				assertions.Equal(bot.ID, got.Open.Counterparty.ID)
				return
			}
			assertions.Nil(got.Open)
		})
	}
}

func TestDetectAppliesMuteToOlderLiveAnchorWithNeutralRecentWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		suppress func(LoopParticipant) Suppressions
	}{
		{name: "participant", suppress: func(p LoopParticipant) Suppressions { return Suppressions{Participants: []int64{p.ID}} }},
		{name: "person", suppress: func(p LoopParticipant) Suppressions { return Suppressions{Persons: []int64{p.PersonID}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			anchor := view.Messages[0]
			neutral := anchor
			neutral.ID, neutral.Direction = 11, Observed
			neutral.OccurredAt = now.Add(-time.Hour)
			full := view
			full.Messages = []LoopMessage{anchor, neutral}
			full.LastMessageAt = neutral.OccurredAt
			view.Messages = []LoopMessage{neutral}
			view.LastMessageAt = neutral.OccurredAt
			view.Extend = func() (ConversationView, error) { return full, nil }

			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), tc.suppress(anchor.Sender))
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal("muted", got.Close[0].Reason)
			assertions.Empty(got.CoverageGaps)
			assertions.Nil(got.Open)
		})
	}
}
