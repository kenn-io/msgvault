package loops

import (
	"errors"
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
		change       func(*ConversationView, *Suppressions)
	}{
		{"list mail", "withdrawn", func(v *ConversationView, _ *Suppressions) { v.Messages[0].ListID = "list.example.test" }},
		{"bulk label", "withdrawn", func(v *ConversationView, _ *Suppressions) { v.Messages[0].Labels = []string{"CATEGORY_PROMOTIONS"} }},
		{"bulk header", "withdrawn", func(v *ConversationView, _ *Suppressions) {
			v.Messages[0].Signals = &mime.AutomationSignals{PrecedenceBulk: true}
		}},
		{"cc only", "withdrawn", func(v *ConversationView, _ *Suppressions) { v.Messages[0].Recipients[0].Type = "cc" }},
		{"conversation mute", "muted", func(v *ConversationView, s *Suppressions) { s.Conversations = []int64{v.ConversationID} }},
		{"sender mute", "muted", func(v *ConversationView, s *Suppressions) { s.Participants = []int64{v.Messages[0].Sender.ID} }},
		{"person mute", "muted", func(v *ConversationView, s *Suppressions) { s.Persons = []int64{v.Messages[0].Sender.PersonID} }},
		{"meeting", "withdrawn", func(v *ConversationView, _ *Suppressions) { v.ConversationType = "meeting" }},
		{"nonmessage source", "withdrawn", func(v *ConversationView, _ *Suppressions) { v.SourceType = "document" }},
		{"outside lookback", "withdrawn", func(v *ConversationView, _ *Suppressions) {
			v.LastMessageAt = v.LastMessageAt.Add(-366 * 24 * time.Hour)
		}},
		{"self only", "withdrawn", func(v *ConversationView, _ *Suppressions) { v.Participants = v.Participants[:1] }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			var supp Suppressions
			tt.change(&view, &supp)
			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), supp)
			requirements.NoError(got.Err)
			assertions.Nil(got.Open)
			requirements.Len(got.Close, 1)
			assertions.Equal(tt.reason, got.Close[0].Reason)
		})
	}
}

func TestDetectBulkWindowRecoversOrPreservesPriorParticipation(t *testing.T) {
	for _, priorOwner := range []bool{true, false} {
		t.Run(map[bool]string{true: "extended owner participation", false: "unknown participation"}[priorOwner], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			oldAnchor := view.Messages[0]
			oldAnchor.ID = 10
			oldAnchor.OccurredAt = now.Add(-72 * time.Hour)
			view.Messages = nil
			for i := range 12 {
				message := oldAnchor
				message.ID = int64(20 + i)
				message.OccurredAt = now.Add(time.Duration(i-24) * time.Hour)
				message.Labels = []string{"CATEGORY_PROMOTIONS"}
				view.Messages = append(view.Messages, message)
			}
			extendedMessages := []LoopMessage{oldAnchor}
			if priorOwner {
				ownerMessage := oldAnchor
				ownerMessage.ID = 9
				ownerMessage.Direction = Outbound
				ownerMessage.Sender = view.Participants[0]
				ownerMessage.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				ownerMessage.OccurredAt = now.Add(-96 * time.Hour)
				extendedMessages = []LoopMessage{ownerMessage, oldAnchor}
			}
			extendedMessages = append(extendedMessages, view.Messages...)
			view.AnchorExists = func(int64) (bool, error) { return true, nil }
			view.Extend = func() (ConversationView, error) {
				extended := view
				extended.Messages = extendedMessages
				return extended, nil
			}
			view.BodyText = func(int64) (string, error) { return "Can you check?", nil }

			got := Detect(view, []LiveLoop{{ID: 7, Kind: ReplyOwed, AnchorMessageID: oldAnchor.ID}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.Empty(got.Close)
			if priorOwner {
				requirements.NotNil(got.Open)
				assertions.Equal(int64(20), got.Open.Anchor.ID)
				assertions.Empty(got.CoverageGaps)
			} else {
				assertions.Nil(got.Open)
				assertions.Equal([]int64{7}, got.CoverageGaps)
			}
		})
	}
}

func TestDetectOutboundQuestion(t *testing.T) {
	for _, tt := range []struct {
		body string
		want bool
	}{
		{"Can you review?", true}, {"FYI, attached", false}, {"See https://example.test/?q=one", false},
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

func TestDetectAcknowledgementKeepsQuestion(t *testing.T) {
	view, now := detectorFixture()
	ack := view.Messages[0]
	ack.ID, ack.Direction, ack.Sender = 11, Outbound, view.Participants[0]
	ack.OccurredAt = now.Add(-24 * time.Hour)
	view.Messages = append(view.Messages, ack)
	for _, body := range []string{"Thanks, Alex!", "Thanks, Avery Example!", "Thanks, Avery and Jordan!", "Thanks, Avery & Jordan!"} {
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

func TestDetectAutomationIsNeutral(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, signal := range []mime.AutomationSignals{{AutoSubmitted: true}, {CalendarPart: true}, {ReplyToNoReply: true}} {
		view, now := detectorFixture()
		m := view.Messages[0]
		m.ID, m.Direction, m.Signals = 11, Outbound, &signal
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
