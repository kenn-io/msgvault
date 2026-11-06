package timeline

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/store"
)

const (
	entryKindBurst   = "burst"
	entryKindMessage = "message"
)

func mergeBursts(full, cold []store.ActivityBurst) []store.ActivityBurst {
	out := slices.Clone(full)
	keys := map[string]bool{}
	ids := map[int64]bool{}
	for _, b := range full {
		keys[b.Key] = true
		for _, id := range b.LatestMessageIDs {
			ids[id] = true
		}
	}
	for _, b := range cold {
		if keys[b.Key] || ids[b.AnchorMessageID] {
			continue
		}
		out = append(out, b)
		keys[b.Key] = true
		for _, id := range b.LatestMessageIDs {
			ids[id] = true
		}
	}
	// Day-mode selection must use the same (FirstAt, Key) order as both
	// readers before applying the shared cap; rendering ranks entries later.
	slices.SortFunc(out, func(a, b store.ActivityBurst) int {
		if n := a.FirstAt.Compare(b.FirstAt); n != 0 {
			return n
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}
func (c *composer) displays(ids []int64) map[int64]store.MessageDisplay {
	r := c.readDisplays(ids)
	c.gap("text", r.err, false)
	return r.value
}

func (c *composer) readDisplays(ids []int64) readResult[map[int64]store.MessageDisplay] {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	r := readWithin(c.ctx, func(ctx context.Context) (map[int64]store.MessageDisplay, error) {
		return c.src.Activity.MessageDisplayContext(ctx, ids)
	})
	if !c.req.Scope.Owner {
		for id, display := range r.value {
			if display.MessageType == "call_record" {
				delete(r.value, id)
			}
		}
	}
	return r
}

func (c *composer) mergeEntries(bursts []store.ActivityBurst, messages []store.ActivityMessage, occurrences []store.CalendarOccurrence, calls []store.LogicalCall) ([]Entry, []Entry) {
	ids := []int64{}
	calendarIDs := []int64{}
	refs := []string{}
	transcriptIDs := []int64{}
	for _, b := range bursts {
		ids = append(ids, b.AnchorMessageID)
		ids = append(ids, b.LatestMessageIDs...)
	}
	for _, m := range messages {
		ids = append(ids, m.MessageID)
		transcriptIDs = append(transcriptIDs, m.MessageID)
	}
	for _, o := range occurrences {
		refs = append(refs, o.Ref)
		calendarIDs = append(calendarIDs, o.MessageID)
	}
	for _, call := range calls {
		ids = append(ids, call.MessageID)
	}
	identity := c.identities
	refs = slices.DeleteFunc(refs, func(ref string) bool { _, ok := identity.ByOccurrence[ref]; return ok })
	transcriptIDs = slices.DeleteFunc(transcriptIDs, func(id int64) bool { _, ok := identity.ByTranscript[id]; return ok })
	if identity.ByOccurrence == nil {
		identity.ByOccurrence = map[string]store.MeetingIdentityInfo{}
	}
	if identity.ByTranscript == nil {
		identity.ByTranscript = map[int64]store.MeetingIdentityInfo{}
	}
	if len(refs) > 0 || len(transcriptIDs) > 0 {
		r := readWithin(c.ctx, func(ctx context.Context) (store.MeetingIdentityResult, error) {
			return c.src.Meetings.ResolveMeetingIdentitiesContext(ctx, store.MeetingIdentityRequest{OccurrenceRefs: slices.Compact(slices.Sorted(slices.Values(refs))), TranscriptIDs: slices.Compact(slices.Sorted(slices.Values(transcriptIDs))), IncludeSuggestions: c.req.Scope.Owner})
		})
		c.gap("meetings", r.err, false)
		maps.Copy(identity.ByOccurrence, r.value.ByOccurrence)
		maps.Copy(identity.ByTranscript, r.value.ByTranscript)
	}

	// Resolve the provider/copy representative before using time, text or people.
	canonical := make([]store.CalendarOccurrence, 0, len(occurrences))
	for _, o := range occurrences {
		if resolved, ok := c.canonicalOccurrence(o, identity.ByOccurrence[o.Ref]); ok {
			canonical = append(canonical, resolved)
			calendarIDs = append(calendarIDs, resolved.MessageID)
		}
	}
	occurrences = canonical
	for _, info := range identity.ByOccurrence {
		ids = append(ids, info.LinkedTranscriptIDs...)
	}
	for id, info := range identity.ByTranscript {
		ids = append(ids, id)
		ids = append(ids, info.LinkedTranscriptIDs...)
	}
	display := c.displays(ids)
	calendarIDs = slices.Compact(slices.Sorted(slices.Values(calendarIDs)))
	calendarDisplay := readWithin(c.ctx, func(ctx context.Context) (map[int64]store.MessageDisplay, error) {
		return c.src.Calendar.CalendarMessageDisplayContext(ctx, calendarIDs)
	})
	c.gap("text", calendarDisplay.err, false)

	actionIDs := []int64{}
	for id := range identity.ByTranscript {
		if _, ok := display[id]; ok {
			actionIDs = append(actionIDs, id)
		}
	}
	actionCounts := readWithin(c.ctx, func(ctx context.Context) (map[int64]int, error) {
		return c.src.Meetings.MeetingActionItemCountsContext(ctx, slices.Compact(slices.Sorted(slices.Values(actionIDs))))
	})
	actions := actionCounts.value
	c.gap("meetings", actionCounts.err, false)

	personIDs := []int64{}
	for _, b := range bursts {
		for _, p := range counterpartPeople(b.Counterparts) {
			personIDs = append(personIDs, p.PersonID)
		}
	}
	for _, m := range messages {
		for _, p := range counterpartPeople(m.Counterparts) {
			personIDs = append(personIDs, p.PersonID)
		}
	}
	for _, call := range calls {
		for _, p := range call.Participants {
			personIDs = append(personIDs, p.PersonID)
		}
	}
	c.loadTiers(personIDs)
	entries := []Entry{}
	upcoming := []Entry{}
	for _, b := range bursts {
		if b.Channel == store.ChannelMeeting {
			continue
		}
		d, ok := display[b.AnchorMessageID]
		if !ok {
			continue
		}
		people := c.tierPeople(counterpartPeople(b.Counterparts))
		if !c.admitTier(people) {
			continue
		}
		hidden := false
		for _, id := range b.LatestMessageIDs {
			hidden = hidden || display[id].HiddenText
		}
		snippet := displaySnippet(d)
		if hidden {
			snippet = ""
		}
		e := Entry{Kind: entryKindBurst, Key: b.Key, Ref: fmt.Sprintf("conv:%d", b.ConversationID), AnchorRef: b.Key, Time: b.LastAt, Open: append(c.open("list_messages", map[string]any{"conversation_id": b.ConversationID}), c.messageOpen(b.AnchorMessageID)...), HiddenText: hidden, Burst: &BurstEntry{Channel: string(b.Channel), People: people, PeopleCount: len(people), Inbound: b.Inbound, Outbound: b.Outbound, FirstAt: b.FirstAt, LastAt: b.LastAt, Snippet: UntrustedText{Text: snippet}, LatestMessageIDs: b.LatestMessageIDs, Copies: b.CopyCounts}}
		c.entryDates[e.Key] = b.LocalDate
		entries = append(entries, e)
	}
	seen := map[string]bool{}
	transcriptPeople := map[int64][]Person{}
	for _, m := range messages {
		transcriptPeople[m.MessageID] = c.tierPeople(counterpartPeople(m.Counterparts))
	}
	for _, o := range occurrences {
		info := identity.ByOccurrence[o.Ref]
		key := info.Key
		if key == "" {
			key = "occ:" + o.Ref
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		attendees := c.occurrencePeople(o)
		people := slices.Clone(attendees)
		linked := []int64{}
		d, present := calendarDisplay.value[o.MessageID]
		hidden := !present || d.HiddenText
		count := 0
		for _, id := range info.LinkedTranscriptIDs {
			if d, ok := display[id]; ok {
				linked = append(linked, id)
				hidden = hidden || d.HiddenText
				count += actions[id]
				for _, p := range transcriptPeople[id] {
					if !slices.ContainsFunc(people, func(existing Person) bool {
						return p.PersonID > 0 && p.PersonID == existing.PersonID || p.ParticipantID > 0 && p.ParticipantID == existing.ParticipantID
					}) {
						people = append(people, p)
					}
				}
			}
		}
		if !c.admitTier(people) {
			continue
		}
		title := o.Subject
		if hidden {
			title = ""
		}
		at := o.Start
		if o.AllDay {
			at, _, _ = calrecur.CivilDateBoundary(o.StartDate, c.loc)
		}
		if at.IsZero() {
			continue
		}
		e := Entry{Kind: "meeting", Key: key, Ref: o.Ref, AnchorRef: o.Ref, Time: at, HiddenText: hidden, Open: c.open("get_calendar_event", map[string]any{"ref": o.Ref}), Meeting: &MeetingEntry{Status: o.Status, DeletedFromSource: o.DeletedFromSource, Title: UntrustedText{Text: title}, Start: at, End: o.End, AllDay: o.AllDay, Attendees: attendees, AttendeeCount: len(attendees), People: people, PeopleCount: len(people), Attended: len(linked) > 0, LinkedTranscriptIDs: linked, ActionItemCount: count}}
		if len(linked) > 0 {
			e.Open = append(e.Open, c.open("get_meeting_context", map[string]any{"message_ids": linked})...)
		}
		if len(e.Open) == 0 {
			e.Open = c.messageOpen(o.MessageID)
		}
		if c.req.Scope.Owner {
			for id, suggestion := range identity.ByTranscript {
				if _, visible := display[id]; visible && slices.Contains(suggestion.SuggestedOccurrenceRefs, o.Ref) {
					e.Meeting.PossiblyRelated = append(e.Meeting.PossiblyRelated, messageRef(id))
				}
			}
			for _, call := range calls {
				if !call.OccurredAt.Before(at) && call.OccurredAt.Before(o.End) {
					e.Meeting.PossiblyRelated = append(e.Meeting.PossiblyRelated, messageRef(call.MessageID))
				}
			}
		}
		e.Meeting.PossiblyRelated = slices.Compact(slices.Sorted(slices.Values(e.Meeting.PossiblyRelated)))
		date := max(at.In(c.loc).Format(time.DateOnly), c.t.Window.From)
		if o.AllDay {
			date = max(o.StartDate, c.t.Window.From)
		}
		c.entryDates[e.Key] = date
		if c.req.Mode != ModeDay || date >= c.t.Window.From && date <= c.t.Window.To {
			entries = append(entries, e)
		}
		until := c.t.Now.Add(48 * time.Hour)
		if c.req.Mode == ModeRelationship {
			until = c.t.Now.AddDate(0, 0, 30)
		}
		if c.req.Brief {
			until = c.t.Now.Add(24 * time.Hour)
		}
		if o.Status != "cancelled" && !o.DeletedFromSource && !at.Before(c.t.Now) && at.Before(until) {
			upcoming = append(upcoming, e)
		}
	}
	for _, m := range messages {
		info, ok := identity.ByTranscript[m.MessageID]
		if !ok || len(info.ExactOccurrenceRefs) > 0 {
			continue
		}
		d, ok := display[m.MessageID]
		if !ok {
			continue
		}
		people := c.tierPeople(counterpartPeople(m.Counterparts))
		if !c.admitTier(people) {
			continue
		}
		title := d.Subject
		if d.HiddenText {
			title = ""
		}
		ref := messageRef(m.MessageID)
		e := Entry{Kind: "transcript", Key: ref, Ref: ref, AnchorRef: ref, Time: m.OccurredAt, HiddenText: d.HiddenText, Open: c.open("get_meeting_context", map[string]any{"message_ids": []int64{m.MessageID}}), Meeting: &MeetingEntry{Title: UntrustedText{Text: title}, Time: m.OccurredAt, People: people, PeopleCount: len(people), ActionItemCount: actions[m.MessageID]}}
		if len(e.Open) == 0 {
			e.Open = c.messageOpen(m.MessageID)
		}
		if c.req.Scope.Owner {
			e.Meeting.PossiblyRelated = info.SuggestedOccurrenceRefs
		}
		c.entryDates[e.Key] = m.LocalDate
		entries = append(entries, e)
	}
	for _, call := range calls {
		d, ok := display[call.MessageID]
		if !ok {
			continue
		}
		people := []Person{}
		for _, p := range call.Participants {
			person := c.participantPerson(p.ParticipantID, p.PersonID)
			person.Participation = p.Participation
			person.Credited = p.Credited
			people = append(people, person)
		}
		people = c.tierPeople(people)
		if !c.admitTier(people) {
			continue
		}
		ref := messageRef(call.MessageID)
		e := Entry{Kind: "call", Key: call.IdentityKey, Ref: ref, AnchorRef: ref, Time: call.OccurredAt, HiddenText: d.HiddenText, Open: c.messageOpen(call.MessageID), Call: &CallEntry{Outcome: call.Outcome, DurationSeconds: call.DurationSeconds, People: people, PeopleCount: len(people)}}
		entries = append(entries, e)
	}
	return entries, upcoming
}
func displaySnippet(d store.MessageDisplay) string {
	if d.HiddenText {
		return ""
	}
	if d.Encrypted {
		return "[encrypted]"
	}
	if d.Snippet != "" {
		return d.Snippet
	}
	return d.Subject
}

func (c *composer) canonicalOccurrence(o store.CalendarOccurrence, info store.MeetingIdentityInfo) (store.CalendarOccurrence, bool) {
	ref, err := store.ParseMeetingIdentityRef(info.OccurrenceRef)
	if info.OccurrenceRef == "" || info.OccurrenceRef == o.Ref || err == nil && ref.MessageID == o.MessageID {
		return o, true
	}
	if resolved, ok := c.occurrences[info.OccurrenceRef]; ok {
		return resolved, true
	}

	r := readWithin(c.ctx, func(ctx context.Context) (store.CalendarOccurrenceDetail, error) {
		return c.src.Calendar.GetCalendarOccurrenceContext(ctx, info.OccurrenceRef)
	})
	c.gap("meetings", r.err, false)
	return r.value.CalendarOccurrence, r.err == nil
}

func (c *composer) occurrencePeople(o store.CalendarOccurrence) []Person {
	emails := []string{}
	for _, a := range o.Attendees {
		if a.Role != "room" && !a.IsCopySelf && !calevent.IsRoomAttendee(a.Email, false) && c.src.Filters.Exclusions.ExcludesAddress(a.Email) == "" {
			owner := readWithin(c.ctx, func(ctx context.Context) (bool, error) {
				return c.src.Meetings.IsAccountIdentityAddressContext(ctx, strings.TrimSpace(a.Email))
			})
			c.gap("people", owner.err, false)
			if owner.err != nil || owner.value {
				continue
			}
			emails = append(emails, a.Email)
		}
	}
	r := readWithin(c.ctx, func(ctx context.Context) (map[string]store.AddressParticipant, error) {
		return c.src.Meetings.ParticipantsByEmailContext(ctx, emails)
	})
	resolved := r.value
	c.gap("people", r.err, false)

	people := []Person{}
	seen := map[string]bool{}
	seenPeople := map[int64]bool{}
	for _, a := range o.Attendees {
		email := strings.ToLower(strings.TrimSpace(a.Email))
		if !slices.Contains(emails, a.Email) || seen[email] {
			continue
		}
		seen[email] = true
		p := resolved[email]
		if p.PersonID > 0 && seenPeople[p.PersonID] {
			continue
		}
		seenPeople[p.PersonID] = true
		name := a.DisplayName
		if name == "" {
			name = a.Email
		}
		people = append(people, Person{PersonID: p.PersonID, ParticipantID: p.ParticipantID, Name: name, TierSource: "none"})
	}
	return c.tierPeople(people)
}
func (c *composer) participantPerson(participant, person int64) Person {
	p := Person{PersonID: person, ParticipantID: participant, TierSource: "none"}

	r := readWithin(c.ctx, func(ctx context.Context) ([]store.PersonCandidate, error) {
		return c.src.People.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(participant, 10), nil, 10)
	})
	c.gap("people", r.err, false)
	if len(r.value) > 0 {
		p.Name = r.value[0].DisplayName
		if p.Name == "" {
			p.Name = r.value[0].Address
		}
	}

	return p
}

const sortTimeLayout = "2006-01-02T15:04:05.000000000Z"

func reverseDigits(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '9' - (r - '0')
		}
		return r
	}, value)
}
func chronologicalKey(e Entry) string {
	return e.Time.UTC().Format(sortTimeLayout) + "|" + e.Kind + "|" + e.Key
}
func historyKey(e Entry) string {
	return reverseDigits(e.Time.UTC().Format(sortTimeLayout)) + "|" + e.Kind + "|" + e.Key
}
func rankKey(e Entry) string {
	tier := 3
	volume := 0
	exchange := 1
	var people []Person
	if e.Burst != nil {
		people = e.Burst.People
		volume = e.Burst.Inbound + e.Burst.Outbound
		if e.Burst.Inbound > 0 && e.Burst.Outbound > 0 {
			exchange = 0
		}
	}
	if e.Call != nil {
		people = e.Call.People
		volume = 1
	}
	for _, p := range people {
		if p.Tier > 0 && p.Tier < tier {
			tier = p.Tier
		}
	}
	return fmt.Sprintf("%d|%d|%020d|%s|%s", tier, exchange, int64(1<<62)-int64(volume), reverseDigits(e.Time.UTC().Format(sortTimeLayout)), e.Key)
}
func (c *composer) dayKey(e Entry) string {
	date := c.entryDates[e.Key]
	if date == "" {
		date = e.Time.In(c.loc).Format(time.DateOnly)
	}
	part := "0"
	order := chronologicalKey(e)
	switch e.Kind {
	case entryKindBurst, entryKindMessage:
		part = "1"
		order = rankKey(e)
	case "call":
		part = "2"
		order = rankKey(e)
	}
	return reverseDigits(date) + "|" + part + "|" + order
}
func (c *composer) sortAndContinue(section string, entries []Entry, descending bool) {
	for _, e := range entries {
		key := chronologicalKey(e)
		if descending {
			key = historyKey(e)
		}
		c.keys[section+"|"+e.Key] = key
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(c.keys[section+"|"+a.Key], c.keys[section+"|"+b.Key]) })
}
func (c *composer) afterPosition(section string, entries []Entry) []Entry {
	result := entries[:0]
	for _, e := range entries {
		if c.keys[section+"|"+e.Key] > c.positions[section] {
			result = append(result, e)
		}
	}
	return result
}
func (c *composer) groupDays(entries []Entry) {
	byDate := map[string][]Entry{}
	for _, e := range entries {
		date := c.entryDates[e.Key]
		if date == "" {
			date = e.Time.In(c.loc).Format(time.DateOnly)
		}
		byDate[date] = append(byDate[date], e)
	}
	dates := []string{}
	for date := range byDate {
		dates = append(dates, date)
	}
	slices.Sort(dates)
	slices.Reverse(dates)
	newest, _, _ := calrecur.CivilDateBoundary(c.t.Window.To, c.loc)
	detailFrom := newest.AddDate(0, 0, -1).Format(time.DateOnly)
	for _, date := range dates {
		day := Day{Date: date}
		items := byDate[date]
		if date < detailFrom {
			if c.req.Brief {
				continue
			}
			summary := &DaySummary{People: []Person{}}
			people := map[string]Person{}
			for _, e := range items {
				if e.Meeting != nil {
					summary.Meetings++
				}
				if e.Burst != nil {
					summary.Messages += e.Burst.Inbound + e.Burst.Outbound
				}
				if e.Call != nil {
					summary.Calls++
				}
				for _, p := range EntryPeople(e) {
					key := "name:" + p.Name
					if p.PersonID > 0 {
						key = fmt.Sprintf("person:%d", p.PersonID)
					} else if p.ParticipantID > 0 {
						key = fmt.Sprintf("participant:%d", p.ParticipantID)
					}
					if _, exists := people[key]; !exists {
						people[key] = p
					}
				}
			}
			for _, p := range people {
				summary.People = append(summary.People, p)
			}
			slices.SortFunc(summary.People, func(a, b Person) int {
				if n := cmp.Compare(a.Name, b.Name); n != 0 {
					return n
				}
				if n := cmp.Compare(a.PersonID, b.PersonID); n != 0 {
					return n
				}
				return cmp.Compare(a.ParticipantID, b.ParticipantID)
			})
			summary.PeopleCount = len(summary.People)
			key := reverseDigits(date) + "|summary"
			c.keys["days|day:"+date] = key
			if key <= c.positions["days"] {
				continue
			}
			day.Summary = summary
		} else {
			for _, e := range items {
				c.keys["days|"+e.Key] = c.dayKey(e)
			}
			slices.SortFunc(items, func(a, b Entry) int { return cmp.Compare(c.keys["days|"+a.Key], c.keys["days|"+b.Key]) })
			items = c.afterPosition("days", items)
			for _, e := range items {
				switch e.Kind {
				case "meeting", "transcript":
					day.Meetings = append(day.Meetings, e)
				case entryKindBurst, entryKindMessage:
					day.Bursts = append(day.Bursts, e)
				case "call":
					day.Calls = append(day.Calls, e)
				}
			}
			if len(items) == 0 {
				continue
			}
		}
		c.t.Days = append(c.t.Days, day)
	}
}

// EntryPeople returns the people represented by an entry.
func EntryPeople(e Entry) []Person {
	switch {
	case e.Burst != nil:
		return e.Burst.People
	case e.Meeting != nil:
		return e.Meeting.People
	case e.Call != nil:
		return e.Call.People
	case e.Message != nil:
		return e.Message.People
	case e.Loop != nil:
		return []Person{e.Loop.Person}
	}
	return nil
}
