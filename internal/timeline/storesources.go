package timeline

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/activitydate"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
)

var (
	_ ActivitySource = (*store.Store)(nil)
	_ ColdSource     = (*store.Store)(nil)
	_ CalendarSource = (*store.Store)(nil)
	_ MeetingsSource = (*store.Store)(nil)
	_ CallsSource    = (*store.Store)(nil)
	_ PeopleSource   = (*store.Store)(nil)
	_ BriefsSource   = (*store.Store)(nil)
)

type storeLoops struct{ st *store.Store }

func (s storeLoops) List(ctx context.Context, opts loops.Options, now time.Time) (loops.Result, error) {
	return loops.List(ctx, s.st, opts, now)
}

// NewStoreSources adapts the daemon's archive readers. The daemon can attach
// its configured person agenda service separately; no remote client is created.
func NewStoreSources(st *store.Store, cfg *config.Config) (Sources, error) {
	if st == nil || cfg == nil {
		return Sources{}, fmt.Errorf("%w: store and configuration are required", ErrInvalidRequest)
	}
	loc, err := activitydate.LoadZone(cfg.Activity.Timezone)
	if err != nil {
		return Sources{}, err
	}
	uid, err := st.ArchiveUID()
	if err != nil {
		return Sources{}, err
	}
	return Sources{db: st.DB(), Activity: st, Cold: st, Calendar: st, Meetings: st, Calls: st, People: st, Briefs: st, Loops: storeLoops{st}, ArchiveUID: uid, Location: loc, ZoneDefault: cfg.Activity.Timezone == "" || cfg.Activity.Timezone == "UTC", Clock: time.Now, Filters: store.ActivityFilters{Exclusions: cfg.People.Tiers}}, nil
}
