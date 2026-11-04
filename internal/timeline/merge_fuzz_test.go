package timeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func FuzzMergeBurstsKeepsCanonicalCopiesOnce(f *testing.F) {
	f.Add(true, false, uint8(0), uint8(1), uint8(1), uint8(3), uint64(10), uint64(11))
	f.Add(false, true, uint8(1), uint8(0), uint8(4), uint8(2), uint64(21), uint64(22))
	f.Fuzz(func(t *testing.T, warmCounted, coldCounted bool, warmDirection, coldDirection, warmCopies, coldCopies uint8, warmID, coldID uint64) {
		assertions, requirements := assert.New(t), require.New(t)
		warmMessageID := int64(warmID%1_000_000 + 1)
		coldMessageID := int64(coldID%1_000_000 + 1)
		if coldMessageID == warmMessageID {
			coldMessageID++
		}
		directions := []store.ActivityDirection{store.DirectionInbound, store.DirectionOutbound}
		at := time.Date(2026, time.October, 22, 13, 30, 0, 0, time.UTC)
		member := func(id int64, counted, cold bool, direction, copies uint8) store.ActivityBurstMember {
			return store.ActivityBurstMember{
				CopyKey: "email:<shared@example.test>", MessageID: id, SourceID: 1, ConversationID: 2,
				Channel: store.ChannelEmail, LocalDate: "2026-10-22", OccurredAt: at,
				Direction: directions[direction%uint8(len(directions))], Counted: counted,
				CopyCount: int(copies%10 + 1), Cold: cold,
			}
		}
		warm := member(warmMessageID, warmCounted, false, warmDirection, warmCopies)
		cold := member(coldMessageID, coldCounted, true, coldDirection, coldCopies)
		burst := func(item store.ActivityBurstMember) store.ActivityBurst {
			return store.ActivityBurst{
				SourceID: item.SourceID, ConversationID: item.ConversationID, Channel: item.Channel,
				LocalDate: item.LocalDate, FirstAt: item.OccurredAt, LastAt: item.OccurredAt,
				LatestMessageIDs: []int64{item.MessageID}, CopyCounts: map[int64]int{item.MessageID: item.CopyCount},
				Counterparts: []store.ActivityMessageCounterpart{}, Members: []store.ActivityBurstMember{item}, Cold: item.Cold,
			}
		}

		merged := mergeBursts([]store.ActivityBurst{burst(warm)}, []store.ActivityBurst{burst(cold)})
		requirements.Len(merged, 1)
		requirements.Len(merged[0].Members, 1)
		want := warm
		if burstMemberPreferred(cold, warm) {
			want = cold
		}
		got := merged[0]
		assertions.Equal(want.MessageID, got.Members[0].MessageID)
		assertions.Equal(warm.CopyCount+cold.CopyCount, got.CopyCounts[want.MessageID])
		assertions.Equal(warm.CopyCount+cold.CopyCount, got.Members[0].CopyCount)
		wantInbound, wantOutbound := 0, 0
		if want.Counted && want.Direction == store.DirectionInbound {
			wantInbound = 1
		}
		if want.Counted && want.Direction == store.DirectionOutbound {
			wantOutbound = 1
		}
		assertions.Equal(wantInbound, got.Inbound)
		assertions.Equal(wantOutbound, got.Outbound)

		mergedAgain := mergeBursts(merged, nil)
		assertions.Equal(merged, mergedAgain)
	})
}
