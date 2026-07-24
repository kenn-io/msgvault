package taskclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluateStatusStates(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		status := Evaluate(context.Background(), IntegrationConfig{})
		assert.Equal(t, StateDisabled, status.State)
	})

	t.Run("not found", func(t *testing.T) {
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			DescriptorPath: filepath.Join(t.TempDir(), "missing.json"),
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateNotFound, status.State)
	})

	t.Run("authentication required", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("descriptor discovery requires Unix file security and fails closed on Windows")
		}
		path := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			DescriptorPath: path,
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateAuthenticationRequired, status.State)
	})

	t.Run("unreachable", func(t *testing.T) {
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       "http://127.0.0.1:1",
			APIKey:         "test-key",
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateUnreachable, status.State)
	})

	t.Run("stale Unix descriptor is unreachable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix socket discovery is covered by Unix test lanes")
		}
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			DescriptorPath: writeStaleUnixDescriptor(t),
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateUnreachable, status.State)
	})

	t.Run("reachable but incompatible", func(t *testing.T) {
		server := capabilityServer(t, Capabilities{
			ProtocolVersion:     ProtocolVersion,
			RevisionReads:       true,
			ProjectOperations:   true,
			MetadataOperations:  true,
			ConditionalMutation: true,
			ConflictResponses:   true,
			IdempotentCreation:  false,
		}, http.StatusOK)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       server.URL,
			APIKey:         "test-key",
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateIncompatible, status.State)
	})

	t.Run("reachable without capability protocol is incompatible", func(t *testing.T) {
		server := capabilityServer(t, Capabilities{}, http.StatusNotFound)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       server.URL,
			APIKey:         "test-key",
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateIncompatible, status.State)
	})

	for _, responseStatus := range []int{http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run("reached unsupported "+http.StatusText(responseStatus), func(t *testing.T) {
			server := capabilityServer(t, Capabilities{}, responseStatus)
			status := Evaluate(context.Background(), IntegrationConfig{
				Enabled:        true,
				Endpoint:       server.URL,
				APIKey:         "test-key",
				DefaultProject: "test-project",
			})
			assert.Equal(t, StateIncompatible, status.State)
		})
	}

	t.Run("live without CAS is incompatible", func(t *testing.T) {
		capabilities := compatibleCapabilities()
		capabilities.ConditionalMutation = false
		server := capabilityServer(t, capabilities, http.StatusOK)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       server.URL,
			APIKey:         "test-key",
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateIncompatible, status.State)
	})

	t.Run("wrong project", func(t *testing.T) {
		server := taskProtocolServer(t, compatibleCapabilities(), http.StatusNotFound)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       server.URL,
			APIKey:         "test-key",
			DefaultProject: "missing-project",
		})
		assert.Equal(t, StateWrongProject, status.State)
	})

	t.Run("well shaped unrelated project", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/capabilities":
				writeTestJSON(t, w, compatibleCapabilities())
			case "/api/v1/projects/test-project":
				writeTestJSON(t, w, Project{ID: "project-other-id", Name: "other-project", Revision: `"p1"`})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       server.URL,
			APIKey:         "test-key",
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateWrongProject, status.State)
		assert.Contains(t, status.Message, "did not match")
	})

	t.Run("ready", func(t *testing.T) {
		server := taskProtocolServer(t, compatibleCapabilities(), http.StatusOK)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			Endpoint:       server.URL,
			APIKey:         "test-key",
			DefaultProject: "test-project",
		})
		assert.Equal(t, StateReady, status.State)
		assert.Equal(t, "test-project", status.Project)
	})
}

func compatibleCapabilities() Capabilities {
	return Capabilities{
		ProtocolVersion:     ProtocolVersion,
		RevisionReads:       true,
		ProjectOperations:   true,
		MetadataOperations:  true,
		ConditionalMutation: true,
		ConflictResponses:   true,
		IdempotentCreation:  true,
	}
}

func capabilityServer(t *testing.T, capabilities Capabilities, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/capabilities", r.URL.Path)
		w.WriteHeader(status)
		if status == http.StatusOK {
			writeTestJSON(t, w, capabilities)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func taskProtocolServer(t *testing.T, capabilities Capabilities, projectStatus int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/capabilities":
			writeTestJSON(t, w, capabilities)
		case "/api/v1/projects/test-project", "/api/v1/projects/missing-project":
			if projectStatus == http.StatusOK {
				w.Header().Set("ETag", `"project-revision"`)
				writeTestJSON(t, w, Project{ID: "project-test", Name: "test-project"})
			} else {
				w.WriteHeader(projectStatus)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCapabilitiesRequireAllSafetyGuarantees(t *testing.T) {
	base := compatibleCapabilities()
	tests := []struct {
		name   string
		mutate func(*Capabilities)
	}{
		{"revision reads", func(c *Capabilities) { c.RevisionReads = false }},
		{"conditional mutation", func(c *Capabilities) { c.ConditionalMutation = false }},
		{"conflict responses", func(c *Capabilities) { c.ConflictResponses = false }},
		{"idempotent creation", func(c *Capabilities) { c.IdempotentCreation = false }},
		{"project operations", func(c *Capabilities) { c.ProjectOperations = false }},
		{"metadata operations", func(c *Capabilities) { c.MetadataOperations = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := base
			tt.mutate(&candidate)
			assert.False(t, candidate.Compatible())
		})
	}
	require.True(t, base.Compatible())
}

func TestEvaluateClientPropagatesSecurityNote(t *testing.T) {
	const note = "peer credentials unavailable; private socket path checks enforced"

	t.Run("wrong project", func(t *testing.T) {
		server := taskProtocolServer(t, compatibleCapabilities(), http.StatusNotFound)
		client := newLoopbackClient(t, server.URL, "note-test-key", nil)
		client.securityNote = note

		status := evaluateClient(context.Background(), client, "missing-project")

		assert.Equal(t, StateWrongProject, status.State)
		assert.Equal(t, note, status.SecurityNote)
	})

	t.Run("post-discovery authentication error", func(t *testing.T) {
		server := capabilityServer(t, Capabilities{}, http.StatusUnauthorized)
		client := newLoopbackClient(t, server.URL, "note-test-key", nil)
		client.securityNote = note

		status := evaluateClient(context.Background(), client, "test-project")

		assert.Equal(t, StateAuthenticationRequired, status.State)
		assert.Equal(t, note, status.SecurityNote)
	})
}

func TestPlatformSecurityLimitStatusContext(t *testing.T) {
	t.Run("descriptor discovery", func(t *testing.T) {
		assertions := assert.New(t)
		descriptorPath := writeDescriptor(t, descriptor{
			ProtocolVersion: ProtocolVersion,
			InstanceID:      "instance-test",
			Endpoint:        "http://127.0.0.1:32145",
		}, 0o600)
		status := Evaluate(context.Background(), IntegrationConfig{
			Enabled:        true,
			APIKey:         "must-not-leak-test-key",
			DefaultProject: "test-project",
			DescriptorPath: descriptorPath,
			platformSecurityCheck: func() error {
				return ErrDescriptorFileSecurityLimit
			},
		})

		assertions.Equal(StateIncompatible, status.State)
		assertions.Contains(status.Message, "descriptor")
		assertions.Contains(status.SecurityNote, "ownership")
		assertions.Contains(status.SecurityNote, "no-follow")
		assertions.NotContains(status.Message+status.SecurityNote, "Unix socket")
		assertions.NotContains(status.Message+status.SecurityNote, "must-not-leak-test-key")
	})

	t.Run("explicit Unix socket", func(t *testing.T) {
		assertions := assert.New(t)
		status := statusForError(ErrUnixSocketSecurityLimit, "test-project")

		assertions.Equal(StateIncompatible, status.State)
		assertions.Contains(status.Message, "Unix socket")
		assertions.Contains(status.SecurityNote, "peer credentials")
		assertions.NotContains(status.Message+status.SecurityNote, "descriptor")
	})
}
