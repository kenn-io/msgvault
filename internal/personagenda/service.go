// Package personagenda projects live Kata tasks into virtual lists for durable
// Msgvault people. It stores no mutable task state locally.
package personagenda

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/taskclient"
)

const (
	PersonMetadataKey = "msgvault.person"
	ListMetadataKey   = "msgvault.list"
	DefaultList       = "agenda"
	MaxTasks          = 100
)

var (
	ErrAlreadyLinked        = errors.New("task is already linked to another person")
	ErrUnsafePersonMetadata = errors.New("person metadata cannot be preserved safely")
	ErrUnsafeListMetadata   = errors.New("list metadata cannot be read safely")
	ErrPersonIdentity       = errors.New("person has no stable vCard UID for task linking")
	ErrIdentityLookup       = errors.New("person identity lookup failed")
)

type IdentityStore interface {
	ListPersonUIDsContext(ctx context.Context, personID int64) ([]string, error)
}

type TaskClient interface {
	ListPersonTasks(ctx context.Context, project, personUID string, limit int) ([]taskclient.KataTask, error)
	CreateTask(ctx context.Context, project, idempotencyKey string, create taskclient.KataCreate) (taskclient.KataTask, error)
	GetTask(ctx context.Context, project, taskID string) (taskclient.KataTask, error)
	MutateMetadata(ctx context.Context, project, taskID, revision string, metadata map[string]any) (taskclient.KataTask, error)
	MutateMetadataKey(ctx context.Context, project, taskID, key string, previous, value any) (taskclient.KataTask, error)
}

type State string

const (
	StateOpen      State = "open"
	StateCompleted State = "completed"
)

type Item struct {
	UID           string   `json:"uid"`
	Ref           string   `json:"ref"`
	QualifiedRef  string   `json:"qualified_ref"`
	Project       string   `json:"project"`
	Title         string   `json:"title"`
	Body          string   `json:"body,omitempty"`
	Revision      string   `json:"revision"`
	List          string   `json:"list"`
	Status        string   `json:"status"`
	State         State    `json:"state"`
	PriorityValue *int64   `json:"priority_value,omitempty"`
	Labels        []string `json:"labels"`
	Owner         string   `json:"owner,omitempty"`
	WebURL        string   `json:"web_url,omitempty"`
}

type Result struct {
	Project    string   `json:"project"`
	PersonUID  string   `json:"person_uid"`
	PersonUIDs []string `json:"person_uids"`
	Items      []Item   `json:"items"`
	Truncated  bool     `json:"truncated"`
}

type CreateInput struct {
	Title, Body, List string
	PriorityValue     *int64
	Labels            []string
}

type UpdateInput struct{ List *string }

type Service struct {
	Tasks   TaskClient
	People  IdentityStore
	Project string
}

func (s Service) List(ctx context.Context, personID int64) (Result, error) {
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return Result{}, err
	}
	result := Result{Project: s.Project, PersonUID: uids[0], PersonUIDs: uids, Items: []Item{}}
	seen := make(map[string]bool)
	for _, uid := range uids {
		tasks, err := s.Tasks.ListPersonTasks(ctx, s.Project, uid, MaxTasks-len(result.Items)+1)
		if err != nil {
			return Result{}, err
		}
		for _, task := range tasks {
			linked, err := personUID(task.Metadata)
			if err != nil {
				return Result{}, err
			}
			if linked != uid || task.Status != "open" || seen[task.UID] {
				continue
			}
			if len(result.Items) == MaxTasks {
				result.Truncated = true
				break
			}
			item, err := itemFromTask(task)
			if err != nil {
				return Result{}, err
			}
			seen[task.UID] = true
			result.Items = append(result.Items, item)
		}
		if result.Truncated {
			break
		}
	}
	slices.SortStableFunc(result.Items, func(a, b Item) int {
		if cmp := strings.Compare(a.List, b.List); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Title, b.Title)
	})
	return result, nil
}

func (s Service) Create(ctx context.Context, personID int64, idempotencyKey string, input CreateInput) (Item, error) {
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return Item{}, err
	}
	list, err := normalizeList(input.List)
	if err != nil {
		return Item{}, err
	}
	task, err := s.Tasks.CreateTask(ctx, s.Project, idempotencyKey, taskclient.KataCreate{
		Title: input.Title, Body: input.Body, PriorityValue: input.PriorityValue, Labels: input.Labels,
		Metadata: map[string]any{PersonMetadataKey: uids[0], ListMetadataKey: list},
	})
	if err != nil {
		return Item{}, err
	}
	return itemFromTask(task)
}

func (s Service) Link(ctx context.Context, personID int64, taskID, listName string) (Item, error) {
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return Item{}, err
	}
	list, err := normalizeList(listName)
	if err != nil {
		return Item{}, err
	}
	task, err := s.Tasks.GetTask(ctx, s.Project, taskID)
	if err != nil {
		return Item{}, err
	}
	linked, err := personUID(task.Metadata)
	if err != nil {
		return Item{}, err
	}
	if linked != "" && !slices.Contains(uids, linked) {
		return Item{}, ErrAlreadyLinked
	}
	item, err := itemFromTask(task)
	if err != nil {
		return Item{}, err
	}
	listSpecified := strings.TrimSpace(listName) != ""
	if linked != "" && (!listSpecified || item.List == list) {
		return item, nil
	}
	if !listSpecified {
		task, err = s.Tasks.MutateMetadataKey(ctx, s.Project, task.UID, PersonMetadataKey, nil, uids[0])
	} else {
		patch := map[string]any{ListMetadataKey: list}
		if linked == "" {
			patch[PersonMetadataKey] = uids[0]
		}
		task, err = s.Tasks.MutateMetadata(ctx, s.Project, task.UID, task.Revision, patch)
	}
	if err != nil {
		return Item{}, err
	}
	return itemFromTask(task)
}

func (s Service) Update(ctx context.Context, personID int64, taskID string, input UpdateInput) (Item, error) {
	if input.List == nil {
		return Item{}, fmt.Errorf("%w: list is required", taskclient.ErrRequestRejected)
	}
	list, err := normalizeList(*input.List)
	if err != nil {
		return Item{}, err
	}
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return Item{}, err
	}
	task, err := s.Tasks.GetTask(ctx, s.Project, taskID)
	if err != nil {
		return Item{}, err
	}
	linked, err := personUID(task.Metadata)
	if err != nil {
		return Item{}, err
	}
	if !slices.Contains(uids, linked) {
		return Item{}, taskclient.ErrNotFound
	}
	if _, err := itemFromTask(task); err != nil {
		return Item{}, err
	}
	// The revision also guards the person link when changing its list.
	task, err = s.Tasks.MutateMetadata(ctx, s.Project, task.UID, task.Revision, map[string]any{ListMetadataKey: list})
	if err != nil {
		return Item{}, err
	}
	return itemFromTask(task)
}

func (s Service) Unlink(ctx context.Context, personID int64, taskID string) (Item, error) {
	uids, err := s.personUIDs(ctx, personID)
	if err != nil {
		return Item{}, err
	}
	task, err := s.Tasks.GetTask(ctx, s.Project, taskID)
	if err != nil {
		return Item{}, err
	}
	linked, err := personUID(task.Metadata)
	if err != nil {
		return Item{}, err
	}
	item, err := itemFromTask(task)
	if err != nil {
		return Item{}, err
	}
	if !slices.Contains(uids, linked) {
		return item, nil
	}
	task, err = s.Tasks.MutateMetadataKey(ctx, s.Project, task.UID, PersonMetadataKey, linked, nil)
	if err != nil {
		return Item{}, err
	}
	return itemFromTask(task)
}

func (s Service) personUIDs(ctx context.Context, personID int64) ([]string, error) {
	if s.Tasks == nil || s.People == nil || strings.TrimSpace(s.Project) == "" {
		return nil, taskclient.ErrWrongProject
	}
	uids, err := s.People.ListPersonUIDsContext(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("%w: person %d: %w", ErrIdentityLookup, personID, err)
	}
	if len(uids) == 0 || strings.TrimSpace(uids[0]) == "" {
		return nil, fmt.Errorf("%w: person %d", ErrPersonIdentity, personID)
	}
	return uids, nil
}

func personUID(metadata map[string]any) (string, error) {
	value, exists := metadata[PersonMetadataKey]
	if !exists {
		return "", nil
	}
	uid, ok := value.(string)
	if !ok || strings.TrimSpace(uid) == "" {
		return "", ErrUnsafePersonMetadata
	}
	return uid, nil
}

func normalizeList(value string) (string, error) {
	value = strings.ToLower(strings.Join(strings.Fields(value), " "))
	if value == "" {
		return DefaultList, nil
	}
	if len(value) > 80 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: invalid list name", taskclient.ErrRequestRejected)
	}
	return value, nil
}

func itemFromTask(task taskclient.KataTask) (Item, error) {
	list := DefaultList
	if value, exists := task.Metadata[ListMetadataKey]; exists {
		text, ok := value.(string)
		if !ok {
			return Item{}, ErrUnsafeListMetadata
		}
		var err error
		list, err = normalizeList(text)
		if err != nil {
			return Item{}, ErrUnsafeListMetadata
		}
	}
	state := StateOpen
	if task.Status == "closed" {
		state = StateCompleted
	}
	return Item{UID: task.UID, Ref: task.Ref, QualifiedRef: task.QualifiedRef, Project: task.Project, Title: task.Title, Body: task.Body, Revision: task.Revision, List: list, Status: task.Status, State: state, PriorityValue: task.PriorityValue, Labels: append([]string{}, task.Labels...), Owner: task.Owner, WebURL: safeWebURL(task.WebURL)}, nil
}

func safeWebURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return raw
}
