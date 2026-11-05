package timeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
)

func (c *composer) resolvePerson() (store.PersonCandidate, error) {
	if c.src.People == nil {
		c.unavailable("person")
		return store.PersonCandidate{}, nil
	}
	kind, value := "name", strings.TrimSpace(c.req.With)
	if k, v, ok := strings.Cut(value, ":"); ok {
		kind, value = k, v
	} else if strings.HasPrefix(value, "+") {
		kind = "phone"
	}
	allowed := c.req.Scope.PersonIDs
	if c.req.Scope.Owner {
		allowed = nil
	} else if allowed == nil {
		allowed = []int64{}
	}
	r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.PersonCandidate, error) {
		return c.src.People.ResolvePersonReferenceContext(ctx, store.PersonRefKind(kind), value, allowed, 11)
	})
	if r.err != nil {
		if errors.Is(r.err, store.ErrInvalidActivityRequest) {
			return store.PersonCandidate{}, fmt.Errorf("%w: %w", ErrInvalidRequest, r.err)
		}
		c.gap("person", r.err, false)
		return store.PersonCandidate{}, nil // Source failures belong in gaps; only invalid requests return errors.
	}
	if len(r.value) == 0 {
		return store.PersonCandidate{}, ErrUnknownReference
	}
	if len(r.value) > 1 {
		for _, p := range r.value[:min(10, len(r.value))] {
			c.t.Candidates = append(c.t.Candidates, Candidate{PersonID: p.PersonID, PersonUID: p.PersonUID, ParticipantIDs: p.ParticipantIDs, DisplayName: p.DisplayName, Address: p.Address})
		}
		return store.PersonCandidate{}, &ErrAmbiguousReference{Candidates: c.t.Candidates}
	}
	return r.value[0], nil
}
func (c *composer) setSubject(p store.PersonCandidate) {
	c.subject = store.ActivitySubject{PersonID: p.PersonID}
	if p.PersonID == 0 {
		c.subject.ParticipantIDs = slices.Clone(p.ParticipantIDs)
	}
	c.participants = slices.Clone(p.ParticipantIDs)
}
func (c *composer) matchesSubject(e Entry) bool {
	if c.subject.PersonID == 0 && len(c.subject.ParticipantIDs) == 0 {
		return true
	}
	for _, p := range entryPeople(e) {
		if c.subject.PersonID > 0 && p.PersonID == c.subject.PersonID || slices.Contains(c.participants, p.ParticipantID) {
			return true
		}
	}
	return false
}
func (c *composer) filterSubject(entries []Entry) []Entry {
	out := entries[:0]
	for _, e := range entries {
		if c.matchesSubject(e) {
			out = append(out, e)
		}
	}
	return out
}
func (c *composer) buildRelationship(p store.PersonCandidate) {
	c.setSubject(p)
	if c.src.Activity == nil {
		c.unavailable("contact")
		c.unavailable("history")
	}
	if c.req.Scope.Owner && c.src.Calls == nil {
		c.unavailable("calls")
	}
	if !c.req.Scope.Owner {
		c.ownerGaps()
	}
	person := Person{PersonID: p.PersonID, Name: p.DisplayName, TierSource: "none"}
	if person.Name == "" {
		person.Name = p.Address
	}
	if len(p.ParticipantIDs) > 0 {
		person.ParticipantID = p.ParticipantIDs[0]
	}
	person = c.tierPeople([]Person{person})[0]
	ref := "unsaved"
	if p.PersonID > 0 {
		ref = "person:" + strconv.FormatInt(p.PersonID, 10)
	}
	c.t.Person = &PersonHeader{Person: person, Ref: ref, Channels: []string{}}
	if len(p.ParticipantIDs) == 0 {
		c.loadFreshness()
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "history", Code: "no-linked-contacts"})
		return
	}
	q := store.ContactQuery{Subject: c.subject, Filters: c.src.Filters, Now: c.t.Now, Location: c.loc}
	q.Filters.IncludeNoise = false
	var messages, calls readResult[store.ContactAggregate]
	var transcripts readResult[[]int64]
	var history readResult[activityWindow]
	var cold readResult[store.BurstPage]
	var coldCount readResult[int]
	var logicalCalls readResult[[]store.LogicalCall]
	var upcoming readResult[store.CalendarOccurrencePage]
	historyQuery := store.BurstQuery{Subject: c.subject, Filters: c.filters(), After: new(time.Time), Descending: true, IncludeCold: c.req.IncludeCold && c.src.Cold != nil, Limit: maxBursts}
	parallel(c.ctx,
		func() {
			if c.src.Activity != nil {
				messages = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.ContactAggregate, error) {
					return c.src.Activity.MessageContactAggregateContext(ctx, q)
				})
				transcripts = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]int64, error) {
					ids, more, err := c.src.Activity.AttendedTranscriptIDsContext(ctx, q, 2000)
					if more && err == nil {
						err = errors.New("attended meeting limit reached")
					}
					return ids, err
				})
				history = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (activityWindow, error) {
					cc := *c
					cc.ctx = ctx
					return cc.readActivity(historyQuery)
				})
			}
		},
		func() {
			if c.src.Cold != nil {
				if c.req.IncludeCold && c.src.Activity == nil {
					cold = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.BurstPage, error) {
						return c.src.Cold.ListColdActivityBurstsContext(ctx, historyQuery)
					})
				} else if !c.req.IncludeCold {
					coldCount = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (int, error) {
						return c.src.Cold.CountColdActivityMessagesContext(ctx, historyQuery)
					})
				}
			}
		},
		func() {
			if c.req.Scope.Owner && c.src.Calls != nil {
				calls = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.ContactAggregate, error) {
					return c.src.Calls.CallContactAggregateContext(ctx, q)
				})
				logicalCalls = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.LogicalCall, error) {
					return c.readCalls(ctx, store.LogicalCallQuery{After: time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC), Before: c.t.Now.Add(time.Nanosecond), ParticipantIDs: c.participants, Limit: 500})
				})
			}
		},
		func() {
			if c.src.Calendar != nil {
				upcoming = c.readCalendar(c.t.Now, c.t.Now.AddDate(0, 0, 30))
			}
		},
		func() { c.loadFreshness() },
	)
	c.gap("contact", messages.err, false)
	c.gap("contact", calls.err, false)
	c.gap("meetings", transcripts.err, len(transcripts.value) > 0)
	c.gap("history", history.err, len(history.value.bursts) > 0)
	c.gap("cold", cold.err, len(cold.value.Bursts) > 0)
	c.gap("cold", coldCount.err, false)
	if c.src.Cold == nil {
		c.unavailable("cold")
	}
	c.t.Omitted.Noise = history.value.noise + cold.value.NoiseOmitted
	c.t.Omitted.Cold = coldCount.value
	c.gap("calls", logicalCalls.err, false)
	c.mergeContact(messages.value, false)
	if c.req.Scope.Owner {
		c.mergeContact(calls.value, true)
	} else {
		c.ownerGaps()
	}
	occurrences := slices.Clone(upcoming.value.Occurrences)
	if c.src.Meetings != nil && len(transcripts.value) > 0 {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.MeetingIdentityResult, error) {
			return c.src.Meetings.ResolveMeetingIdentitiesContext(ctx, store.MeetingIdentityRequest{TranscriptIDs: transcripts.value})
		})
		c.gap("meetings", r.err, false)
		displays := c.displays(transcripts.value)
		seen := map[string]bool{}
		for _, id := range transcripts.value {
			info := r.value.ByTranscript[id]
			key := info.Key
			if key == "" {
				key = messageRef(id)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			at := displays[id].OccurredAt
			for _, ref := range info.ExactOccurrenceRefs {
				if c.src.Calendar != nil {
					d, e := c.src.Calendar.GetCalendarOccurrenceContext(c.ctx, ref)
					if e == nil {
						o := d.CalendarOccurrence
						occurrences = append(occurrences, o)
						stamp := o.Start
						if o.AllDay {
							stamp, _ = time.ParseInLocation(time.DateOnly, o.StartDate, c.loc)
						}
						at = &stamp
						break
					}
					c.gap("meetings", e, false)
				}
			}
			if at != nil && !at.After(c.t.Now) {
				c.mergeContact(store.ContactAggregate{FirstAt: at, LastAt: at, Count: 1, Channels: []store.ActivityChannel{store.ChannelMeeting}, Monthly: monthlyAt(*at, c.t.Now, c.loc)}, false)
			}
		}
	}
	c.calendarGaps(upcoming, "upcoming")
	bursts := mergeBursts(history.value.bursts, cold.value.Bursts)
	slices.Reverse(bursts)
	entries, future := c.mergeEntries(bursts, history.value.meetings, occurrences, logicalCalls.value, nil)
	c.t.Upcoming = c.filterSubject(future)
	c.sortAndContinue("upcoming", c.t.Upcoming, false)
	c.t.Upcoming = c.afterPosition("upcoming", c.t.Upcoming)
	entries = c.filterSubject(entries)
	entries = slices.DeleteFunc(entries, func(e Entry) bool { return e.Time.After(c.t.Now) })
	c.sortAndContinue("history", entries, true)
	entries = c.afterPosition("history", entries)
	if len(entries) > 20 {
		c.more["history"] = true
		entries = entries[:20]
	}
	c.t.History = entries
	if history.value.more || cold.value.NextKey != nil {
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "history", Code: "partial"})
	}
	if !c.req.Scope.Owner {
		return
	}
	if p.PersonID == 0 {
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "loops", Code: "no-person"})
		return
	}
	c.relationshipExtras(p.PersonID)
}
func monthlyAt(at, now time.Time, loc *time.Location) [12]int {
	var monthly [12]int
	at, now = at.In(loc), now.In(loc)
	index := 11 - ((now.Year()-at.Year())*12 + int(now.Month()-at.Month()))
	if index >= 0 && index < 12 {
		monthly[index] = 1
	}
	return monthly
}
func mergeTime(dst **time.Time, src *time.Time, first bool) {
	if src != nil && (*dst == nil || first && src.Before(**dst) || !first && src.After(**dst)) {
		at := src.UTC()
		*dst = &at
	}
}
func (c *composer) mergeContact(a store.ContactAggregate, call bool) {
	h := c.t.Person
	mergeTime(&h.FirstAt, a.FirstAt, true)
	mergeTime(&h.LastAt, a.LastAt, false)
	mergeTime(&h.LastInboundAt, a.LastInboundAt, false)
	mergeTime(&h.LastOutboundAt, a.LastOutboundAt, false)
	h.InteractionCount += a.Count
	if call {
		if a.Count > 0 {
			h.Channels = append(h.Channels, "call")
		}
	} else {
		for _, ch := range a.Channels {
			h.Channels = append(h.Channels, string(ch))
		}
		h.InitiatedByContact += a.InitiatedByContact
		h.InitiatedByOwner += a.InitiatedByOwner
	}
	slices.Sort(h.Channels)
	h.Channels = slices.Compact(h.Channels)
	for i, n := range a.Monthly {
		h.Monthly[i] += n
	}
}
func (c *composer) loadFreshness() {
	if c.src.Inventory == nil {
		c.unavailable("freshness")
		return
	}
	r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (Freshness, error) { return c.src.Inventory.Build(ctx, c.t.Now) })
	if r.err != nil {
		c.gap("freshness", r.err, false)
	} else {
		c.t.Freshness = r.value
	}
}
func (c *composer) relationshipExtras(person int64) {
	if c.src.Activity == nil {
		c.unavailable("cadence")
	} else {
		status, due, err := c.src.Activity.CadenceForPersonContext(c.ctx, person, c.t.Person.LastAt, c.t.Now)
		c.gap("cadence", err, false)
		c.t.Person.CadenceStatus, c.t.Person.DueAt = status, due
	}
	if c.src.Loops == nil {
		c.unavailable("loops")
	} else {
		c.addLoops(c.collectLoops(person))
	}
	if c.src.Briefs == nil {
		c.unavailable("brief")
	} else {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (*store.PersonBrief, error) {
			return c.src.Briefs.GetPersonBriefContext(ctx, person, 0)
		})
		c.gap("brief", r.err, false)
		if r.value != nil {
			c.t.Person.Brief = &UntrustedText{Text: r.value.RenderedText}
		}
	}
	if c.src.Agenda == nil {
		c.unavailable("agenda")
	} else {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (personagenda.Result, error) { return c.src.Agenda.List(ctx, person) })
		c.gap("agenda", r.err, false)
		for _, item := range r.value.Items {
			c.t.Person.Agenda = append(c.t.Person.Agenda, UntrustedText{Text: item.Title})
		}
	}
}
