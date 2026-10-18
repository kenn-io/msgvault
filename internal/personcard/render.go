package personcard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

var gapLabels = map[string]string{
	"not_a_saved_profile": "not a saved profile", "not_in_archive": "not in the archive", "not_tracked": "contact tracking", "contact_state_none": "contact state", "contact_state_unavailable": "contact state (not available)", "brief_none": "brief", "brief_unavailable": "brief (not available)", "open_loops_none": "open loops", "open_loops_unavailable": "open loops (not available on this daemon)", "actions_none": "meeting actions", "actions_unavailable": "meeting actions (not available)", "facts_none": "recent fact changes", "facts_unavailable": "recent fact changes (not available)", "agenda_none": "agenda", "agenda_not_configured": "agenda (not configured)", "agenda_unavailable": "agenda (not available)", "meetings_none": "meeting history", "meetings_unavailable": "meeting history (not available)", "sources_unavailable": "source freshness (not available)",
}

// EscapeMarkdown quotes every source-controlled string as inert, single-line data.
func EscapeMarkdown(s string) string {
	s, _ = clean(s, len([]rune(s)))
	s = strings.Join(strings.Fields(s), " ")
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '`':
			b.WriteByte('\'')
		case '[', '<':
			b.WriteByte('(')
		case ']', '>':
			b.WriteByte(')')
		case '*', '_', '#', '|', '~', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
func renderedCite(c Cite) string {
	if !ValidCitationRef(c.Ref) {
		return ""
	}
	if c.EvidenceRef != "" && validEvidence(c.EvidenceRef) {
		return "[" + c.Ref + " ← " + c.EvidenceRef + "]"
	}
	return "[" + c.Ref + "]"
}

// RenderMarkdown displays only validated citations and quarantined quoted text.
func RenderMarkdown(card Card, affiliation string) string {
	var b strings.Builder
	p := card.Person.UntrustedText
	fmt.Fprintf(&b, "### %s", EscapeMarkdown(p.DisplayName))
	if p.OrganizationName != "" || p.Title != "" {
		b.WriteString(" — ")
		if p.OrganizationName != "" {
			b.WriteString(EscapeMarkdown(p.OrganizationName))
		}
		if p.Title != "" {
			if p.OrganizationName != "" {
				b.WriteString(", ")
			}
			b.WriteString(EscapeMarkdown(p.Title))
		}
	}
	if enum(affiliation, "internal", "external") != "" {
		fmt.Fprintf(&b, " · %s", affiliation)
	}
	b.WriteByte('\n')
	if l := card.LastInteraction; l == nil || l.At == nil {
		b.WriteString("Last interaction: none recorded\n")
	} else {
		fmt.Fprintf(&b, "Last interaction: %s %s %s", date(*l.At), enum(l.Direction, "inbound", "outbound", "unknown"), channel(l.Channel))
		if ValidCitationRef(l.Ref) {
			fmt.Fprintf(&b, " [%s]", l.Ref)
		}
		fmt.Fprintf(&b, " · %d interactions · cadence %s", l.InteractionCount, enum(l.CadenceStatus, "ok", "overdue", "unknown"))
		if l.CadenceDueAt != nil {
			fmt.Fprintf(&b, " (due %s)", date(*l.CadenceDueAt))
		}
		b.WriteByte('\n')
	}
	for _, i := range card.Items {
		fmt.Fprintf(&b, "%d. ", i.Rank)
		if i.Overdue {
			fmt.Fprintf(&b, "**overdue %s**", i.Date)
		} else {
			b.WriteString(i.Date)
		}
		b.WriteString(" · ")
		switch i.Kind {
		case "open_loop":
			fmt.Fprintf(&b, "open loop (%s): %s", strings.ReplaceAll(enum(i.Subkind, "reply_owed", "awaiting_reply", "commitment"), "_", " "), EscapeMarkdown(i.UntrustedText.Text))
		case "meeting_action":
			fmt.Fprintf(&b, "meeting action (pending, %s): %s", enum(i.AssigneeClass, "owner", "person", "other", "none"), EscapeMarkdown(i.UntrustedText.Text))
		case "fact_change":
			fmt.Fprintf(&b, "changed %s: %s", EscapeMarkdown(i.UntrustedText.Label), EscapeMarkdown(i.UntrustedText.Text))
		case "agenda":
			fmt.Fprintf(&b, "agenda: %s", EscapeMarkdown(i.UntrustedText.Text))
		}
		if i.Stale {
			b.WriteString(" (source stale)")
		}
		if citation := renderedCite(i.Cite); citation != "" {
			b.WriteByte(' ')
			b.WriteString(citation)
		}
		b.WriteByte('\n')
	}
	m := card.Meetings
	meetingCount := strconv.Itoa(m.Count)
	if card.Coverage.MeetingsTruncated {
		meetingCount = "at least " + meetingCount
	}
	fmt.Fprintf(&b, "Meetings: %s in the last %d days", meetingCount, m.WindowDays)
	if m.Count > 0 {
		fmt.Fprintf(&b, " (%d invited, %d with notes)", m.CountByBasis["invited"], m.Count-m.CountByBasis["invited"])
	}
	if m.LastMetAt != nil {
		fmt.Fprintf(&b, " · last met %s", date(*m.LastMetAt))
	}
	if m.LastInvitedAt != nil {
		fmt.Fprintf(&b, " · last invited %s", date(*m.LastInvitedAt))
	}
	if len(m.Recent) > 0 {
		r := m.Recent[0]
		fmt.Fprintf(&b, " %s", EscapeMarkdown(r.UntrustedText.Title))
		if validEvidence(r.Ref) {
			fmt.Fprintf(&b, " [%s]", r.Ref)
		}
	}
	b.WriteByte('\n')
	if card.Coverage.MeetingsTruncated {
		b.WriteString("Meeting history is incomplete; some meetings or participation details may be omitted.\n")
	}
	if brief := card.Brief; brief != nil && brief.UntrustedText != nil {
		fmt.Fprintf(&b, "Brief (v%d, generated %s, chat and text only): %s", brief.Version, date(brief.GeneratedAt), EscapeMarkdown(brief.UntrustedText.Text))
		ref := fmt.Sprintf("brief:%d/%d", card.Person.PersonID, brief.Version)
		if ValidCitationRef(ref) {
			fmt.Fprintf(&b, " [%s]", ref)
		}
		b.WriteByte('\n')
	}
	if card.Freshness.Stale {
		b.WriteString("Source freshness: stale\n")
		for _, s := range card.Freshness.Sources {
			if s.Stale {
				fmt.Fprintf(&b, "Source %s: stale", EscapeMarkdown(s.Identifier))
				if s.SyncCommand != "" {
					fmt.Fprintf(&b, " · %s", EscapeMarkdown(s.SyncCommand))
				} else if s.SyncUnavailableReason != "" {
					fmt.Fprintf(&b, " · %s", EscapeMarkdown(s.SyncUnavailableReason))
				}
				b.WriteByte('\n')
			}
		}
	}
	if len(card.Gaps) > 0 {
		labels := []string{}
		for _, g := range card.Gaps {
			if label, ok := gapLabels[g]; ok && !slices.Contains(labels, label) {
				labels = append(labels, label)
			}
		}
		if len(labels) > 0 {
			b.WriteString("No data: ")
			b.WriteString(strings.Join(labels, "; "))
			b.WriteByte('\n')
		}
	}
	return b.String()
}
