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

func (c *composer) readActivity(q store.BurstQuery) (activityWindow, error) {
	var result activityWindow
	for {
		page, err := c.src.Activity.ListActivityBurstsContext(c.ctx, q)
		result.bursts = append(result.bursts, page.Bursts...)
		result.noise = page.NoiseOmitted
		result.more = page.NextKey != nil
		if err != nil {
			return result, err
		}
		if q.FromDate == "" || page.NextKey == nil {
			break
		}
		q.AfterKey = page.NextKey
	}
	seen := map[int64]bool{}
	from, to := c.bounds()
	for _, burst := range result.bursts {
		if burst.Channel != store.ChannelMeeting || seen[burst.ConversationID] || burst.ConversationID <= 0 {
			continue
		}
		seen[burst.ConversationID] = true
		mq := store.MessageQuery{ConversationID: burst.ConversationID, Limit: 500, Filters: q.Filters}
		if q.FromDate != "" {
			mq.After, mq.Before = &from, &to
		} else {
			mq.After, mq.Before = q.After, q.Before
		}
		for {
			messages, err := c.src.Activity.ListActivityMessagesContext(c.ctx, mq)
			if err != nil {
				return result, err
			}
			result.meetings = append(result.meetings, messages...)
			if len(messages) < mq.Limit {
				break
			}
			last := messages[len(messages)-1]
			mq.AfterKey = &store.ActivityKey{OccurredAt: last.OccurredAt, MessageID: last.MessageID}
		}
	}
	return result, nil
}

type dayRead struct {
	activity           readResult[activityWindow]
	cold               readResult[store.BurstPage]
	coldCount          readResult[int]
	calendar, upcoming readResult[store.CalendarOccurrencePage]
	calls              readResult[[]store.LogicalCall]
	loops              readResult[loops.Result]
	notes              readResult[[]store.DailyNoteEntry]
	inventory          readResult[Freshness]
}

func (c *composer) collectDay() dayRead {
	from, to := c.bounds()
	query := store.BurstQuery{FromDate: c.t.Window.From, ToDate: c.t.Window.To, Subject: c.subject, Filters: c.filters(), IncludeCold: c.req.IncludeCold && c.src.Cold != nil, Limit: maxBursts}
	var r dayRead
	parallel(c.ctx,
		func() {
			if c.src.Activity != nil {
				r.activity = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (activityWindow, error) {
					cc := *c
					cc.ctx = ctx
					return cc.readActivity(query)
				})
			}
		},
		func() {
			if c.src.Cold != nil {
				if c.req.IncludeCold && c.src.Activity == nil {
					r.cold = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.BurstPage, error) {
						var result store.BurstPage
						query := query
						for {
							page, err := c.src.Cold.ListColdActivityBurstsContext(ctx, query)
							result.Bursts = append(result.Bursts, page.Bursts...)
							result.NoiseOmitted, result.NextKey = page.NoiseOmitted, page.NextKey
							if err != nil || page.NextKey == nil {
								return result, err
							}
							query.AfterKey = page.NextKey
						}
					})
				} else if !c.req.IncludeCold {
					r.coldCount = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (int, error) { return c.src.Cold.CountColdActivityMessagesContext(ctx, query) })
				}
			}
		},
		func() {
			if c.src.Calendar != nil {
				r.calendar = c.readCalendar(from, to)
			}
		},
		func() {
			if c.src.Calendar != nil {
				hours := 48
				if c.req.Brief {
					hours = 24
				}
				r.upcoming = c.readCalendar(c.t.Now, c.t.Now.Add(time.Duration(hours)*time.Hour))
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Calls != nil {
				r.calls = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.LogicalCall, error) {
					return c.readCalls(ctx, store.LogicalCallQuery{After: from, Before: to, ParticipantIDs: c.participants, Limit: 500})
				})
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Loops != nil {
				r.loops = c.collectLoops(c.subject.PersonID)
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Notes != nil {
				r.notes = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.DailyNoteEntry, error) { return c.readNotes(ctx, from, to) })
			}
		},
		func() {
			if c.src.Inventory != nil {
				r.inventory = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (Freshness, error) { return c.src.Inventory.Build(ctx, c.t.Now) })
			}
		},
	)
	return r
}
func (c *composer) readCalendar(from, to time.Time) readResult[store.CalendarOccurrencePage] {
	return readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.CalendarOccurrencePage, error) {
		result := store.CalendarOccurrencePage{}
		limit := 500
		query := store.CalendarOccurrenceQuery{From: from, To: to, Limit: &limit}
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
	for date := from; date.Before(to); date = date.AddDate(0, 0, 1) {
		for offset := 0; ; offset += 100 {
			notes, err := c.src.Notes.ListDailyNoteEntriesContext(ctx, date.Format(time.DateOnly), 100, offset)
			result = append(result, notes...)
			if err != nil {
				return result, err
			}
			if len(notes) < 100 {
				break
			}
		}
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
	bursts := mergeBursts(r.activity.value.bursts, r.cold.value.Bursts)
	occurrences := append(slices.Clone(r.calendar.value.Occurrences), r.upcoming.value.Occurrences...)
	entries, upcoming := c.mergeEntries(bursts, r.activity.value.meetings, occurrences, r.calls.value, r.notes.value)
	c.t.Upcoming = c.filterSubject(upcoming)
	c.sortAndContinue("upcoming", c.t.Upcoming, false)
	c.t.Upcoming = c.afterPosition("upcoming", c.t.Upcoming)
	c.groupDays(c.filterSubject(entries))
	if c.req.Scope.Owner && c.src.Loops != nil {
		c.addLoops(r.loops)
		if c.t.OpenLoops != nil {
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
