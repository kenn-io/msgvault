package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/persontiers"
)

func TestBuildTierSignalsCountsRowsByWindowDirectionAndKind(t *testing.T) {
	assert := assert.New(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	row := func(message int64, date, kind, direction, role, channel string) tierSignalRow {
		return tierSignalRow{PersonID: 1, MessageID: message, LocalDate: date, RefKind: kind, Direction: direction, Role: role, Channel: channel}
	}
	rows := []tierSignalRow{
		row(1, "2026-10-17", "message", "outbound", "addressed", "email"),
		row(2, "2026-10-17", "message", "inbound", "sender", "email"),
		row(3, "2026-07-20", "message", "outbound", "member", "chat"),  // 89 days back, inside the short window
		row(4, "2026-07-19", "message", "inbound", "sender", "email"),  // 90 days back, long window only
		row(5, "2025-10-18", "message", "inbound", "sender", "email"),  // 364 days back
		row(6, "2025-10-17", "message", "inbound", "sender", "email"),  // 365 days back, outside
		row(7, "2026-10-18", "message", "inbound", "sender", "email"),  // after as-of
		row(8, "2026-10-10", "message", "outbound", "sender", "email"), // co-author: not counted
		row(9, "2026-10-10", "message", "inbound", "addressed", "email"),
		row(10, "2026-10-12", "meeting", "inbound", "organizer", "meeting"),
		row(11, "2026-10-12", "meeting", "outbound", "attendee", "meeting"),
		row(12, "2026-10-11", "meeting", "inbound", "attendee", "meeting"),
	}

	got := buildTierSignals(rows, nil, persontiers.DefaultConfig(), asOf)[1]

	assert.Equal(2, got.OutboundLong)
	assert.Equal(2, got.OutboundShort)
	assert.Equal(3, got.InboundLong)
	assert.Equal(1, got.InboundShort)
	assert.Equal(2, got.MeetingDaysLong, "two meeting dates, a date counted once")
	assert.Equal(4, got.ActiveDaysShort)
	assert.Equal(6, got.ActiveDaysLong)
	assert.Equal(3, got.ChannelsLong)
}

func TestBuildTierSignalsExcludesListsAndBroadcastsOnceEach(t *testing.T) {
	assert := assert.New(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	rows := []tierSignalRow{
		{PersonID: 1, MessageID: 1, LocalDate: "2026-10-16", RefKind: "message", Direction: "inbound", Role: "sender", Channel: "email", List: true},
		{PersonID: 1, MessageID: 2, LocalDate: "2026-10-15", RefKind: "message", Direction: "inbound", Role: "sender", Channel: "email"},
		{PersonID: 1, MessageID: 3, LocalDate: "2026-10-14", RefKind: "meeting", Direction: "inbound", Role: "organizer", Channel: "meeting"},
		{PersonID: 1, MessageID: 4, LocalDate: "2026-10-13", RefKind: "message", Direction: "outbound", Role: "addressed", Channel: "email"},
		{PersonID: 1, MessageID: 5, LocalDate: "2026-10-12", RefKind: "message", Direction: "outbound", Role: "sender", Channel: "email", List: true},
	}
	broadcast := map[int64]bool{1: true, 2: true, 3: true}

	got := buildTierSignals(rows, broadcast, persontiers.DefaultConfig(), asOf)[1]

	assert.Equal(1, got.ExcludedListEvents, "a list row that is also a broadcast counts as a list; a co-author row counts nowhere")
	assert.Equal(2, got.LargeAudienceInboundEvents, "the message and the all-hands meeting")
	assert.Equal(1, got.OutboundLong)
	assert.Zero(got.InboundLong)
	assert.Zero(got.MeetingDaysLong)
	assert.Equal(1, got.ActiveDaysLong)

	cfg := persontiers.DefaultConfig()
	cfg.ExcludeListMessages = false
	got = buildTierSignals(rows, map[int64]bool{}, cfg, asOf)[1]
	assert.Zero(got.ExcludedListEvents)
	assert.Equal(2, got.InboundLong)
}
