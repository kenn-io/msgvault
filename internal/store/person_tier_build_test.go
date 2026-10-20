package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/persontiers"
)

func TestTierTallyExcludesListsAndBroadcastsOnceEach(t *testing.T) {
	assert := assert.New(t)
	asOf := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	rows := []tierSignalRow{
		{PersonID: 1, MessageID: 1, LocalDate: "2026-10-16", Channel: "email", Inbound: true, List: true},
		{PersonID: 1, MessageID: 2, LocalDate: "2026-10-15", Channel: "email", Inbound: true},
		{PersonID: 1, MessageID: 3, LocalDate: "2026-10-14", Channel: "meeting", Inbound: true, Meeting: true},
		{PersonID: 1, MessageID: 4, LocalDate: "2026-10-13", Channel: "email"},
	}
	broadcast := map[int64]bool{1: true, 2: true, 3: true}

	got := tallyTierRows(rows, broadcast, persontiers.DefaultConfig(), asOf)[1]

	assert.Equal(1, got.ExcludedListEvents, "a list row that is also a broadcast counts as a list")
	assert.Equal(2, got.LargeAudienceInboundEvents, "the message and the all-hands meeting")
	assert.Equal(1, got.OutboundLong)
	assert.Zero(got.InboundLong)
	assert.Zero(got.MeetingDaysLong)
	assert.Equal(1, got.ActiveDaysLong)
	assert.Equal("2026-10-13", got.LastDate, "excluded rows never set the last counted day")

	cfg := persontiers.DefaultConfig()
	cfg.ExcludeListMessages = false
	got = tallyTierRows(rows, map[int64]bool{}, cfg, asOf)[1]
	assert.Zero(got.ExcludedListEvents)
	assert.Equal(2, got.InboundLong)
	assert.Equal("2026-10-16", got.LastDate)
}

func tallyTierRows(rows []tierSignalRow, broadcast map[int64]bool, cfg persontiers.Config, asOf time.Time) map[int64]persontiers.Signals {
	tally := newTierTally(cfg, asOf)
	tally.add(rows, broadcast)
	return tally.result
}
