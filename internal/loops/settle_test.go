package loops

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func TestSettlementWindowGap(t *testing.T) {
	for _, tt := range []struct {
		name            string
		present, exists bool
		wantReason      string
	}{
		{"extended anchor", true, true, "turn_flip"},
		{"outside both windows", false, true, ""},
		{"deleted anchor", false, false, "withdrawn"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			old := view.Messages[0]
			reply := old
			reply.ID, reply.Direction, reply.Sender = 11, Outbound, view.Participants[0]
			reply.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			reply.OccurredAt = now.Add(-time.Hour)
			view.Messages = []LoopMessage{reply}
			view.AnchorExists = func(int64) (bool, error) { return tt.exists, nil }
			var extensions int
			view.Extend = func() (ConversationView, error) {
				extensions++
				extended := view
				if tt.present {
					extended.Messages = []LoopMessage{old, reply}
				}
				return extended, nil
			}
			got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			assertions.LessOrEqual(extensions, 1)
			if tt.wantReason != "" {
				requirements.Len(got.Close, 1)
				assertions.Equal(tt.wantReason, got.Close[0].Reason)
			} else {
				assertions.Empty(got.Close)
				assertions.Equal([]int64{1}, got.CoverageGaps)
			}
		})
	}
}

func TestCoverageGapDoesNotReanchorLiveTurn(t *testing.T) {
	assertions := assert.New(t)
	view, now := detectorFixture()
	view.Messages[0].ID = 20
	view.AnchorExists = func(int64) (bool, error) { return true, nil }
	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	require.NoError(t, got.Err)
	assertions.Empty(got.Close)
	assertions.Equal([]int64{1}, got.CoverageGaps)
	assertions.Nil(got.Open, "a bounded window must not move a live anchor forward")
}

func TestSettlementWithdrawsReclassifiedAnchor(t *testing.T) {
	for _, kind := range []string{ReplyOwed, AwaitingReply} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			if kind == ReplyOwed {
				view.Messages[0].Direction = Outbound
				view.Messages[0].Sender = view.Participants[0]
				view.Messages[0].Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
			}
			got := Detect(view, []LiveLoop{{ID: 1, Kind: kind, AnchorMessageID: view.Messages[0].ID}}, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			requirements.Len(got.Close, 1)
			assertions.Equal("withdrawn", got.Close[0].Reason)
			assertions.Empty(got.CoverageGaps)
		})
	}
}

func TestSettlementLookupErrorPreventsApply(t *testing.T) {
	view, now := detectorFixture()
	view.AnchorExists = func(int64) (bool, error) { return false, errors.New("archive unavailable") }
	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	require.ErrorContains(t, got.Err, "archive unavailable")
	assert.Empty(t, got.Close)
	assert.Nil(t, got.Open)
}

func TestOwnerBodyUnreadableStillSettles(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	view, now := detectorFixture()
	owner := view.Messages[0]
	owner.ID, owner.Direction, owner.Sender = 11, Outbound, view.Participants[0]
	view.Messages = append(view.Messages, owner)
	view.BodyText = func(int64) (string, error) { return "", errors.New("not available") }
	got := Detect(view, []LiveLoop{{ID: 1, Kind: ReplyOwed, AnchorMessageID: 10}}, now, DefaultThresholds(), Suppressions{})
	requirements.NoError(got.Err)
	requirements.Len(got.Close, 1)
	assertions.Equal("turn_flip", got.Close[0].Reason)
	assertions.Nil(got.Open)
}

func TestChatSourceEligibility(t *testing.T) {
	for _, source := range []string{"apple_messages", "beeper", "discord", "facebook_messenger", "google_messages", "imazing_csv", "imessage", "matrix", "slack", "slackdump", "synctech-sms", "synctech_sms", "teams", "whatsapp"} {
		t.Run(source, func(t *testing.T) {
			view, now := detectorFixture()
			view.SourceType, view.ConversationType = source, "direct_chat"
			view.Messages[0].Recipients = nil
			got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
			require.NoError(t, got.Err)
			require.NotNil(t, got.Open)
		})
	}
}

func TestLargeGroupRequiresMention(t *testing.T) {
	view, now := detectorFixture()
	view.SourceType, view.ConversationType = "beeper", "group_chat"
	for i := range 7 {
		view.Participants = append(view.Participants, LoopParticipant{ID: int64(i + 3)})
	}
	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	assert.Nil(t, got.Open)
	view.Messages[0].Recipients[0].Type = "mention"
	got = Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	require.NotNil(t, got.Open)
	assert.Equal(t, ReplyOwed, got.Open.Kind)
}

func TestSmallGroupMembershipCountsAsAddressed(t *testing.T) {
	view, now := detectorFixture()
	view.SourceType, view.ConversationType = "beeper", "group_chat"
	view.Messages[0].Recipients[0].Type = "member"
	got := Detect(view, nil, now, DefaultThresholds(), Suppressions{})
	require.NoError(t, got.Err)
	require.NotNil(t, got.Open)
	assert.Equal(t, ReplyOwed, got.Open.Kind)
}

func TestDetectNeutralWindowKeepsUnsettledLive(t *testing.T) {
	for _, kind := range []string{ReplyOwed, AwaitingReply} {
		for _, neutral := range []string{"automated", "calendar", "deleted", "observed"} {
			t.Run(kind+"/"+neutral, func(t *testing.T) {
				assertions := assert.New(t)
				view, now := detectorFixture()
				anchor := view.Messages[0]
				if kind == AwaitingReply {
					anchor.Direction, anchor.Sender = Outbound, view.Participants[0]
					anchor.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				}
				view.Messages = nil
				for i := range 12 {
					message := anchor
					message.ID = int64(i + 11)
					message.OccurredAt = anchor.OccurredAt.Add(time.Duration(i+1) * time.Hour)
					switch neutral {
					case "automated":
						message.Signals = &mime.AutomationSignals{AutoSubmitted: true}
					case "calendar":
						message.Signals = &mime.AutomationSignals{CalendarPart: true}
					case "deleted":
						message.Deleted = true
					case "observed":
						message.Direction = Observed
					}
					view.Messages = append(view.Messages, message)
				}
				view.AnchorExists = func(int64) (bool, error) { return true, nil }
				var extensions int
				view.Extend = func() (ConversationView, error) {
					extensions++
					extended := view
					extended.Messages = append([]LoopMessage{anchor}, view.Messages...)
					return extended, nil
				}
				got := Detect(view, []LiveLoop{{ID: 1, Kind: kind, AnchorMessageID: anchor.ID}}, now, DefaultThresholds(), Suppressions{})
				require.NoError(t, got.Err)
				assertions.Equal(1, extensions)
				assertions.Empty(got.Close, "neutral messages cannot withdraw an older unsettled turn")
				assertions.Nil(got.Open)
				assertions.Equal([]int64{1}, got.CoverageGaps)
			})
		}
	}
}
