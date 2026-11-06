package timeline

import (
	"cmp"
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
	page, err := c.src.Activity.ListActivityBurstsContext(c.ctx, q)
	result := activityWindow{bursts: page.Bursts, noise: page.NoiseOmitted, more: page.NextKey != nil}
	if err != nil {
		return result, err
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
	inventory          readResult[Freshness]
}

func (c *composer) collectDay() dayRead {
	from, to := c.bounds()
	query := store.BurstQuery{FromDate: c.t.Window.From, ToDate: c.t.Window.To, Filters: c.filters(), Limit: maxBursts, Descending: true}
	var r dayRead
	c.parallel(
		func() {
			r.activity = readWithin(c.ctx, func(ctx context.Context) (activityWindow, error) {
				cc := *c
				cc.ctx = ctx
				return cc.readActivity(query)
			})
		},
		func() {
			if c.req.IncludeCold {
				r.cold = readWithin(c.ctx, func(ctx context.Context) (store.BurstPage, error) {
					return c.src.Cold.ListColdActivityBurstsContext(ctx, query)
				})
			} else {
				r.coldCount = readWithin(c.ctx, func(ctx context.Context) (int, error) { return c.src.Cold.CountColdActivityMessagesContext(ctx, query) })
			}
		},
		func() {
			r.calendar = c.readCalendar(from, to)
		},
		func() {
			today := c.t.Now.In(c.loc).Format(time.DateOnly)
			if c.t.Window.From > today || c.t.Window.To < today {
				return
			}
			hours := 48
			if c.req.Brief {
				hours = 24
			}
			r.upcoming = c.readCalendar(c.t.Now, c.t.Now.Add(time.Duration(hours)*time.Hour))
		},
		func() {
			if c.req.Scope.Owner {
				r.calls = readWithin(c.ctx, func(ctx context.Context) ([]store.LogicalCall, error) {
					return c.readCalls(ctx, store.LogicalCallQuery{After: from, Before: to, Limit: 500})
				})
			}
		},
		func() {
			if c.req.Scope.Owner {
				r.loops = c.collectLoops(0)
			}
		},
		func() {
			r.inventory = readWithin(c.ctx, func(ctx context.Context) (Freshness, error) { return c.readFreshness(ctx) })
		},
	)
	return r
}
func (c *composer) readCalendar(from, to time.Time) readResult[store.CalendarOccurrencePage] {
	return readWithin(c.ctx, func(ctx context.Context) (store.CalendarOccurrencePage, error) {
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
func (c *composer) buildDay() {
	r := c.collectDay()

	c.gap("days", r.activity.err, len(r.activity.value.bursts) > 0)
	c.gap("cold", r.cold.err, len(r.cold.value.Bursts) > 0)
	c.gap("cold", r.coldCount.err, false)
	c.t.Omitted.Noise = r.activity.value.noise
	c.t.Omitted.Cold = r.coldCount.value
	c.t.Omitted.Noise += r.cold.value.NoiseOmitted
	if r.activity.value.more || r.cold.value.NextKey != nil {
		oldest := ""
		for _, bursts := range [][]store.ActivityBurst{r.activity.value.bursts, r.cold.value.Bursts} {
			for _, burst := range bursts {
				if oldest == "" || burst.LocalDate < oldest {
					oldest = burst.LocalDate
				}
			}
		}
		gap := Gap{Section: "days", Code: "partial"}
		if oldest != "" {
			gap.Items = []string{oldest}
		}
		c.t.Gaps = append(c.t.Gaps, gap)
	}
	if r.inventory.err == nil {
		c.t.Freshness = r.inventory.value
	} else {
		c.gap("freshness", r.inventory.err, false)
	}
	c.calendarGaps(r.calendar, "meetings")
	c.calendarGaps(r.upcoming, "upcoming")
	c.gap("calls", r.calls.err, len(r.calls.value) > 0)
	if !c.req.Scope.Owner {
		c.ownerGaps()
	}
	bursts := mergeBursts(r.activity.value.bursts, r.cold.value.Bursts)
	occurrences := append(slices.Clone(r.calendar.value.Occurrences), r.upcoming.value.Occurrences...)
	entries, upcoming := c.mergeEntries(bursts, r.activity.value.meetings, occurrences, r.calls.value)
	c.t.Upcoming = upcoming
	c.sortAndContinue("upcoming", c.t.Upcoming, false)
	c.t.Upcoming = c.afterPosition("upcoming", c.t.Upcoming)
	c.groupDays(entries)
	if c.req.Scope.Owner {
		c.addLoops(r.loops)
	}
}
func (c *composer) calendarGaps(r readResult[store.CalendarOccurrencePage], section string) {
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
	for _, section := range []string{"calls", "loops", "meeting-link-suggestions", "brief", "agenda", "cadence"} {
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
		p := c.tierPeople([]Person{{PersonID: loop.Counterparty.PersonID, ParticipantID: loop.Counterparty.ParticipantID, Name: cmp.Or(loop.Counterparty.DisplayName, loop.Counterparty.Address)}})[0]
		if !c.admitTier([]Person{p}) {
			continue
		}
		age := max(0, int(c.t.Now.Sub(loop.OwedSince).Hours()/24))
		ref := messageRef(loop.Anchor.ID)
		c.t.OpenLoops.Items = append(c.t.OpenLoops.Items, Entry{Kind: "loop", Key: ref, Ref: ref, AnchorRef: ref, Time: loop.Anchor.OccurredAt, HiddenText: d.HiddenText, Open: c.messageOpen(loop.Anchor.ID), Loop: &LoopEntry{Kind: loop.Kind, Age: strconv.Itoa(age) + "d", Person: p}})
	}
	if limit := c.loopLimit(); len(c.t.OpenLoops.Items) > limit {
		c.t.OpenLoops.Items, c.t.OpenLoops.Truncated = c.t.OpenLoops.Items[:limit], true
	}
}
