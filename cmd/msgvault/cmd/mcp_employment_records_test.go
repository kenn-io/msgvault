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
	second := operationOutput[mcpserver.EmploymentRecord](t, call("create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_current": true, "is_primary": false}}))
	for _, tc := range []struct {
		name string
		key  string
		id   int64
	}{
		{"list_person_employments", "person_id", person},
		{"list_organization_employments", "organization_id", org},
	} {
		current := operationOutput[generated.EmploymentsResponse](t, call(tc.name, map[string]any{tc.key: tc.id, "current_only": true}))
		require.Len(t, current.Employments, 1)
		assert.Equal(t, second.Employment.ID, current.Employments[0].ID)
		firstPage := operationOutput[generated.EmploymentsResponse](t, call(tc.name, map[string]any{tc.key: tc.id, "limit": 1}))
		require.Len(t, firstPage.Employments, 1)
		assert.Equal(t, second.Employment.ID, firstPage.Employments[0].ID)
		secondPage := operationOutput[generated.EmploymentsResponse](t, call(tc.name, map[string]any{tc.key: tc.id, "limit": 1, "offset": 1}))
		require.Len(t, secondPage.Employments, 1)
		assert.Equal(t, ended.Employment.ID, secondPage.Employments[0].ID)
	}
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
	readOnly := operationMCPSession(t, b, nil, nil)
	listed, err := readOnly.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_employments", Arguments: map[string]any{"person_id": person}})
	require.NoError(t, err)
	require.False(t, listed.IsError)
	data, err = json.Marshal(listed.StructuredContent)
	require.NoError(t, err)
	var list generated.EmploymentsResponse
	require.NoError(t, json.Unmarshal(data, &list))
	require.Len(t, list.Employments, 1)
	assert.Equal(t, record.Employment.ID, list.Employments[0].ID)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "replace_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "title": "Engineer", "is_primary": false}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, 2, approvals)
	data, err = json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var replaced mcpserver.EmploymentRecord
	require.NoError(t, json.Unmarshal(data, &replaced))
	require.NotNil(t, replaced.Employment.Title)
	assert.Equal(t, "Engineer", *replaced.Employment.Title)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "delete_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag}})
	require.NoError(t, err)
	assert.Equal(t, "employment_revision_conflict", loopToolFailure(t, result)["error"])
	assert.Equal(t, false, loopToolFailure(t, result)["operation_may_have_completed"])
	assert.Equal(t, 3, approvals)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "replace_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "title": "Stale", "is_primary": false}}})
	require.NoError(t, err)
	assert.Equal(t, "employment_revision_conflict", loopToolFailure(t, result)["error"])
	assert.Equal(t, false, loopToolFailure(t, result)["operation_may_have_completed"])
	assert.Equal(t, 4, approvals)
	unchanged, err := st.GetEmploymentContext(t.Context(), record.Employment.ID)
	require.NoError(t, err)
	require.NotNil(t, unchanged.Title)
	assert.Equal(t, "Engineer", *unchanged.Title)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "delete_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": replaced.ETag}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Equal(t, 5, approvals)
	_, err = st.GetEmploymentContext(t.Context(), record.Employment.ID)
	assert.ErrorIs(t, err, store.ErrEmploymentNotFound)
}

func TestMCPEmploymentNativePrimaryConflictAndDisclosure(t *testing.T) {
	b, st, person, org := employmentFixture(t)
	other, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Other Example", Kind: store.OrganizationKindCompany})
	require.NoError(t, err)
	first, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": true, "description": "Synthetic employment note"}})
	require.NoError(t, err)
	record := operationOutput[mcpserver.EmploymentRecord](t, first)
	end, err := b.OperationDisclosure(t.Context(), "end_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "end_date": "2024-05"})
	require.NoError(t, err)
	assert.NotContains(t, end, "Synthetic employment note")
	assert.Contains(t, end, `"operation":"end_employment"`)
	assert.Contains(t, end, `"end_date":"2024-05"`)
	assert.Contains(t, end, "End this employment on the given date; it is no longer current or primary.")
	primary, err := b.OperationDisclosure(t.Context(), "set_primary_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag})
	require.NoError(t, err)
	assert.Contains(t, primary, `"operation":"set_primary_employment"`)
	assert.Contains(t, primary, "Make this employment primary; native demotes whichever other employment is primary when it runs. demotes shows the one read now, if any.")
	assert.NotContains(t, primary, `"demotes"`)
	args := map[string]any{"employment": map[string]any{"person_id": person, "organization_id": other.ID, "source": "user", "is_primary": true}}
	conflict, err := b.ExecuteOperation(t.Context(), "create_employment", args)
	require.NoError(t, err)
	require.True(t, conflict.IsError)
	data, err := json.Marshal(conflict.Output)
	require.NoError(t, err)
	assert.Contains(t, string(data), "employment_primary_conflict")
	assert.Contains(t, string(data), `"operation_may_have_completed":false`)
	second, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": other.ID, "source": "user", "is_primary": false, "is_current": true}})
	require.NoError(t, err)
	secondRecord := operationOutput[mcpserver.EmploymentRecord](t, second)
	disclosure, err := b.OperationDisclosure(t.Context(), "set_primary_employment", map[string]any{"employment_id": secondRecord.Employment.ID, "etag": secondRecord.ETag})
	require.NoError(t, err)
	var got struct {
		Demotes generated.Employment `json:"demotes"`
	}
	require.NoError(t, json.Unmarshal([]byte(disclosure), &got))
	assert.Equal(t, record.Employment.ID, got.Demotes.ID)
	assert.NotContains(t, disclosure, "Synthetic employment note")
	staleDisclosure, err := b.OperationDisclosure(t.Context(), "delete_employment", map[string]any{"employment_id": record.Employment.ID, "etag": "stale"})
	require.NoError(t, err)
	assert.Contains(t, staleDisclosure, `"etag":"stale"`)
	assert.Contains(t, staleDisclosure, `"operation":"delete_employment"`)
	_, err = b.OperationDisclosure(t.Context(), "set_primary_employment", map[string]any{"employment_id": int64(999999), "etag": `"employment-999999-r1"`})
	var refusal *mcpserver.OperationRefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, "employment_not_found", refusal.Code)
}

func TestMCPEmploymentCapabilityRequiresNativeRoute(t *testing.T) {
	backend, _, _, org := employmentFixture(t)
	_ = org
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
		name      string
		status    int
		body      string
		code      string
		operation string
	}{
		{"unknown daemon failure", http.StatusInternalServerError, `{"error":"private details"}`, "operation_refused", "create_employment"},
		{"oversized response", http.StatusCreated, strings.Repeat("x", (1<<20)+1), "operation_response_limit_exceeded", "create_employment"},
		{"delete daemon failure", http.StatusInternalServerError, `{"error":"private details"}`, "operation_refused", "delete_employment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.operation == "delete_employment" {
					assert.Equal(t, http.MethodDelete, r.Method)
					assert.Equal(t, "/api/v1/employments/1", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			b := &daemonMCPOperations{client: client, supported: []string{tc.operation}}
			args := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user"}}
			if tc.operation == "delete_employment" {
				args = map[string]any{"employment_id": 1, "etag": `"employment-1-r1"`}
			}
			result, err := b.ExecuteOperation(t.Context(), tc.operation, args)
			require.NoError(t, err)
			assertLoopRefusal(t, result, tc.code, true)
			require.True(t, result.IsError)
			data, err := json.Marshal(result.Output)
			require.NoError(t, err)
			assert.Contains(t, string(data), tc.code)
			assert.Contains(t, string(data), `"operation_may_have_completed":true`)
			assert.NotContains(t, string(data), "private details")
		})
	}
	gate := api.NewSerialOperationGate()
	gate.StartDrain()
	busy := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: testutil.NewTestStore(t), OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	result, err := busy.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user"}})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "server_busy", false)
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
