package timeline

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// Fit selects ordered prefixes of each section and measures the final wire
// encoding, including omission metadata and the actual continuation. The input
// and its payloads are never mutated. SetContinuation supplies cursor bindings
// before fitting a pageable result.
func Fit(t *Timeline, format string, budget int) (*Timeline, error) {
	if t == nil || budget < 2048 || budget > 65536 || (format != "json" && format != "markdown") {
		return nil, ErrInvalidRequest
	}
	for _, day := range t.Days {
		if day.Summary != nil && len(day.Meetings)+len(day.Bursts)+len(day.Calls)+len(day.Notes) > 0 {
			return nil, fmt.Errorf("%w: day %s has both summary and entries", ErrInvalidRequest, day.Date)
		}
	}
	// Validate the wire values once; cloning the typed payloads preserves int64
	// opener arguments without a lossy JSON-to-float round trip.
	if _, err := json.Marshal(t); err != nil {
		return nil, fmt.Errorf("encode timeline: %w", err)
	}
	original := cloneTimeline(t)
	var err error
	boundTimeline(&original)
	items := timelineItems(&original)
	totals := map[string]int{}
	for _, item := range items {
		totals[item.section]++
	}
	out := original
	out.Days = nil
	out.Upcoming = nil
	out.History = nil
	if original.OpenLoops != nil {
		loops := *original.OpenLoops
		loops.Items = nil
		out.OpenLoops = &loops
	}
	counts := map[string]int{}
	if err = preparePage(&out, &original, totals, counts, format); err != nil {
		return nil, err
	}
	base, err := timelineSize(&out, format)
	if err != nil {
		return nil, err
	}
	if base > budget {
		return nil, ErrBudgetTooSmall
	}
	available := budget - base
	// A running ceiling passes each unused share to the following section.
	ceilings := map[string]int{"open_loops": base + available*15/100, "upcoming": base + available*25/100, "days": budget, "history": budget}
	for _, section := range []string{"open_loops", "upcoming", "days", "history"} {
		admitted := 0
		for _, item := range items {
			if item.section != section {
				continue
			}
			candidate := clonePage(out)
			appendItem(&candidate, item, false)
			candidateCounts := copyMap(counts)
			candidateCounts[section]++
			if err = preparePage(&candidate, &original, totals, candidateCounts, format); err != nil {
				return nil, err
			}
			size, err := timelineSize(&candidate, format)
			if err != nil {
				return nil, err
			}
			limit := ceilings[section]
			if size > limit {
				if admitted > 0 {
					break
				}
				// A section's first row always makes progress, using a minimal identity
				// stub when the complete row exceeds its share. It may borrow from the
				// remaining budget when even its stub exceeds that share.
				candidate = clonePage(out)
				appendItem(&candidate, item, true)
				if err = preparePage(&candidate, &original, totals, candidateCounts, format); err != nil {
					return nil, err
				}
				size, err = timelineSize(&candidate, format)
				if err != nil {
					return nil, err
				}
				if size > budget {
					return nil, ErrBudgetTooSmall
				}
			}
			out = candidate
			counts = candidateCounts
			admitted++
		}
	}
	if err = preparePage(&out, &original, totals, counts, format); err != nil {
		return nil, err
	}
	if original.continuation == nil {
		for section, total := range totals {
			if section != "open_loops" && counts[section] < total {
				return nil, fmt.Errorf("%w: pageable result needs SetContinuation", ErrInvalidRequest)
			}
		}
	}
	finalSize, err := timelineSize(&out, format)
	if err != nil {
		return nil, err
	}
	if finalSize > budget {
		return nil, ErrBudgetTooSmall
	}
	return &out, nil
}

type pageItem struct {
	section, part, date string
	entry               *Entry
	summary             *DaySummary
}

func timelineItems(t *Timeline) []pageItem {
	var out []pageItem
	add := func(section, part, date string, entries []Entry) {
		for i := range entries {
			out = append(out, pageItem{section: section, part: part, date: date, entry: &entries[i]})
		}
	}
	if t.OpenLoops != nil {
		add("open_loops", "", "", t.OpenLoops.Items)
	}
	add("upcoming", "", "", t.Upcoming)
	for _, d := range t.Days {
		if d.Summary != nil {
			out = append(out, pageItem{section: "days", date: d.Date, summary: d.Summary})
			continue
		}
		add("days", "meetings", d.Date, d.Meetings)
		add("days", "bursts", d.Date, d.Bursts)
		add("days", "calls", d.Date, d.Calls)
		add("days", "notes", d.Date, d.Notes)
	}
	add("history", "", "", t.History)
	return out
}
func appendItem(t *Timeline, item pageItem, stub bool) {
	var e Entry
	if item.entry != nil {
		e = *item.entry
		if stub {
			e = entryStub(e)
		}
	}
	switch item.section {
	case "open_loops":
		t.OpenLoops.Items = append(t.OpenLoops.Items, e)
	case "upcoming":
		t.Upcoming = append(t.Upcoming, e)
	case "history":
		t.History = append(t.History, e)
	case "days":
		if len(t.Days) == 0 || t.Days[len(t.Days)-1].Date != item.date {
			t.Days = append(t.Days, Day{Date: item.date})
		}
		d := &t.Days[len(t.Days)-1]
		if item.summary != nil {
			summary := *item.summary
			if stub {
				summary.People = nil
				summary.Truncated = true
			}
			d.Summary = &summary
			return
		}
		switch item.part {
		case "meetings":
			d.Meetings = append(d.Meetings, e)
		case "bursts":
			d.Bursts = append(d.Bursts, e)
		case "calls":
			d.Calls = append(d.Calls, e)
		case "notes":
			d.Notes = append(d.Notes, e)
		}
	}
}
func entryStub(e Entry) Entry {
	e.Meeting = nil
	e.Burst = nil
	e.Message = nil
	e.Call = nil
	e.Loop = nil
	e.Note = nil
	switch e.Kind {
	case "meeting", "transcript":
		e.Meeting = &MeetingEntry{}
	case "burst":
		e.Burst = &BurstEntry{}
	case "message":
		e.Message = &MessageEntry{}
	case "call":
		e.Call = &CallEntry{}
	case "loop":
		e.Loop = &LoopEntry{}
	case "note":
		e.Note = &NoteEntry{}
	}
	return e
}
func clonePage(t Timeline) Timeline {
	t.History = append([]Entry(nil), t.History...)
	t.Upcoming = append([]Entry(nil), t.Upcoming...)
	t.Days = append([]Day(nil), t.Days...)
	for i := range t.Days {
		d := &t.Days[i]
		d.Meetings = append([]Entry(nil), d.Meetings...)
		d.Bursts = append([]Entry(nil), d.Bursts...)
		d.Calls = append([]Entry(nil), d.Calls...)
		d.Notes = append([]Entry(nil), d.Notes...)
	}
	if t.OpenLoops != nil {
		loops := *t.OpenLoops
		loops.Items = append([]Entry(nil), loops.Items...)
		t.OpenLoops = &loops
	}
	t.Omitted.Budget = copyMap(t.Omitted.Budget)
	return t
}
func preparePage(t, original *Timeline, totals, counts map[string]int, format string) error {
	t.Omitted.Budget = copyMap(original.Omitted.Budget)
	needsCursor := false
	for section, total := range totals {
		lost := total - counts[section]
		if lost > 0 {
			if section != "open_loops" {
				needsCursor = true
			}
			if old, ok := t.Omitted.Budget[section]; ok {
				if n, ok := old.(float64); ok {
					lost += int(n)
				} else if n, ok := old.(int); ok {
					lost += n
				} else {
					t.Omitted.Budget[section] = "more"
					continue
				}
			}
			t.Omitted.Budget[section] = lost
			if section != "open_loops" {
				needsCursor = true
			}
		}
	}
	if original.OpenLoops != nil && original.OpenLoops.Truncated {
		t.Omitted.Budget["open_loops"] = "more"
	}
	c := original.continuation
	if c != nil {
		for section, more := range c.More {
			if more && section != "open_loops" {
				t.Omitted.Budget[section] = "more"
				needsCursor = true
			}
		}
	}
	t.Cursor = original.Cursor
	if c == nil {
		return nil
	}
	t.Cursor = ""
	if !needsCursor {
		return nil
	}
	positions := copyMap(c.Positions)
	delete(positions, "open_loops")
	for _, item := range timelineItems(t) {
		if item.section == "open_loops" {
			continue
		}
		key := "day:" + item.date
		if item.entry != nil {
			key = item.entry.Key
		}
		sortKey := c.SortKeys[item.section+"|"+key]
		if sortKey == "" {
			sortKey = c.SortKeys[key]
		}
		if sortKey == "" {
			if item.section == "days" {
				return fmt.Errorf("%w: missing day sort key", ErrInvalidRequest)
			}
			// Fixed precision keeps lexical order chronological across subsecond times.
			sortKey = item.entry.Time.UTC().Format("2006-01-02T15:04:05.000000000Z") + "|" + item.entry.Kind + "|" + item.entry.Key
		}
		positions[item.section] = sortKey
	}
	var err error
	binding := c.Binding
	binding.Request.Format = format
	t.Cursor, err = EncodeCursor(binding, positions)
	return err
}
func timelineSize(t *Timeline, format string) (int, error) {
	if format == "markdown" {
		return len(Markdown(t)), nil
	}
	b, err := json.Marshal(t, jsontext.EscapeForHTML(true))
	return len(b), err
}
func boundText(t *UntrustedText, n int) {
	r := []rune(t.Text)
	if len(r) > n {
		t.Text = string(r[:n])
		t.Truncated = true
	}
}
func boundPerson(p Person) Person {
	r := []rune(p.Name)
	if len(r) > 80 {
		p.Name = string(r[:80])
		p.Truncated = true
	}
	if p.PersonID == 0 {
		p.Tier = 0
		p.TierSource = "none"
	} else if p.TierSource == "" {
		p.TierSource = "none"
	}
	return p
}
func boundPeople(ps []Person, count int) ([]Person, int, bool) {
	count = max(count, len(ps))
	for i := range ps {
		ps[i] = boundPerson(ps[i])
	}
	return ps[:min(8, len(ps))], count, count > 8
}
func boundTimeline(t *Timeline) {
	if len(t.Candidates) > 10 {
		t.Candidates = t.Candidates[:10]
		t.Candidates[9].Truncated = true
	}
	if p := t.Person; p != nil {
		if p.Brief != nil {
			boundText(p.Brief, 600)
		}
		p.Person = boundPerson(p.Person)
		for i := range p.Agenda {
			boundText(&p.Agenda[i], 80)
		}
	}
	for i := range t.Days {
		if s := t.Days[i].Summary; s != nil {
			if len(s.People) > 8 {
				s.Truncated = true
			}
			s.People, s.PeopleCount, s.PeopleTruncated = boundPeople(s.People, s.PeopleCount)
		}
	}
	for _, item := range timelineItems(t) {
		e := item.entry
		if e == nil {
			continue
		}
		if p := e.Meeting; p != nil {
			boundText(&p.Title, 80)
			p.Attendees, p.AttendeeCount, p.AttendeesTruncated = boundPeople(p.Attendees, p.AttendeeCount)
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
			if e.HiddenText {
				p.Title = UntrustedText{}
			}
		}
		if p := e.Burst; p != nil {
			boundText(&p.Snippet, 80)
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
			if e.HiddenText {
				p.Snippet = UntrustedText{}
			}
		}
		if p := e.Message; p != nil {
			boundText(&p.Snippet, 80)
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
			if e.HiddenText {
				p.Snippet = UntrustedText{}
			}
		}
		if p := e.Call; p != nil {
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
		}
		if p := e.Loop; p != nil {
			p.Person = boundPerson(p.Person)
		}
		if p := e.Note; p != nil {
			boundText(&p.Text, 600)
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
			if e.HiddenText {
				p.Text = UntrustedText{}
			}
		}
	}
}

func cloneTimeline(t *Timeline) Timeline {
	out := clonePage(*t)
	if t.Person != nil {
		p := *t.Person
		p.Channels = append([]string(nil), p.Channels...)
		p.Agenda = append([]UntrustedText(nil), p.Agenda...)
		if p.Brief != nil {
			brief := *p.Brief
			p.Brief = &brief
		}
		out.Person = &p
	}
	out.Candidates = append([]Candidate(nil), t.Candidates...)
	for i := range out.Days {
		if out.Days[i].Summary != nil {
			s := *out.Days[i].Summary
			s.People = append([]Person(nil), s.People...)
			out.Days[i].Summary = &s
		}
	}
	for _, item := range timelineItems(&out) {
		e := item.entry
		if e == nil {
			continue
		}
		if e.Meeting != nil {
			p := *e.Meeting
			p.Attendees = append([]Person(nil), p.Attendees...)
			p.People = append([]Person(nil), p.People...)
			e.Meeting = &p
		}
		if e.Burst != nil {
			p := *e.Burst
			p.People = append([]Person(nil), p.People...)
			e.Burst = &p
		}
		if e.Message != nil {
			p := *e.Message
			p.People = append([]Person(nil), p.People...)
			e.Message = &p
		}
		if e.Call != nil {
			p := *e.Call
			p.People = append([]Person(nil), p.People...)
			e.Call = &p
		}
		if e.Loop != nil {
			p := *e.Loop
			e.Loop = &p
		}
		if e.Note != nil {
			p := *e.Note
			p.People = append([]Person(nil), p.People...)
			e.Note = &p
		}
	}
	return out
}
