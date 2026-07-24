package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/taskclient"
)

const (
	taskIntegrationStatusPath = "/api/v1/integrations/tasks/status"
	taskIntegrationTestPath   = "/api/v1/integrations/tasks/test"
)

// TaskIntegrationProbe is the server-side capability boundary. Tests replace
// it with a synthetic probe; production uses taskclient.Evaluate.
type TaskIntegrationProbe func(context.Context, taskclient.IntegrationConfig) taskclient.Status

type TaskIntegrationStatusResponse struct {
	State        taskclient.State `json:"state" enum:"disabled,not_found,authentication_required,unreachable,incompatible,wrong_project,ready" doc:"Current task integration state"`
	Project      string           `json:"project,omitempty" doc:"Configured project when available"`
	Message      string           `json:"message" doc:"Credential-free status explanation"`
	SecurityNote string           `json:"security_note,omitempty" doc:"Platform-specific local socket security limitation"`
}

func (s *Server) registerTaskIntegrationRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRoute[TaskIntegrationStatusResponse](
		api,
		"getTaskIntegrationStatus",
		http.MethodGet,
		"/integrations/tasks/status",
		"Get task integration capability status",
		s.handleTaskIntegrationStatus,
	)
	registerAPIV1RawHumaJSONRoute[TaskIntegrationStatusResponse](
		api,
		"testTaskIntegration",
		http.MethodPost,
		"/integrations/tasks/test",
		"Test task integration discovery, authentication, capabilities, and project",
		s.handleTaskIntegrationStatus,
	)
}

func (s *Server) handleTaskIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Integrations.Tasks
	status := s.taskIntegrationProbe(r.Context(), taskclient.IntegrationConfig{
		Enabled:        cfg.Enabled,
		Endpoint:       cfg.Endpoint,
		APIKey:         cfg.APIKey,
		DefaultProject: cfg.DefaultProject,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, TaskIntegrationStatusResponse{
		State:        status.State,
		Project:      status.Project,
		Message:      status.Message,
		SecurityNote: status.SecurityNote,
	})
}
