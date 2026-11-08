package personagenda

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/taskclient"
)

// Schedule is a safe read projection, not an authorization to manage the job.
// Editable describes only whether a form can preserve the definition losslessly.
type Schedule struct {
	UID                string `json:"uid"`
	DefinitionEventUID string `json:"definition_event_uid"`
	State              string `json:"state"`
	Summary            string `json:"summary"`
	Timezone           string `json:"timezone"`
	Catchup            string `json:"catchup"`
	TriggerKind        string `json:"trigger_kind"`
	Cron               string `json:"cron,omitempty"`
	OccurrenceTitle    string `json:"occurrence_title"`
	OccurrenceBody     string `json:"occurrence_body,omitempty"`
	Recipient          string `json:"recipient"`
	GraceSeconds       int64  `json:"grace_seconds"`
	IntervalSeconds    int64  `json:"interval_seconds"`
	Enabled            bool   `json:"enabled"`
	ParentClosed       bool   `json:"parent_closed"`
	Deleted            bool   `json:"deleted"`
	Editable           bool   `json:"editable"`
}

type Occurrence struct {
	JobUID             string `json:"job_uid"`
	DefinitionEventUID string `json:"definition_event_uid"`
	ScheduledAt        string `json:"scheduled_at"`
}

type RecurrenceStatus struct {
	State   string `json:"state"`
	Message string `json:"message"`
}

// RecurrenceClient is optional so ordinary TaskClient implementations remain
// usable when their provider cannot expose native recurrence.
type RecurrenceClient interface {
	RecurrenceCatalog(ctx context.Context, project string) (taskclient.KataCronCatalog, error)
	GetTaskFamily(ctx context.Context, project, parentUID string) (taskclient.KataTaskFamily, error)
}

const (
	maxRecurrenceFamilies = 24
	maxRecurrenceAliases  = 32
	recurrenceTimeout     = 20 * time.Second
)

type recurringFamily struct {
	summary  Item
	children []Item
}

func recurrenceStatus(state string) RecurrenceStatus {
	messages := map[string]string{
		"ready":                   "Recurring schedules loaded.",
		"partial":                 "Some recurring schedules or occurrences could not be loaded.",
		"unsupported":             "Recurring schedules are not supported by this task integration.",
		"forbidden":               "Access to recurring schedules is forbidden.",
		"authentication_required": "Authentication is required to read recurring schedules.",
		"unavailable":             "Recurring schedules are unavailable.",
	}
	return RecurrenceStatus{State: state, Message: messages[state]}
}

func recurrenceError(err error) RecurrenceStatus {
	switch {
	case taskclient.ErrorStatus(err) == http.StatusForbidden:
		return recurrenceStatus("forbidden")
	case errors.Is(err, taskclient.ErrAuthenticationRequired):
		return recurrenceStatus("authentication_required")
	case errors.Is(err, taskclient.ErrIncompatible), errors.Is(err, taskclient.ErrNotFound):
		return recurrenceStatus("unsupported")
	default:
		return recurrenceStatus("unavailable")
	}
}

func (s Service) projectRecurrence(ctx context.Context, uids []string, result *Result) {
	client, ok := s.Tasks.(RecurrenceClient)
	if !ok {
		result.Recurrence = recurrenceStatus("unsupported")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, recurrenceTimeout)
	defer cancel()
	catalog, err := client.RecurrenceCatalog(ctx, s.Project)
	if err != nil {
		result.Recurrence = recurrenceError(err)
		return
	}
	if catalog.ProjectID <= 0 || !taskclient.ValidKataUID(catalog.ProjectUID) {
		result.Recurrence = recurrenceStatus("unavailable")
		return
	}
	result.Recurrence = recurrenceStatus("ready")
	partial := catalog.Truncated
	aliasesComplete := true
	aliases := make(map[string]bool)
	for _, uid := range uids {
		if aliases[uid] {
			continue
		}
		if len(aliases) == maxRecurrenceAliases {
			partial = true
			aliasesComplete = false
			break
		}
		aliases[uid] = true
	}
	jobsByParent := make(map[string][]taskclient.KataCronJob)
	for _, job := range catalog.Jobs {
		// RecurrenceClient is optional and may have implementations other than
		// KataClient, so reject cross-project jobs at this boundary too.
		if job.ProjectID != catalog.ProjectID {
			partial = true
			continue
		}
		jobsByParent[job.ParentUID] = append(jobsByParent[job.ParentUID], job)
	}
	parents := make([]string, 0, len(jobsByParent))
	for uid := range jobsByParent {
		parents = append(parents, uid)
	}
	slices.Sort(parents)
	direct := make(map[string]Item)
	for _, item := range result.Items {
		direct[item.UID] = item
	}
	directOccurrences := make(map[string]Item)
	var families []recurringFamily
	replaced := make(map[string]bool)
	familyReads := 0
	loaded := make(map[string]taskclient.KataTaskFamily)
	for _, uid := range parents {
		if familyReads == maxRecurrenceFamilies {
			partial = true
			continue
		}
		familyReads++
		family, err := client.GetTaskFamily(ctx, s.Project, uid)
		if err != nil {
			partial = true
			continue
		}
		parent := family.Parent
		if parent.UID != uid || !sameProject(parent, catalog) || parent.Deleted {
			partial = true
			continue
		}
		loaded[uid] = family
	}
	// Discover inherited series from verified family edges before projecting any
	// family. Catalog ordering must not decide whether a nested series is seen.
	inherited := make(map[string]Item)
	for changed := true; changed; {
		changed = false
		for _, uid := range parents {
			family, ok := loaded[uid]
			if !ok {
				continue
			}
			person, err := personUID(family.Parent.Metadata)
			if err != nil {
				continue
			}
			parentItem, inheritedParent := inherited[uid]
			if person != "" {
				if !aliases[person] {
					continue
				}
				parentItem, err = itemFromTask(family.Parent)
				if err != nil {
					continue
				}
			} else if !inheritedParent {
				continue
			}
			for _, child := range family.Children {
				nested, exists := loaded[child.UID]
				if !exists || child.UID == uid || child.ParentUID != uid || child.Deleted || child.RecurrenceID != nil || !sameProject(child, catalog) {
					continue
				}
				childPerson, childErr := personUID(child.Metadata)
				nestedPerson, nestedErr := personUID(nested.Parent.Metadata)
				if childErr != nil || nestedErr != nil || childPerson != "" || nestedPerson != "" {
					continue
				}
				occurrence, valid := occurrenceFromMetadata(child.Metadata)
				if !valid {
					continue
				}
				matched := false
				for _, job := range jobsByParent[uid] {
					matched = matched || job.UID == occurrence.JobUID
				}
				if !matched {
					continue
				}
				if _, exists := inherited[child.UID]; exists {
					continue
				}
				source := nested.Parent
				source.Metadata = map[string]any{ListMetadataKey: parentItem.List}
				item, err := itemFromTask(source)
				if err != nil {
					continue
				}
				item.Membership = "inherited"
				item.ParentUID = uid
				item.Occurrence = &occurrence
				inherited[child.UID] = item
				changed = true
			}
		}
	}
	for _, uid := range parents {
		family, ok := loaded[uid]
		if !ok {
			continue
		}
		parent := family.Parent
		jobs := jobsByParent[uid]
		slices.SortFunc(jobs, func(a, b taskclient.KataCronJob) int { return strings.Compare(a.UID, b.UID) })
		linked, err := personUID(parent.Metadata)
		if err != nil {
			partial = true
		}
		parentLinked := err == nil && aliases[linked]
		var summary Item
		if parentLinked {
			summary, err = itemFromTask(parent)
			if err != nil {
				partial = true
				parentLinked = false
			}
		} else if err == nil && linked == "" {
			summary, parentLinked = inherited[uid]
		}
		summary.Kind = "series"
		summary.OccurrencesComplete = !catalog.Truncated && aliasesComplete
		summary.Schedules = make([]Schedule, 0, len(jobs))
		jobIDs := make(map[string]bool)
		for _, job := range jobs {
			if jobIDs[job.UID] {
				continue
			}
			jobIDs[job.UID] = true
			summary.Schedules = append(summary.Schedules, scheduleFromJob(job, parent.Status == "closed"))
		}
		projected := recurringFamily{summary: summary}
		seen := make(map[string]bool)
		for _, child := range family.Children {
			if child.UID == "" || child.UID == parent.UID || child.Status != "open" || child.Deleted || child.ParentUID != uid || !sameProject(child, catalog) || child.RecurrenceID != nil {
				continue
			}
			occurrence, ok := occurrenceFromMetadata(child.Metadata)
			if !ok {
				if _, reserved := child.Metadata["cron.occurrence"]; reserved && parentLinked {
					projected.summary.OccurrencesComplete = false
					partial = true
				}
				continue
			}
			if !jobIDs[occurrence.JobUID] {
				continue
			}
			linked, err := personUID(child.Metadata)
			if err != nil {
				projected.summary.OccurrencesComplete = false
				partial = true
				continue
			}
			if linked != "" && !aliases[linked] {
				continue
			}
			// No arbitrary child metadata is inherited. Only person membership and
			// list are supplied by the verified parent when the person key is absent.
			inherited := linked == ""
			if inherited && !parentLinked {
				continue
			}
			source := child
			if inherited {
				source.Metadata = map[string]any{ListMetadataKey: summary.List}
			}
			item, err := itemFromTask(source)
			if err != nil {
				projected.summary.OccurrencesComplete = false
				partial = true
				continue
			}
			if seen[child.UID] {
				continue
			}
			seen[child.UID] = true
			item.Kind = "occurrence"
			if inherited {
				item.Membership = "inherited"
			}
			item.ParentUID = uid
			item.Occurrence = &occurrence
			if !parentLinked {
				// Preserve the independently listed direct item and its list.
				// No unrelated parent or schedule content enters the result.
				if ordinary, ok := direct[child.UID]; ok {
					ordinary.Kind = "occurrence"
					ordinary.ParentUID = uid
					ordinary.Occurrence = &occurrence
					directOccurrences[child.UID] = ordinary
				}
				continue
			}
			projected.children = append(projected.children, item)
			replaced[child.UID] = true
		}
		slices.SortFunc(projected.children, func(a, b Item) int {
			// RFC3339Nano strings have variable precision, so compare instants.
			at, _ := time.Parse(time.RFC3339Nano, a.Occurrence.ScheduledAt)
			bt, _ := time.Parse(time.RFC3339Nano, b.Occurrence.ScheduledAt)
			if cmp := at.Compare(bt); cmp != 0 {
				return cmp
			}
			return strings.Compare(a.UID, b.UID)
		})
		if !parentLinked {
			continue
		}
		projected.summary.OpenOccurrences = len(projected.children)
		replaced[uid] = true
		families = append(families, projected)
	}
	slices.SortFunc(families, func(a, b recurringFamily) int {
		if cmp := strings.Compare(a.summary.List, b.summary.List); cmp != 0 {
			return cmp
		}
		if cmp := strings.Compare(a.summary.Title, b.summary.Title); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.summary.UID, b.summary.UID)
	})
	// A verified occurrence may itself own a native job. Reserve its series
	// row once, keeping its schedules; the verified family count above still
	// includes that open occurrence. Remove duplicate display roles before
	// round-robin allocation so they do not consume a family's turn.
	seriesUIDs := make(map[string]bool, len(families))
	for _, family := range families {
		seriesUIDs[family.summary.UID] = true
	}
	for i := range families {
		families[i].children = slices.DeleteFunc(families[i].children, func(item Item) bool {
			return seriesUIDs[item.UID]
		})
	}
	ordinary := result.Items
	result.Items = make([]Item, 0, MaxTasks)
	for _, family := range families {
		if len(result.Items) == MaxTasks {
			result.Truncated = true
			break
		}
		result.Items = append(result.Items, family.summary)
	}
	for _, item := range ordinary {
		if occurrence, ok := directOccurrences[item.UID]; ok {
			item = occurrence
		}
		if replaced[item.UID] {
			continue
		}
		if len(result.Items) == MaxTasks {
			result.Truncated = true
			break
		}
		result.Items = append(result.Items, item)
	}
	for index := 0; ; index++ {
		added := false
		for _, family := range families {
			if index >= len(family.children) {
				continue
			}
			added = true
			if len(result.Items) == MaxTasks {
				result.Truncated = true
				returnPartial(result, partial)
				return
			}
			result.Items = append(result.Items, family.children[index])
		}
		if !added {
			break
		}
	}
	returnPartial(result, partial)
}

func returnPartial(result *Result, partial bool) {
	if partial {
		result.Recurrence = recurrenceStatus("partial")
		result.Truncated = true
	}
}

func sameProject(task taskclient.KataTask, catalog taskclient.KataCronCatalog) bool {
	return task.ProjectID == catalog.ProjectID && (task.ProjectUID == "" || task.ProjectUID == catalog.ProjectUID)
}

func scheduleFromJob(job taskclient.KataCronJob, parentClosed bool) Schedule {
	state := "active"
	if !job.Enabled {
		state = "paused"
	}
	if parentClosed {
		state = "parent_closed"
	}
	if job.Deleted {
		state = "deleted"
	}
	zone := job.Timezone
	if zone == "" {
		zone = "UTC"
	}
	summary := cadenceSummary(job)
	return Schedule{UID: job.UID, DefinitionEventUID: job.DefinitionEventUID, State: state, Summary: summary, Timezone: zone, Catchup: job.Catchup, TriggerKind: job.TriggerKind, Cron: job.Cron, OccurrenceTitle: job.OccurrenceTitle, OccurrenceBody: job.OccurrenceBody, Recipient: job.Recipient, GraceSeconds: job.GraceSeconds, IntervalSeconds: job.IntervalSeconds, Enabled: job.Enabled, ParentClosed: parentClosed, Deleted: job.Deleted, Editable: false}
}

func occurrenceFromMetadata(metadata map[string]any) (Occurrence, bool) {
	raw, ok := metadata["cron.occurrence"]
	if !ok {
		return Occurrence{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Occurrence{}, false
	}
	var provenance struct {
		Version            int    `json:"version"`
		JobUID             string `json:"job_uid"`
		DefinitionEventUID string `json:"definition_event_uid"`
		ScheduledAt        string `json:"scheduled_at"`
	}
	if err := json.Unmarshal(encoded, &provenance); err != nil || provenance.Version != 1 || !taskclient.ValidKataUID(provenance.JobUID) || !taskclient.ValidKataUID(provenance.DefinitionEventUID) {
		return Occurrence{}, false
	}
	scheduled, err := time.Parse(time.RFC3339Nano, provenance.ScheduledAt)
	if err != nil {
		return Occurrence{}, false
	}
	return Occurrence{JobUID: provenance.JobUID, DefinitionEventUID: provenance.DefinitionEventUID, ScheduledAt: scheduled.UTC().Format(time.RFC3339Nano)}, true
}

// cadenceSummary recognizes common display shapes in an already accepted Kata
// definition. Other expressions remain available in the cron field; this
// neither validates cron syntax nor computes future run times.
func cadenceSummary(job taskclient.KataCronJob) string {
	if job.TriggerKind == "interval" {
		seconds := job.IntervalSeconds
		for _, unit := range []struct {
			seconds int64
			name    string
		}{{86400, "day"}, {3600, "hour"}, {60, "minute"}} {
			if seconds > 0 && seconds%unit.seconds == 0 {
				count := seconds / unit.seconds
				if count == 1 {
					return "Every " + unit.name
				}
				return fmt.Sprintf("Every %d %ss", count, unit.name)
			}
		}
		return fmt.Sprintf("Every %d seconds", seconds)
	}
	if job.Cron == "@hourly" {
		return "Every hour"
	}
	fields := strings.Fields(job.Cron)
	if len(fields) == 5 {
		minute, merr := strconv.Atoi(fields[0])
		hour, herr := strconv.Atoi(fields[1])
		if merr == nil && herr == nil && minute >= 0 && minute < 60 && hour >= 0 && hour < 24 && fields[3] == "*" {
			clock := fmt.Sprintf("%02d:%02d", hour, minute)
			if fields[2] == "*" {
				if fields[4] == "*" {
					return "Daily at " + clock
				}
				if fields[4] == "1-5" {
					return "Weekdays at " + clock
				}
				days := map[string]string{"0": "Sunday", "1": "Monday", "2": "Tuesday", "3": "Wednesday", "4": "Thursday", "5": "Friday", "6": "Saturday", "7": "Sunday"}
				if day, ok := days[fields[4]]; ok {
					return "Weekly on " + day + " at " + clock
				}
			} else if fields[4] == "*" {
				if day, err := strconv.Atoi(fields[2]); err == nil && day >= 1 && day <= 31 {
					return fmt.Sprintf("Monthly on day %d at %s", day, clock)
				}
			}
		}
	}
	return "Custom schedule"
}
