package dedup

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	msgmime "go.kenn.io/msgvault/internal/mime"
)

// appleFingerprints describes one copy's parts; true marks a placeholder.
// Restored parts agree on their header and content across copies.
func appleFingerprints(placeholders ...bool) []msgmime.PartFingerprint {
	parts := make([]msgmime.PartFingerprint, len(placeholders))
	for i, placeholder := range placeholders {
		parts[i] = msgmime.PartFingerprint{PartID: strconv.Itoa(i), HeaderHash: "header",
			RestorableHeaderHash: "restorable", ContentHash: "body", EpilogueHash: "epilogue"}
		if placeholder {
			parts[i].HeaderHash, parts[i].ContentHash = "placeholder header", ""
			parts[i].HasAppleContentLength = true
		}
	}
	return parts
}

func TestSelectSurvivor_ApplePlaceholderOrdering(t *testing.T) {
	older := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	partial := DuplicateMessage{ID: 1, SourceType: "emlx", HasRawMIME: true,
		appleParts: appleFingerprints(true, true), normalizedHash: "partial",
		LabelCount: 3, ArchivedAt: older}
	restored := DuplicateMessage{ID: 2, SourceType: "emlx", HasRawMIME: true,
		appleParts: appleFingerprints(false, false), normalizedHash: "restored",
		LabelCount: 1, ArchivedAt: older.Add(time.Hour)}
	unknown := DuplicateMessage{ID: 3, SourceType: "emlx", HasRawMIME: true,
		normalizedHash: "unknown", LabelCount: 2, ArchivedAt: older.Add(time.Hour)}
	tests := []struct {
		name   string
		edit   func([]DuplicateMessage)
		prefer []string
		want   int64
	}{
		{"unknown parsed MIME disables tier", func([]DuplicateMessage) {}, nil, 1},
		{"missing raw MIME disables tier", func(m []DuplicateMessage) { m[2].HasRawMIME = false }, nil, 1},
		{"all fingerprinted", func(m []DuplicateMessage) { m[2].appleParts = appleFingerprints(false, false) }, nil, 3},
		{"fewer placeholders win", func(m []DuplicateMessage) {
			m[1].appleParts = appleFingerprints(false, true)
			m[2].appleParts = appleFingerprints(true, true)
		}, nil, 2},
		{"equal placeholder counts use fallback", func(m []DuplicateMessage) {
			m[0].appleParts = appleFingerprints(true, false)
			m[1].appleParts = appleFingerprints(false, true)
			m[2].appleParts = appleFingerprints(true, false)
		}, nil, 1},
		{"unknown received copy excluded", func(m []DuplicateMessage) {
			m[0].IsFromMe, m[1].IsFromMe = true, true
		}, nil, 2},
		{"unknown lower-priority source excluded", func(m []DuplicateMessage) {
			m[2].SourceType = "hey"
		}, nil, 2},
		{"sent placeholder wins", func(m []DuplicateMessage) {
			m[0].IsFromMe, m[2].appleParts = true, appleFingerprints(false, false)
		}, nil, 1},
		{"default source authority", func(m []DuplicateMessage) {
			m[0].SourceType, m[2].appleParts = "gmail", appleFingerprints(false, false)
		}, nil, 1},
		{"explicit source authority", func(m []DuplicateMessage) {
			m[1].SourceType, m[2].SourceType = "gmail", "gmail"
			m[2].appleParts = appleFingerprints(false, false)
		}, []string{"emlx", "gmail"}, 1},
		{"equivalent MIME retains payload tier", func(m []DuplicateMessage) {
			for i := range m {
				m[i].appleParts = appleFingerprints(true, true)
				m[i].normalizedHash = "same"
			}
			m[1].AttachmentCount = 5
		}, nil, 2},
		{"no placeholder collisions keep fallback", func(m []DuplicateMessage) {
			m[0].appleParts = appleFingerprints(false, false)
			m[2].appleParts = appleFingerprints(false, false)
			m[1].AttachmentCount = 50
		}, nil, 1},
	}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			messages := []DuplicateMessage{partial, restored, unknown}
			tc.edit(messages)
			engine := NewEngine(nil, Config{SourcePreference: tc.prefer}, nil)
			for _, order := range orders {
				group := DuplicateGroup{KeyType: "message-id", Messages: []DuplicateMessage{
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
					LabelCount: 3 - i, appleParts: appleFingerprints(i == 0)}
			}
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
