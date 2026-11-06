package keepintouch

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
	"hegel.dev/go/hegel"
)

func TestKeepInTouchCadenceBoundary(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		days := hegel.Draw(ht, hegel.Integers(1, 3660))
		window := hegel.Draw(ht, hegel.Integers(0, 3660))
		offset := hegel.Draw(ht, hegel.Integers(-1, 1))
		last := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		due := last.AddDate(0, 0, days)
		boundary := due.AddDate(0, 0, -min(window, days-1))
		now := boundary.Add(time.Duration(offset))
		state := store.ContactState{LastContactAt: &last, CadenceDueAt: &due}
		assert.Equal(ht, offset >= 0, Eligible(state, now, window))
	})
}

func TestKeepInTouchStaleOrUnknownCadenceAreNeverEligible(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.Integers(0, 1))
		window := hegel.Draw(ht, hegel.Integers(0, 3660))
		now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		last, due := now.AddDate(0, 0, -60), now.AddDate(0, 0, -1)
		state := store.ContactState{LastContactAt: &last, CadenceDueAt: &due}
		switch kind {
		case 0:
			state.Stale = true
		case 1:
			state.CadenceDueAt = nil
		}
		assert.False(ht, Eligible(state, now, window))
	})
}

func TestKeepInTouchCycleIgnoresTimezoneRepresentation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		seconds := hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt32)))
		last := time.Unix(seconds, 0)
		shifted := last.In(time.FixedZone("synthetic", 3600))
		assert.Equal(ht, CycleKey("archive", "person", last), CycleKey("archive", "person", shifted))
	})
}

func TestKeepInTouchRecentContactStartsANewIneligibleCycle(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -30)
	oldDue := old.AddDate(0, 0, 21)
	assert.True(t, Eligible(store.ContactState{LastContactAt: &old, CadenceDueAt: &oldDue}, now, 7))
	due := now.AddDate(0, 0, 21)
	assert.False(t, Eligible(store.ContactState{LastContactAt: &now, CadenceDueAt: &due}, now, 7))
	assert.NotEqual(t, CycleKey("archive", "person", old), CycleKey("archive", "person", now))
}

func TestKeepInTouchEligibilityIsMonotone(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		year := hegel.Draw(ht, hegel.Integers(1, 9998))
		nanos := hegel.Draw(ht, hegel.Integers(0, 999999999))
		offset := hegel.Draw(ht, hegel.Integers(0, 110000))
		window := hegel.Draw(ht, hegel.Integers(0, 3660))
		larger := hegel.Draw(ht, hegel.Integers(window, 3660))
		last := time.Date(year, 1, 1, 12, 0, 0, nanos, time.UTC)
		maxDays := int((time.Date(9999, 12, 31, 12, 0, 0, nanos, time.UTC).Unix() - last.Unix()) / 86400)
		days := hegel.Draw(ht, hegel.Integers(1, maxDays))
		due := last.AddDate(0, 0, days)
		now := last.AddDate(0, 0, offset)
		state := store.ContactState{LastContactAt: &last, CadenceDueAt: &due}
		if Eligible(state, now, window) {
			assert.True(ht, Eligible(state, now.Add(24*time.Hour), window))
			assert.True(ht, Eligible(state, now, larger))
		}
		assert.True(ht, Eligible(state, due, window))
		assert.False(ht, Eligible(state, last, window))
	})
}
