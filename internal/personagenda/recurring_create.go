package personagenda

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/taskclient"
)

var ErrRecurringRetryExpired = errors.New("unresolved recurring creation key is too old to retry safely")
var ErrInvalidRecurringKey = errors.New("invalid recurring creation retry key")
var ErrRecurringProjectChanged = errors.New("recurring creation target project changed")

const recurringRequestKey = "msgvault.recurring_request"

type RecurringCreateInput struct {
	Project  string        `json:"project"`
	Target   string        `json:"target"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
	List     string        `json:"list"`
	Owner    string        `json:"owner"`
	Settings ScheduleInput `json:"settings"`
}

type RecurringCreateResult struct {
	Parent   Item      `json:"parent"`
	Schedule *Schedule `json:"schedule,omitempty"`
}

type recurringCreateClient interface {
	CheckRecurrenceSupport(ctx context.Context, project string) error
	FindActionTask(ctx context.Context, project, marker string) (taskclient.KataTask, bool, error)
	GetCronJob(ctx context.Context, project, jobID string) (taskclient.KataCronJob, error)
	CreateCronJob(ctx context.Context, project, jobID, parentID string, settings taskclient.KataCronSettings) (taskclient.KataCronJob, error)
}

// CreateRecurring retains one parent marker and one job UID across retries.
// A missing marker after Kata's replay window is not proof of absence.
func (s Service) CreateRecurring(ctx context.Context, personID int64, key string, input RecurringCreateInput) (RecurringCreateResult, error) {
	if input.Project == "" || input.Project != s.Project {
		return RecurringCreateResult{}, ErrRecurringProjectChanged
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	createdAt, err := recurringKeyTime(key, now)
	if err != nil {
		return RecurringCreateResult{}, ErrInvalidRecurringKey
	}
	if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Owner) == "" || !validScheduleInput(input.Settings) {
		return RecurringCreateResult{}, ErrInvalidSchedule
	}
	input.Title = strings.TrimSpace(input.Title)
	input.Owner = strings.TrimSpace(input.Owner)
	if input.Settings.TriggerKind == "interval" {
		input.Settings.Cron = ""
	} else {
		input.Settings.IntervalSeconds = 0
	}
	input.List, err = NormalizeList(input.List)
	if err != nil {
		return RecurringCreateResult{}, ErrInvalidSchedule
	}
	client, ok := s.Tasks.(recurringCreateClient)
	if !ok {
		return RecurringCreateResult{}, ErrUnsupportedSchedule
	}
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return RecurringCreateResult{}, err
	}
	marker := kataevidence.Digest("msgvault.recurring.create.v1", s.Project, key)
	encoded, err := json.Marshal(input, json.Deterministic(true))
	if err != nil {
		return RecurringCreateResult{}, ErrInvalidSchedule
	}
	hash := kataevidence.Digest("msgvault.recurring.request.v1", uids[0], string(encoded))
	sum := sha256.Sum256([]byte(marker))
	var jobID ulid.ULID
	copy(jobID[:], sum[:len(jobID)])
	parent, found, err := client.FindActionTask(ctx, s.Project, marker)
	if err != nil {
		return RecurringCreateResult{}, err
	}
	if !found {
		if now.Sub(createdAt) > 7*24*time.Hour {
			return RecurringCreateResult{}, ErrRecurringRetryExpired
		}
		if err := client.CheckRecurrenceSupport(ctx, s.Project); err != nil {
			return RecurringCreateResult{}, err
		}
		parent, err = s.Tasks.CreateTask(ctx, s.Project, marker, taskclient.KataCreate{
			Title: input.Title, Body: input.Body, Owner: input.Owner,
			Metadata: map[string]any{taskclient.ActionMetadataKey: marker, recurringRequestKey: hash, PersonMetadataKey: uids[0], ListMetadataKey: input.List},
		})
		if err != nil {
			if errors.Is(err, taskclient.ErrUnreachable) || errors.Is(err, taskclient.ErrNotFound) {
				return RecurringCreateResult{}, ErrScheduleOutcomeUnknown
			}
			return RecurringCreateResult{}, err
		}
	} else {
		parent, err = s.Tasks.GetTask(ctx, s.Project, parent.UID)
		if err != nil {
			return RecurringCreateResult{}, ErrScheduleOutcomeUnknown
		}
	}
	person, personErr := personUID(parent.Metadata)
	recorded, _ := parent.Metadata[recurringRequestKey].(string)
	if personErr != nil || !slices.Contains(uids, person) || recorded != kataevidence.Digest("msgvault.recurring.request.v1", person, string(encoded)) || parent.Owner != input.Owner || parent.Status != "open" || parent.Deleted || parent.RecurrenceID != nil {
		return RecurringCreateResult{}, taskclient.ErrConflict
	}
	item, err := itemFromTask(parent)
	if err != nil {
		return RecurringCreateResult{}, err
	}
	result := RecurringCreateResult{Parent: item}
	settings := taskclient.KataCronSettings{TriggerKind: input.Settings.TriggerKind, Cron: input.Settings.Cron, Timezone: input.Settings.Timezone, IntervalSeconds: input.Settings.IntervalSeconds, OccurrenceTitle: input.Settings.OccurrenceTitle, OccurrenceBody: input.Settings.OccurrenceBody, Recipient: input.Settings.Recipient, Catchup: input.Settings.Catchup, GraceSeconds: input.Settings.GraceSeconds}
	job, err := client.GetCronJob(ctx, s.Project, jobID.String())
	if errors.Is(err, taskclient.ErrNotFound) {
		job, err = client.CreateCronJob(ctx, s.Project, jobID.String(), parent.UID, settings)
		if errors.Is(err, taskclient.ErrConflict) || errors.Is(err, taskclient.ErrUnreachable) || errors.Is(err, taskclient.ErrInvalidResponse) {
			return result, ErrScheduleOutcomeUnknown
		}
	}
	if err != nil {
		return result, err
	}
	if job.Deleted || !job.Enabled || job.ParentUID != parent.UID || job.TriggerKind != settings.TriggerKind || job.Cron != settings.Cron || job.IntervalSeconds != settings.IntervalSeconds || job.Timezone != settings.Timezone || job.OccurrenceTitle != settings.OccurrenceTitle || job.OccurrenceBody != settings.OccurrenceBody || job.Recipient != settings.Recipient || job.Catchup != settings.Catchup || job.GraceSeconds != settings.GraceSeconds {
		return result, taskclient.ErrConflict
	}
	schedule := scheduleFromJob(job, false)
	result.Schedule = &schedule
	return result, nil
}

func recurringKeyTime(key string, now time.Time) (time.Time, error) {
	stamp, id, ok := strings.Cut(key, "/")
	if !ok || len(key) > 128 {
		return time.Time{}, ErrInvalidSchedule
	}
	created, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || created.After(now.Add(5*time.Minute)) {
		return time.Time{}, ErrInvalidSchedule
	}
	if _, err := uuid.Parse(id); err != nil {
		return time.Time{}, ErrInvalidSchedule
	}
	return created, nil
}
