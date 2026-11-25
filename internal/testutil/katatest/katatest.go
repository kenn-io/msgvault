// Package katatest runs a real Kata service behind a static token for tests.
package katatest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	kata "go.kenn.io/kata"
)

const (
	// Token is the static bearer token the service accepts.
	Token = "synthetic-token"
	// Project is the name of the seeded project.
	Project = "example"
)

// Kata is a running service with one seeded project.
type Kata struct {
	Service   *kata.Service
	ProjectID int64
}

// New starts a Kata service with a static token and the Project seeded, and
// closes it when the test ends. Callers serve Service.Handler() themselves.
func New(t *testing.T) Kata {
	t.Helper()
	return start(t, Project, kata.Config{Auth: kata.AuthConfig{Token: Token}})
}

// NewHosted starts Kata behind a host that admits every signed-in caller,
// with project seeded. Callers serve Handler().
func NewHosted(t *testing.T, project string) Kata {
	t.Helper()
	return start(t, project, kata.Config{Access: hostGrant{}, WorkerTransactionFence: allowTransaction})
}

// NewScoped starts Kata behind a host whose grant lets callers read and write
// issues one project at a time but never list issues across projects. Callers
// serve Handler().
func NewScoped(t *testing.T) Kata {
	t.Helper()
	return start(t, Project, kata.Config{Access: hostGrant{scoped: true}, WorkerTransactionFence: allowTransaction})
}

// Handler serves Service as the host would, with the caller already signed in.
func (k Kata) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Authorization")
		k.Service.Handler().ServeHTTP(w, r.WithContext(kata.WithPrincipal(r.Context(), kata.Principal{Subject: "synthetic-subject", Actor: "msgvault"})))
	})
}

// hostGrant admits every request, or with scoped set, refuses issue reads
// across projects.
type hostGrant struct{ scoped bool }

func (g hostGrant) Authorize(_ context.Context, request kata.AccessRequest) (kata.AccessDecision, error) {
	if g.scoped && request.Operation.AllProjects && request.Operation.Policy.Kind == kata.OperationTaskRead {
		return kata.AccessDecision{}, kata.ErrAccessDenied
	}
	return kata.AccessDecision{TransactionFence: allowTransaction}, nil
}

func allowTransaction(context.Context, kata.Transaction) error { return nil }

func start(t *testing.T, name string, config kata.Config) Kata {
	t.Helper()
	config.DSN = filepath.Join(t.TempDir(), "kata.db")
	service, err := kata.New(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project, err := service.EnsureProject(t.Context(), kata.ProjectSpec{UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: name})
	require.NoError(t, err)
	return Kata{Service: service, ProjectID: project.Project.ID}
}

// Endpoint addresses k behind server for actions msgvault never takes.
func (k Kata) Endpoint(server *httptest.Server) Endpoint {
	return Endpoint{URL: server.URL, Client: server.Client(), ProjectID: k.ProjectID}
}

// Endpoint is where a test serves Kata, for actions msgvault never takes.
type Endpoint struct {
	URL       string
	Client    *http.Client
	ProjectID int64
}

// CloseIssue closes an issue in the seeded project.
func (e Endpoint) CloseIssue(t *testing.T, uid string) {
	t.Helper()
	e.action(t, uid, "close", `{"actor":"msgvault","reason":"wontfix","message":"Closing this synthetic issue because the example no longer needs it."}`, nil)
}

// DeleteIssue soft-deletes an issue in the seeded project.
func (e Endpoint) DeleteIssue(t *testing.T, uid, qualifiedRef string) {
	t.Helper()
	e.action(t, uid, "delete", `{"actor":"msgvault"}`, map[string]string{"X-Kata-Confirm": "DELETE " + qualifiedRef})
}

// MoveIssue moves an issue at revision to the project with toProjectUID.
func (e Endpoint) MoveIssue(t *testing.T, uid, revision, toProjectUID string) {
	t.Helper()
	e.action(t, uid, "move", `{"actor":"msgvault","to_project_uid":"`+toProjectUID+`"}`, map[string]string{"If-Match": `"rev-` + revision + `"`})
}

func (e Endpoint) action(t *testing.T, uid, action, body string, headers map[string]string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/%s", e.URL, e.ProjectID, uid, action), strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+Token)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := e.Client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode, action)
}
