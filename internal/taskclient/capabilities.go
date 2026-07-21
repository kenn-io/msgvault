package taskclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

type Capabilities struct {
	ProtocolVersion     string `json:"protocol_version"`
	RevisionReads       bool   `json:"revision_reads"`
	ConditionalMutation bool   `json:"conditional_metadata_mutation"`
	ConflictResponses   bool   `json:"conflict_responses"`
	IdempotentCreation  bool   `json:"idempotent_task_creation"`
	ProjectOperations   bool   `json:"project_operations"`
	MetadataOperations  bool   `json:"metadata_operations"`
}

func (c Capabilities) Compatible() bool {
	return c.ProtocolVersion == ProtocolVersion &&
		c.RevisionReads &&
		c.ConditionalMutation &&
		c.ConflictResponses &&
		c.IdempotentCreation &&
		c.ProjectOperations &&
		c.MetadataOperations
}

type State string

const (
	StateDisabled               State = "disabled"
	StateNotFound               State = "not_found"
	StateAuthenticationRequired State = "authentication_required"
	StateUnreachable            State = "unreachable"
	StateIncompatible           State = "incompatible"
	StateWrongProject           State = "wrong_project"
	StateReady                  State = "ready"
)

type Status struct {
	State        State  `json:"state"`
	Project      string `json:"project,omitempty"`
	Message      string `json:"message"`
	SecurityNote string `json:"security_note,omitempty"`
}

type IntegrationConfig struct {
	Enabled          bool
	Endpoint         string
	APIKey           string
	DefaultProject   string
	DescriptorPath   string
	Timeout          time.Duration
	MaxResponseBytes int64
	HTTPClient       *http.Client
	// platformSecurityCheck is a package-local test seam. Production callers
	// always use the platform check selected by Discover.
	platformSecurityCheck func() error
}

func Evaluate(ctx context.Context, cfg IntegrationConfig) Status {
	project := strings.TrimSpace(cfg.DefaultProject)
	if !cfg.Enabled {
		return status(StateDisabled, project, "Task integration is disabled.")
	}
	var client *Client
	var err error
	if strings.TrimSpace(cfg.Endpoint) == "" {
		client, err = Discover(ctx, DiscoveryOptions{
			DescriptorPath:        cfg.DescriptorPath,
			APIKey:                cfg.APIKey,
			Timeout:               cfg.Timeout,
			MaxResponseBytes:      cfg.MaxResponseBytes,
			HTTPClient:            cfg.HTTPClient,
			platformSecurityCheck: cfg.platformSecurityCheck,
		})
	} else {
		client, err = New(ClientOptions{
			Endpoint:         cfg.Endpoint,
			APIKey:           cfg.APIKey,
			Timeout:          cfg.Timeout,
			MaxResponseBytes: cfg.MaxResponseBytes,
			HTTPClient:       cfg.HTTPClient,
		})
	}
	if err != nil {
		return statusForError(err, project)
	}
	return evaluateClient(ctx, client, project)
}

// Connect returns an authenticated client only after the required capability
// and configured-project checks succeed. Callers use this same gate for every
// mutation so a previously-ready service cannot silently downgrade.
func Connect(ctx context.Context, cfg IntegrationConfig) (*Client, Project, error) {
	if !cfg.Enabled {
		return nil, Project{}, ErrNotFound
	}
	var client *Client
	var err error
	if strings.TrimSpace(cfg.Endpoint) == "" {
		client, err = Discover(ctx, DiscoveryOptions{
			DescriptorPath: cfg.DescriptorPath, APIKey: cfg.APIKey, Timeout: cfg.Timeout,
			MaxResponseBytes: cfg.MaxResponseBytes, HTTPClient: cfg.HTTPClient,
			platformSecurityCheck: cfg.platformSecurityCheck,
		})
	} else {
		client, err = New(ClientOptions{Endpoint: cfg.Endpoint, APIKey: cfg.APIKey, Timeout: cfg.Timeout,
			MaxResponseBytes: cfg.MaxResponseBytes, HTTPClient: cfg.HTTPClient})
	}
	if err != nil {
		return nil, Project{}, err
	}
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		return nil, Project{}, err
	}
	if !capabilities.Compatible() {
		return nil, Project{}, ErrIncompatible
	}
	projectName := strings.TrimSpace(cfg.DefaultProject)
	if projectName == "" {
		return nil, Project{}, ErrWrongProject
	}
	project, err := client.ResolveProject(ctx, projectName)
	if err != nil {
		return nil, Project{}, err
	}
	return client, project, nil
}

func evaluateClient(ctx context.Context, client *Client, project string) Status {
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return withClientSecurityNote(status(StateIncompatible, project, "Task service is reachable but does not implement the required capability protocol."), client)
		}
		return withClientSecurityNote(statusForError(err, project), client)
	}
	if !capabilities.Compatible() {
		return withClientSecurityNote(status(StateIncompatible, project, "Task service is reachable but lacks required revision, conditional mutation, conflict, idempotency, project, or metadata capabilities."), client)
	}
	if project == "" {
		return withClientSecurityNote(status(StateWrongProject, project, "The configured task project is empty or unavailable."), client)
	}
	if _, err := client.ResolveProject(ctx, project); err != nil {
		if errors.Is(err, ErrWrongProject) {
			return withClientSecurityNote(status(StateWrongProject, project, "The resolved task project did not match the configured project."), client)
		}
		if errors.Is(err, ErrNotFound) {
			return withClientSecurityNote(status(StateWrongProject, project, "The configured task project was not found."), client)
		}
		return withClientSecurityNote(statusForError(err, project), client)
	}
	return withClientSecurityNote(status(StateReady, project, "Task integration is ready."), client)
}

func withClientSecurityNote(result Status, client *Client) Status {
	if note := client.SecurityNote(); note != "" {
		result.SecurityNote = note
	}
	return result
}

func statusForError(err error, project string) Status {
	switch {
	case errors.Is(err, ErrDescriptorFileSecurityLimit):
		result := status(StateIncompatible, project, "This platform cannot safely use local task descriptor discovery.")
		result.SecurityNote = "Secure descriptor and token-file ownership, no-follow opening, and handle validation are unavailable; discovery was rejected."
		return result
	case errors.Is(err, ErrUnixSocketSecurityLimit):
		result := status(StateIncompatible, project, "This platform cannot safely use the configured Unix socket endpoint.")
		result.SecurityNote = "Required peer credentials and socket ownership/mode enforcement are unavailable; the endpoint was rejected."
		return result
	case errors.Is(err, ErrPlatformSecurityLimit):
		result := status(StateIncompatible, project, "This platform cannot prove the required local task integration security.")
		result.SecurityNote = "The operation was rejected because required platform security checks are unavailable."
		return result
	case errors.Is(err, ErrNotFound):
		return status(StateNotFound, project, "No compatible local task service was discovered.")
	case errors.Is(err, ErrAuthenticationRequired):
		return status(StateAuthenticationRequired, project, "Task service authentication is required.")
	case errors.Is(err, ErrIncompatible), errors.Is(err, ErrInsecureDescriptor), errors.Is(err, ErrInsecureEndpoint), errors.Is(err, ErrInvalidResponse), errors.Is(err, ErrResponseTooLarge), errors.Is(err, ErrRedirect):
		return status(StateIncompatible, project, "Task service is reachable or configured but incompatible with the required secure protocol.")
	default:
		return status(StateUnreachable, project, "Task service is unavailable.")
	}
}

func status(state State, project, message string) Status {
	return Status{State: state, Project: project, Message: message}
}
