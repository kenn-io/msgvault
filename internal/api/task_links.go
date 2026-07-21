package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/tasklinks"
)

const (
	taskLinkEmailMessageType = "email"
	MaxTaskLinkRequestBytes  = 64 << 10
	maxTaskSearchQueryLength = 256
)

type TaskLinkMutationRequest struct {
	TaskID      string   `json:"task_id,omitempty" doc:"Existing task to link; omit to create a task"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Priority    string   `json:"priority,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	AddedAt     string   `json:"added_at" doc:"Browser-generated retry-stable RFC3339 timestamp for the outbound mail link"`
}

type TaskLinkTask struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Title    string `json:"title"`
	Revision string `json:"revision,omitempty"`
}

type TaskLinkMutationResponse struct {
	Task TaskLinkTask `json:"task"`
}

type TaskLinkOutboundMetadata struct {
	ArchiveUID       string `json:"archive_uid"`
	MessageID        int64  `json:"message_id"`
	ConversationID   int64  `json:"conversation_id"`
	Subject          string `json:"subject"`
	From             string `json:"from"`
	SentAt           string `json:"sent_at"`
	SourceType       string `json:"source_type"`
	SourceIdentifier string `json:"source_identifier"`
	SourceMessageID  string `json:"source_message_id"`
}

type TaskLinkLookupResponse struct {
	tasklinks.IndexStatus

	Tasks            []tasklinks.TaskSummary  `json:"tasks"`
	OutboundMetadata TaskLinkOutboundMetadata `json:"outbound_metadata"`
}

type TaskSearchResponse struct {
	Tasks []tasklinks.TaskSummary `json:"tasks"`
}

func (s *Server) registerTaskLinkRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRouteWithRequest[TaskLinkMutationRequest, TaskLinkMutationResponse](api,
		"createOrLinkMessageTask", http.MethodPost, "/messages/{id}/tasks", "Create or link a task for an archived email", s.handleCreateOrLinkMessageTask, http.StatusCreated)
	registerAPIV1RawHumaJSONRoute[TaskLinkLookupResponse](api,
		"listMessageTasks", http.MethodGet, "/messages/{id}/tasks", "List tasks linked to an archived email", s.handleListMessageTasks)
	registerAPIV1RawHumaJSONRoute[TaskLinkMutationResponse](api,
		"unlinkMessageTask", http.MethodDelete, "/messages/{id}/tasks/{task_id}", "Unlink a task from an archived email", s.handleUnlinkMessageTask)
	registerAPIV1RawHumaJSONRoute[TaskSearchResponse](api,
		"searchIntegrationTasks", http.MethodGet, "/integrations/tasks/search", "Search tasks in the configured project", s.handleSearchIntegrationTasks)
}

func (s *Server) taskMessage(r *http.Request) (tasklinks.MessageIdentity, *apiHTTPError) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return tasklinks.MessageIdentity{}, newAPIHTTPError(http.StatusBadRequest, "invalid_message_id", "Message ID must be a positive integer")
	}
	message, err := s.getMessage(r.Context(), id)
	if err != nil {
		return tasklinks.MessageIdentity{}, newAPIHTTPError(http.StatusNotFound, "not_found", "Message not found")
	}
	if message.MessageType != taskLinkEmailMessageType {
		return tasklinks.MessageIdentity{}, newAPIHTTPError(http.StatusUnprocessableEntity, "email_required", "Task links are available only for concrete email rows")
	}
	identity, err := s.taskIdentityResolver(r.Context(), message)
	if err != nil {
		return tasklinks.MessageIdentity{}, newAPIHTTPError(http.StatusServiceUnavailable, "identity_unavailable", "Archive identity is unavailable")
	}
	return identity, nil
}

func (s *Server) handleCreateOrLinkMessageTask(w http.ResponseWriter, r *http.Request) {
	browserRequestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if browserRequestID == "" {
		writeError(w, http.StatusBadRequest, "request_id_required", "X-Request-Id is required")
		return
	}
	identity, httpErr := s.taskMessage(r)
	if httpErr != nil {
		writeAPIHTTPError(w, httpErr)
		return
	}
	if s.taskLinkOperations == nil {
		writeError(w, http.StatusServiceUnavailable, "task_integration_unavailable", "Task integration is unavailable")
		return
	}
	var request TaskLinkMutationRequest
	r.Body = http.MaxBytesReader(w, r.Body, MaxTaskLinkRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Task request is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid task request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Task request must contain exactly one JSON value")
		return
	}
	addedAt, err := time.Parse(time.RFC3339, request.AddedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "added_at_required", "added_at must be an RFC3339 timestamp")
		return
	}
	var task taskclient.Task
	if request.TaskID != "" {
		task, err = s.taskLinkOperations.Link(r.Context(), request.TaskID, identity, addedAt)
	} else {
		if strings.TrimSpace(request.Title) == "" {
			writeError(w, http.StatusBadRequest, "title_required", "Task title is required")
			return
		}
		key := taskIdempotencyKey(browserRequestID, identity)
		task, err = s.taskLinkOperations.Create(r.Context(), key, taskclient.TaskCreate{Title: request.Title, Notes: request.Description, Priority: request.Priority, Labels: request.Labels}, identity, addedAt)
	}
	if err != nil {
		writeTaskLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, TaskLinkMutationResponse{Task: publicTask(task)})
}

func (s *Server) handleListMessageTasks(w http.ResponseWriter, r *http.Request) {
	identity, httpErr := s.taskMessage(r)
	if httpErr != nil {
		writeAPIHTTPError(w, httpErr)
		return
	}
	if s.taskLinkOperations == nil {
		writeJSON(w, http.StatusOK, taskLinkLookupResponse(identity, tasklinks.LookupResult{IndexStatus: tasklinks.IndexStatus{State: tasklinks.StateUnavailable, Complete: false, Reason: tasklinks.ReasonUnavailable}, Tasks: []tasklinks.TaskSummary{}}))
		return
	}
	writeJSON(w, http.StatusOK, taskLinkLookupResponse(identity, s.taskLinkOperations.Lookup(r.Context(), identity)))
}

func taskLinkLookupResponse(identity tasklinks.MessageIdentity, result tasklinks.LookupResult) TaskLinkLookupResponse {
	metadata := tasklinks.NewMailLink(identity, time.Time{})
	return TaskLinkLookupResponse{IndexStatus: result.IndexStatus, Tasks: result.Tasks, OutboundMetadata: TaskLinkOutboundMetadata{
		ArchiveUID: metadata.ArchiveUID, MessageID: metadata.MessageID, ConversationID: metadata.ConversationID,
		Subject: metadata.Subject, From: metadata.From, SentAt: metadata.SentAt,
		SourceType: metadata.SourceType, SourceIdentifier: metadata.SourceIdentifier, SourceMessageID: metadata.SourceMessageID,
	}}
}

func (s *Server) handleSearchIntegrationTasks(w http.ResponseWriter, r *http.Request) {
	if s.taskLinkOperations == nil {
		writeError(w, http.StatusServiceUnavailable, "task_integration_unavailable", "Task integration is unavailable")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" || len(query) > maxTaskSearchQueryLength {
		writeError(w, http.StatusBadRequest, "invalid_query", "Task search query is required and must be at most 256 bytes")
		return
	}
	tasks, err := s.taskLinkOperations.Search(r.Context(), query)
	if err != nil {
		writeTaskLinkError(w, err)
		return
	}
	if tasks == nil {
		tasks = []tasklinks.TaskSummary{}
	}
	writeJSON(w, http.StatusOK, TaskSearchResponse{Tasks: tasks})
}

func (s *Server) handleUnlinkMessageTask(w http.ResponseWriter, r *http.Request) {
	identity, httpErr := s.taskMessage(r)
	if httpErr != nil {
		writeAPIHTTPError(w, httpErr)
		return
	}
	if s.taskLinkOperations == nil {
		writeError(w, http.StatusServiceUnavailable, "task_integration_unavailable", "Task integration is unavailable")
		return
	}
	taskID := r.PathValue("task_id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task_id_required", "Task ID is required")
		return
	}
	task, err := s.taskLinkOperations.Unlink(r.Context(), taskID, identity)
	if err != nil {
		writeTaskLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, TaskLinkMutationResponse{Task: publicTask(task)})
}

func taskIdempotencyKey(requestID string, identity tasklinks.MessageIdentity) string {
	material := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d", requestID, identity.ArchiveUID, identity.SourceType, identity.SourceIdentifier, identity.SourceMessageID, identity.MessageID)
	digest := sha256.Sum256([]byte(material))
	return "msgvault-" + hex.EncodeToString(digest[:])
}

func publicTask(task taskclient.Task) TaskLinkTask {
	return TaskLinkTask{ID: task.ID, Project: task.Project, Title: task.Title, Revision: task.Revision}
}

func writeTaskLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskclient.ErrConflict):
		writeError(w, http.StatusConflict, "revision_conflict", "Task changed again; retry the operation")
	case errors.Is(err, taskclient.ErrAuthenticationRequired):
		writeError(w, http.StatusUnauthorized, "authentication_required", "Task service authentication is required")
	case errors.Is(err, taskclient.ErrIncompatible):
		writeError(w, http.StatusServiceUnavailable, "task_integration_incompatible", "Task service is incompatible")
	case errors.Is(err, taskclient.ErrRequestRejected):
		writeError(w, http.StatusUnprocessableEntity, "task_request_rejected", "The task service rejected this request; correct the request and retry")
	case errors.Is(err, tasklinks.ErrUnsafeMailLinks):
		writeError(w, http.StatusConflict, "unsafe_task_metadata", "Existing task metadata cannot be updated safely")
	default:
		writeError(w, http.StatusServiceUnavailable, "task_integration_unavailable", "Task service is unavailable")
	}
}

type archiveUIDReader interface {
	ArchiveUID() (string, error)
	ArchiveRevision() (string, error)
}

func (s *Server) resolveTaskMessageIdentity(_ context.Context, message *APIMessage) (tasklinks.MessageIdentity, error) {
	archive, ok := s.store.(archiveUIDReader)
	if !ok {
		return tasklinks.MessageIdentity{}, errors.New("archive identity unavailable")
	}
	uid, err := archive.ArchiveUID()
	if err != nil {
		return tasklinks.MessageIdentity{}, err
	}
	revision, err := archive.ArchiveRevision()
	if err != nil {
		return tasklinks.MessageIdentity{}, err
	}
	sources, ok := s.store.(SourceStatusStore)
	if !ok {
		return tasklinks.MessageIdentity{}, errors.New("source identity unavailable")
	}
	all, err := sources.ListSources("")
	if err != nil {
		return tasklinks.MessageIdentity{}, err
	}
	var sourceType, identifier string
	for _, source := range all {
		if source.ID == message.SourceID {
			sourceType, identifier = source.SourceType, source.Identifier
			break
		}
	}
	if sourceType == "" {
		return tasklinks.MessageIdentity{}, fmt.Errorf("source %d not found", message.SourceID)
	}
	return tasklinks.MessageIdentity{ArchiveUID: uid, ArchiveRevision: revision, MessageID: message.ID, ConversationID: message.ConversationID,
		Subject: message.Subject, From: message.From, SentAt: message.SentAt, SourceType: sourceType,
		SourceIdentifier: identifier, SourceMessageID: message.SourceMessageID}, nil
}

var _ archiveUIDReader = (*store.Store)(nil)
