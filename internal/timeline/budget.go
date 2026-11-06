package timeline

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/textutil"
)

// MaxBudget is the largest timeline response in bytes.
const MaxBudget = 65536

const freshnessSection = "freshness"

// Fit selects ordered prefixes of each section and measures the final wire
// encoding, including omission metadata and the actual continuation. Fit owns
// the input and its payloads. Format and budget must be normalized by Build.
// SetContinuation supplies cursor bindings
// before fitting a pageable result.
func Fit(t *Timeline, format string, budget int) (*Timeline, error) {
	if t == nil {
		return nil, ErrInvalidRequest
	}
	original := *t
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
	out.Freshness.Sources = nil
	out.Freshness.Projections = nil
	out.Freshness.Warnings = nil
	if original.OpenLoops != nil {
		loops := *original.OpenLoops
		loops.Items = nil
		out.OpenLoops = &loops
	}
	counts := map[string]int{}
	if err = preparePage(&out, &original, totals, counts, format); err != nil {
		return nil, err
	}
	base, err := fitPersonText(&out, &original, totals, counts, format, budget)
	if err != nil {
		return nil, err
	}
	available := budget - base
	// A running ceiling passes each unused share to the following section.
	ceilings := map[string]int{freshnessSection: base + available*15/100, "open_loops": base + available*15/100, "upcoming": base + available*25/100, "days": budget, "history": budget}
	for _, section := range []string{freshnessSection, "open_loops", "upcoming", "days", "history"} {
		admitted := 0
		for _, item := range items {
			if item.section != section {
				continue
			}
			candidate := clonePage(out)
			appendItem(&candidate, item, false)
			candidateCounts := maps.Clone(counts)
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
				if admitted > 0 || section == freshnessSection {
					break
				}
				// A section's first row always makes progress, using a minimal identity
				// stub when the complete row exceeds its share. It may borrow from the
				// remaining budget, then shorten optional relationship text, before refusing.
				candidate = clonePage(out)
				appendItem(&candidate, item, true)
				if err = preparePage(&candidate, &original, totals, candidateCounts, format); err != nil {
					return nil, err
				}
				if _, err = fitPersonText(&candidate, &original, totals, candidateCounts, format, budget); err != nil {
					return nil, err
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
			if section != "open_loops" && section != freshnessSection && counts[section] < total {
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

// fitPersonText measures a page that must be sent, halving the optional relationship
// brief and then the longest agenda title until it fits the budget. The runes
// cut are reported under Omitted.Budget.
func fitPersonText(out, original *Timeline, totals, counts map[string]int, format string, budget int) (int, error) {
	if p := out.Person; p != nil {
		header := *p
		header.Agenda = slices.Clone(p.Agenda)
		if p.Brief != nil {
			brief := *p.Brief
			header.Brief = &brief
		}
		out.Person = &header
	}
	for {
		size, err := timelineSize(out, format)
		if err != nil || size <= budget {
			return size, err
		}
		key, text := personTextToCut(out.Person)
		if text == nil {
			return 0, ErrBudgetTooSmall
		}
		runes := utf8.RuneCountInString(text.Text)
		boundText(text, runes/2)
		if original.Omitted.Budget == nil {
			original.Omitted.Budget = map[string]any{}
		}
		cut, _ := original.Omitted.Budget[key].(int)
		original.Omitted.Budget[key] = cut + runes - runes/2
		if err = preparePage(out, original, totals, counts, format); err != nil {
			return 0, err
		}
	}
}
func personTextToCut(p *PersonHeader) (string, *UntrustedText) {
	if p == nil {
		return "", nil
	}
	if p.Brief != nil && p.Brief.Text != "" {
		return "brief_text", p.Brief
	}
	var longest *UntrustedText
	for i := range p.Agenda {
		if p.Agenda[i].Text != "" && (longest == nil || len(p.Agenda[i].Text) > len(longest.Text)) {
			longest = &p.Agenda[i]
		}
	}
	if longest == nil {
		return "", nil
	}
	return "agenda_text", longest
}

type pageItem struct {
	section, part, date string
	entry               *Entry
	summary             *DaySummary
	source              *SourceFreshness
	projection          *ProjectionFreshness
	warning             *string
}

func timelineItems(t *Timeline) []pageItem {
	var out []pageItem
	for i := range t.Freshness.Sources {
		out = append(out, pageItem{section: freshnessSection, source: &t.Freshness.Sources[i]})
	}
	for i := range t.Freshness.Projections {
		out = append(out, pageItem{section: freshnessSection, projection: &t.Freshness.Projections[i]})
	}
	for i := range t.Freshness.Warnings {
		out = append(out, pageItem{section: freshnessSection, warning: &t.Freshness.Warnings[i]})
	}
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
	case freshnessSection:
		switch {
		case item.source != nil:
			t.Freshness.Sources = append(t.Freshness.Sources, *item.source)
		case item.projection != nil:
			t.Freshness.Projections = append(t.Freshness.Projections, *item.projection)
		case item.warning != nil:
			t.Freshness.Warnings = append(t.Freshness.Warnings, *item.warning)
		}
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
		}
	}
}
func entryStub(e Entry) Entry {
	e.Truncated = true
	if e.Meeting != nil {
		p := *e.Meeting
		p.Title = UntrustedText{Truncated: true}
		p.Attendees, p.People = nil, nil
		p.AttendeesTruncated, p.PeopleTruncated = p.AttendeeCount > 0, p.PeopleCount > 0
		p.LinkedTranscriptIDs, p.PossiblyRelated = nil, nil
		e.Meeting = &p
	}
	if e.Burst != nil {
		p := *e.Burst
		p.People, p.LatestMessageIDs, p.Copies = nil, nil, nil
		p.PeopleTruncated = p.PeopleCount > 0
		p.Snippet = UntrustedText{Truncated: true}
		e.Burst = &p
	}
	if e.Message != nil {
		p := *e.Message
		p.People = nil
		p.PeopleTruncated = p.PeopleCount > 0
		p.Snippet = UntrustedText{Truncated: true}
		e.Message = &p
	}
	if e.Call != nil {
		p := *e.Call
		p.People = nil
		p.PeopleTruncated = p.PeopleCount > 0
		e.Call = &p
	}
	if e.Loop != nil {
		p := *e.Loop
		p.Person.Name, p.Person.Truncated = "", true
		e.Loop = &p
	}
	return e
}

func clonePage(t Timeline) Timeline {
	t.Freshness.Sources = append([]SourceFreshness(nil), t.Freshness.Sources...)
	t.Freshness.Projections = append([]ProjectionFreshness(nil), t.Freshness.Projections...)
	t.Freshness.Warnings = append([]string(nil), t.Freshness.Warnings...)
	t.History = append([]Entry(nil), t.History...)
	t.Upcoming = append([]Entry(nil), t.Upcoming...)
	t.Days = append([]Day(nil), t.Days...)
	for i := range t.Days {
		d := &t.Days[i]
		d.Meetings = append([]Entry(nil), d.Meetings...)
		d.Bursts = append([]Entry(nil), d.Bursts...)
		d.Calls = append([]Entry(nil), d.Calls...)
	}
	if t.OpenLoops != nil {
		loops := *t.OpenLoops
		loops.Items = append([]Entry(nil), loops.Items...)
		t.OpenLoops = &loops
	}
	t.Omitted.Budget = maps.Clone(t.Omitted.Budget)
	return t
}
func preparePage(t, original *Timeline, totals, counts map[string]int, format string) error {
	t.Omitted.Budget = make(map[string]any, len(original.Omitted.Budget))
	maps.Copy(t.Omitted.Budget, original.Omitted.Budget)
	truncated := 0
	for _, item := range timelineItems(t) {
		if item.entry != nil && item.entry.Truncated {
			truncated++
		}
	}
	if truncated > 0 {
		t.Omitted.Budget["entry_fields"] = truncated
	}
	needsCursor := false
	for section, total := range totals {
		lost := total - counts[section]
		if lost > 0 {
			if section != "open_loops" && section != freshnessSection {
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
			if section != "open_loops" && section != freshnessSection {
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
	positions := make(map[string]string, len(c.Positions))
	maps.Copy(positions, c.Positions)
	delete(positions, "open_loops")
	for _, item := range timelineItems(t) {
		if item.section == "open_loops" || item.section == freshnessSection {
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
			sortKey = chronologicalKey(*item.entry)
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
	b, err := json.Marshal(t, jsontext.EscapeForHTML(true), jsontext.AllowInvalidUTF8(true))
	return len(b), err
}
func boundText(t *UntrustedText, n int) {
	prefix := textutil.PrefixRunes(t.Text, n)
	if prefix != t.Text {
		t.Text = prefix
		t.Truncated = true
	}
}
func boundPerson(p Person) Person {
	prefix := textutil.PrefixRunes(p.Name, 80)
	if prefix != p.Name {
		p.Name = prefix
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
	for i := range t.Gaps {
		gap := &t.Gaps[i]
		if len(gap.Items) > 10 {
			if t.Omitted.Budget == nil {
				t.Omitted.Budget = map[string]any{}
			}
			dropped, _ := t.Omitted.Budget["gap_items"].(int)
			t.Omitted.Budget["gap_items"] = dropped + len(gap.Items) - 10
			gap.Items = gap.Items[:10]
		}
	}
	if len(t.Candidates) > 10 {
		t.Candidates = t.Candidates[:10]
		t.Candidates[9].Truncated = true
	}
	if p := t.Person; p != nil {
		if len(p.Agenda) > 5 {
			if t.Omitted.Budget == nil {
				t.Omitted.Budget = map[string]any{}
			}
			t.Omitted.Budget["agenda"] = len(p.Agenda) - 5
			p.Agenda = p.Agenda[:5]
		}
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
		}
		if p := e.Burst; p != nil {
			boundText(&p.Snippet, 80)
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
		}
		if p := e.Message; p != nil {
			boundText(&p.Snippet, 80)
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
		}
		if p := e.Call; p != nil {
			p.People, p.PeopleCount, p.PeopleTruncated = boundPeople(p.People, p.PeopleCount)
		}
		if p := e.Loop; p != nil {
			p.Person = boundPerson(p.Person)
		}
	}
}
