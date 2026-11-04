package timeline

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
)

type activityWindow struct {
	bursts   []store.ActivityBurst
	meetings []store.ActivityMessage
	noise    int
	more     bool
}

type coldActivityWindow struct {
	store.BurstPage

	meetings []store.ActivityMessage
}

func (c *composer) readMeetingMessages(ctx context.Context, bursts []store.ActivityBurst, q store.BurstQuery, read func(context.Context, store.MessageQuery) ([]store.ActivityMessage, error)) ([]store.ActivityMessage, error) {
	seen := map[int64]bool{}
	result := []store.ActivityMessage{}
	from, to := c.bounds()
	for _, burst := range bursts {
		if burst.Channel != store.ChannelMeeting || seen[burst.ConversationID] || burst.ConversationID <= 0 {
			continue
		}
		seen[burst.ConversationID] = true
		query := store.MessageQuery{ConversationID: burst.ConversationID, Limit: 500, Filters: q.Filters}
		if q.FromDate != "" {
			query.After, query.Before = &from, &to
		} else {
			query.After, query.Before = q.After, q.Before
		}
		for {
			messages, err := read(ctx, query)
			result = append(result, messages...)
			if err != nil {
				return result, err
			}
			if len(messages) < query.Limit {
				break
			}
			last := messages[len(messages)-1]
			query.AfterKey = &store.ActivityKey{OccurredAt: last.OccurredAt, MessageID: last.MessageID}
		}
	}
	return result, nil
}

func (c *composer) readActivity(q store.BurstQuery) (activityWindow, error) {
	page, err := c.src.Activity.ListActivityBurstsContext(c.ctx, q)
	result := activityWindow{bursts: page.Bursts, noise: page.NoiseOmitted, more: page.NextKey != nil}
	if err != nil {
		return result, err
	}
	result.meetings, err = c.readMeetingMessages(c.ctx, page.Bursts, q, c.src.Activity.ListActivityMessagesContext)
	return result, err
}

func (c *composer) readColdActivity(ctx context.Context, q store.BurstQuery) (coldActivityWindow, error) {
	page, err := c.src.Cold.ListColdActivityBurstsContext(ctx, q)
	result := coldActivityWindow{BurstPage: page}
	if err != nil {
		return result, err
	}
	result.meetings, err = c.readMeetingMessages(ctx, page.Bursts, q, c.src.Cold.ListColdActivityMessagesContext)
	return result, err
}

type dayRead struct {
	activity           readResult[activityWindow]
	cold               readResult[coldActivityWindow]
	coldCount          readResult[int]
	calendar, upcoming readResult[store.CalendarOccurrencePage]
	calls              readResult[[]store.LogicalCall]
	loops              readResult[loops.Result]
	notes              readResult[[]store.DailyNoteEntry]
	inventory          readResult[Freshness]
}

func (c *composer) collectDay() dayRead {
	from, to := c.bounds()
	query := store.BurstQuery{FromDate: c.t.Window.From, ToDate: c.t.Window.To, Subject: c.subject, Filters: c.filters(), Limit: maxBursts, MergeCopies: c.req.IncludeCold}
	// Keep the initial snapshot reads inside half the request budget so the
	// subsequent display hydration still has time to use the same snapshot.
	collectionCtx, cancel := context.WithTimeout(c.ctx, requestTimeout/2)
	defer cancel()
	cc := *c
	cc.ctx = collectionCtx
	var r dayRead
	parallel(collectionCtx,
		func() {
			if c.src.Activity != nil {
				r.activity = readWithin(collectionCtx, requestTimeout, func(ctx context.Context) (activityWindow, error) {
					reader := cc
					reader.ctx = ctx
					return reader.readActivity(query)
				})
			}
		},
		func() {
			if c.src.Cold != nil {
				if c.req.IncludeCold {
					r.cold = readWithin(collectionCtx, requestTimeout, func(ctx context.Context) (coldActivityWindow, error) {
						return cc.readColdActivity(ctx, query)
					})
				} else {
					r.coldCount = readWithin(collectionCtx, requestTimeout, func(ctx context.Context) (int, error) { return c.src.Cold.CountColdActivityMessagesContext(ctx, query) })
				}
			}
		},
		func() {
			if c.src.Calendar != nil {
				r.calendar = cc.readCalendar(from, to)
			}
		},
		func() {
			if c.src.Calendar != nil {
				hours := 48
				if c.req.Brief {
					hours = 24
				}
				r.upcoming = cc.readCalendar(c.t.Now, c.t.Now.Add(time.Duration(hours)*time.Hour))
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Calls != nil {
				r.calls = readWithin(collectionCtx, requestTimeout, func(ctx context.Context) ([]store.LogicalCall, error) {
					dateBefore, err := addCivilDate(c.t.Window.To, 1)
					if err != nil {
						return nil, err
					}
					return cc.readCalls(ctx, store.LogicalCallQuery{
						After: from, Before: to, DateAfter: c.t.Window.From, DateBefore: dateBefore, Limit: 500,
					})
				})
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Loops != nil {
				r.loops = cc.collectLoops(c.subject)
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Notes != nil {
				r.notes = readWithin(collectionCtx, requestTimeout, func(ctx context.Context) ([]store.DailyNoteEntry, error) { return cc.readNotes(ctx, from, to) })
			}
		},
		func() {
			if c.src.Inventory != nil {
				r.inventory = readWithin(collectionCtx, inventoryTimeout, func(ctx context.Context) (Freshness, error) { return c.src.Inventory.Build(ctx, c.t.Now) })
			}
		},
	)
	c.recoverSnapshotAfterCollection(collectionCtx)
	return r
}
func (c *composer) readCalendar(from, to time.Time) readResult[store.CalendarOccurrencePage] {
	return readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.CalendarOccurrencePage, error) {
		result := store.CalendarOccurrencePage{}
		limit := 500
		query := store.CalendarOccurrenceQuery{From: from.In(c.loc), To: to.In(c.loc), Limit: &limit}
		for {
			page, err := c.src.Calendar.ListCalendarOccurrencesContext(ctx, query)
			result.Occurrences = append(result.Occurrences, page.Occurrences...)
			result.Unexpandable = append(result.Unexpandable, page.Unexpandable...)
			if err != nil {
				return result, err
			}
			if page.NextCursor == "" {
				return result, nil
			}
			query.Cursor = page.NextCursor
		}
	})
}
func (c *composer) readCalls(ctx context.Context, q store.LogicalCallQuery) ([]store.LogicalCall, error) {
	result := []store.LogicalCall{}
	for {
		calls, key, err := c.src.Calls.ListLogicalCallsContext(ctx, q)
		result = append(result, calls...)
		if err != nil {
			return result, err
		}
		if key == "" {
			return result, nil
		}
		q.AfterKey = key
	}
}
func (c *composer) readNotes(ctx context.Context, from, to time.Time) ([]store.DailyNoteEntry, error) {
	result := []store.DailyNoteEntry{}
	if !from.Before(to) {
		return result, nil
	}
	date := from.In(c.loc).Format(time.DateOnly)
	lastDate := to.Add(-time.Nanosecond).In(c.loc).Format(time.DateOnly)
	for date <= lastDate {
		for offset := 0; ; offset += 100 {
			notes, err := c.src.Notes.ListDailyNoteEntriesContext(ctx, date, 100, offset)
			result = append(result, notes...)
			if err != nil {
				return result, err
			}
			if len(notes) < 100 {
				break
			}
		}
		next, err := addCivilDate(date, 1)
		if err != nil {
			return result, fmt.Errorf("advance daily note date: %w", err)
		}
		date = next
	}
	return result, nil
}

func (c *composer) buildDay() {
	r := c.collectDay()
	if c.src.Activity == nil {
		c.unavailable("days")
	}
	c.gap("days", r.activity.err, len(r.activity.value.bursts) > 0)
	c.gap("cold", r.cold.err, len(r.cold.value.Bursts) > 0)
	c.gap("cold", r.coldCount.err, false)
	if c.src.Cold == nil {
		c.unavailable("cold")
	}
	c.t.Omitted.Noise = r.activity.value.noise
	c.t.Omitted.Cold = r.coldCount.value
	c.t.Omitted.Noise += r.cold.value.NoiseOmitted
	if r.activity.value.more || r.cold.value.NextKey != nil {
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "days", Code: "partial"})
	}
	if c.src.Inventory == nil {
		c.unavailable("freshness")
	} else if r.inventory.err == nil {
		c.t.Freshness = r.inventory.value
	} else {
		c.gap("freshness", r.inventory.err, false)
	}
	c.calendarGaps(r.calendar, "meetings")
	c.calendarGaps(r.upcoming, "upcoming")
	c.gap("calls", r.calls.err, len(r.calls.value) > 0)
	c.gap("notes", r.notes.err, len(r.notes.value) > 0)
	if c.req.Scope.Owner {
		if c.src.Calls == nil {
			c.unavailable("calls")
		}
		if c.src.Notes == nil {
			c.unavailable("notes")
		}
		if c.src.Loops == nil {
			c.unavailable("loops")
		}
	} else {
		c.ownerGaps()
	}
	bursts, mergeNoise := mergeBurstsWithNoise(r.activity.value.bursts, r.cold.value.Bursts, c.req.IncludeNoise)
	c.t.Omitted.Noise += mergeNoise
	if len(bursts) > maxBursts {
		bursts = bursts[:maxBursts]
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "days", Code: "partial"})
	}
	occurrences := append(slices.Clone(r.calendar.value.Occurrences), r.upcoming.value.Occurrences...)
	meetings := append(slices.Clone(r.activity.value.meetings), r.cold.value.meetings...)
	entries, upcoming := c.mergeEntries(bursts, meetings, occurrences, r.calls.value, r.notes.value)
	if c.req.With != "" {
		entries = c.filterSubject(entries)
		upcoming = c.filterSubject(upcoming)
	}
	c.t.Upcoming = upcoming
	c.sortAndContinue("upcoming", c.t.Upcoming, false)
	c.t.Upcoming = c.afterPosition("upcoming", c.t.Upcoming)
	c.groupDays(entries)
	if c.req.Scope.Owner && c.src.Loops != nil {
		c.addLoops(r.loops)
		if c.req.With != "" && c.t.OpenLoops != nil {
			c.t.OpenLoops.Items = c.filterSubject(c.t.OpenLoops.Items)
		}
	}
}
func (c *composer) calendarGaps(r readResult[store.CalendarOccurrencePage], section string) {
	if c.src.Calendar == nil {
		c.unavailable(section)
		return
	}
	c.gap(section, r.err, len(r.value.Occurrences) > 0)
	if len(r.value.Unexpandable) > 0 {
		items := []string{}
		for _, item := range r.value.Unexpandable {
			items = append(items, fmt.Sprintf("evt:%d", item.MessageID))
		}
		slices.Sort(items)
		items = slices.Compact(items)
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "meetings", Code: "partial", Items: items})
	}
}
func (c *composer) ownerGaps() {
	for _, section := range []string{"calls", "loops", "meeting-link-suggestions", "brief", "agenda", "cadence", "notes"} {
		c.t.Gaps = append(c.t.Gaps, Gap{Section: section, Code: "owner-only"})
	}
}
func (c *composer) addLoops(r readResult[loops.Result]) {
	c.gap("loops", r.err, false)
	if r.err != nil {
		return
	}
	c.t.OpenLoops = &OpenLoops{Items: []Entry{}, Truncated: r.value.Truncated, CoverageIncomplete: r.value.CoverageIncomplete, SkippedUnparseable: r.value.SkippedUnparseable}
	ids := []int64{}
	for _, loop := range r.value.Loops {
		ids = append(ids, loop.Anchor.ID)
	}
	display := c.displays(ids)
	personIDs := make([]int64, 0, len(r.value.Loops))
	for _, loop := range r.value.Loops {
		personIDs = append(personIDs, loop.Counterparty.PersonID)
	}
	c.loadTiers(personIDs)
	for _, loop := range r.value.Loops {
		d, ok := display[loop.Anchor.ID]
		if !ok {
			continue
		}
		p := c.tierPeople([]Person{{PersonID: loop.Counterparty.PersonID, ParticipantID: loop.Counterparty.ParticipantID, Name: loop.Counterparty.DisplayName}})[0]
		if p.Name == "" {
			p.Name = loop.Counterparty.Address
		}
		if !c.admitTier([]Person{p}) {
			continue
		}
		age := max(0, int(c.t.Now.Sub(loop.OwedSince).Hours()/24))
		ref := messageRef(loop.Anchor.ID)
		c.t.OpenLoops.Items = append(c.t.OpenLoops.Items, Entry{Kind: "loop", Key: ref, Ref: ref, AnchorRef: ref, Time: loop.Anchor.OccurredAt, HiddenText: d.HiddenText, Open: c.messageOpen(loop.Anchor.ID), Loop: &LoopEntry{Kind: loop.Kind, Age: strconv.Itoa(age) + "d", Person: p}})
	}
}
