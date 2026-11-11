package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func employmentFixture(t *testing.T) (*daemonMCPOperations, *store.Store, int64, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("worker@example.test", "Example Worker", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	org, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	return backend, st, person.ID, org.ID
}

func TestMCPEmploymentLifecycleNativeStore(t *testing.T) {
	b, st, person, org := employmentFixture(t)
	for _, name := range []string{"get_employment", "list_person_employments", "list_organization_employments", "create_employment", "replace_employment", "end_employment", "set_primary_employment", "delete_employment"} {
		assert.Contains(t, b.capabilities(), name)
	}
	call := func(name string, args map[string]any) *mcpserver.OperationResult {
		t.Helper()
		r, err := b.ExecuteOperation(t.Context(), name, args)
		require.NoError(t, err)
		require.NotNil(t, r)
		return r
	}
	body := map[string]any{"person_id": person, "organization_id": org, "source": "user", "start_date": "2020-03", "department": "Research", "is_current": true, "is_primary": false}
	created := operationOutput[mcpserver.EmploymentRecord](t, call("create_employment", map[string]any{"employment": body}))
	require.NotEmpty(t, created.ETag)
	assert.False(t, created.Employment.IsPrimary)
	require.NotNil(t, created.Employment.StartDate)
	require.NotNil(t, created.Employment.StartDate.Year)
	require.NotNil(t, created.Employment.StartDate.Month)
	assert.Equal(t, int64(2020), *created.Employment.StartDate.Year)
	assert.Equal(t, int64(3), *created.Employment.StartDate.Month)
	assert.Nil(t, created.Employment.StartDate.Day)
	got := operationOutput[mcpserver.EmploymentRecord](t, call("get_employment", map[string]any{"employment_id": created.Employment.ID}))
	assert.Equal(t, created.ETag, got.ETag)
	listed := operationOutput[generated.EmploymentsResponse](t, call("list_person_employments", map[string]any{"person_id": person, "current_only": true}))
	require.Len(t, listed.Employments, 1)
	listed = operationOutput[generated.EmploymentsResponse](t, call("list_organization_employments", map[string]any{"organization_id": org}))
	require.Len(t, listed.Employments, 1)
	body["title"] = "Engineer"
	delete(body, "department")
	replaced := operationOutput[mcpserver.EmploymentRecord](t, call("replace_employment", map[string]any{"employment_id": got.Employment.ID, "etag": got.ETag, "employment": body}))
	require.NotEqual(t, got.ETag, replaced.ETag)
	require.NotNil(t, replaced.Employment.Title)
	assert.Equal(t, "Engineer", *replaced.Employment.Title)
	assert.Nil(t, replaced.Employment.Department)
	stale := call("end_employment", map[string]any{"employment_id": got.Employment.ID, "etag": got.ETag, "end_date": "2024"})
	require.True(t, stale.IsError)
	data, err := json.Marshal(stale.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "employment_revision_conflict")
	assert.Contains(t, string(data), `"operation_may_have_completed":false`)
	primary := operationOutput[mcpserver.EmploymentRecord](t, call("set_primary_employment", map[string]any{"employment_id": got.Employment.ID, "etag": replaced.ETag}))
	assert.True(t, primary.Employment.IsPrimary)
	ended := operationOutput[mcpserver.EmploymentRecord](t, call("end_employment", map[string]any{"employment_id": got.Employment.ID, "etag": primary.ETag, "end_date": "2024-05"}))
	assert.False(t, ended.Employment.IsCurrent)
	assert.False(t, ended.Employment.IsPrimary)
	require.NotNil(t, ended.Employment.EndDate)
	require.NotNil(t, ended.Employment.EndDate.Year)
	require.NotNil(t, ended.Employment.EndDate.Month)
	assert.Equal(t, int64(2024), *ended.Employment.EndDate.Year)
	assert.Equal(t, int64(5), *ended.Employment.EndDate.Month)
	assert.Nil(t, ended.Employment.EndDate.Day)
	deleted := call("delete_employment", map[string]any{"employment_id": got.Employment.ID, "etag": ended.ETag})
	assert.False(t, deleted.IsError)
	_, err = st.GetEmploymentContext(t.Context(), got.Employment.ID)
	assert.ErrorIs(t, err, store.ErrEmploymentNotFound)
}

func TestMCPEmploymentSDKApprovalAndReadOnly(t *testing.T) {
	b, st, person, org := employmentFixture(t)
	denied := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyEmployment}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	request := &sdkmcp.CallToolParams{Name: "create_employment", Arguments: map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}}}
	refused, err := denied.CallTool(t.Context(), request)
	require.NoError(t, err)
	assert.True(t, refused.IsError)
	rows, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{PersonID: person})
	require.NoError(t, err)
	assert.Empty(t, rows)
	approvals := 0
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyEmployment}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(t, request.Params.Message, `"is_primary":false`)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_employment", Arguments: map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, approvals)
	var record mcpserver.EmploymentRecord
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &record))
	assert.Equal(t, person, record.Employment.PersonID)
	assert.False(t, record.Employment.IsPrimary)
	assert.NotEmpty(t, record.ETag)
}

func TestMCPEmploymentNativePrimaryConflictAndDisclosure(t *testing.T) {
	b, st, person, org := employmentFixture(t)
	other, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Other Example", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	first, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": true}})
	require.NoError(t, err)
	record := operationOutput[mcpserver.EmploymentRecord](t, first)
	args := map[string]any{"employment": map[string]any{"person_id": person, "organization_id": other.ID, "source": "user", "is_primary": true}}
	conflict, err := b.ExecuteOperation(t.Context(), "create_employment", args)
	require.NoError(t, err)
	require.True(t, conflict.IsError)
	data, err := json.Marshal(conflict.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "employment_primary_conflict")
	assert.Contains(t, string(data), `"operation_may_have_completed":false`)
	_, err = b.OperationDisclosure(t.Context(), "delete_employment", map[string]any{"employment_id": record.Employment.ID, "etag": "stale"})
	require.Error(t, err)
	assert.Equal(t, "employment_revision_conflict", err.Error())
}

func TestMCPEmploymentCapabilityRequiresNativeRoute(t *testing.T) {
	backend, _, _, _ := employmentFixture(t)
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	for i := range capabilities.Routes {
		switch capabilities.Routes[i].OperationID {
		case "createEmployment":
			capabilities.Routes[i].RequestProperties = nil
		case "listPersonEmployments":
			capabilities.Routes[i].QueryParameters = nil
		}
	}
	capabilities.Routes = slices.DeleteFunc(capabilities.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == "getEmployment" })
	missing := newDaemonMCPOperations(backend.client, capabilities).capabilities()
	for _, name := range []string{"get_employment", "create_employment", "list_person_employments", "replace_employment", "end_employment", "set_primary_employment", "delete_employment"} {
		assert.NotContains(t, missing, name)
	}
	assert.Contains(t, missing, "list_organization_employments")
}

// A fixed HTTP response exercises the daemon's ambiguous failure and response-size
// contract, which the real store cannot deterministically produce after a write.
func TestMCPEmploymentUncertainAndBoundedResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"unknown daemon failure", http.StatusInternalServerError, `{"error":"private details"}`, "operation_refused"},
		{"oversized response", http.StatusCreated, strings.Repeat("x", (1<<20)+1), "operation_response_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			b := &daemonMCPOperations{client: client, supported: []string{"create_employment"}}
			result, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user"}})
			require.NoError(t, err)
			require.True(t, result.IsError)
			data, err := json.Marshal(result.Output)
			require.NoError(t, err)
			assert.Contains(t, string(data), tc.code)
			assert.Contains(t, string(data), `"operation_may_have_completed":true`)
			assert.NotContains(t, string(data), "private details")
		})
	}
}

func TestMCPEmploymentWriteFlagsAreIndependent(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	addMCPWriteFlags(cmd, "mcp-")
	var opts mcpserver.ServeOptions
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyEmployment)
	require.NoError(t, cmd.Flags().Set("mcp-allow-employment-writes", "true"))
	opts = mcpserver.ServeOptions{}
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	assert.Contains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyEmployment)
	assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyRecords)
}
