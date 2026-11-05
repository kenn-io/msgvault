package timeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type anchorTarget struct {
	ref                       AnchorRef
	at                        time.Time
	messageID, conversationID int64
	display                   store.MessageDisplay
	occurrence                *store.CalendarOccurrence
}

func (c *composer) resolveAnchor() (anchorTarget, error) {
	ref, err := ParseAnchor(c.req.Around, c.loc)
	if err != nil {
		return anchorTarget{}, err
	}
	target := anchorTarget{ref: ref, at: ref.Time}

	switch ref.Kind {
	case "date", "datetime":
		return target, nil
	case "meeting":

		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.CalendarOccurrenceDetail, error) {
			return c.src.Calendar.GetCalendarOccurrenceContext(ctx, ref.Ref)
		})
		if r.err != nil {
			if errors.Is(r.err, store.ErrCalendarOccurrenceNotFound) {
				return target, ErrUnknownReference
			}
			c.gap("anchor", r.err, false)
			return target, nil
		}
		o := r.value.CalendarOccurrence

		identity := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.MeetingIdentityResult, error) {
			return c.src.Meetings.ResolveMeetingIdentitiesContext(ctx, store.MeetingIdentityRequest{OccurrenceRefs: []string{o.Ref}})
		})
		c.gap("meetings", identity.err, false)
		var ok bool
		o, ok = c.canonicalOccurrence(o, identity.value.ByOccurrence[o.Ref])
		if !ok {
			return target, nil
		}

		target.occurrence = &o
		target.at = o.Start
		if o.AllDay {
			target.at, _ = time.ParseInLocation(time.DateOnly, o.StartDate, c.loc)
		}
		return target, nil
	case "conversation":
		r := c.conversationAnchor(ref.ID)
		if r.err != nil {
			c.gap("anchor", r.err, false)
			return target, nil //nolint:nilerr // Source failures are reported as anchor gaps while preserving the envelope.
		}
		target.messageID = r.value
		target.conversationID = ref.ID
	case entryKindMessage:
		target.messageID = ref.ID
	case entryKindBurst:
		q := store.BurstQuery{FromDate: ref.LocalDate, ToDate: ref.LocalDate, ConversationID: ref.ConversationID, Limit: 5000, Filters: c.filters()}
		q.Filters.IncludeNoise = true
		r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.BurstPage, error) {
			return c.src.Activity.ListActivityBurstsContext(ctx, q)
		})
		c.gap("anchor", r.err, false)
		failed := r.err != nil
		candidates := r.value.Bursts

		cold := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.BurstPage, error) {
			return c.src.Cold.ListColdActivityBurstsContext(ctx, q)
		})
		c.gap("anchor", cold.err, false)
		failed = failed || cold.err != nil
		candidates = append(candidates, cold.value.Bursts...)

		for _, b := range candidates {
			if b.Key == ref.Ref {
				target.messageID = b.AnchorMessageID
				target.conversationID = b.ConversationID
				break
			}
		}
		if target.messageID <= 0 && failed {
			return target, nil
		}
	}
	if target.messageID <= 0 {
		return target, ErrUnknownReference
	}
	display := c.readDisplays([]int64{target.messageID})
	if display.err != nil {
		c.gap("anchor", display.err, false)
		return target, nil //nolint:nilerr // Source failures are reported as anchor gaps while preserving the envelope.
	}
	d, ok := display.value[target.messageID]
	if !ok {
		return target, ErrUnknownReference
	}
	if d.OccurredAt == nil {
		return target, ErrUndatedAnchor
	}
	target.display = d
	target.at = d.OccurredAt.In(c.loc)
	target.conversationID = d.ConversationID
	return target, nil
}

// conversationAnchor finds the latest eligible conversation message, falling
// back to the cold reader when include_cold admits conversations kept out of
// the live projection.
func (c *composer) conversationAnchor(id int64) readResult[int64] {
	r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (int64, error) {
		return c.src.Activity.LatestEligibleMessageContext(ctx, id)
	})
	if r.err != nil || r.value > 0 || !c.req.IncludeCold {
		return r
	}
	q := store.MessageQuery{ConversationID: id, Descending: true, Limit: 1, Filters: c.filters()}
	q.Filters.IncludeNoise = true
	return readWithin(c.ctx, requestTimeout, func(ctx context.Context) (int64, error) {
		page, err := c.src.Cold.ListColdActivityMessagesContext(ctx, q)
		if len(page) == 0 {
			return 0, err
		}
		return page[0].MessageID, err
	})
}

func anchorWindow(at time.Time, loc *time.Location) Window {
	return Window{From: at.In(loc).AddDate(0, 0, -31).Format(time.DateOnly), To: at.In(loc).AddDate(0, 0, 31).Format(time.DateOnly)}
}

func (c *composer) probeAnchor(target anchorTarget, days int) ([]Entry, error) {
	from := target.at.In(c.loc).AddDate(0, 0, -days)
	to := target.at.In(c.loc).AddDate(0, 0, days).Add(time.Nanosecond)
	q := store.BurstQuery{After: &from, Before: &to, Subject: c.subject, Filters: c.filters(), Limit: maxBursts}
	if c.req.Conversation {
		q.ConversationID = target.conversationID
	}
	var activity readResult[activityWindow]
	var cold readResult[store.BurstPage]
	var coldCount readResult[int]
	var calendar readResult[store.CalendarOccurrencePage]
	var calls readResult[[]store.LogicalCall]
	var notes readResult[[]store.DailyNoteEntry]
	parallel(c.ctx,
		func() {
			activity = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (activityWindow, error) {
				if c.req.Conversation {
					page, err := c.src.Activity.ListActivityBurstsContext(ctx, q)
					return activityWindow{noise: page.NoiseOmitted, more: page.NextKey != nil}, err
				}
				cc := *c
				cc.ctx = ctx
				return cc.readActivity(q)
			})
		},
		func() {
			if c.req.IncludeCold {
				cold = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (store.BurstPage, error) {
					return c.src.Cold.ListColdActivityBurstsContext(ctx, q)
				})
			} else {
				coldCount = readWithin(c.ctx, requestTimeout, func(ctx context.Context) (int, error) {
					return c.src.Cold.CountColdActivityMessagesContext(ctx, q)
				})
			}
		},
		func() {
			if !c.req.Conversation {
				calendar = c.readCalendar(from, to)
			}
		},
		func() {
			if c.req.Scope.Owner && !c.req.Conversation {
				calls = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.LogicalCall, error) {
					return c.readCalls(ctx, store.LogicalCallQuery{After: from, Before: to, ParticipantIDs: c.participants, Limit: 500})
				})
			}
		},
		func() {
			if c.req.Scope.Owner && !c.req.Conversation {
				notes = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.DailyNoteEntry, error) {
					start, _ := time.ParseInLocation(time.DateOnly, from.Format(time.DateOnly), c.loc)
					end, _ := time.ParseInLocation(time.DateOnly, to.Format(time.DateOnly), c.loc)
					return c.readNotes(ctx, start, end.AddDate(0, 0, 1))
				})
			}
		},
	)
	c.gap("history", activity.err, len(activity.value.bursts) > 0)
	c.gap("cold", cold.err, false)
	c.gap("cold", coldCount.err, false)
	c.calendarGaps(calendar, "meetings")
	c.gap("calls", calls.err, false)
	c.gap("notes", notes.err, false)
	if activity.value.more || cold.value.NextKey != nil {
		c.t.Gaps = append(c.t.Gaps, Gap{Section: "history", Code: "partial"})
	}
	c.t.Omitted.Noise = activity.value.noise + cold.value.NoiseOmitted
	c.t.Omitted.Cold = coldCount.value
	probeErr := errors.Join(activity.err, cold.err, coldCount.err, calendar.err, calls.err, notes.err)
	if c.req.Conversation {
		entries, err := c.conversationEntries(target.conversationID, from, to)
		return entries, errors.Join(probeErr, err)
	}
	occurrences := calendar.value.Occurrences
	if target.occurrence != nil {
		occurrences = append(slices.Clone(occurrences), *target.occurrence)
	}
	entries, _ := c.mergeEntries(mergeBursts(activity.value.bursts, cold.value.Bursts), activity.value.meetings, occurrences, calls.value, notes.value)
	return c.filterSubject(entries), probeErr
}
func anchorMatches(e Entry, target anchorTarget) bool {
	if e.Key == target.ref.Ref || e.Ref == target.ref.Ref || target.occurrence != nil && e.Ref == target.occurrence.Ref {
		return true
	}
	if target.messageID > 0 {
		if e.Ref == messageRef(target.messageID) {
			return true
		}
		if e.Burst != nil && (slices.Contains(e.Burst.LatestMessageIDs, target.messageID) || e.Key == fmt.Sprintf("burst:%d:%d:%s", target.display.SourceID, target.conversationID, target.at.Format(time.DateOnly))) {
			return true
		}
		if e.Meeting != nil && slices.Contains(e.Meeting.LinkedTranscriptIDs, target.messageID) {
			return true
		}
	}
	return false
}
func (c *composer) anchorFallback(target anchorTarget) Entry {
	d := target.display
	ref := messageRef(target.messageID)
	snippet := displaySnippet(d)
	return Entry{Kind: entryKindMessage, Key: ref, Ref: ref, AnchorRef: ref, Time: target.at, Open: c.messageOpen(target.messageID), Anchor: true, Filtered: true, HiddenText: d.HiddenText, Message: &MessageEntry{Snippet: UntrustedText{Text: snippet}, People: []Person{}}}
}
func (c *composer) buildAnchor(target anchorTarget) {
	c.loadFreshness()
	if !c.req.Scope.Owner {
		c.ownerGaps()
	}
	if target.at.IsZero() {
		return
	}
	c.t.Anchor = &Anchor{Ref: target.ref.Ref, Kind: target.ref.Kind, Time: target.at}
	var entries []Entry
	for _, days := range []int{1, 2, 4, 8, 16, 31} {
		probe, err := c.probeAnchor(target, days)
		if err != nil {
			if entries == nil {
				entries = probe
			}
			break
		}
		entries = probe
		before, after := 0, 0
		for _, e := range entries {
			if anchorMatches(e, target) {
				continue
			}
			if e.Time.Before(target.at) {
				before++
			} else {
				after++
			}
		}
		if before >= c.req.Before && after >= c.req.After {
			break
		}
	}
	found := false
	for i := range entries {
		if anchorMatches(entries[i], target) {
			entries[i].Anchor = true
			found = true
		}
	}
	if !found && target.messageID > 0 {
		c.t.Anchor.Filtered = true
		entries = append(entries, c.anchorFallback(target))
	}
	c.sortAndContinue("history", entries, false)
	var before, after, anchors []Entry
	for _, e := range entries {
		if e.Anchor {
			anchors = append(anchors, e)
		} else if e.Time.Before(target.at) {
			before = append(before, e)
		} else {
			after = append(after, e)
		}
	}
	entries = append(before[max(0, len(before)-c.req.Before):], anchors...)
	entries = append(entries, after[:min(len(after), c.req.After)]...)
	c.sortAndContinue("history", entries, false)
	c.t.History = c.afterPosition("history", entries)
	if !c.req.Scope.Owner {
		c.ownerGaps()
	}
}

func (c *composer) conversationEntries(conversation int64, from, to time.Time) ([]Entry, error) {
	readMessages := func(ctx context.Context, read func(context.Context, store.MessageQuery) ([]store.ActivityMessage, error)) ([]store.ActivityMessage, error) {
		messages := []store.ActivityMessage{}
		q := store.MessageQuery{ConversationID: conversation, After: &from, Before: &to, Limit: 500, Filters: c.filters()}
		for {
			page, err := read(ctx, q)
			messages = append(messages, page...)
			if err != nil {
				return messages, err
			}
			if len(page) < q.Limit {
				return messages, nil
			}
			last := page[len(page)-1]
			q.AfterKey = &store.ActivityKey{OccurredAt: last.OccurredAt, MessageID: last.MessageID}
		}
	}
	var r, cold readResult[[]store.ActivityMessage]
	parallel(c.ctx,
		func() {
			r = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.ActivityMessage, error) {
				return readMessages(ctx, c.src.Activity.ListActivityMessagesContext)
			})
		},
		func() {
			if c.req.IncludeCold {
				cold = readWithin(c.ctx, requestTimeout, func(ctx context.Context) ([]store.ActivityMessage, error) {
					return readMessages(ctx, c.src.Cold.ListColdActivityMessagesContext)
				})
			}
		},
	)
	c.gap("history", r.err, len(r.value) > 0)
	c.gap("cold", cold.err, len(cold.value) > 0)
	seen := map[int64]bool{}
	messages := []store.ActivityMessage{}
	for _, message := range append(r.value, cold.value...) {
		if !seen[message.MessageID] {
			seen[message.MessageID] = true
			messages = append(messages, message)
		}
	}
	ids := []int64{}
	for _, m := range messages {
		ids = append(ids, m.MessageID)
	}
	display := c.displays(ids)
	personIDs := []int64{}
	for _, m := range messages {
		for _, p := range counterpartPeople(m.Counterparts) {
			personIDs = append(personIDs, p.PersonID)
		}
	}
	c.loadTiers(personIDs)
	entries := []Entry{}
	for _, m := range messages {
		d, ok := display[m.MessageID]
		if !ok {
			continue
		}
		people := c.tierPeople(counterpartPeople(m.Counterparts))
		if !c.admitTier(people) {
			continue
		}
		text := displaySnippet(d)
		ref := messageRef(m.MessageID)
		entries = append(entries, Entry{Kind: entryKindMessage, Key: ref, Ref: ref, AnchorRef: ref, Time: m.OccurredAt, Open: c.messageOpen(m.MessageID), HiddenText: d.HiddenText, Message: &MessageEntry{Direction: string(m.Direction), People: people, PeopleCount: len(people), Snippet: UntrustedText{Text: text}}})
	}
	return c.filterSubject(entries), errors.Join(r.err, cold.err)
}
