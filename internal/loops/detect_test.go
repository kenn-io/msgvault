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
	for _, body := range []string{"Thanks, Alex!", "Thanks, Alex Example!"} {
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

func TestDetectUncertainAwaitingAnchorKeepsProvenClosures(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "muted counterpart", true: "deleted anchor"}[deleted], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			view.Messages[0].Direction, view.Messages[0].Sender = Outbound, view.Participants[0]
			view.Messages[0].Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			view.BodyText = func(int64) (string, error) { return "", errors.New("body unavailable") }
			supp := Suppressions{}
			reason := "muted"
			if deleted {
				view.AnchorExists = func(int64) (bool, error) { return false, nil }
				reason = "withdrawn"
			} else {
				supp.Participants = []int64{view.Participants[1].ID}
			}
			got := Detect(view, []LiveLoop{{ID: 1, Kind: AwaitingReply, AnchorMessageID: 10}}, now, DefaultThresholds(), supp)
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal(reason, got.Close[0].Reason)
			assertions.Nil(got.Open)
			assertions.Empty(got.CoverageGaps)
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
	current.ID, current.OccurredAt, current.Signals = 13, now.Add(-24*time.Hour), nil
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

func TestDetectAggregatesBulkSignalsAcrossInboundTurn(t *testing.T) {
	for _, signal := range []struct {
		name  string
		value mime.AutomationSignals
	}{
		{name: "precedence bulk", value: mime.AutomationSignals{PrecedenceBulk: true}},
		{name: "list unsubscribe", value: mime.AutomationSignals{ListUnsubscribe: true}},
	} {
		t.Run(signal.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			older := view.Messages[0]
			older.Signals = &signal.value
			view.Messages[0] = older
			newer := older
			newer.ID, newer.Signals = 11, nil
			newer.OccurredAt = now.Add(-24 * time.Hour)
			view.Messages = append(view.Messages, newer)
			view.LastMessageAt = newer.OccurredAt

			got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Nil(got.Open)
		})
	}
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
