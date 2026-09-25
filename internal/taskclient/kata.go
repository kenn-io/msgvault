package taskclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	kata "go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/mod/semver"
)

const minKataAPISchemaVersion = "0.21.0"

// KataClient speaks Kata's native issue API. Its transport shares the generic
// client's endpoint validation, authentication, timeouts, and response limits.
type KataClient struct {
	transport *Client
	project   string
	projectID int64
}

type KataTask struct {
	UID           string
	Ref           string
	QualifiedRef  string
	Project       string
	Title         string
	Body          string
	Revision      string
	Status        string
	Owner         string
	WebURL        string
	PriorityValue *int64
	Labels        []string
	Metadata      map[string]any
}

type KataCreate struct {
	Title, Body   string
	PriorityValue *int64
	Labels        []string
	Metadata      map[string]any
}

func NewKata(options ClientOptions) (*KataClient, error) {
	transport, err := New(options)
	if err != nil {
		return nil, err
	}
	return &KataClient{transport: transport}, nil
}

// ConnectKata requires an explicit endpoint and checks the native API and project.
func ConnectKata(ctx context.Context, cfg IntegrationConfig) (*KataClient, error) {
	if !cfg.Enabled {
		return nil, ErrNotFound
	}
	client, err := NewKata(ClientOptions{Endpoint: cfg.Endpoint, APIKey: cfg.APIKey,
		Timeout: cfg.Timeout, MaxResponseBytes: cfg.MaxResponseBytes, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, err
	}
	if err := client.checkSchema(ctx); err != nil {
		return nil, err
	}
	project := strings.TrimSpace(cfg.DefaultProject)
	id, err := client.resolveProject(ctx, project)
	if err != nil {
		return nil, err
	}
	client.project, client.projectID = project, id
	return client, nil
}

func EvaluateKata(ctx context.Context, cfg IntegrationConfig) Status {
	project := strings.TrimSpace(cfg.DefaultProject)
	if !cfg.Enabled {
		return status(StateDisabled, project, "Kata integration is disabled.")
	}
	client, err := ConnectKata(ctx, cfg)
	if err != nil {
		if errors.Is(err, ErrWrongProject) {
			return status(StateWrongProject, project, "The configured Kata project is empty or unavailable.")
		}
		return statusForError(err, project)
	}
	return withClientSecurityNote(status(StateReady, project, "Kata integration is ready."), client.transport)
}

func (c *KataClient) checkSchema(ctx context.Context) error {
	var health kata.HealthResponseBody
	if err := c.transport.doJSON(ctx, http.MethodGet, "/api/v1/health", nil, nil, &health, http.StatusOK); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrIncompatible
		}
		return err
	}
	if health.APISchemaVersion == nil || !compatibleKataAPISchema(*health.APISchemaVersion) {
		return ErrIncompatible
	}
	return nil
}

func compatibleKataAPISchema(reported string) bool {
	version := "v" + strings.TrimPrefix(strings.TrimSpace(reported), "v")
	return semver.IsValid(version) && semver.Compare(version, "v"+minKataAPISchemaVersion) >= 0
}

func (c *KataClient) resolveProject(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, ErrWrongProject
	}
	if name == c.project {
		return c.projectID, nil
	}
	var response kata.ListProjectsResponseBody
	if err := c.transport.doJSON(ctx, http.MethodGet, "/api/v1/projects", nil, nil, &response, http.StatusOK); err != nil {
		return 0, err
	}
	for _, project := range response.Projects {
		if project.Name == name && project.Active && project.DeletedAt == nil {
			return project.ID, nil
		}
	}
	return 0, ErrWrongProject
}

func (c *KataClient) issuePath(ctx context.Context, project, suffix string) (string, error) {
	id, err := c.resolveProject(ctx, project)
	if err != nil {
		return "", err
	}
	return "/api/v1/projects/" + strconv.FormatInt(id, 10) + "/issues" + suffix, nil
}

// ListPersonTasks filters before Kata serializes issues, so unrelated tasks do
// not consume the bounded response. The caller requests one extra to detect truncation.
func (c *KataClient) ListPersonTasks(ctx context.Context, project, personUID string, limit int) ([]KataTask, error) {
	if strings.TrimSpace(personUID) == "" || limit < 1 || limit > 101 {
		return nil, ErrRequestRejected
	}
	query := url.Values{"meta": {"msgvault.person=" + personUID}, "status": {"open"}, "limit": {strconv.Itoa(limit)}}
	path, err := c.issuePath(ctx, project, "?"+query.Encode())
	if err != nil {
		return nil, err
	}
	var response kata.ListIssuesResponseBody
	if err := c.transport.doJSON(ctx, http.MethodGet, path, nil, nil, &response, http.StatusOK); err != nil {
		return nil, err
	}
	result := make([]KataTask, 0, len(response.Issues))
	for _, issue := range response.Issues {
		task := taskFromKataIssue(project, kata.Issue{UID: issue.UID, ShortID: issue.ShortID, Title: issue.Title, Body: issue.Body, Revision: issue.Revision, Metadata: issue.Metadata, Status: issue.Status, Priority: issue.Priority, Owner: issue.Owner}, issue.Labels, issue.WebURL)
		task.QualifiedRef = issue.QualifiedID
		result = append(result, task)
	}
	return result, nil
}

func (c *KataClient) CreateTask(ctx context.Context, project, idempotencyKey string, create KataCreate) (KataTask, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return KataTask{}, ErrIdempotencyKeyRequired
	}
	if strings.TrimSpace(create.Title) == "" {
		return KataTask{}, fmt.Errorf("%w: task title is required", ErrRequestRejected)
	}
	if create.PriorityValue != nil && (*create.PriorityValue < 0 || *create.PriorityValue > 4) {
		return KataTask{}, fmt.Errorf("%w: unsupported priority", ErrRequestRejected)
	}
	path, err := c.issuePath(ctx, project, "")
	if err != nil {
		return KataTask{}, err
	}
	// Different people can need identical todos. The retry key still prevents
	// duplicate creation when the same request is sent again.
	request := kata.CreateIssueRequestBody{Title: create.Title, Body: &create.Body, Labels: create.Labels, Metadata: create.Metadata, Priority: create.PriorityValue, ForceNew: new(true)}
	var response kata.MutationResponseBody
	if err := c.transport.doJSON(ctx, http.MethodPost, path, request, http.Header{"Idempotency-Key": {idempotencyKey}}, &response, http.StatusOK); err != nil {
		return KataTask{}, err
	}
	return c.GetTask(ctx, project, response.Issue.UID)
}

func (c *KataClient) GetTask(ctx context.Context, project, taskID string) (KataTask, error) {
	if err := validatePathSegment(taskID); err != nil {
		return KataTask{}, err
	}
	path, err := c.issuePath(ctx, project, "/"+taskID)
	if err != nil {
		return KataTask{}, err
	}
	var response kata.ShowIssueResponseBody
	if err := c.transport.doJSON(ctx, http.MethodGet, path, nil, nil, &response, http.StatusOK); err != nil {
		return KataTask{}, err
	}
	labels := make([]string, 0, len(response.Labels))
	for _, label := range response.Labels {
		labels = append(labels, label.Label)
	}
	return taskFromKataIssue(project, response.Issue, labels, response.WebURL), nil
}

// MutateMetadata binds compound edits to the revision read by the caller.
func (c *KataClient) MutateMetadata(ctx context.Context, project, taskID, revision string, patch map[string]any) (KataTask, error) {
	if strings.TrimSpace(revision) == "" {
		return KataTask{}, ErrRevisionRequired
	}
	number, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || number < 0 {
		return KataTask{}, ErrInvalidResponse
	}
	return c.patchMetadata(ctx, project, taskID, kata.PatchIssueMetadataRequestBody{Patch: patch}, http.Header{"If-Match": {fmt.Sprintf(`"rev-%d"`, number)}})
}

// MutateMetadataKey guards just the changed key, preserving unrelated concurrent
// edits. A nil previous value requires the key to be absent.
func (c *KataClient) MutateMetadataKey(ctx context.Context, project, taskID, key string, previous, value any) (KataTask, error) {
	guard := &kata.MetadataPatchGuard_OneOf{}
	if previous == nil {
		guard.N, guard.B = 2, kata.MetadataPatchGuard_OneOf_1{Key: key, IfAbsent: kata.True}
	} else {
		encoded, err := json.Marshal(previous)
		if err != nil {
			return KataTask{}, fmt.Errorf("encode metadata guard: %w", err)
		}
		guard.N, guard.A = 1, kata.MetadataPatchGuard_OneOf_0{Key: key, IfValue: string(encoded)}
	}
	return c.patchMetadata(ctx, project, taskID, kata.PatchIssueMetadataRequestBody{Patch: map[string]any{key: value}, Guard: &kata.MetadataPatchGuard{MetadataPatchGuard_OneOf: guard}}, nil)
}

func (c *KataClient) patchMetadata(ctx context.Context, project, taskID string, request kata.PatchIssueMetadataRequestBody, headers http.Header) (KataTask, error) {
	if err := validatePathSegment(taskID); err != nil {
		return KataTask{}, err
	}
	path, err := c.issuePath(ctx, project, "/"+taskID+"/metadata")
	if err != nil {
		return KataTask{}, err
	}
	var response kata.PatchIssueMetadataResponseBody
	if err := c.transport.doJSON(ctx, http.MethodPost, path, request, headers, &response, http.StatusOK); err != nil {
		return KataTask{}, err
	}
	return c.GetTask(ctx, project, response.Issue.UID)
}

func taskFromKataIssue(project string, issue kata.Issue, labels []string, webURL *string) KataTask {
	task := KataTask{UID: issue.UID, Ref: issue.ShortID, QualifiedRef: project + "#" + issue.ShortID, Project: project, Title: issue.Title, Body: issue.Body, Revision: strconv.FormatInt(issue.Revision, 10), Metadata: issue.Metadata, Status: issue.Status, PriorityValue: issue.Priority, Labels: labels}
	if issue.Owner != nil {
		task.Owner = *issue.Owner
	}
	if webURL != nil {
		task.WebURL = *webURL
	}
	return task
}
