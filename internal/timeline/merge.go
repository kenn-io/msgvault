package timeline

import (
	"cmp"
	"context"
	"fmt"
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
	if hasBurstMembers(full, cold) {
		return mergeBurstMembers(full, cold)
	}
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
		return compare(a.Key, b.Key)
	})
	return out
}

func hasBurstMembers(full, cold []store.ActivityBurst) bool {
	total, members := 0, 0
	for _, burst := range append(slices.Clone(full), cold...) {
		total++
		members += len(burst.Members)
		if len(burst.LatestMessageIDs) > 0 && len(burst.Members) == 0 {
			return false
		}
	}
	return total > 0 && members > 0
}

func mergeBurstMembers(full, cold []store.ActivityBurst) []store.ActivityBurst {
	selected := map[string]store.ActivityBurstMember{}
	copyCounts := map[string]int{}
	seenMessageIDs := map[string]map[int64]bool{}
	for _, burst := range append(slices.Clone(full), cold...) {
		for _, member := range burst.Members {
			key := member.CopyKey
			if key == "" {
				key = fmt.Sprintf("id:%d", member.MessageID)
			}
			member.CopyKey = key
			if seenMessageIDs[key] == nil {
				seenMessageIDs[key] = map[int64]bool{}
			}
			if !seenMessageIDs[key][member.MessageID] {
				copyCounts[key] += max(1, member.CopyCount)
				seenMessageIDs[key][member.MessageID] = true
			}
			current, found := selected[key]
			if !found || burstMemberPreferred(member, current) {
				selected[key] = member
			}
		}
	}
	members := make([]store.ActivityBurstMember, 0, len(selected))
	for key, member := range selected {
		member.CopyCount = copyCounts[key]
		members = append(members, member)
	}
	burstKey := func(member store.ActivityBurstMember) string {
		return fmt.Sprintf("burst:%d:%d:%s", member.SourceID, member.ConversationID, member.LocalDate)
	}
	slices.SortFunc(members, func(a, b store.ActivityBurstMember) int {
		if n := strings.Compare(burstKey(a), burstKey(b)); n != 0 {
			return n
		}
		if n := b.OccurredAt.Compare(a.OccurredAt); n != 0 {
			return n
		}
		return cmp.Compare(b.MessageID, a.MessageID)
	})
	groups := map[string]*store.ActivityBurst{}
	counterparts := map[string]map[int64]store.ActivityMessageCounterpart{}
	for _, member := range members {
		key := burstKey(member)
		burst := groups[key]
		if burst == nil {
			burst = &store.ActivityBurst{
				Key: key, SourceID: member.SourceID, ConversationID: member.ConversationID,
				Channel: member.Channel, LocalDate: member.LocalDate, FirstAt: member.OccurredAt,
				LastAt: member.OccurredAt, LatestMessageIDs: []int64{},
				CopyCounts: map[int64]int{}, Counterparts: []store.ActivityMessageCounterpart{},
				Members: []store.ActivityBurstMember{}, Cold: member.Cold,
			}
			groups[key] = burst
			counterparts[key] = map[int64]store.ActivityMessageCounterpart{}
		}
		if member.OccurredAt.Before(burst.FirstAt) {
			burst.FirstAt = member.OccurredAt
		}
		if member.OccurredAt.After(burst.LastAt) {
			burst.LastAt = member.OccurredAt
		}
		if member.Channel < burst.Channel {
			burst.Channel = member.Channel
		}
		if burst.AnchorMessageID == 0 {
			burst.AnchorMessageID = member.MessageID
		}
		if len(burst.LatestMessageIDs) < 5 {
			burst.LatestMessageIDs = append(burst.LatestMessageIDs, member.MessageID)
		}
		if member.Counted {
			if member.Direction == store.DirectionInbound {
				burst.Inbound++
			}
			if member.Direction == store.DirectionOutbound {
				burst.Outbound++
			}
		}
		burst.Members = append(burst.Members, member)
		burst.Cold = burst.Cold && member.Cold
		for _, cp := range member.Counterparts {
			old, found := counterparts[key][cp.ParticipantID]
			if !found || (!old.Counted && cp.Counted) {
				counterparts[key][cp.ParticipantID] = cp
			}
		}
	}
	out := make([]store.ActivityBurst, 0, len(groups))
	for key, burst := range groups {
		if burst.Channel == store.ChannelEmail {
			for _, member := range burst.Members {
				burst.CopyCounts[member.MessageID] = member.CopyCount
			}
		}
		for _, cp := range counterparts[key] {
			burst.Counterparts = append(burst.Counterparts, cp)
		}
		slices.SortFunc(burst.Counterparts, func(a, b store.ActivityMessageCounterpart) int {
			return cmp.Compare(a.ParticipantID, b.ParticipantID)
		})
		out = append(out, *burst)
	}
	slices.SortFunc(out, func(a, b store.ActivityBurst) int {
		if n := a.FirstAt.Compare(b.FirstAt); n != 0 {
			return n
		}
		return compare(a.Key, b.Key)
	})
	return out
}

func burstMemberPreferred(candidate, current store.ActivityBurstMember) bool {
	if candidate.Counted != current.Counted {
		return candidate.Counted
	}
	if candidate.Cold != current.Cold {
		return !candidate.Cold
	}
	if candidate.SourceID != current.SourceID {
		return candidate.SourceID < current.SourceID
	}
	return candidate.MessageID < current.MessageID
}

func (c *composer) displays(ids []int64) map[int64]store.MessageDisplay {
	r := c.readDisplays(ids)
	c.gap("text", r.err, false)
	return r.value
}

func (c *composer) readDisplays(ids []int64) readResult[map[int64]store.MessageDisplay] {
	if c.src.Activity == nil {
		return readResult[map[int64]store.MessageDisplay]{value: map[int64]store.MessageDisplay{}}
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (map[int64]store.MessageDisplay, error) {
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

func (c *composer) mergeEntries(bursts []store.ActivityBurst, messages []store.ActivityMessage, occurrences []store.CalendarOccurrence, calls []store.LogicalCall, notes []store.DailyNoteEntry) ([]Entry, []Entry) {
	messagesByID := make(map[int64]store.ActivityMessage, len(messages))
	for _, message := range messages {
		messagesByID[message.MessageID] = message
	}
	ids := []int64{}
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
		ids = append(ids, o.MessageID)
	}
	for _, call := range calls {
		ids = append(ids, call.MessageID)
	}
	identity := store.MeetingIdentityResult{ByOccurrence: map[string]store.MeetingIdentityInfo{}, ByTranscript: map[int64]store.MeetingIdentityInfo{}}
	if c.src.Meetings != nil {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.MeetingIdentityResult, error) {
			return c.src.Meetings.ResolveMeetingIdentitiesContext(ctx, store.MeetingIdentityRequest{OccurrenceRefs: sortedSet(refs), TranscriptIDs: sortedSet(transcriptIDs), IncludeSuggestions: c.req.Scope.Owner})
		})
		c.gap("meetings", r.err, false)
		identity = r.value
	} else {
		c.unavailable("meetings")
	}
	// Resolve the provider/copy representative before using time, text or people.
	canonical := make([]store.CalendarOccurrence, 0, len(occurrences))
	for _, o := range occurrences {
		if resolved, ok := c.canonicalOccurrence(o, identity.ByOccurrence[o.Ref]); ok {
			canonical = append(canonical, resolved)
			ids = append(ids, resolved.MessageID)
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
	actions := map[int64]int{}
	if c.src.Meetings != nil {
		actionIDs := []int64{}
		for id := range identity.ByTranscript {
			if _, ok := display[id]; ok {
				actionIDs = append(actionIDs, id)
			}
		}
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (map[int64]int, error) {
			return c.src.Meetings.MeetingActionItemCountsContext(ctx, sortedSet(actionIDs))
		})
		actions = r.value
		c.gap("meetings", r.err, false)
	}
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
	for _, note := range notes {
		personIDs = append(personIDs, note.PersonIDs...)
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
		seenPeople := make(map[int64]bool, len(people))
		for _, person := range people {
			seenPeople[person.ParticipantID] = true
		}
		for _, transcriptID := range info.LinkedTranscriptIDs {
			message, ok := messagesByID[transcriptID]
			if !ok {
				continue
			}
			for _, person := range counterpartPeople(message.Counterparts) {
				if !seenPeople[person.ParticipantID] {
					people = append(people, person)
					seenPeople[person.ParticipantID] = true
				}
			}
		}
		people = c.tierPeople(people)
		if !c.admitTier(people) {
			continue
		}
		linked := []int64{}
		hidden := display[o.MessageID].HiddenText
		count := 0
		for _, id := range info.LinkedTranscriptIDs {
			if d, ok := display[id]; ok {
				linked = append(linked, id)
				hidden = hidden || d.HiddenText
				count += actions[id]
			}
		}
		title := o.Subject
		if hidden {
			title = ""
		}
		at := o.Start
		end := o.End
		if o.AllDay {
			var exists bool
			var err error
			at, exists, err = calrecur.CivilDateBoundary(o.StartDate, c.loc)
			if err != nil || !exists {
				continue
			}
			end, _, err = calrecur.CivilDateBoundary(o.EndDate, c.loc)
			if err != nil {
				continue
			}
		}
		if at.IsZero() {
			continue
		}
		e := Entry{Kind: "meeting", Key: key, Ref: o.Ref, AnchorRef: o.Ref, Time: at, HiddenText: hidden, Open: c.open("get_calendar_event", map[string]any{"ref": o.Ref}), Meeting: &MeetingEntry{Title: UntrustedText{Text: title}, Start: at, End: end, Status: o.Status, DeletedFromSource: o.DeletedFromSource, AllDay: o.AllDay, Attendees: attendees, AttendeeCount: len(attendees), People: people, PeopleCount: len(people), Attended: len(linked) > 0, LinkedTranscriptIDs: linked, ActionItemCount: count}}
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
		e.Meeting.PossiblyRelated = sortedSet(e.Meeting.PossiblyRelated)
		date := at.In(c.loc).Format(time.DateOnly)
		if o.AllDay {
			date = o.StartDate
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
		if !strings.EqualFold(o.Status, "cancelled") && !o.DeletedFromSource && !at.Before(c.t.Now) && at.Before(until) {
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
	for _, note := range notes {
		people := []Person{}
		for _, id := range note.PersonIDs {
			people = append(people, c.savedPerson(id))
		}
		people = c.tierPeople(people)
		if !c.admitTier(people) {
			continue
		}
		at, _, err := calrecur.CivilDateBoundary(note.LocalDate, c.loc)
		if err != nil {
			continue
		}
		e := Entry{Kind: "note", Key: fmt.Sprintf("note:%d", note.ID), Ref: note.LocalDate, AnchorRef: note.LocalDate, Time: at, Open: []Open{}, Note: &NoteEntry{Text: UntrustedText{Text: note.Body}, People: people, PeopleCount: len(people)}}
		c.entryDates[e.Key] = note.LocalDate
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
	if info.OccurrenceRef == "" || info.OccurrenceRef == o.Ref {
		return o, true
	}
	if c.src.Calendar == nil {
		c.unavailable("meetings")
		return store.CalendarOccurrence{}, false
	}
	r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.CalendarOccurrenceDetail, error) {
		return c.src.Calendar.GetCalendarOccurrenceContext(ctx, info.OccurrenceRef)
	})
	c.gap("meetings", r.err, false)
	return r.value.CalendarOccurrence, r.err == nil
}

func (c *composer) occurrencePeople(o store.CalendarOccurrence) []Person {
	emails := []string{}
	for _, a := range o.Attendees {
		if a.Role != "room" && !a.IsCopySelf && !calevent.IsRoomAttendee(a.Email, false) && c.src.Filters.Exclusions.ExcludesAddress(a.Email) == "" {
			emails = append(emails, a.Email)
		}
	}
	resolved := map[string]store.AddressParticipant{}
	if c.src.Meetings != nil {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (map[string]store.AddressParticipant, error) {
			return c.src.Meetings.ParticipantsByEmailContext(ctx, emails)
		})
		resolved = r.value
		c.gap("people", r.err, false)
	}
	people := []Person{}
	seen := map[string]bool{}
	for _, a := range o.Attendees {
		email := strings.ToLower(strings.TrimSpace(a.Email))
		if !slices.Contains(emails, a.Email) || seen[email] {
			continue
		}
		seen[email] = true
		p := resolved[email]
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
	if c.src.People != nil {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.PersonCandidate, error) {
			return c.src.People.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(participant, 10), nil, 10)
		})
		c.gap("people", r.err, false)
		if len(r.value) > 0 {
			p.Name = r.value[0].DisplayName
			if p.Name == "" {
				p.Name = r.value[0].Address
			}
		}
	}
	return p
}
func (c *composer) savedPerson(id int64) Person {
	p := Person{PersonID: id, TierSource: "none"}
	if c.src.People != nil {
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.PersonCandidate, error) {
			return c.src.People.ResolvePersonReferenceContext(ctx, "person", strconv.FormatInt(id, 10), nil, 10)
		})
		c.gap("people", r.err, false)
		if len(r.value) > 0 {
			p.Name = r.value[0].DisplayName
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
	case "note":
		part = "3"
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
	slices.SortFunc(entries, func(a, b Entry) int { return compare(c.keys[section+"|"+a.Key], c.keys[section+"|"+b.Key]) })
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
	detailFrom := shiftCivilDate(c.t.Window.To, -1)
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
				for _, p := range entryPeople(e) {
					key := fmt.Sprintf("%d:%d:%s", p.PersonID, p.ParticipantID, p.Name)
					people[key] = p
				}
			}
			for _, p := range people {
				summary.People = append(summary.People, p)
			}
			slices.SortFunc(summary.People, func(a, b Person) int {
				if n := compare(a.Name, b.Name); n != 0 {
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
			slices.SortFunc(items, func(a, b Entry) int { return compare(c.keys["days|"+a.Key], c.keys["days|"+b.Key]) })
			items = c.afterPosition("days", items)
			for _, e := range items {
				switch e.Kind {
				case "meeting", "transcript":
					day.Meetings = append(day.Meetings, e)
				case entryKindBurst, entryKindMessage:
					day.Bursts = append(day.Bursts, e)
				case "call":
					day.Calls = append(day.Calls, e)
				case "note":
					day.Notes = append(day.Notes, e)
				}
			}
			if len(items) == 0 {
				continue
			}
		}
		c.t.Days = append(c.t.Days, day)
	}
}
func entryPeople(e Entry) []Person {
	switch {
	case e.Burst != nil:
		return e.Burst.People
	case e.Meeting != nil:
		return e.Meeting.People
	case e.Call != nil:
		return e.Call.People
	case e.Message != nil:
		return e.Message.People
	case e.Note != nil:
		return e.Note.People
	}
	return nil
}
