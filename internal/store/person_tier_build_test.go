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
	message := func(id int64, date, channel string, inbound bool) tierSignalRow {
		return tierSignalRow{PersonID: 1, MessageID: id, LocalDate: date, Channel: channel, Inbound: inbound}
	}
	meeting := func(id int64, date string, inbound bool) tierSignalRow {
		return tierSignalRow{PersonID: 1, MessageID: id, LocalDate: date, Channel: "meeting", Inbound: inbound, Meeting: true}
	}
	rows := []tierSignalRow{
		message(1, "2026-10-17", "email", false),
		message(2, "2026-10-17", "email", true),
		message(3, "2026-07-20", "chat", false), // 89 days back, inside the short window
		message(4, "2026-07-19", "email", true), // 90 days back, long window only
		message(5, "2025-10-18", "email", true), // 364 days back
		meeting(6, "2026-10-12", true),
		meeting(7, "2026-10-12", false),
		meeting(8, "2026-10-11", true),
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
	assert.Equal("2026-10-17", got.LastDate)
}

func TestBuildTierSignalsExcludesListsAndBroadcastsOnceEach(t *testing.T) {
	assert := assert.New(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	rows := []tierSignalRow{
		{PersonID: 1, MessageID: 1, LocalDate: "2026-10-16", Channel: "email", Inbound: true, List: true},
		{PersonID: 1, MessageID: 2, LocalDate: "2026-10-15", Channel: "email", Inbound: true},
		{PersonID: 1, MessageID: 3, LocalDate: "2026-10-14", Channel: "meeting", Inbound: true, Meeting: true},
		{PersonID: 1, MessageID: 4, LocalDate: "2026-10-13", Channel: "email"},
	}
	broadcast := map[int64]bool{1: true, 2: true, 3: true}

	got := buildTierSignals(rows, broadcast, persontiers.DefaultConfig(), asOf)[1]

	assert.Equal(1, got.ExcludedListEvents, "a list row that is also a broadcast counts as a list")
	assert.Equal(2, got.LargeAudienceInboundEvents, "the message and the all-hands meeting")
	assert.Equal(1, got.OutboundLong)
	assert.Zero(got.InboundLong)
	assert.Zero(got.MeetingDaysLong)
	assert.Equal(1, got.ActiveDaysLong)
	assert.Equal("2026-10-13", got.LastDate, "excluded rows never set the last counted day")

	cfg := persontiers.DefaultConfig()
	cfg.ExcludeListMessages = false
	got = buildTierSignals(rows, map[int64]bool{}, cfg, asOf)[1]
	assert.Zero(got.ExcludedListEvents)
	assert.Equal(2, got.InboundLong)
	assert.Equal("2026-10-16", got.LastDate)
}
