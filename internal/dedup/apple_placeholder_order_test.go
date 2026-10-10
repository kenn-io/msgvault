package dedup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	msgmime "go.kenn.io/msgvault/internal/mime"
)

func TestSelectSurvivor_ApplePlaceholderOrdering(t *testing.T) {
	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	partial := DuplicateMessage{ID: 1, SourceType: "emlx", HasRawMIME: true,
		applePlaceholderChecked: true, hasAppleContentLength: true,
		normalizedHash: "partial", LabelCount: 3, ArchivedAt: older}
	restored := DuplicateMessage{ID: 2, SourceType: "emlx", HasRawMIME: true,
		applePlaceholderChecked: true, normalizedHash: "restored",
		LabelCount: 1, ArchivedAt: older.Add(time.Hour)}
	unknown := DuplicateMessage{ID: 3, SourceType: "emlx", HasRawMIME: true,
		normalizedHash: "unknown", LabelCount: 2, ArchivedAt: older.Add(time.Hour)}
	tests := []struct {
		name   string
		edit   func([]DuplicateMessage)
		prefer []string
		key    string
		want   int64
	}{
		{"unknown parsed MIME disables tier", func([]DuplicateMessage) {}, nil, "message-id", 1},
		{"missing raw MIME disables tier", func(m []DuplicateMessage) { m[2].HasRawMIME = false }, nil, "message-id", 1},
		{"all checked", func(m []DuplicateMessage) { m[2].applePlaceholderChecked = true }, nil, "message-id", 3},
		{"unknown received copy excluded", func(m []DuplicateMessage) {
			m[0].IsFromMe, m[1].IsFromMe = true, true
		}, nil, "message-id", 2},
		{"sent placeholder wins", func(m []DuplicateMessage) {
			m[0].IsFromMe, m[2].applePlaceholderChecked = true, true
		}, nil, "message-id", 1},
		{"default source authority", func(m []DuplicateMessage) {
			m[0].SourceType, m[2].applePlaceholderChecked = "gmail", true
		}, nil, "message-id", 1},
		{"explicit source authority", func(m []DuplicateMessage) {
			m[1].SourceType, m[2].SourceType = "gmail", "gmail"
			m[2].applePlaceholderChecked = true
		}, []string{"emlx", "gmail"}, "message-id", 1},
		{"partially restored copies use fallback", func(m []DuplicateMessage) {
			m[1].hasAppleContentLength = true
			m[2].applePlaceholderChecked, m[2].hasAppleContentLength = true, true
		}, nil, "message-id", 1},
		{"equivalent MIME retains payload tier", func(m []DuplicateMessage) {
			for i := range m {
				m[i].applePlaceholderChecked, m[i].hasAppleContentLength = true, true
				m[i].normalizedHash = "same"
			}
			m[1].AttachmentCount = 5
		}, nil, "message-id", 2},
		{"hash groups keep existing fallback", func(m []DuplicateMessage) {
			m[2].applePlaceholderChecked = true
		}, nil, "normalized-hash", 1},
		{"no placeholder collisions keep fallback", func(m []DuplicateMessage) {
			m[0].hasAppleContentLength, m[2].applePlaceholderChecked = false, true
			m[1].AttachmentCount = 50
		}, nil, "message-id", 1},
	}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			messages := []DuplicateMessage{partial, restored, unknown}
			tc.edit(messages)
			for i := range messages {
				messages[i].appleParts = []msgmime.PartFingerprint{{
					PartID: "0", HeaderHash: "header", RestorableHeaderHash: "restorable",
					ContentHash: "body", EpilogueHash: "epilogue",
					HasAppleContentLength: messages[i].hasAppleContentLength,
				}}
			}
			engine := NewEngine(nil, Config{SourcePreference: tc.prefer}, nil)
			for _, order := range orders {
				group := DuplicateGroup{KeyType: tc.key, Messages: []DuplicateMessage{
					messages[order[0]], messages[order[1]], messages[order[2]],
				}}
				engine.selectSurvivor(&group)
				assert.Equal(t, tc.want, group.Messages[group.Survivor].ID, "order %v", order)
			}
		})
	}
}

func TestSelectSurvivor_AppleRestorationAgreementOrdering(t *testing.T) {
	for _, kind := range []string{"matching", "conflicting content", "conflicting encoding"} {
		t.Run(kind, func(t *testing.T) {
			messages := make([]DuplicateMessage, 3)
			for i := range messages {
				messages[i] = DuplicateMessage{ID: int64(i + 1), SourceType: "emlx", HasRawMIME: true,
					applePlaceholderChecked: true, LabelCount: 3 - i,
					appleParts: []msgmime.PartFingerprint{{PartID: "0", HeaderHash: "header",
						RestorableHeaderHash: "restorable", ContentHash: "restored"}}}
			}
			messages[0].hasAppleContentLength = true
			messages[0].appleParts[0].HasAppleContentLength = true
			wantID := int64(2)
			switch kind {
			case "conflicting content":
				messages[2].appleParts[0].ContentHash = "contradictory"
				wantID = 1
			case "conflicting encoding":
				messages[2].appleParts[0].HeaderHash = "another encoding"
				wantID = 1
			}
			engine := NewEngine(nil, Config{}, nil)
			for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
				group := DuplicateGroup{KeyType: "message-id", Messages: []DuplicateMessage{
					messages[order[0]], messages[order[1]], messages[order[2]],
				}}
				engine.selectSurvivor(&group)
				assert.Equal(t, wantID, group.Messages[group.Survivor].ID, "order %v", order)
			}
		})
	}
}
