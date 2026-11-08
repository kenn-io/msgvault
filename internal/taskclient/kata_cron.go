package taskclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
	kata "go.kenn.io/kata/pkg/client/generated"
)

const MaxCronJobs = 100

// KataCronCatalog is bounded and project scoped. Truncated means jobs were
// omitted before their parents' membership could be established.
type KataCronCatalog struct {
	ProjectID  int64
	ProjectUID string
	Jobs       []KataCronJob
	Truncated  bool
}

// KataCronJob exposes native read fields.
type KataCronJob struct {
	UID, DefinitionEventUID, ParentUID         string
	ProjectID                                  int64
	Enabled, Deleted                           bool
	TriggerKind, Cron, Timezone, Catchup       string
	IntervalSeconds, GraceSeconds              int64
	OccurrenceTitle, OccurrenceBody, Recipient string
	name                                       string
	definition                                 jsontext.Value
}

type KataTaskFamily struct {
	Parent   KataTask
	Children []KataTask
}

// ValidKataUID accepts the canonical full identity used by native cron bindings.
func ValidKataUID(value string) bool {
	_, err := ulid.ParseStrict(value)
	return err == nil && len(value) == 26 && value == strings.ToUpper(value)
}

// RecurrenceCatalog checks native support independently of ordinary task access.
// It never widens the normal 1 MiB response bound or fetches individual jobs.
func (c *KataClient) RecurrenceCatalog(ctx context.Context, project string) (KataCronCatalog, error) {
	id, err := c.resolveProject(ctx, project)
	if err != nil {
		return KataCronCatalog{}, err
	}
	base := "/api/v1/projects/" + strconv.FormatInt(id, 10) + "/cron/"
	transport := *c.transport
	transport.maxResponseBytes = min(transport.maxResponseBytes, DefaultMaxResponseSize)
	var capabilities kata.CronCapabilitiesResponseBody
	if err := transport.doJSON(ctx, http.MethodGet, base+"capabilities", nil, nil, &capabilities, http.StatusOK); err != nil {
		return KataCronCatalog{}, err
	}
	if !slices.Contains(capabilities.EventFeatures, "cron_v1") {
		return KataCronCatalog{}, ErrIncompatible
	}
	if !ValidKataUID(capabilities.ProjectUID) {
		return KataCronCatalog{}, ErrInvalidResponse
	}
	var response struct {
		Jobs []jsontext.Value `json:"jobs"`
	}
	if err := transport.doJSON(ctx, http.MethodGet, base+"jobs?include_deleted=true", nil, nil, &response, http.StatusOK); err != nil {
		return KataCronCatalog{}, err
	}
	if response.Jobs == nil {
		return KataCronCatalog{}, ErrInvalidResponse
	}
	result := KataCronCatalog{ProjectID: id, ProjectUID: capabilities.ProjectUID, Jobs: []KataCronJob{}, Truncated: len(response.Jobs) > MaxCronJobs}
	for _, raw := range response.Jobs[:min(len(response.Jobs), MaxCronJobs)] {
		var job kata.CronJob
		if err := json.Unmarshal(raw, &job); err != nil {
			return KataCronCatalog{}, ErrInvalidResponse
		}
		if job.ProjectID != id {
			result.Truncated = true
			continue
		}
		native, ok := nativeCronJob(job)
		if !ok {
			d := job.Definition
			if d.Action.Kind == "notify" && d.Issue != nil && d.Issue.Kind == "per-run" && (d.Trigger.Kind == "cron" || d.Trigger.Kind == "interval") {
				result.Truncated = true
			}
			continue
		}
		var document struct {
			Definition jsontext.Value `json:"definition"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			return KataCronCatalog{}, ErrInvalidResponse
		}
		native.name = job.Name
		native.definition = append(jsontext.Value(nil), document.Definition...)
		result.Jobs = append(result.Jobs, native)
	}
	return result, nil
}

func nativeCronJob(job kata.CronJob) (KataCronJob, bool) {
	d := job.Definition
	if d.Version != 1 || d.Kind != "job" || d.Action.Kind != "notify" || d.Issue == nil || d.Issue.Kind != "per-run" || d.Overlap != "allow" || (d.Trigger.Kind != "cron" && d.Trigger.Kind != "interval") {
		return KataCronJob{}, false
	}
	if !ValidKataUID(job.UID) || !ValidKataUID(job.DefinitionEventUID) || !ValidKataUID(value(d.Issue.ParentUID)) {
		return KataCronJob{}, false
	}
	return KataCronJob{UID: job.UID, DefinitionEventUID: job.DefinitionEventUID, ParentUID: value(d.Issue.ParentUID), ProjectID: job.ProjectID, Enabled: d.Enabled, Deleted: job.DeletedAt != nil, TriggerKind: d.Trigger.Kind, Cron: value(d.Trigger.Cron), Timezone: value(d.Trigger.Timezone), Catchup: d.Catchup, IntervalSeconds: value(d.Trigger.IntervalSeconds), GraceSeconds: value(d.GraceSeconds), OccurrenceTitle: value(d.Issue.Title), OccurrenceBody: value(d.Issue.Body), Recipient: value(d.Action.Recipient)}, true
}

func value[T any](p *T) (zero T) {
	if p != nil {
		return *p
	}
	return zero
}

// GetTaskFamily uses issue-show's current child rows, including their own
// authoritative project identities. No child GET or project-wide scan is used.
func (c *KataClient) GetTaskFamily(ctx context.Context, project, parentUID string) (KataTaskFamily, error) {
	if !ValidKataUID(parentUID) {
		return KataTaskFamily{}, ErrInvalidRef
	}
	path, err := c.issuePath(ctx, project, "/"+parentUID)
	if err != nil {
		return KataTaskFamily{}, err
	}
	transport := *c.transport
	transport.maxResponseBytes = min(transport.maxResponseBytes, DefaultMaxResponseSize)
	var response kata.ShowIssueResponseBody
	if err := transport.doJSON(ctx, http.MethodGet, path, nil, nil, &response, http.StatusOK); err != nil {
		return KataTaskFamily{}, err
	}
	parent := taskFromKataIssue(project, response.Issue, response.Issue.Labels, response.WebURL)
	if parent.UID != parentUID {
		return KataTaskFamily{}, ErrInvalidResponse
	}
	result := KataTaskFamily{Parent: parent, Children: make([]KataTask, 0, len(response.Children))}
	for _, issue := range response.Children {
		// qualified_id belongs to the child's project; never stamp the parent's
		// project name onto a child whose link crosses project boundaries.
		childProject, _, ok := strings.Cut(issue.QualifiedID, "#")
		if !ok {
			childProject = ""
		}
		child := taskFromKataIssue(childProject, kata.ShowIssueOut{UID: issue.UID, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body, Revision: issue.Revision, ProjectID: issue.ProjectID, ProjectUID: issue.ProjectUID, RecurrenceID: issue.RecurrenceID, DeletedAt: issue.DeletedAt, Metadata: issue.Metadata, Status: issue.Status, Priority: issue.Priority, Owner: issue.Owner}, issue.Labels, issue.WebURL)
		child.QualifiedRef = issue.QualifiedID
		// Inclusion in issue-show's children is the current parent relationship.
		// Keep a conflicting explicit parent so the projection rejects it.
		child.ParentUID = parentUID
		if issue.Parent != nil {
			child.ParentUID = issue.Parent.UID
		}
		result.Children = append(result.Children, child)
	}
	return result, nil
}
