package loops

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultTrackTitleUsesKindAndBoundsText(t *testing.T) {
	for _, tc := range []struct {
		kind, name, want string
		participant      int64
	}{
		{"reply_owed", "Example Person", "Reply to Example Person: A question", 0},
		{"awaiting_reply", "Example Person", "Waiting on Example Person: A question", 0},
		{"reply_owed", "", "Reply to participant 42: A question", 42},
		{"commitment_owed_by_me", "Example Person", "A question", 0},
	} {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, DefaultTrackTitle(Loop{Kind: tc.kind, Summary: "A question", Counterparty: Counterparty{DisplayName: tc.name, ParticipantID: tc.participant}}))
		})
	}
	title := DefaultTrackTitle(Loop{Kind: "reply_owed", Summary: strings.Repeat("界", 600), Counterparty: Counterparty{DisplayName: "Example"}})
	assert.LessOrEqual(t, len([]rune(title)), 512)
}
