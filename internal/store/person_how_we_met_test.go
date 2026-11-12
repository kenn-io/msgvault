package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRenderHowWeMet(t *testing.T) {
	at := time.Date(2021, 4, 1, 0, 30, 0, 0, time.FixedZone("east", 3600))
	first := &firstAppearance{event: ActivityEvent{OccurredAt: at}, candidate: ActivityCandidate{ConversationType: "email_thread"}, introducer: &FirstContactIntroducer{DisplayName: " "}}
	for _, tc := range []struct {
		kind FirstContactKind
		want string
	}{
		{FirstContactEmailIntro, "Someone introduced them by email in March 2021."},
		{FirstContactOwnerOutreach, "In your archive, you contacted them first by email in March 2021."},
		{FirstContactInboundOutreach, "In your archive, they contacted you first by email in March 2021."},
		{FirstContactGroupShared, "In your archive, they first appeared in a group conversation in March 2021."},
		{FirstContactMeeting, "In your archive, they first appeared in a meeting in March 2021."},
		{FirstContactObserved, ""},
	} {
		t.Run(string(tc.kind), func(t *testing.T) { first.kind = tc.kind; assert.Equal(t, tc.want, renderHowWeMet(first)) })
	}
	first.kind = FirstContactEmailIntro
	first.introducer = &FirstContactIntroducer{DisplayName: strings.Repeat("雪", 500)}
	result := renderHowWeMet(first)
	assert.LessOrEqual(t, len([]rune(result)), 280)
	assert.Equal(t, strings.Repeat("雪", 120)+" introduced them by email in March 2021.", result)
	assert.Empty(t, renderHowWeMet(nil))
	first.candidate.ConversationType = "direct_chat"
	first.kind = FirstContactOwnerOutreach
	assert.Equal(t, "In your archive, you contacted them first by chat in March 2021.", renderHowWeMet(first))
}
