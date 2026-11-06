package timeline

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Markdown renders compact rows. Sender-controlled text is flattened and escaped.
func Markdown(t *Timeline) string {
	text, _ := MarkdownWithEntries(t)
	return text
}

// RenderedEntry locates a selectable entry in the rendered Markdown.
// Several entries can share one zero-based line, such as missed calls.
type RenderedEntry struct {
	Entry Entry
	Line  int
}

// MarkdownWithEntries renders Markdown and records each entry's line.
func MarkdownWithEntries(t *Timeline) (string, []RenderedEntry) {
	if t == nil {
		return "", nil
	}
	var b strings.Builder
	var entries []RenderedEntry
	record := func(e Entry) {
		entries = append(entries, RenderedEntry{Entry: e, Line: b.Len()})
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		loc = time.UTC
	}
	zone := t.Timezone
	if t.TimezoneIsDefault {
		zone += " (set [activity].timezone)"
	}
	oneDay := t.Mode == ModeDay && t.Window.From == t.Window.To
	heading := t.Window.To
	if oneDay {
		date, _ := time.Parse(time.DateOnly, t.Window.To)
		heading = date.Format("Mon 2006-01-02")
	}
	if t.Window.From != t.Window.To {
		heading = t.Window.From + " – " + t.Window.To
	}
	if t.Mode == ModeRelationship && t.Person != nil {
		heading = personName(t.Person.Person)
	}
	if t.Mode == ModeAnchor && t.Anchor != nil {
		heading = "Around " + FormatShortRef(t.Anchor.Ref)
	}
	fmt.Fprintf(&b, "# %s · %s\n", heading, zone)
	for _, s := range t.Freshness.Sources {
		if s.Reason != "" || s.Outcome == "failed" || s.Outcome == "cancelled" || s.Outcome == "paused" || s.Scheduling == "disabled" || s.LastSyncErrors > 0 {
			fmt.Fprintf(&b, "⚠ %s %s", singleLine(strings.TrimSuffix(s.SourceType+":"+s.Identifier, ":"), 80), singleLine(cmp.Or(s.Reason, s.Outcome, "current"), 80))
			if s.Reason != "" && (s.Outcome == "failed" || s.Outcome == "cancelled" || s.Outcome == "paused" || s.Outcome == "partial") {
				fmt.Fprintf(&b, " %s", singleLine(s.Outcome, 80))
			}
			if s.SyncCommand != "" {
				fmt.Fprintf(&b, " → %s", singleLine(s.SyncCommand, 160))
			}
			b.WriteByte('\n')
		}
	}
	for _, p := range t.Freshness.Projections {
		if p.State != "" && p.State != "current" && p.State != "complete" {
			fmt.Fprintf(&b, "⚠ %s %s\n", singleLine(p.Name, 80), singleLine(p.State, 80))
		}
	}
	for _, warning := range t.Freshness.Warnings {
		if strings.HasPrefix(warning, "The latest sync ") {
			continue
		}
		fmt.Fprintf(&b, "⚠ %s\n", singleLine(warning, 160))
	}
	if p := t.Person; p != nil && t.Mode == ModeRelationship {
		fmt.Fprintf(&b, "%s · %s · %d interactions\n", singleLine(p.Ref, 80), strings.Join(p.Channels, ", "), p.InteractionCount)
		fmt.Fprintf(&b, "first %s · last %s · ↓ %s · ↑ %s\n", dateText(p.FirstAt, loc), dateText(p.LastAt, loc), dateText(p.LastInboundAt, loc), dateText(p.LastOutboundAt, loc))
		if p.CadenceStatus != "" {
			fmt.Fprintf(&b, "cadence %s · due %s\n", singleLine(p.CadenceStatus, 80), dateText(p.DueAt, loc))
		}
		fmt.Fprintf(&b, "initiation: contact %d · owner %d\n12m:", p.InitiatedByContact, p.InitiatedByOwner)
		for _, n := range p.Monthly {
			fmt.Fprintf(&b, " %d", n)
		}
		b.WriteByte('\n')
		if p.Brief != nil {
			fmt.Fprintf(&b, "\"%s\"\n", textLine(*p.Brief, 600))
		}
		for _, a := range p.Agenda {
			fmt.Fprintf(&b, "- \"%s\"\n", textLine(a, 80))
		}
	}
	count := 0
	rows := func(entries []Entry) {
		for _, e := range entries {
			record(e)
			fmt.Fprintf(&b, "- %s\n", entryRow(e, loc))
			count++
		}
	}
	if t.OpenLoops != nil {
		fmt.Fprintf(&b, "\nOpen (%d)\n", len(t.OpenLoops.Items))
		rows(t.OpenLoops.Items)
		if t.OpenLoops.Truncated {
			b.WriteString("more available\n")
		}
		if t.OpenLoops.CoverageIncomplete {
			b.WriteString("coverage incomplete\n")
		}
		if t.OpenLoops.SkippedUnparseable > 0 {
			fmt.Fprintf(&b, "%d unparseable\n", t.OpenLoops.SkippedUnparseable)
		}
	}
	for _, d := range t.Days {
		date, _ := time.Parse(time.DateOnly, d.Date)
		label := date.Format("Mon Jan 02")
		if d.Summary != nil {
			fmt.Fprintf(&b, "\n%s · %d meetings · %d people", label, d.Summary.Meetings, max(d.Summary.PeopleCount, len(d.Summary.People)))
			if len(d.Summary.People) > 0 {
				fmt.Fprintf(&b, " (%s)", peopleNames(d.Summary.People, d.Summary.PeopleCount))
			}
			b.WriteByte('\n')
			count++
			continue
		}
		if oneDay {
			b.WriteByte('\n')
		} else {
			fmt.Fprintf(&b, "\n%s\n", label)
		}
		rows(d.Meetings)
		rows(d.Bursts)
		var other []string
		var otherEntries []Entry
		for _, e := range d.Calls {
			if e.Call != nil && e.Call.Outcome != "answered" {
				other = append(other, entryRow(e, loc))
				otherEntries = append(otherEntries, e)
				count++
			} else {
				rows([]Entry{e})
			}
		}
		if len(other) > 0 {
			for _, e := range otherEntries {
				record(e)
			}
			fmt.Fprintf(&b, "- %s\n", strings.Join(other, " · "))
		}
		rows(d.Notes)
	}
	if len(t.History) > 0 {
		b.WriteString("\nHistory\n")
		rows(t.History)
	}
	if len(t.Upcoming) > 0 {
		b.WriteString("\nUpcoming\n")
		for _, e := range t.Upcoming {
			record(e)
			fmt.Fprintf(&b, "- %s %s\n", e.Time.In(loc).Format("Mon"), entryRow(e, loc))
			count++
		}
	}
	if count == 0 {
		b.WriteString("\nnothing archived\n")
	}
	var omitted []string
	for section, n := range t.Omitted.Budget {
		if section == "entry_fields" || section == "open_loops" {
			continue
		}
		omitted = append(omitted, fmt.Sprintf("%s %v", section, n))
	}
	sort.Strings(omitted)
	if t.Omitted.Noise > 0 {
		omitted = append(omitted, fmt.Sprintf("noise %d", t.Omitted.Noise))
	}
	if t.Omitted.Cold > 0 {
		omitted = append(omitted, fmt.Sprintf("cold %d", t.Omitted.Cold))
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\nomitted: %s\n", strings.Join(omitted, " · "))
	}
	var missing, retry []string
	for _, g := range t.Gaps {
		if g.Retryable {
			retry = appendUnique(retry, g.Section)
		} else {
			missing = appendUnique(missing, g.Section)
		}
	}
	if len(missing) > 0 || len(retry) > 0 {
		b.WriteByte('\n')
		if len(missing) > 0 {
			b.WriteString("not shown: " + strings.Join(missing, ", "))
		}
		if len(retry) > 0 {
			if len(missing) > 0 {
				b.WriteString(" · ")
			}
			b.WriteString("retry: " + strings.Join(retry, ", "))
		}
		b.WriteByte('\n')
	}
	if t.Cursor != "" {
		fmt.Fprintf(&b, "\ncursor: %s\n", t.Cursor)
	}
	text := b.String()
	offset, line := 0, 0
	for i := range entries {
		end := entries[i].Line
		line += strings.Count(text[offset:end], "\n")
		entries[i].Line = line
		offset = end
	}
	return text, entries
}
func entryRow(e Entry, loc *time.Location) string {
	ref := FormatShortRef(cmp.Or(e.AnchorRef, e.Ref))
	clock := e.Time.In(loc).Format("15:04")
	var body string
	switch {
	case e.Truncated:
		body = clock + " " + e.Kind + " [details omitted]"
	case e.Meeting != nil:
		m := e.Meeting
		clock = e.Time.In(loc).Format("15:04")
		if m.AllDay {
			clock = "all-day"
		}
		body = clock + " " + textLine(m.Title, 80)
		if m.Status == "cancelled" {
			body += " · cancelled"
		}
		if m.DeletedFromSource {
			body += " · removed from source"
		}
		if m.Attended {
			body += " ✓"
		}
		ps, total := m.Attendees, m.AttendeeCount
		if e.Kind == "transcript" {
			ps, total = m.People, m.PeopleCount
		}
		if len(ps) > 0 {
			body += " " + peopleNames(ps, total)
		}
		if len(m.LinkedTranscriptIDs) > 0 {
			refs := make([]string, 0, len(m.LinkedTranscriptIDs))
			for _, id := range m.LinkedTranscriptIDs {
				refs = append(refs, "#"+strconv.FormatInt(id, 10))
			}
			body += " · notes " + strings.Join(refs, ", ")
		}
		if m.ActionItemCount > 0 {
			body += fmt.Sprintf(" · %d actions", m.ActionItemCount)
		}
		if len(m.PossiblyRelated) > 0 {
			refs := make([]string, 0, len(m.PossiblyRelated))
			for _, ref := range m.PossiblyRelated {
				refs = append(refs, FormatShortRef(ref))
			}
			body += " · possibly related " + strings.Join(refs, ", ")
		}
	case e.Burst != nil:
		p := e.Burst
		body = peopleNames(p.People, p.PeopleCount) + fmt.Sprintf(" · %s ↓%d ↑%d · %s", singleLine(p.Channel, 80), p.Inbound, p.Outbound, clock)
		if !e.HiddenText && p.Snippet.Text != "" {
			body += " \"" + textLine(p.Snippet, 80) + "\""
		}
		copies, groups := 0, 0
		for _, n := range p.Copies {
			if n > 1 {
				copies += n - 1
				groups++
			}
		}
		if groups == 1 {
			body += fmt.Sprintf(" (%d copies)", copies+1)
		} else if groups > 1 {
			body += fmt.Sprintf(" (%d extra copies)", copies)
		}
	case e.Message != nil:
		body = clock + " " + singleLine(e.Message.Direction, 80) + " " + peopleNames(e.Message.People, e.Message.PeopleCount)
		if !e.HiddenText && e.Message.Snippet.Text != "" {
			body += " \"" + textLine(e.Message.Snippet, 80) + "\""
		}
	case e.Call != nil:
		body = clock + " call " + singleLine(e.Call.Outcome, 80) + " " + peopleNames(e.Call.People, e.Call.PeopleCount)
		if e.Call.DurationSeconds != nil {
			body += fmt.Sprintf(" %.0fs", *e.Call.DurationSeconds)
		}
	case e.Loop != nil:
		kind := e.Loop.Kind
		switch kind {
		case "reply_owed":
			kind = "owed"
		case "awaiting_reply":
			kind = "awaiting"
		}
		body = kind + " " + personName(e.Loop.Person) + " " + singleLine(e.Loop.Age, 80)
	case e.Note != nil:
		body = clock + " note \"" + textLine(e.Note.Text, 80) + "\" " + peopleNames(e.Note.People, e.Note.PeopleCount)
	default:
		body = clock + " " + e.Kind
	}
	// Suppress all message-derived displayed text for hidden-text rows.
	if e.HiddenText {
		body = clock + " " + e.Kind + " [hidden text]"
	}
	body += " · " + ref
	if e.Filtered {
		body += " [filtered]"
	}
	if e.Anchor {
		body += " ◀"
	}
	return body
}
func singleLine(s string, limit int) string {
	r := []rune(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " "))
	truncated := len(r) > limit
	if truncated {
		r = r[:limit]
	}
	s = string(r)
	s = strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "\"", "\\\"").Replace(s)
	if truncated {
		s += "…"
	}
	return s
}
func personName(p Person) string {
	s := singleLine(p.Name, 80)
	if p.Truncated && len([]rune(p.Name)) <= 80 {
		s += "…"
	}
	if p.Tier == 1 || p.Tier == 2 {
		s += fmt.Sprintf(" T%d", p.Tier)
	}
	return s
}
func peopleNames(ps []Person, total int) string {
	total = max(total, len(ps))
	var names []string
	for _, p := range ps[:min(8, len(ps))] {
		names = append(names, personName(p))
	}
	if displayed := min(8, len(ps)); total > displayed {
		names = append(names, fmt.Sprintf("+%d", total-displayed))
	}
	return strings.Join(names, ", ")
}
func appendUnique(ss []string, s string) []string {
	if slices.Contains(ss, s) {
		return ss
	}
	return append(ss, s)
}
func dateText(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "unknown"
	}
	return t.In(loc).Format("2006-01-02")
}

func textLine(t UntrustedText, limit int) string {
	s := singleLine(t.Text, limit)
	if t.Truncated && len([]rune(t.Text)) <= limit {
		s += "…"
	}
	return s
}
