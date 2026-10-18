package personcard

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/textutil"
)

var digits = regexp.MustCompile(`^[0-9]+$`)
var qualified = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}#[A-Za-z0-9]{1,64}$`)

// ValidCitationRef accepts only structural references the renderer can emit.
func ValidCitationRef(ref string) bool {
	kind, value, ok := strings.Cut(ref, ":")
	if !ok {
		return false
	}
	if kind == "kata" {
		return qualified.MatchString(value)
	}
	if kind == "brief" {
		a, b, ok := strings.Cut(value, "/")
		return ok && positiveID(a) && positiveID(b)
	}
	number, key, hasKey := strings.Cut(value, "@")
	if !positiveID(number) {
		return false
	}
	switch kind {
	case "message", "msg", "meeting", "fact-decision", "attribute-value":
		return !hasKey
	case "evt":
		if !hasKey {
			return true
		}
		_, err := calrecur.ParseKey(key)
		return err == nil
	default:
		return false
	}
}
func positiveID(s string) bool {
	if !digits.MatchString(s) {
		return false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return err == nil && id > 0 && id <= 9007199254740991
}
func validEvidence(ref string) bool {
	return ValidCitationRef(ref) && (strings.HasPrefix(ref, "message:") || strings.HasPrefix(ref, "msg:") || strings.HasPrefix(ref, "meeting:") || strings.HasPrefix(ref, "evt:"))
}
func clean(s string, n int) (string, bool) {
	s = textutil.SanitizeTerminal(strings.ToValidUTF8(s, "�"))
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]), true
	}
	return s, false
}
func enum(s string, values ...string) string {
	if slices.Contains(values, s) {
		return s
	}
	return ""
}
func cappedStrings(in []string, n int) ([]string, bool) {
	out := make([]string, 0, min(n, len(in)))
	more := len(in) > n
	for _, s := range in[:min(n, len(in))] {
		v, cut := clean(s, 120)
		out = append(out, v)
		more = more || cut
	}
	return out, more
}
func date(t time.Time) string {
	if t.IsZero() {
		return "undated"
	}
	return t.UTC().Format("2006-01-02")
}
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}
func cite(kind, ref string, at time.Time, base string) Cite {
	c := Cite{Kind: kind, Ref: ref, At: timePtr(at)}
	if !ValidCitationRef(ref) {
		return Cite{Kind: kind}
	}
	c.WebURL = webURL(ref, base)
	return c
}
func webURL(ref, base string) string {
	if !validEvidence(ref) {
		return ""
	}
	_, value, _ := strings.Cut(ref, ":")
	id, _, _ := strings.Cut(value, "@")
	return strings.TrimRight(base, "/") + "/messages/" + id
}

// Assemble selects from bounded current snapshots; as_of only evaluates dates.
func Assemble(in Inputs, opts Options) Card {
	if opts.AsOf.IsZero() {
		opts.AsOf = in.CollectedAt
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = DefaultMaxItems
	}
	opts.MaxItems = min(opts.MaxItems, 10)
	p := in.Person
	p.ContentTrust = "source_controlled"
	p.UntrustedText.DisplayName, p.UntrustedText.DisplayNameMore = clean(p.UntrustedText.DisplayName, 120)
	p.UntrustedText.OrganizationName, p.UntrustedText.OrganizationNameMore = clean(p.UntrustedText.OrganizationName, 120)
	p.UntrustedText.Title, p.UntrustedText.TitleMore = clean(p.UntrustedText.Title, 120)
	p.UntrustedText.Categories, p.CategoriesMore = cappedStrings(p.UntrustedText.Categories, 5)
	p.Emails, p.EmailsMore = cappedStrings(p.Emails, 5)
	p.VCardUID, p.VCardUIDMore = clean(p.VCardUID, 120)
	p.PrimaryChannel = channel(p.PrimaryChannel)
	p.InferredChannel = channel(p.InferredChannel)
	in.Freshness.CollectedAt = in.CollectedAt
	if in.ContactState != nil {
		in.Freshness.ContactStateStale = in.ContactState.Stale
		in.Freshness.ActivityComputedAt = timePtr(in.ContactState.ComputedAt)
	}
	in.Freshness.OpenLoopsComputedAt = timePtr(in.Loops.ComputedAt)
	if in.Brief != nil {
		in.Freshness.BriefComputedAt = timePtr(in.Brief.GeneratedAt)
	}
	c := Card{Person: p, AsOf: opts.AsOf.UTC(), Items: []Item{}, Coverage: in.Coverage, Meetings: Meetings{WindowDays: 365, CountByBasis: map[string]int{}, Recent: []RecentMeeting{}}, Freshness: buildFreshness(in.Freshness), Gaps: []string{}, Excluded: []string{"sensitive_attributes", "notes", "media"}, affiliation: enum(opts.Affiliation, "internal", "external")}
	for _, g := range in.Gaps {
		if _, ok := gapLabels[g]; ok && !slices.Contains(c.Gaps, g) {
			c.Gaps = append(c.Gaps, g)
		}
	}
	if !p.Tracked {
		c.addGap("not_tracked")
	}
	c.buildContact(in.ContactState, opts.WebBaseURL)
	c.buildBrief(in.Brief)
	mirrors := map[string]bool{}
	c.Coverage.LoopsTruncated = c.Coverage.LoopsTruncated || in.Loops.Truncated || len(in.Loops.Loops) > 20
	loopCount := 0
	for _, l := range in.Loops.Loops[:min(20, len(in.Loops.Loops))] {
		if l.Status != "open" || !positiveID(l.ID) {
			continue
		}
		if len(l.Evidence) == 0 || !validEvidence(l.Evidence[0].Ref) {
			c.Coverage.LoopsInvalidEvidence++
			continue
		}
		at := l.LastActivityAt
		role := "observed"
		if l.DueAt != nil {
			at = *l.DueAt
			role = "due"
		}
		item := Item{ID: "L" + l.ID, Kind: "open_loop", Subkind: enum(l.Kind, "reply_owed", "awaiting_reply", "commitment"), Date: date(at), DateRole: role, Stale: c.Freshness.SourceStale(l.SourceID), ContentTrust: "may_carry_third_party_message_text", Cite: cite(evidenceKind(l.Evidence[0].Ref), l.Evidence[0].Ref, l.Evidence[0].At, opts.WebBaseURL), when: at}
		item.UntrustedText.Text, item.UntrustedText.TextMore = clean(l.Summary, 300)
		c.addItem(item)
		loopCount++
		if qualified.MatchString(l.KataRef) {
			mirrors[l.KataRef] = true
		}
	}
	if loopCount == 0 {
		c.emptyGap("open_loops")
	}
	c.Coverage.Actions.Truncated = c.Coverage.Actions.Truncated || len(in.Actions) > 200
	actionCount := 0
	for _, a := range in.Actions[:min(200, len(in.Actions))] {
		if a.Status != "pending" || !positiveID(strconv.FormatInt(a.MessageID, 10)) || a.Ordinal < 0 {
			continue
		}
		at := a.OccurredAt
		role := "observed"
		due, err := time.Parse("2006-01-02", a.DueDate)
		if err != nil {
			due, err = time.Parse(time.RFC3339, a.DueDate)
		}
		if err == nil {
			at = due
			role = "due"
		}
		if at.IsZero() {
			role = "undated"
		}
		item := Item{ID: fmt.Sprintf("A%d:%d", a.MessageID, a.Ordinal), Kind: "meeting_action", Date: date(at), DateRole: role, Stale: c.Freshness.SourceStale(a.SourceID), ContentTrust: "may_carry_third_party_message_text", AssigneeClass: enum(a.AssigneeClass, "owner", "person", "other", "none"), Cite: cite("meeting", fmt.Sprintf("message:%d", a.MessageID), a.OccurredAt, opts.WebBaseURL), when: at, ordinal: a.Ordinal, meetingID: a.MessageID}
		if item.AssigneeClass == "" {
			if a.Assignee == "" {
				item.AssigneeClass = "none"
			} else {
				item.AssigneeClass = "other"
			}
		}
		item.UntrustedText.Text, item.UntrustedText.TextMore = clean(a.Text, 300)
		item.UntrustedText.Assignee, item.UntrustedText.AssigneeMore = clean(a.Assignee, 120)
		item.UntrustedText.StatusLabel = "pending"
		c.addItem(item)
		actionCount++
	}
	if actionCount == 0 {
		c.emptyGap("actions")
	}
	var fact *FactChange
	for i := range in.Facts {
		f := in.Facts[i]
		if !positiveID(strconv.FormatInt(f.ID, 10)) || f.Sensitive || (!f.User && (!f.Supported || f.EvidenceRef != "" && !validEvidence(f.EvidenceRef))) || f.ChangedAt.Before(opts.AsOf.Add(-90*24*time.Hour)) || f.ChangedAt.After(opts.AsOf) {
			continue
		}
		if fact == nil || f.ChangedAt.After(fact.ChangedAt) || (f.ChangedAt.Equal(fact.ChangedAt) && (f.User && !fact.User || f.User == fact.User && f.ID < fact.ID)) {
			fact = &f
		}
	}
	if fact == nil {
		c.emptyGap("facts")
	} else {
		f := *fact
		id := fmt.Sprintf("F%d", f.ID)
		ref := fmt.Sprintf("fact-decision:%d", f.ID)
		trust := "derived_from_third_party_messages"
		kind := "fact_decision"
		if f.User {
			id = fmt.Sprintf("U%d", f.ID)
			ref = fmt.Sprintf("attribute-value:%d", f.ID)
			trust = "user"
			kind = "attribute_value"
		}
		item := Item{ID: id, Kind: "fact_change", Date: date(f.ChangedAt), DateRole: "changed", ContentTrust: trust, Cite: cite(kind, ref, f.ChangedAt, opts.WebBaseURL), when: f.ChangedAt}
		if !f.User {
			item.Cite.EvidenceRef = f.EvidenceRef
			item.Cite.WebURL = webURL(f.EvidenceRef, opts.WebBaseURL)
		}
		for _, id := range f.SourceIDs {
			item.Stale = item.Stale || c.Freshness.SourceStale(id)
		}
		item.UntrustedText.Text, item.UntrustedText.TextMore = clean(f.Text, 300)
		item.UntrustedText.Label, item.UntrustedText.LabelMore = clean(f.Label, 120)
		c.addItem(item)
	}
	c.Coverage.AgendaTruncated = c.Coverage.AgendaTruncated || len(in.Agenda) > 100
	agendaCount := 0
	for _, a := range in.Agenda[:min(100, len(in.Agenda))] {
		if !qualified.MatchString(a.QualifiedRef) || mirrors[a.QualifiedRef] {
			continue
		}
		item := Item{ID: "K" + a.QualifiedRef, Kind: "agenda", Date: "undated", DateRole: "undated", ContentTrust: "may_carry_third_party_message_text", Cite: Cite{Kind: "kata", Ref: "kata:" + a.QualifiedRef}, priority: a.Priority}
		item.UntrustedText.Text, item.UntrustedText.TextMore = clean(a.Title, 300)
		item.UntrustedText.List, item.UntrustedText.ListMore = clean(a.List, 120)
		item.UntrustedText.WebURL, item.UntrustedText.WebURLMore = clean(a.WebURL, 120)
		c.addItem(item)
		agendaCount++
	}
	if agendaCount == 0 {
		c.emptyGap("agenda")
	}
	slices.SortStableFunc(c.Items, compareItems)
	c.ItemsDroppedByRank = max(0, len(c.Items)-opts.MaxItems)
	c.Items = c.Items[:min(len(c.Items), opts.MaxItems)]
	for i := range c.Items {
		c.Items[i].Rank = i + 1
	}
	c.buildMeetings(in.Meetings, in.CollectedAt)
	return c
}
func channel(s string) string {
	return enum(s, "email", "chat", "text", "sms", "whatsapp", "imessage", "telegram", "slack", "signal", "discord", "calendar", "meeting", "phone", "in_person", "other", "unknown")
}
func (c *Card) addItem(i Item) {
	i.Overdue = i.DateRole == "due" && i.Date < date(c.AsOf)
	c.Items = append(c.Items, i)
}
func (c *Card) addGap(g string) {
	if !slices.Contains(c.Gaps, g) {
		c.Gaps = append(c.Gaps, g)
	}
}
func (c *Card) emptyGap(prefix string) {
	if slices.Contains(c.Gaps, prefix+"_unavailable") || prefix == "agenda" && slices.Contains(c.Gaps, "agenda_not_configured") {
		return
	}
	c.addGap(prefix + "_none")
}
func (c *Card) buildContact(v *ContactState, base string) {
	if v == nil {
		c.emptyGap("contact_state")
		return
	}
	dir := "unknown"
	if v.LastContactAt != nil {
		if v.LastInboundAt != nil && v.LastInboundAt.Equal(*v.LastContactAt) {
			dir = "inbound"
		} else if v.LastOutboundAt != nil && v.LastOutboundAt.Equal(*v.LastContactAt) {
			dir = "outbound"
		}
	}
	ref := v.Ref
	if !validEvidence(ref) {
		ref = ""
	}
	status := enum(v.CadenceStatus, "ok", "overdue", "unknown")
	if status == "" {
		status = "unknown"
	}
	c.LastInteraction = &LastInteraction{At: v.LastContactAt, Channel: channel(v.Channel), Direction: dir, Ref: ref, WebURL: webURL(ref, base), LastInboundAt: v.LastInboundAt, LastOutboundAt: v.LastOutboundAt, InteractionCount: v.InteractionCount, CadenceStatus: status, CadenceDueAt: v.CadenceDueAt, CadenceOverdueAtAsOf: v.CadenceDueAt != nil && v.CadenceDueAt.Before(c.AsOf), ProjectionComputedAt: v.ComputedAt.UTC(), ProjectionStale: v.Stale || c.Freshness.Projections["activity"] != nil && c.Freshness.Projections["activity"].Stale}
}
func (c *Card) buildBrief(v *Brief) {
	if v == nil {
		c.emptyGap("brief")
		return
	}
	b := *v
	b.ContentTrust = "derived_from_third_party_messages"
	b.CitationsCount = len(b.Citations)
	b.Citations, b.CitationsMore = briefRefs(v.Citations, 20)
	b.Evidence, b.EvidenceMore = briefRefs(v.Evidence, 20)
	if v.UntrustedText != nil {
		t := *v.UntrustedText
		t.Text, t.TextMore = clean(t.Text, 300)
		b.UntrustedText = &t
	}
	c.Brief = &b
}
func briefRefs(in []string, n int) ([]string, bool) {
	out := []string{}
	for _, s := range in[:min(n, len(in))] {
		if validEvidence(s) {
			out = append(out, s)
		}
	}
	return out, len(in) > n
}
func (c *Card) buildMeetings(in []Meeting, collected time.Time) {
	c.Coverage.MeetingsTruncated = c.Coverage.MeetingsTruncated || len(in) > 100
	list := slices.Clone(in[:min(100, len(in))])
	slices.SortFunc(list, func(a, b Meeting) int {
		if n := b.Start.Compare(a.Start); n != 0 {
			return n
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	end := c.AsOf
	if collected.Before(end) {
		end = collected
	}
	start := c.AsOf.Add(-365 * 24 * time.Hour)
	for _, m := range list {
		basis := enum(m.Basis, "invited", "transcript", "notes", "note")
		if m.Cancelled || m.Start.Before(start) || !m.Start.Before(end) || !validEvidence(m.Ref) || basis == "" {
			continue
		}
		c.Meetings.Count++
		c.Meetings.CountByBasis[basis]++
		if (basis == "invited" || m.Invited) && c.Meetings.LastInvitedAt == nil {
			c.Meetings.LastInvitedAt = timePtr(m.Start)
		}
		if basis != "invited" && m.Participated && c.Meetings.LastMetAt == nil {
			c.Meetings.LastMetAt = timePtr(m.Start)
		}
		if len(c.Meetings.Recent) < 3 {
			title, more := clean(m.Title, 120)
			c.Meetings.Recent = append(c.Meetings.Recent, RecentMeeting{Ref: m.Ref, Start: m.Start.UTC(), Basis: basis, ContentTrust: "source_controlled", UntrustedText: MeetingText{Title: title, TitleMore: more}})
		} else {
			c.Meetings.RecentMore = true
		}
	}
	if c.Meetings.Count == 0 {
		c.emptyGap("meetings")
	}
}
func itemGroup(i Item) int {
	if i.DateRole == "due" {
		if i.Overdue {
			return 0
		}
		return 1
	}
	switch i.Kind {
	case "meeting_action":
		return 2
	case "fact_change":
		return 3
	case "open_loop":
		return 4
	default:
		return 5
	}
}
func compareItems(a, b Item) int {
	ga, gb := itemGroup(a), itemGroup(b)
	if ga != gb {
		return ga - gb
	}
	switch ga {
	case 0, 1:
		if n := a.when.Compare(b.when); n != 0 {
			return n
		}
	case 2:
		if a.meetingID == b.meetingID {
			order := map[string]int{"owner": 0, "person": 1, "other": 2, "none": 3}
			if n := order[a.AssigneeClass] - order[b.AssigneeClass]; n != 0 {
				return n
			}
			if a.ordinal != b.ordinal {
				return a.ordinal - b.ordinal
			}
		}
		if n := b.when.Compare(a.when); n != 0 {
			return n
		}
	case 4:
		if n := b.when.Compare(a.when); n != 0 {
			return n
		}
	case 5:
		if a.priority == nil && b.priority != nil {
			return 1
		}
		if a.priority != nil && b.priority == nil {
			return -1
		}
		if a.priority != nil && b.priority != nil && *a.priority != *b.priority {
			return *a.priority - *b.priority
		}
	}
	return strings.Compare(a.ID, b.ID)
}

func evidenceKind(ref string) string {
	if strings.HasPrefix(ref, "message:") {
		return "message"
	}
	return "meeting"
}
