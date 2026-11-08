package importantdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	ListPersonsContext(ctx context.Context) ([]store.Person, error)
	GetPersonContext(ctx context.Context, personID int64) (*store.Person, error)
	GetPersonByUIDContext(ctx context.Context, uid string) (*store.Person, error)
	ListPersonDatesContext(ctx context.Context, personID int64, history bool) ([]store.PersonDate, error)
	ListPersonAttributeValuesContext(ctx context.Context, personID int64, query store.PersonAttributeQuery) ([]store.PersonAttributeValue, error)
	ListPersonRelationshipsContext(ctx context.Context, personID int64, options store.PersonRelationshipListOptions) ([]store.PersonRelationshipView, error)
	ListImportantDateSubjectsContext(ctx context.Context, ownerUID string) ([]store.ImportantDateSubject, error)
	ImportantDateIdentityStableContext(ctx context.Context, personID int64) (bool, error)
	ImportantDateHasOtherPendingContext(ctx context.Context, prefix, key, fingerprint string) (bool, error)
}

// Candidates scans current declared facts without inference or external writes.
// Subjects involved in a merge or split are quarantined until reconciliation.
func Candidates(ctx context.Context, st CandidateStore, cfg Config, now time.Time) ([]Candidate, error) {
	return candidates(ctx, st, cfg, now, nil, "", "")
}

// Preview reports aggregate reasons for exclusions without exposing date evidence.
// Counts describe evaluation decisions, not necessarily unique people.
type Preview struct {
	Candidates []Candidate    `json:"candidates"`
	Skipped    map[string]int `json:"skipped"`
}

func PreviewCandidates(ctx context.Context, st CandidateStore, cfg Config, now time.Time) (Preview, error) {
	result := Preview{Skipped: make(map[string]int)}
	rows, err := candidates(ctx, st, cfg, now, result.Skipped, "", "")
	result.Candidates = rows
	return result, err
}

// CandidateForSubject applies the same routing rules to one subject and its
// possible parents, avoiding a complete archive scan under the delivery fence.
func CandidateForSubject(ctx context.Context, st CandidateStore, cfg Config, now time.Time, subject, owner string) ([]Candidate, error) {
	return candidates(ctx, st, cfg, now, nil, subject, owner)
}

func candidates(ctx context.Context, st CandidateStore, cfg Config, now time.Time, skipped map[string]int, subject, owner string) ([]Candidate, error) {
	skip := func(reason string) {
		if skipped != nil {
			skipped[reason]++
		}
	}
	if !cfg.Enabled {
		skip("disabled")
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load important-date time zone: %w", err)
	}
	var people []store.Person
	if subject == "" {
		people, err = st.ListPersonsContext(ctx)
	} else {
		people, err = relevantPeople(ctx, st, subject, owner)
	}
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
		if len(values) == 0 {
			if cfg.UnassignedTier != "" {
				tiers[person.ID] = cfg.UnassignedTier
			} else {
				skip("unassigned_cadence")
			}
		} else if len(values) == 1 && values[0].Source.IsDeclared() && !values[0].ActiveFrom.After(now) && values[0].Value.Integer != nil && *values[0].Value.Integer > 0 {
			cadence := *values[0].Value.Integer
			tier, preset := Tier(""), false
			if cadence <= 365 {
				tier, preset = TierForCadence(int(cadence), mapping)
			}
			if preset {
				if cfg.UseCadencePresets {
					tiers[person.ID] = tier
				} else {
					skip("preset_mapping_not_enabled")
				}
			} else if cfg.CustomTier != "" {
				tiers[person.ID] = cfg.CustomTier
			} else {
				skip("custom_cadence")
			}
		} else {
			skip("cadence_not_declared_or_ambiguous")
		}
	}
	selected := make(map[string]Candidate)
	add := func(candidateSubject, owner, kind string, date store.PartialDate, tier Tier, direct bool) {
		if subject != "" && candidateSubject != subject {
			return
		}
		occurrence, ok := NextOccurrence(date, now, location, cfg.LeapDay)
		if !ok {
			skip("no_future_occurrence")
			return
		}
		key := fmt.Sprintf("%s:%s:%d", candidateSubject, kind, occurrence.Year())
		candidate := Candidate{Fingerprint: DateFingerprint(date), OccurrenceKey: key, SubjectUID: candidateSubject, OwnerUID: owner, Kind: kind, Occurrence: occurrence, Tier: tier}
		previous, exists := selected[key]
		// Direct eligibility wins over inheritance; otherwise choose the most
		// important tier, then stable UID for deterministic duplicate routing.
		if !exists || direct && previous.OwnerUID != candidateSubject || (!direct && previous.OwnerUID != candidateSubject && (tier < previous.Tier || tier == previous.Tier && owner < previous.OwnerUID)) {
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

// relevantPeople includes the selected subject and every possible parent, so
// direct and inherited route precedence remains identical to a full scan.
func relevantPeople(ctx context.Context, st CandidateStore, subject, owner string) ([]store.Person, error) {
	ownerUID, ok := strings.CutPrefix(owner, "person/")
	if !ok || ownerUID == "" {
		return nil, errors.New("invalid important-date owner")
	}
	ownerPerson, err := st.GetPersonByUIDContext(ctx, ownerUID)
	if err != nil {
		return nil, err
	}
	people := map[int64]store.Person{ownerPerson.ID: *ownerPerson}
	if subjectUID, ok := strings.CutPrefix(subject, "person/"); ok {
		person, err := st.GetPersonByUIDContext(ctx, subjectUID)
		if err != nil {
			return nil, err
		}
		people[person.ID] = *person
		relations, err := st.ListPersonRelationshipsContext(ctx, person.ID, store.PersonRelationshipListOptions{})
		if err != nil {
			return nil, err
		}
		for _, view := range relations {
			rel := view.Relationship
			if rel.TypeSlug != "parent" || rel.TargetPersonID != person.ID {
				continue
			}
			parent, err := st.GetPersonContext(ctx, rel.SourcePersonID)
			if err != nil {
				return nil, err
			}
			people[parent.ID] = *parent
		}
	} else if !strings.HasPrefix(subject, "owned/") {
		return nil, errors.New("invalid important-date subject")
	}
	result := make([]store.Person, 0, len(people))
	for _, person := range people {
		result = append(result, person)
	}
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
