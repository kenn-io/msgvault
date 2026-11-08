package importantdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Candidate is an internal, local-only preparation target. It is not a Kata
// payload; callers must revalidate it and check recipient-specific consent.
type Candidate struct {
	OccurrenceKey string
	Fingerprint   string
	SubjectUID    string
	OwnerUID      string
	Kind          string
	Occurrence    time.Time
	Tier          Tier
}

type CandidateStore interface {
	ListPersonsContext(context.Context) ([]store.Person, error)
	ListPersonDatesContext(context.Context, int64, bool) ([]store.PersonDate, error)
	ListPersonAttributeValuesContext(context.Context, int64, store.PersonAttributeQuery) ([]store.PersonAttributeValue, error)
	ListPersonRelationshipsContext(context.Context, int64, store.PersonRelationshipListOptions) ([]store.PersonRelationshipView, error)
	ListImportantDateSubjectsContext(context.Context, string) ([]store.ImportantDateSubject, error)
	ImportantDateIdentityStableContext(context.Context, int64) (bool, error)
	ImportantDateHasOtherPendingContext(context.Context, string, string, string) (bool, error)
}

// Candidates scans current declared facts without inference or external writes.
// Subjects involved in a merge or split are quarantined until reconciliation.
func Candidates(ctx context.Context, st CandidateStore, cfg Config, now time.Time) ([]Candidate, error) {
	return candidates(ctx, st, cfg, now, nil)
}

// Preview reports aggregate reasons for exclusions without exposing date evidence.
// Counts describe evaluation decisions, not necessarily unique people.
type Preview struct {
	Candidates []Candidate    `json:"candidates"`
	Skipped    map[string]int `json:"skipped"`
}

func PreviewCandidates(ctx context.Context, st CandidateStore, cfg Config, now time.Time) (Preview, error) {
	result := Preview{Skipped: make(map[string]int)}
	rows, err := candidates(ctx, st, cfg, now, result.Skipped)
	result.Candidates = rows
	return result, err
}

func candidates(ctx context.Context, st CandidateStore, cfg Config, now time.Time, skipped map[string]int) ([]Candidate, error) {
	skip := func(reason string) {
		if skipped != nil {
			skipped[reason]++
		}
	}
	if !cfg.Enabled || !cfg.UseCadencePresets {
		skip("disabled_or_preset_mapping_not_enabled")
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, err
	}
	people, err := st.ListPersonsContext(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]store.Person, len(people))
	stable := make(map[int64]bool, len(people))
	tiers := make(map[int64]Tier, len(people))
	mapping := map[int]Tier{21: TierA, 60: TierB, 180: TierC, 365: TierD}
	for _, person := range people {
		byID[person.ID] = person
		stable[person.ID], err = st.ImportantDateIdentityStableContext(ctx, person.ID)
		if err != nil {
			return nil, err
		}
		if !stable[person.ID] {
			skip("identity_requires_reconciliation")
			continue
		}
		values, err := st.ListPersonAttributeValuesContext(ctx, person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugContactFrequency})
		if err != nil {
			return nil, err
		}
		if len(values) == 1 && values[0].Source.IsDeclared() && !values[0].ActiveFrom.After(now) && values[0].Value.Integer != nil && *values[0].Value.Integer > 0 && *values[0].Value.Integer <= 365 {
			if tier, ok := TierForCadence(int(*values[0].Value.Integer), mapping); ok {
				tiers[person.ID] = tier
			}
		}
	}
	for _, person := range people {
		if stable[person.ID] {
			if _, ok := tiers[person.ID]; !ok {
				skip("unassigned_or_custom_cadence")
			}
		}
	}
	selected := make(map[string]Candidate)
	add := func(subject, owner, kind string, date store.PartialDate, tier Tier, direct bool) {
		occurrence, ok := NextOccurrence(date, now, location, cfg.LeapDay)
		if !ok {
			skip("no_future_occurrence")
			return
		}
		key := fmt.Sprintf("%s:%s:%d", subject, kind, occurrence.Year())
		candidate := Candidate{Fingerprint: DateFingerprint(date), OccurrenceKey: key, SubjectUID: subject, OwnerUID: owner, Kind: kind, Occurrence: occurrence, Tier: tier}
		previous, exists := selected[key]
		// Direct eligibility wins over inheritance; otherwise choose the most
		// important tier, then stable UID for deterministic duplicate routing.
		if !exists || direct && previous.OwnerUID != subject || (!direct && previous.OwnerUID != subject && (tier < previous.Tier || tier == previous.Tier && owner < previous.OwnerUID)) {
			selected[key] = candidate
		}
	}
	for _, person := range people {
		tier, ok := tiers[person.ID]
		if !ok {
			continue
		}
		dates, err := st.ListPersonDatesContext(ctx, person.ID, true)
		if err != nil {
			return nil, err
		}
		for _, kind := range []store.PersonDateKind{store.PersonDateBirthday, store.PersonDateAnniversary} {
			date, status := SelectDeclaredDate(dates, kind, now)
			if status == DateEligible {
				add("person/"+person.VCardUID, "person/"+person.VCardUID, string(kind), date, tier, true)
			} else {
				skip(string(status))
			}
		}
		children, err := st.ListPersonRelationshipsContext(ctx, person.ID, store.PersonRelationshipListOptions{})
		if err != nil {
			return nil, err
		}
		for _, view := range children {
			rel := view.Relationship
			if rel.TypeSlug != "parent" || rel.SourcePersonID != person.ID || rel.Status != store.RelationshipStatusActive || !rel.Source.IsDeclared() || !relationshipStarted(rel.StartDate, now, location) || !stable[view.CounterpartPersonID] {
				continue
			}
			child, ok := byID[view.CounterpartPersonID]
			if !ok {
				continue
			}
			childDates, err := st.ListPersonDatesContext(ctx, child.ID, true)
			if err != nil {
				return nil, err
			}
			date, status := SelectDeclaredDate(childDates, store.PersonDateBirthday, now)
			if status == DateEligible {
				add("person/"+child.VCardUID, "person/"+person.VCardUID, "birthday", date, tier, false)
			} else {
				skip(string(status))
			}
		}
		owned, err := st.ListImportantDateSubjectsContext(ctx, person.VCardUID)
		if err != nil {
			return nil, err
		}
		for _, subject := range owned {
			add("owned/"+subject.UID, "person/"+person.VCardUID, "birthday", subject.Date, tier, false)
		}
	}
	result := make([]Candidate, 0, len(selected))
	for _, candidate := range selected {
		lead := map[Tier]int{TierA: cfg.LeadA, TierB: cfg.LeadB, TierC: cfg.LeadC, TierD: cfg.LeadD}[candidate.Tier]
		days := DaysUntil(now, candidate.Occurrence, location)
		if days < 0 || days > lead {
			skip("outside_lead_window")
			continue
		}
		prefix := candidate.SubjectUID + ":" + candidate.Kind + ":"
		pending, err := st.ImportantDateHasOtherPendingContext(ctx, prefix, candidate.OccurrenceKey, candidate.Fingerprint)
		if err != nil {
			return nil, err
		}
		if !pending {
			result = append(result, candidate)
		} else {
			skip("prior_occurrence_requires_reconciliation")
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OccurrenceKey < result[j].OccurrenceKey })
	return result, nil
}

// Partial or invalid relationship start dates cannot prove an active edge.
func relationshipStarted(start *store.PartialDate, now time.Time, location *time.Location) bool {
	if start == nil {
		return true
	}
	if start.Year == nil || start.Month == nil || start.Day == nil || start.Validate() != nil {
		return false
	}
	local := now.In(location)
	today := local.Format(time.DateOnly)
	date := fmt.Sprintf("%04d-%02d-%02d", *start.Year, *start.Month, *start.Day)
	return date <= today
}

// DateFingerprint identifies the declared month and day without encoding a year.
func DateFingerprint(date store.PartialDate) string {
	if date.Month == nil || date.Day == nil || date.Validate() != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("date-v1:%d:%d", *date.Month, *date.Day)))
	return hex.EncodeToString(digest[:])
}
