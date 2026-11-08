package taskclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strconv"
	"time"

	kata "go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/msgvault/internal/cronexpr"
)

const cronWriteTimeout = 20 * time.Second

type KataCronSettings struct {
	TriggerKind, Cron, Timezone, OccurrenceTitle, OccurrenceBody, Recipient, Catchup string
	IntervalSeconds, GraceSeconds                                                    int64
}

// Editable reports form compatibility, not authority to mutate the job.
func (job KataCronJob) Editable() bool {
	var definition map[string]jsontext.Value
	if json.Unmarshal(job.definition, &definition) != nil {
		return false
	}
	var trigger map[string]jsontext.Value
	if json.Unmarshal(definition["trigger"], &trigger) != nil || trigger == nil {
		return false
	}
	for key := range trigger {
		if key != "kind" && key != "cron" && key != "interval_seconds" && key != "timezone" {
			return false
		}
	}
	switch job.TriggerKind {
	case "cron":
		if _, exists := trigger["interval_seconds"]; exists {
			return false
		}
		_, err := cronexpr.Parser.Parse(job.Cron)
		return err == nil
	case "interval":
		_, exists := trigger["cron"]
		return !exists && job.IntervalSeconds >= 60
	default:
		return false
	}
}

// GetCronJob reads the exact native job before a person-scoped mutation. Its
// original definition is retained privately for lossless conditional updates.
func (c *KataClient) GetCronJob(ctx context.Context, project, jobUID string) (KataCronJob, error) {
	if !ValidKataUID(jobUID) {
		return KataCronJob{}, ErrInvalidRef
	}
	projectID, err := c.resolveProject(ctx, project)
	if err != nil {
		return KataCronJob{}, err
	}
	transport := *c.transport
	transport.maxResponseBytes = min(transport.maxResponseBytes, DefaultMaxResponseSize)
	var response struct {
		Job    jsontext.Value `json:"job"`
		Events jsontext.Value `json:"events,omitempty"`
	}
	path := "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + "/cron/jobs/" + jobUID
	if err := transport.doJSON(ctx, http.MethodGet, path, nil, nil, &response, http.StatusOK); err != nil {
		return KataCronJob{}, err
	}
	var job kata.CronJob
	if err := json.Unmarshal(response.Job, &job); err != nil || job.UID != jobUID {
		return KataCronJob{}, ErrInvalidResponse
	}
	if job.ProjectID != projectID {
		return KataCronJob{}, ErrWrongProject
	}
	native, ok := nativeCronJob(job)
	if !ok {
		return KataCronJob{}, ErrIncompatible
	}
	var document struct {
		Definition jsontext.Value `json:"definition"`
	}
	if err := json.Unmarshal(response.Job, &document); err != nil || len(document.Definition) == 0 {
		return KataCronJob{}, ErrInvalidResponse
	}
	native.name = job.Name
	native.definition = append(jsontext.Value(nil), document.Definition...)
	return native, nil
}

// SetCronJobEnabled replaces only enabled while retaining the definition
// obtained from Kata. The caller must verify person and parent scope first.
func (c *KataClient) SetCronJobEnabled(ctx context.Context, project string, job KataCronJob, expected string, enabled bool) (KataCronJob, error) {
	if !ValidKataUID(job.UID) || !ValidKataUID(expected) || job.DefinitionEventUID != expected || job.name == "" {
		return KataCronJob{}, ErrInvalidRef
	}
	var definition map[string]jsontext.Value
	if err := json.Unmarshal(job.definition, &definition); err != nil || definition == nil {
		return KataCronJob{}, ErrInvalidResponse
	}
	encoded, err := json.Marshal(enabled)
	if err != nil {
		return KataCronJob{}, ErrInvalidResponse
	}
	definition["enabled"] = encoded
	body := struct {
		Actor            string                    `json:"actor"`
		Name             string                    `json:"name"`
		ExpectedEventUID string                    `json:"expected_event_uid"`
		Definition       map[string]jsontext.Value `json:"definition"`
	}{Actor: writeActor, Name: job.name, ExpectedEventUID: expected, Definition: definition}
	result, err := c.mutateCronJob(ctx, project, job, http.MethodPut, "", body)
	if err == nil && (result.Deleted || result.Enabled != enabled) {
		return KataCronJob{}, ErrInvalidResponse
	}
	return result, err
}

// EditCronJob patches the fields represented by the form and retains all other
// native definition fields. The caller validates settings and person scope.
func (c *KataClient) EditCronJob(ctx context.Context, project string, job KataCronJob, expected string, settings KataCronSettings) (KataCronJob, error) {
	if !ValidKataUID(job.UID) || !ValidKataUID(expected) || job.DefinitionEventUID != expected || job.name == "" || !job.Editable() {
		return KataCronJob{}, ErrInvalidRef
	}
	var definition map[string]jsontext.Value
	if err := json.Unmarshal(job.definition, &definition); err != nil {
		return KataCronJob{}, ErrInvalidResponse
	}
	for key, changes := range map[string]map[string]any{
		"trigger": {"kind": settings.TriggerKind, "timezone": settings.Timezone},
		"action":  {"recipient": settings.Recipient},
		"issue":   {"title": settings.OccurrenceTitle, "body": settings.OccurrenceBody},
	} {
		var object map[string]jsontext.Value
		if err := json.Unmarshal(definition[key], &object); err != nil || object == nil {
			return KataCronJob{}, ErrInvalidResponse
		}
		if key == "trigger" {
			delete(object, "cron")
			delete(object, "interval_seconds")
			if settings.TriggerKind == "cron" {
				changes["cron"] = settings.Cron
			} else {
				changes["interval_seconds"] = settings.IntervalSeconds
			}
		}
		for field, value := range changes {
			encoded, err := json.Marshal(value)
			if err != nil {
				return KataCronJob{}, ErrInvalidResponse
			}
			object[field] = encoded
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return KataCronJob{}, ErrInvalidResponse
		}
		definition[key] = encoded
	}
	for field, value := range map[string]any{"catchup": settings.Catchup, "grace_seconds": settings.GraceSeconds} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return KataCronJob{}, ErrInvalidResponse
		}
		definition[field] = encoded
	}
	body := struct {
		Actor            string                    `json:"actor"`
		Name             string                    `json:"name"`
		ExpectedEventUID string                    `json:"expected_event_uid"`
		Definition       map[string]jsontext.Value `json:"definition"`
	}{Actor: writeActor, Name: job.name, ExpectedEventUID: expected, Definition: definition}
	result, err := c.mutateCronJob(ctx, project, job, http.MethodPut, "", body)
	if err == nil && (result.Deleted || result.TriggerKind != settings.TriggerKind || result.Timezone != settings.Timezone || result.OccurrenceTitle != settings.OccurrenceTitle || result.OccurrenceBody != settings.OccurrenceBody || result.Recipient != settings.Recipient || result.Catchup != settings.Catchup || result.GraceSeconds != settings.GraceSeconds || (settings.TriggerKind == "cron" && result.Cron != settings.Cron) || (settings.TriggerKind == "interval" && result.IntervalSeconds != settings.IntervalSeconds)) {
		return KataCronJob{}, ErrInvalidResponse
	}
	return result, err
}

// SetCronJobDeleted uses Kata's reversible tombstone and the viewed event UID.
func (c *KataClient) SetCronJobDeleted(ctx context.Context, project string, job KataCronJob, expected string, deleted bool) (KataCronJob, error) {
	if !ValidKataUID(job.UID) || !ValidKataUID(expected) || job.DefinitionEventUID != expected {
		return KataCronJob{}, ErrInvalidRef
	}
	method, suffix := http.MethodDelete, ""
	if !deleted {
		method, suffix = http.MethodPost, "/restore"
	}
	body := struct {
		Actor            string `json:"actor"`
		ExpectedEventUID string `json:"expected_event_uid"`
	}{Actor: writeActor, ExpectedEventUID: expected}
	result, err := c.mutateCronJob(ctx, project, job, method, suffix, body)
	if err == nil && result.Deleted != deleted {
		return KataCronJob{}, ErrInvalidResponse
	}
	return result, err
}

func (c *KataClient) mutateCronJob(ctx context.Context, project string, original KataCronJob, method, suffix string, body any) (KataCronJob, error) {
	projectID, err := c.resolveProject(ctx, project)
	if err != nil {
		return KataCronJob{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, cronWriteTimeout)
	defer cancel()
	transport := *c.transport
	client := *transport.httpClient
	client.Timeout = cronWriteTimeout
	transport.httpClient = &client
	transport.maxResponseBytes = min(transport.maxResponseBytes, DefaultMaxResponseSize)
	var response struct {
		Job    jsontext.Value `json:"job"`
		Events jsontext.Value `json:"events,omitempty"`
	}
	path := "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + "/cron/jobs/" + original.UID + suffix
	if err := transport.doJSON(ctx, method, path, body, nil, &response, http.StatusOK); err != nil {
		return KataCronJob{}, err
	}
	var job kata.CronJob
	if err := json.Unmarshal(response.Job, &job); err != nil {
		return KataCronJob{}, ErrInvalidResponse
	}
	result, ok := nativeCronJob(job)
	if !ok || result.UID != original.UID || result.ParentUID != original.ParentUID || (result.ProjectID != 0 && result.ProjectID != projectID) {
		return KataCronJob{}, ErrInvalidResponse
	}
	var document struct {
		Definition jsontext.Value `json:"definition"`
	}
	if err := json.Unmarshal(response.Job, &document); err != nil || len(document.Definition) == 0 {
		return KataCronJob{}, ErrInvalidResponse
	}
	result.name = job.Name
	result.definition = append(jsontext.Value(nil), document.Definition...)
	return result, nil
}
