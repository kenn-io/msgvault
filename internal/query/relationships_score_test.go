package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRelationshipScore(t *testing.T) {
	tests := []struct {
		name    string
		signals RelationshipSignals
		want    float64
	}{
		{
			name:    "zero signals score zero",
			signals: RelationshipSignals{},
			want:    0,
		},
		{
			name:    "one decayed sent unit scores the sent weight",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 1},
			want:    2.0,
		},
		{
			name:    "one decayed meeting unit scores the meeting weight",
			signals: RelationshipSignals{MeetingsTogether: 1, Modalities: 1},
			want:    3.0,
		},
		{
			name:    "one decayed received unit scores the received weight",
			signals: RelationshipSignals{ReceivedFromThem: 1, Modalities: 1},
			want:    1.0,
		},
		{
			name:    "combined signals sum the weighted contributions",
			signals: RelationshipSignals{SentToThem: 1, MeetingsTogether: 1, ReceivedFromThem: 1, Modalities: 1},
			want:    6.0,
		},
		{
			name:    "three modalities apply a 1.5x breadth boost",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 3},
			want:    3.0,
		},
		{
			name:    "two modalities apply a 1.25x breadth boost",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 2},
			want:    2.5,
		},
		{
			name:    "modalities of zero behave like one (no boost, no penalty)",
			signals: RelationshipSignals{SentToThem: 1, Modalities: 0},
			want:    2.0,
		},
		{
			name: "counts and timestamps do not affect score",
			signals: RelationshipSignals{
				SentToThem: 1, Modalities: 1,
				SentCount: 1000, MeetingCount: 1000,
				LastInteractionAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			want: 2.0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, RelationshipScore(tc.signals), 1e-9)
		})
	}
}
