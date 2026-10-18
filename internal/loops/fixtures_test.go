package loops

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func TestConversationFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/conversations/turns.json")
	require.NoError(t, err)
	var cases []struct {
		Name     string `json:"name"`
		Messages []struct {
			Side      Direction `json:"side"`
			AgeHours  int       `json:"age_hours"`
			Body      string    `json:"body"`
			CCOnly    bool      `json:"cc_only"`
			Automated bool      `json:"automated"`
			Calendar  bool      `json:"calendar"`
			Deleted   bool      `json:"deleted"`
			ListID    string    `json:"list_id"`
			Labels    []string  `json:"labels"`
		} `json:"messages"`
		LiveKind         string `json:"live_kind"`
		WantKind         string `json:"want_kind"`
		WantAnchor       int64  `json:"want_anchor"`
		WantStatus       string `json:"want_status"`
		WantCloseMessage int64  `json:"want_close_message"`
		WantCloseReason  string `json:"want_close_reason"`
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			view, now := detectorFixture()
			base := view.Messages[0]
			view.Messages = nil
			bodies := map[int64]string{}
			for i, input := range tt.Messages {
				m := base
				m.ID, m.Direction = int64(i+1), input.Side
				m.OccurredAt = now.Add(-time.Duration(input.AgeHours) * time.Hour)
				m.ListID, m.Labels, m.Deleted = input.ListID, input.Labels, input.Deleted
				m.Signals = &mime.AutomationSignals{AutoSubmitted: input.Automated, CalendarPart: input.Calendar}
				if input.Side == Outbound {
					m.Sender = view.Participants[0]
					m.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[1], Type: "to"}}
				}
				if input.CCOnly {
					m.Recipients = []LoopRecipient{{LoopParticipant: view.Participants[0], Type: "cc"}}
				}
				view.Messages = append(view.Messages, m)
				bodies[m.ID] = input.Body
			}
			view.LastMessageAt = view.Messages[len(view.Messages)-1].OccurredAt
			view.BodyText = func(id int64) (string, error) { return bodies[id], nil }
			var live []LiveLoop
			if tt.LiveKind != "" {
				live = []LiveLoop{{ID: 1, Kind: tt.LiveKind, AnchorMessageID: 1}}
			}
			got := Detect(view, live, now, DefaultThresholds(), Suppressions{})
			requirements.NoError(got.Err)
			if tt.WantKind == "" {
				assertions.Nil(got.Open)
			} else {
				requirements.NotNil(got.Open)
				assertions.Equal(tt.WantKind, got.Open.Kind)
				assertions.Equal(tt.WantAnchor, got.Open.Anchor.ID)
				status := tt.WantStatus
				if status == "" {
					status = "open"
				}
				assertions.Equal(status, got.Open.Status)
			}
			if tt.WantCloseMessage != 0 || tt.WantCloseReason != "" {
				requirements.Len(got.Close, 1)
				reason := tt.WantCloseReason
				if reason == "" {
					reason = "turn_flip"
				}
				assertions.Equal(reason, got.Close[0].Reason)
				assertions.Equal(tt.WantCloseMessage, got.Close[0].MessageID)
			} else {
				assertions.Empty(got.Close)
			}
		})
	}
}
