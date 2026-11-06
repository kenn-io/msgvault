package timeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
)

func (c *composer) resolvePerson() (store.PersonCandidate, error) {
	r := readWithin(c.ctx, c.personCandidates)
	switch {
	case errors.Is(r.err, store.ErrInvalidActivityRequest):
		return store.PersonCandidate{}, fmt.Errorf("%w: %w", ErrInvalidRequest, r.err)
	case r.err != nil && c.req.Mode == ModeRelationship:
		// A relationship reports its unavailable subject; a person filter fails closed.
		c.gap("person", r.err, false)
		return store.PersonCandidate{}, nil
	case r.err != nil:
		return store.PersonCandidate{}, fmt.Errorf("resolve timeline person: %w", r.err)
	case len(r.value) == 0:
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

// personCandidates refuses the owner's own account address: it is no relationship.
func (c *composer) personCandidates(ctx context.Context) ([]store.PersonCandidate, error) {
	kind, value := personReference(c.req.With)
	if kind == store.PersonRefKind(store.ContactAddressEmail) {
		owner, err := c.src.Meetings.IsAccountIdentityAddressContext(ctx, strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("check timeline owner address: %w", err)
		}
		if owner {
			return nil, fmt.Errorf("%w: owner account address", store.ErrInvalidActivityRequest)
		}
	}
	return c.src.People.ResolvePersonReferenceContext(ctx, kind, value, nil, 11)
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
	for _, p := range EntryPeople(e) {
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
	var transcripts readResult[store.AttendedTranscripts]
	var history readResult[activityWindow]
	var cold readResult[store.BurstPage]
	var coldCount readResult[int]
	var logicalCalls readResult[[]store.LogicalCall]
	var upcoming readResult[store.CalendarOccurrencePage]
	historyQuery := store.BurstQuery{Subject: c.subject, Filters: c.filters(), After: new(time.Time), Until: &c.t.Now, Descending: true, Limit: maxBursts}
	c.parallel(
		func() {
			messages = readWithin(c.ctx, func(ctx context.Context) (store.ContactAggregate, error) {
				return c.src.Activity.MessageContactAggregateContext(ctx, q)
			})
			transcripts = readWithin(c.ctx, func(ctx context.Context) (store.AttendedTranscripts, error) {
				ids, more, err := c.src.Activity.AttendedTranscriptIDsContext(ctx, q, 2000)
				if more && err == nil {
					err = errors.New("attended meeting limit reached")
				}
				return ids, err
			})
			history = readWithin(c.ctx, func(ctx context.Context) (activityWindow, error) {
				cc := *c
				cc.ctx = ctx
				return cc.readActivity(historyQuery)
			})
		},
		func() {
			if c.req.IncludeCold {
				cold = readWithin(c.ctx, func(ctx context.Context) (store.BurstPage, error) {
					return c.src.Cold.ListColdActivityBurstsContext(ctx, historyQuery)
				})
			} else {
				coldCount = readWithin(c.ctx, func(ctx context.Context) (int, error) {
					return c.src.Cold.CountColdActivityMessagesContext(ctx, historyQuery)
				})
			}
		},
		func() {
			calls = readWithin(c.ctx, func(ctx context.Context) (store.ContactAggregate, error) {
				return c.src.Calls.CallContactAggregateContext(ctx, store.CallContactQuery{Subject: q.Subject, Now: q.Now, Location: q.Location})
			})
			logicalCalls = readWithin(c.ctx, func(ctx context.Context) ([]store.LogicalCall, error) {
				return c.readCalls(ctx, store.LogicalCallQuery{After: time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC), Before: c.t.Now.Add(time.Millisecond), ParticipantIDs: c.participants, Limit: 500})
			})
		},
		func() {
			upcoming = c.readCalendar(c.t.Now, c.t.Now.AddDate(0, 0, 30))
		},
		func() { c.loadFreshness() },
	)
	c.gap("contact", messages.err, false)
	c.gap("contact", calls.err, false)
	c.gap("meetings", transcripts.err, len(transcripts.value.IDs) > 0)
	c.gap("history", history.err, len(history.value.bursts) > 0)
	c.gap("cold", cold.err, len(cold.value.Bursts) > 0)
	c.gap("cold", coldCount.err, false)
	c.t.Omitted.Noise = history.value.noise + cold.value.NoiseOmitted
	c.t.Omitted.Cold = coldCount.value
	c.gap("calls", logicalCalls.err, false)
	c.mergeContact(messages.value, false)
	c.mergeContact(calls.value, true)
	c.identities = transcripts.value.Identities
	c.occurrences = transcripts.value.Occurrences
	occurrences := slices.Clone(upcoming.value.Occurrences)
	if len(transcripts.value.IDs) > 0 {
		displays := c.displays(transcripts.value.IDs)
		seen := map[string]bool{}
		for _, id := range transcripts.value.IDs {
			info := c.identities.ByTranscript[id]
			key := info.Key
			if key == "" {
				key = messageRef(id)
			}
			if seen[key] {
				continue
			}
			resolved := len(info.ExactOccurrenceRefs) > 0
			for _, ref := range info.ExactOccurrenceRefs {
				if !transcripts.value.MatchedOccurrences[ref] {
					continue
				}
				o, ok := c.occurrences[ref]
				if !ok {
					continue
				}
				if seen[o.Ref] {
					continue
				}
				seen[o.Ref] = true
				occurrences = append(occurrences, o)
				at := o.Start
				if o.AllDay {
					at, _, _ = calrecur.CivilDateBoundary(o.StartDate, c.loc)
				}
				if !at.After(c.t.Now) {
					c.mergeContact(store.ContactAggregate{FirstAt: &at, LastAt: &at, Count: 1, Channels: []store.ActivityChannel{store.ChannelMeeting}, Monthly: monthlyAt(at, c.t.Now, c.loc)}, false)
				}
			}
			if !resolved {
				seen[key] = true
				at := displays[id].OccurredAt
				if at != nil && !at.After(c.t.Now) {
					c.mergeContact(store.ContactAggregate{FirstAt: at, LastAt: at, Count: 1, Channels: []store.ActivityChannel{store.ChannelMeeting}, Monthly: monthlyAt(*at, c.t.Now, c.loc)}, false)
				}
			}
		}
	}
	c.calendarGaps(upcoming, "upcoming")
	bursts := mergeBursts(history.value.bursts, cold.value.Bursts)
	slices.Reverse(bursts)
	entries, future := c.mergeEntries(bursts, history.value.meetings, occurrences, logicalCalls.value)
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
	c.addLoops(c.collectLoops(p.PersonID))
	if p.PersonID == 0 {
		return
	}
	c.relationshipExtras(p.PersonID)
}
func monthlyAt(at, now time.Time, loc *time.Location) [12]int {
	var monthly [12]int
	at, now = at.In(loc), now.In(loc)
	index := store.ActivityMonthIndex(at, now)
	if index >= 0 && index < 12 {
		monthly[index] = 1
	}
	return monthly
}
func (c *composer) mergeContact(a store.ContactAggregate, call bool) {
	h := c.t.Person
	store.MergeActivityTime(&h.FirstAt, a.FirstAt, true)
	store.MergeActivityTime(&h.LastAt, a.LastAt, false)
	store.MergeActivityTime(&h.LastInboundAt, a.LastInboundAt, false)
	store.MergeActivityTime(&h.LastOutboundAt, a.LastOutboundAt, false)
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
	r := readWithin(c.ctx, func(ctx context.Context) (Freshness, error) { return c.readFreshness(ctx) })
	if r.err != nil {
		c.gap("freshness", r.err, false)
	} else {
		c.t.Freshness = r.value
	}
}
func (c *composer) relationshipExtras(person int64) {
	status, due, err := c.src.Activity.CadenceForPersonContext(c.ctx, person, c.t.Person.LastAt, c.t.Now)
	c.gap("cadence", err, false)
	c.t.Person.CadenceStatus, c.t.Person.DueAt = status, due

	r := readWithin(c.ctx, func(ctx context.Context) (*store.PersonBrief, error) {
		return c.src.Briefs.GetPersonBriefContext(ctx, person, 0)
	})
	c.gap("brief", r.err, false)
	if r.value != nil {
		c.t.Person.Brief = &UntrustedText{Text: r.value.RenderedText}
	}

	if c.src.Agenda == nil {
		c.unavailable("agenda")
	} else {
		r := readWithin(c.ctx, func(ctx context.Context) (personagenda.Result, error) { return c.src.Agenda.List(ctx, person) })
		c.gap("agenda", r.err, false)
		for _, item := range r.value.Items {
			c.t.Person.Agenda = append(c.t.Person.Agenda, UntrustedText{Text: item.Title})
		}
	}
}
