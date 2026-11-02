package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

// Local midnight can be absent on either today or a window's boundary. The
// stored activity dates still count as whole civil days, including both ends.
func TestPersonTierCivilDateWindows(t *testing.T) {
	for _, tc := range []struct {
		name, zone, today, event string
		short, long, tier        int
	}{
		{"skipped today", "America/Sao_Paulo", "2018-11-04", "2018-11-04", 1, 1, 1},
		{"before short window", "America/Sao_Paulo", "2018-11-04", "2018-11-02", 0, 1, 2},
		{"before long window", "America/Sao_Paulo", "2018-11-04", "2018-10-31", 0, 0, 3},
		{"skipped short boundary", "America/Sao_Paulo", "2018-11-05", "2018-11-03", 0, 1, 2},
		{"skipped long boundary", "America/Sao_Paulo", "2018-11-07", "2018-11-03", 0, 0, 3},
		{"spring DST boundary", "America/New_York", "2026-03-09", "2026-03-08", 1, 1, 1},
		{"fall DST boundary", "America/New_York", "2026-11-02", "2026-11-01", 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, personID := newActivityQueryFixture(t)
			owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
			var counterpart int64
			require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
				`SELECT participant_id FROM person_participants WHERE person_id = ?`), personID).Scan(&counterpart))
			zone, err := time.LoadLocation(tc.zone)
			require.NoError(t, err)
			at, err := time.ParseInLocation("2006-01-02 15:04", tc.today+" 23:30", zone)
			require.NoError(t, err)
			event, err := time.ParseInLocation("2006-01-02 15:04", tc.event+" 12:00", zone)
			require.NoError(t, err)
			for i, outbound := range []bool{true, false} {
				from, to := counterpart, owner
				if outbound {
					from, to = owner, counterpart
				}
				message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("civil-date-%d", i)).WithSentAt(event).Build()
				message.SenderID = sql.NullInt64{Int64: from, Valid: true}
				id, err := f.Store.UpsertMessage(message)
				require.NoError(t, err)
				require.NoError(t, f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
				require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
			}
			projectTierSignals(t, f, tc.zone, 25)
			cfg := persontiers.DefaultConfig()
			cfg.ShortWindowDays, cfg.LongWindowDays = 2, 4
			cfg.Tier1.MinOutboundShort, cfg.Tier1.MinInboundShort, cfg.Tier1.MinActiveDaysShort = 1, 1, 1
			cfg.Tier2.MinActiveDaysLong = 1
			// The UTC input falls on tomorrow; projection settings determine today.
			options := store.TierOptionsAsOf(store.PersonTierOptions{
				Config: cfg, ActivityTimezone: tc.zone, ActivityMaxDirect: 25,
			}, at.UTC())
			explanation, err := f.Store.ExplainPersonTierContext(t.Context(), personID, options)
			require.NoError(t, err)
			assert.Equal(t, tc.today, explanation.AsOf)
			assert.Equal(t, tc.short, explanation.Signals.InboundShort)
			assert.Equal(t, tc.short, explanation.Signals.OutboundShort)
			assert.Equal(t, tc.long, explanation.Signals.InboundLong)
			assert.Equal(t, tc.long, explanation.Signals.OutboundLong)
			assert.Equal(t, tc.tier, explanation.Tier)
			preview, err := f.Store.PreviewPersonTiersContext(t.Context(), options)
			require.NoError(t, err)
			assert.Equal(t, tc.today, preview.AsOf)
			if tc.tier == 3 {
				assert.Empty(t, preview.People)
			} else {
				require.Len(t, preview.People, 1)
				assert.Equal(t, tc.tier, preview.People[0].Tier)
			}
		})
	}
}
