package personagenda

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/cronexpr"
	"go.kenn.io/msgvault/internal/taskclient"
)

var (
	ErrInvalidSchedule        = errors.New("invalid schedule")
	ErrUnsupportedSchedule    = errors.New("schedule cannot be edited safely")
	ErrScheduleOutcomeUnknown = errors.New("schedule update outcome is unknown")
)

type ScheduleInput struct {
	TriggerKind     string `json:"trigger_kind"`
	Cron            string `json:"cron"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Timezone        string `json:"timezone"`
	OccurrenceTitle string `json:"occurrence_title"`
	OccurrenceBody  string `json:"occurrence_body"`
	Recipient       string `json:"recipient"`
	Catchup         string `json:"catchup"`
	GraceSeconds    int64  `json:"grace_seconds"`
}

type ScheduleUpdateInput struct {
	Action           string         `json:"action"`
	ExpectedEventUID string         `json:"expected_event_uid"`
	Settings         *ScheduleInput `json:"settings,omitempty"`
}

type scheduleClient interface {
	GetCronJob(ctx context.Context, project, jobID string) (taskclient.KataCronJob, error)
	EditCronJob(ctx context.Context, project string, job taskclient.KataCronJob, eventID string, settings taskclient.KataCronSettings) (taskclient.KataCronJob, error)
	SetCronJobEnabled(ctx context.Context, project string, job taskclient.KataCronJob, eventID string, enabled bool) (taskclient.KataCronJob, error)
	SetCronJobDeleted(ctx context.Context, project string, job taskclient.KataCronJob, eventID string, deleted bool) (taskclient.KataCronJob, error)
}

// UpdateSchedule rechecks the person, project, parent and job before a native
// conditional lifecycle operation or a lossless edit of a supported definition.
func (s Service) UpdateSchedule(ctx context.Context, personID int64, parentID, jobID string, input ScheduleUpdateInput) (Schedule, error) {
	if !taskclient.ValidKataUID(jobID) || !taskclient.ValidKataUID(input.ExpectedEventUID) {
		return Schedule{}, ErrInvalidSchedule
	}
	switch input.Action {
	case "edit":
		if input.Settings == nil || !validScheduleInput(*input.Settings) {
			return Schedule{}, ErrInvalidSchedule
		}
	case "pause", "resume", "delete", "restore":
		if input.Settings != nil {
			return Schedule{}, ErrInvalidSchedule
		}
	default:
		return Schedule{}, ErrInvalidSchedule
	}
	client, ok := s.Tasks.(scheduleClient)
	if !ok {
		return Schedule{}, ErrUnsupportedSchedule
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return Schedule{}, err
	}
	parent, err := s.Tasks.GetTask(ctx, s.Project, parentID)
	if err != nil {
		return Schedule{}, err
	}
	person, err := personUID(parent.Metadata)
	if err != nil || person == "" || !slices.Contains(uids, person) || parent.Deleted {
		return Schedule{}, taskclient.ErrNotFound
	}
	job, err := client.GetCronJob(ctx, s.Project, jobID)
	if err != nil {
		return Schedule{}, err
	}
	if job.ParentUID != parent.UID || job.ProjectID <= 0 || job.ProjectID != parent.ProjectID {
		return Schedule{}, taskclient.ErrNotFound
	}
	if job.DefinitionEventUID != input.ExpectedEventUID {
		return Schedule{}, taskclient.ErrConflict
	}
	if job.Deleted && (input.Action == "pause" || input.Action == "resume" || input.Action == "edit") {
		return Schedule{}, taskclient.ErrConflict
	}
	if input.Action == "edit" && !job.Editable() {
		return Schedule{}, ErrUnsupportedSchedule
	}
	if parent.RecurrenceID != nil && (input.Action == "resume" || (input.Action == "restore" && job.Enabled)) {
		return Schedule{}, ErrUnsupportedSchedule
	}
	var updated taskclient.KataCronJob
	switch input.Action {
	case "edit":
		settings := input.Settings
		updated, err = client.EditCronJob(ctx, s.Project, job, input.ExpectedEventUID, taskclient.KataCronSettings{
			TriggerKind: settings.TriggerKind, Cron: settings.Cron, Timezone: settings.Timezone,
			IntervalSeconds: settings.IntervalSeconds, OccurrenceTitle: settings.OccurrenceTitle,
			OccurrenceBody: settings.OccurrenceBody, Recipient: settings.Recipient,
			Catchup: settings.Catchup, GraceSeconds: settings.GraceSeconds,
		})
	case "pause", "resume":
		updated, err = client.SetCronJobEnabled(ctx, s.Project, job, input.ExpectedEventUID, input.Action == "resume")
	case "delete", "restore":
		updated, err = client.SetCronJobDeleted(ctx, s.Project, job, input.ExpectedEventUID, input.Action == "delete")
	}
	if err != nil {
		if errors.Is(err, taskclient.ErrUnreachable) || errors.Is(err, taskclient.ErrInvalidResponse) {
			return Schedule{}, ErrScheduleOutcomeUnknown
		}
		return Schedule{}, err
	}
	return scheduleFromJob(updated, parent.Status == "closed"), nil
}

func validScheduleInput(input ScheduleInput) bool {
	if strings.TrimSpace(input.Timezone) == "" || input.Timezone == "Local" || strings.TrimSpace(input.OccurrenceTitle) == "" || strings.TrimSpace(input.Recipient) == "" {
		return false
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil {
		return false
	}
	if input.GraceSeconds < 0 || (input.Catchup == "skip" && input.GraceSeconds == 0) {
		return false
	}
	if input.Catchup != "all" && input.Catchup != "latest" && input.Catchup != "skip" {
		return false
	}
	switch input.TriggerKind {
	case "cron":
		_, err := cronexpr.Parser.Parse(input.Cron)
		return err == nil
	case "interval":
		return input.IntervalSeconds >= 60
	default:
		return false
	}
}
