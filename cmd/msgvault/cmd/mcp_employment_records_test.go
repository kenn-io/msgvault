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
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	for _, name := range []string{"get_employment", "list_person_employments", "list_organization_employments", "create_employment", "replace_employment", "end_employment", "delete_employment"} {
		assert.Contains(b.capabilities(), name)
	}
	call := func(name string, args map[string]any) *mcpserver.OperationResult {
		t.Helper()
		r, err := b.ExecuteOperation(t.Context(), name, args)
		require.NoError(err)
		require.NotNil(r)
		return r
	}
	body := map[string]any{"person_id": person, "organization_id": org, "source": "user", "start_date": "2020-03", "department": "Research", "is_current": true, "is_primary": false}
	created := operationOutput[mcpserver.EmploymentRecord](t, call("create_employment", map[string]any{"employment": body}))
	require.NotEmpty(created.ETag)
	assert.False(created.Employment.IsPrimary)
	got := operationOutput[mcpserver.EmploymentRecord](t, call("get_employment", map[string]any{"employment_id": created.Employment.ID}))
	assert.Equal(created.ETag, got.ETag)
	listed := operationOutput[generated.EmploymentsResponse](t, call("list_person_employments", map[string]any{"person_id": person, "current_only": true}))
	require.Len(listed.Employments, 1)
	listed = operationOutput[generated.EmploymentsResponse](t, call("list_organization_employments", map[string]any{"organization_id": org}))
	require.Len(listed.Employments, 1)
	body["title"] = "Engineer"
	delete(body, "department")
	replaced := operationOutput[mcpserver.EmploymentRecord](t, call("replace_employment", map[string]any{"employment_id": got.Employment.ID, "etag": got.ETag, "employment": body}))
	require.NotEqual(got.ETag, replaced.ETag)
	require.NotNil(replaced.Employment.Title)
	assert.Equal("Engineer", *replaced.Employment.Title)
	assert.Nil(replaced.Employment.Department)
	stale := call("end_employment", map[string]any{"employment_id": got.Employment.ID, "etag": got.ETag, "end_date": "2024"})
	require.True(stale.IsError)
	data, err := json.Marshal(stale.Output)
	require.NoError(err)
	assert.Contains(string(data), "employment_revision_conflict")
	assert.Contains(string(data), `"operation_may_have_completed":false`)
	body["is_primary"] = true
	primary := operationOutput[mcpserver.EmploymentRecord](t, call("replace_employment", map[string]any{"employment_id": got.Employment.ID, "etag": replaced.ETag, "employment": body}))
	assert.True(primary.Employment.IsPrimary)
	ended := operationOutput[mcpserver.EmploymentRecord](t, call("end_employment", map[string]any{"employment_id": got.Employment.ID, "etag": primary.ETag, "end_date": "2024-05"}))
	assert.False(ended.Employment.IsCurrent)
	assert.False(ended.Employment.IsPrimary)
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
		require.Len(current.Employments, 1)
		assert.Equal(second.Employment.ID, current.Employments[0].ID)
	}
	deleted := call("delete_employment", map[string]any{"employment_id": got.Employment.ID, "etag": ended.ETag})
	assert.False(deleted.IsError)
	_, err = st.GetEmploymentContext(t.Context(), got.Employment.ID)
	assert.ErrorIs(err, store.ErrEmploymentNotFound)
}

func TestMCPEmploymentSDKApprovalAndReadOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	denied := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyEmployment}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "decline"}, nil
	}})
	request := &sdkmcp.CallToolParams{Name: "create_employment", Arguments: map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}}}
	refused, err := denied.CallTool(t.Context(), request)
	require.NoError(err)
	assert.True(refused.IsError)
	rows, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{PersonID: person})
	require.NoError(err)
	assert.Empty(rows)
	approvals := 0
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyEmployment}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		assert.Contains(request.Params.Message, `"is_primary":false`)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_employment", Arguments: map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}}})
	require.NoError(err)
	assert.False(result.IsError)
	assert.Equal(1, approvals)
	var record mcpserver.EmploymentRecord
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(err)
	require.NoError(json.Unmarshal(data, &record))
	assert.Equal(person, record.Employment.PersonID)
	assert.False(record.Employment.IsPrimary)
	assert.NotEmpty(record.ETag)
	readOnly := operationMCPSession(t, b, nil, nil)
	listed, err := readOnly.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "list_person_employments", Arguments: map[string]any{"person_id": person}})
	require.NoError(err)
	require.False(listed.IsError)
	data, err = json.Marshal(listed.StructuredContent)
	require.NoError(err)
	var list generated.EmploymentsResponse
	require.NoError(json.Unmarshal(data, &list))
	require.Len(list.Employments, 1)
	assert.Equal(record.Employment.ID, list.Employments[0].ID)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "replace_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "title": "Engineer", "is_primary": false}}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(2, approvals)
	data, err = json.Marshal(result.StructuredContent)
	require.NoError(err)
	var replaced mcpserver.EmploymentRecord
	require.NoError(json.Unmarshal(data, &replaced))
	require.NotNil(replaced.Employment.Title)
	assert.Equal("Engineer", *replaced.Employment.Title)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "delete_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag}})
	require.NoError(err)
	assert.Equal("employment_revision_conflict", loopToolFailure(t, result)["error"])
	assert.Equal(false, loopToolFailure(t, result)["operation_may_have_completed"])
	assert.Equal(3, approvals)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "replace_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "title": "Stale", "is_primary": false}}})
	require.NoError(err)
	assert.Equal("employment_revision_conflict", loopToolFailure(t, result)["error"])
	assert.Equal(false, loopToolFailure(t, result)["operation_may_have_completed"])
	assert.Equal(4, approvals)
	unchanged, err := st.GetEmploymentContext(t.Context(), record.Employment.ID)
	require.NoError(err)
	require.NotNil(unchanged.Title)
	assert.Equal("Engineer", *unchanged.Title)
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "delete_employment", Arguments: map[string]any{"employment_id": record.Employment.ID, "etag": replaced.ETag}})
	require.NoError(err)
	require.False(result.IsError)
	assert.Equal(5, approvals)
	_, err = st.GetEmploymentContext(t.Context(), record.Employment.ID)
	assert.ErrorIs(err, store.ErrEmploymentNotFound)
}

func TestMCPEmploymentNativePrimaryConflictAndDisclosure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	other, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Other Example", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	first, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": true, "description": "Synthetic employment note"}})
	require.NoError(err)
	record := operationOutput[mcpserver.EmploymentRecord](t, first)
	end, err := b.OperationDisclosure(t.Context(), "end_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "end_date": "2024-05"})
	require.NoError(err)
	assert.NotContains(end, "Synthetic employment note")
	assert.Contains(end, `"operation":"end_employment"`)
	assert.Contains(end, `"end_date":"2024-05"`)
	assert.Contains(end, "End this employment on the given date; it is no longer current or primary.")
	replace, err := b.OperationDisclosure(t.Context(), "replace_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "end_date": "2024-05"}})
	require.NoError(err)
	assert.Contains(replace, "Replace mutable fields; omitted ones clear. Omitted is_current is kept only without end_date; omitted is_primary only while the record stays current.")
	assert.Contains(replace, "Synthetic employment note")
	args := map[string]any{"employment": map[string]any{"person_id": person, "organization_id": other.ID, "source": "user", "is_primary": true}}
	conflict, err := b.ExecuteOperation(t.Context(), "create_employment", args)
	require.NoError(err)
	require.True(conflict.IsError)
	data, err := json.Marshal(conflict.Output)
	require.NoError(err)
	assert.Contains(string(data), "employment_primary_conflict")
	assert.Contains(string(data), `"operation_may_have_completed":false`)
	second, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": other.ID, "source": "user", "is_primary": false, "is_current": true}})
	require.NoError(err)
	secondRecord := operationOutput[mcpserver.EmploymentRecord](t, second)
	replaceArgs := map[string]any{"employment_id": secondRecord.Employment.ID, "etag": secondRecord.ETag, "employment": map[string]any{"person_id": person, "organization_id": other.ID, "source": "user", "is_primary": true}}
	disclosure, err := b.OperationDisclosure(t.Context(), "replace_employment", replaceArgs)
	require.NoError(err)
	var got struct {
		Primary generated.Employment `json:"primary"`
	}
	require.NoError(json.Unmarshal([]byte(disclosure), &got))
	assert.Equal(record.Employment.ID, got.Primary.ID)
	assert.NotContains(disclosure, "Synthetic employment note")
	staleDisclosure, err := b.OperationDisclosure(t.Context(), "delete_employment", map[string]any{"employment_id": record.Employment.ID, "etag": "stale"})
	require.NoError(err)
	assert.Contains(staleDisclosure, `"etag":"stale"`)
	assert.Contains(staleDisclosure, `"operation":"delete_employment"`)
	assert.Contains(staleDisclosure, "Synthetic employment note")

}

func TestMCPEmploymentCapabilityRequiresNativeRoute(t *testing.T) {
	backend, _, _, _ := employmentFixture(t) //nolint:dogsled // shared fixture returns four values; test needs only backend
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
	for _, name := range []string{"get_employment", "create_employment", "list_person_employments", "replace_employment", "end_employment", "delete_employment"} {
		assert.NotContains(t, missing, name)
	}
	assert.Contains(t, missing, "list_organization_employments")
}

func TestMCPEmploymentReplaceRequiresPrimaryReadAndGuard(t *testing.T) {
	for _, missing := range []string{"listPersonEmployments", "expected_primary"} {
		t.Run(missing, func(t *testing.T) {
			require := require.New(t)
			backend, _, _, _ := employmentFixture(t) //nolint:dogsled // shared fixture returns four values
			capabilities, err := backend.client.MCPCapabilities(t.Context())
			require.NoError(err)
			require.Contains(newDaemonMCPOperations(backend.client, capabilities).capabilities(), "replace_employment")
			if missing == "listPersonEmployments" {
				capabilities.Routes = slices.DeleteFunc(capabilities.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == missing })
			} else {
				for i := range capabilities.Routes {
					if capabilities.Routes[i].OperationID == "patchEmployment" {
						capabilities.Routes[i].RequestProperties = slices.DeleteFunc(capabilities.Routes[i].RequestProperties, func(property string) bool { return property == missing })
					}
				}
			}
			assert.NotContains(t, newDaemonMCPOperations(backend.client, capabilities).capabilities(), "replace_employment")
		})
	}
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
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.operation == "delete_employment" {
					assert.Equal(http.MethodDelete, r.Method)
					assert.Equal("/api/v1/employments/1", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			b := &daemonMCPOperations{client: client, supported: []string{tc.operation}}
			args := map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "is_primary": false}}
			if tc.operation == "delete_employment" {
				args = map[string]any{"employment_id": 1, "etag": `"employment-1-r1"`}
			}
			result, err := b.ExecuteOperation(t.Context(), tc.operation, args)
			require.NoError(err)
			assertLoopRefusal(t, result, tc.code, true)
			require.True(result.IsError)
			data, err := json.Marshal(result.Output)
			require.NoError(err)
			assert.Contains(string(data), tc.code)
			assert.Contains(string(data), `"operation_may_have_completed":true`)
			assert.NotContains(string(data), "private details")
		})
	}
	gate := api.NewSerialOperationGate()
	gate.StartDrain()
	busy := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: testutil.NewTestStore(t), OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	result, err := busy.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "is_primary": false}})
	require.NoError(t, err)
	assertLoopRefusal(t, result, "server_busy", false)
}

func TestMCPEmploymentWriteFlagsAreIndependent(t *testing.T) {
	assert := assert.New(t)
	cmd := &cobra.Command{Use: "test"}
	addMCPWriteFlags(cmd, "mcp-")
	var opts mcpserver.ServeOptions
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	assert.NotContains(opts.OperationWriteFamilies, mcpserver.OperationFamilyEmployment)
	require.NoError(t, cmd.Flags().Set("mcp-allow-employment-writes", "true"))
	opts = mcpserver.ServeOptions{}
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	assert.Contains(opts.OperationWriteFamilies, mcpserver.OperationFamilyEmployment)
	assert.NotContains(opts.OperationWriteFamilies, mcpserver.OperationFamilyRecords)
}

func TestMCPEmploymentApprovedPrimaryIsGuarded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	first, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("First"), Source: store.ProvenanceUser})
	require.NoError(err)
	second, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Second"), IsPrimary: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	third, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Third"), IsPrimary: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	read, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": second.ID})
	require.NoError(err)
	secondRecord := operationOutput[mcpserver.EmploymentRecord](t, read)
	args := map[string]any{"employment_id": second.ID, "etag": secondRecord.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "title": "Second", "source": "user", "is_primary": true}}
	disclosure, err := b.OperationDisclosure(t.Context(), "replace_employment", args)
	require.NoError(err)
	assert.Contains(disclosure, `"primary":`)
	assert.Contains(disclosure, `"title":"First"`)
	third, err = st.SetPrimaryEmploymentContext(t.Context(), third.ID, third.Revision)
	require.NoError(err)
	result, err := b.ExecuteOperation(t.Context(), "replace_employment", args)
	require.NoError(err)
	require.True(result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(err)
	assert.Contains(string(data), "employment_primary_conflict")
	current, err := st.GetEmploymentContext(t.Context(), third.ID)
	require.NoError(err)
	assert.True(current.IsPrimary)
	current, err = st.GetEmploymentContext(t.Context(), first.ID)
	require.NoError(err)
	assert.False(current.IsPrimary)

}

func TestMCPEmploymentPrimaryAppearsAfterDisclosure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	target, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Target"), IsPrimary: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	read, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": target.ID})
	require.NoError(err)
	record := operationOutput[mcpserver.EmploymentRecord](t, read)
	replace := map[string]any{"employment_id": target.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "title": "Target", "source": "user", "is_primary": true}}
	_, err = b.OperationDisclosure(t.Context(), "replace_employment", replace)
	require.NoError(err)
	create := map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "title": "Another", "source": "user", "is_primary": true}}
	_, err = b.OperationDisclosure(t.Context(), "create_employment", create)
	require.NoError(err)
	primary, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Primary"), IsPrimary: new(true), Source: store.ProvenanceUser})
	require.NoError(err)
	for _, call := range []struct {
		name string
		args map[string]any
	}{{"replace_employment", replace}, {"create_employment", create}} {
		result, err := b.ExecuteOperation(t.Context(), call.name, call.args)
		require.NoError(err)
		require.True(result.IsError)
		data, err := json.Marshal(result.Output)
		require.NoError(err)
		assert.Contains(string(data), "employment_primary_conflict")
	}
	current, err := st.GetEmploymentContext(t.Context(), primary.ID)
	require.NoError(err)
	assert.True(current.IsPrimary)
}

func TestMCPNativeRefusalsAreDefinite(t *testing.T) {
	codes := []string{"invalid_arguments", "unauthorized", "not_found", "scheduler_unavailable", "source_not_schedulable", "store_unavailable", "missing_account", "sync_error", "operation_in_progress", "settings_conflict", "settings_edit_rejected", "if_match_required", "person_revision_conflict", "person_profile_not_found", "person_enrichment_dispatch_in_progress", "persons_unavailable", "cache_build_not_found", "cache_build_unavailable", "bad_request", "invalid_participant_id", "participant_not_found", "analytical_cache_unavailable", "query_resource_exhausted", "engine_unavailable", "employment_not_found", "employment_revision_conflict", "employment_primary_conflict", "employment_duplicate_active", "invalid_employment", "invalid_employment_id", "invalid_partial_date", "invalid_source", "invalid_person_id", "invalid_organization_id", "invalid_organization", "organization_not_found", "organizations_unavailable", "invalid_if_match", "employments_unavailable", "server_busy", "profile_values_unavailable", "profile_media_not_found", "profile_media_content_unavailable", "invalid_profile_media_id", "profile_value_not_found", "invalid_profile_value", "profile_patch_too_large", "person_category_duplicate", "service_alias_conflict", "loop_not_found", "loop_decision_not_found", "loop_scan_incomplete", "loops_unavailable", "invalid_request", "invalid_status", "organization_revision_conflict", "organization_has_employments", "organization_merge_conflict", "organization_profile_too_large", "invalid_attribute_slug", "invalid_attribute_definition", "attribute_definition_not_found", "attribute_definition_inactive", "attribute_definition_not_writable", "invalid_attribute_value", "attribute_value_not_found", "attribute_value_conflict", "invalid_ordinal", "invalid_expected_value_id"}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			b := &daemonMCPOperations{client: client, supported: []string{"create_employment"}}
			result, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": 1, "organization_id": 2, "source": "user", "is_primary": false}})
			require.NoError(err)
			assertLoopRefusal(t, result, code, false)
		})
	}
}

func TestMCPEmploymentResumedApprovalRechecksPrimary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	first, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("First"), Source: store.ProvenanceUser})
	require.NoError(err)
	second, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Second"), IsPrimary: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	third, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Third"), IsPrimary: new(false), Source: store.ProvenanceUser})
	require.NoError(err)
	read, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": second.ID})
	require.NoError(err)
	record := operationOutput[mcpserver.EmploymentRecord](t, read)
	session := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyEmployment}, &sdkmcp.ClientOptions{ElicitationHandler: func(ctx context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assert.Contains(request.Params.Message, `"title":"First"`)
		_, changeErr := st.SetPrimaryEmploymentContext(ctx, third.ID, third.Revision)
		require.NoError(changeErr)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "replace_employment", Arguments: map[string]any{"employment_id": second.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "title": "Second", "source": "user", "is_primary": true}}})
	require.NoError(err)
	require.True(result.IsError)
	data, err := json.Marshal(result.Content)
	require.NoError(err)
	assert.Contains(string(data), "confirmation does not match")
	current, err := st.GetEmploymentContext(t.Context(), third.ID)
	require.NoError(err)
	assert.True(current.IsPrimary)
	current, err = st.GetEmploymentContext(t.Context(), first.ID)
	require.NoError(err)
	assert.False(current.IsPrimary)
	stable := operationMCPSession(t, b, []mcpserver.OperationFamily{mcpserver.OperationFamilyEmployment}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err = stable.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "replace_employment", Arguments: map[string]any{"employment_id": second.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "title": "Second", "source": "user", "is_primary": true}}})
	require.NoError(err)
	assert.False(result.IsError, "%v", result.Content)
	current, err = st.GetEmploymentContext(t.Context(), second.ID)
	require.NoError(err)
	assert.True(current.IsPrimary)
}

func legacyOperationSession(t *testing.T, b *daemonMCPOperations, family mcpserver.OperationFamily, handler func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error)) *sdkmcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeTransport(ctx, mcpserver.ServeOptions{Operations: b, OperationCapabilities: b.capabilities(), OperationWriteFamilies: []mcpserver.OperationFamily{family}, AllowProfileWrites: family == mcpserver.OperationFamilyRecords}, serverTransport)
	}()
	t.Cleanup(func() { cancel(); <-done })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, &sdkmcp.ClientOptions{ElicitationHandler: handler})
	session, err := client.Connect(ctx, clientTransport, &sdkmcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestMCPEmploymentDescriptionDisclosure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		refuse bool
	}{{"small", 12, false}, {"limit", 8 << 10, false}, {"over limit", (8 << 10) + 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			b, st, person, org := employmentFixture(t)
			description := strings.Repeat("x", tc.size)
			first, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser, Description: &description, IsPrimary: new(true)})
			must.NoError(err)
			read, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": first.ID})
			must.NoError(err)
			for _, call := range []struct {
				name string
				args map[string]any
			}{
				{"replace_employment", map[string]any{"employment_id": first.ID, "etag": read.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user"}}},
				{"delete_employment", map[string]any{"employment_id": first.ID, "etag": read.ETag}},
			} {
				disclosure, err := b.OperationDisclosure(t.Context(), call.name, call.args)
				if tc.refuse {
					var refusal *mcpserver.OperationRefusalError
					must.ErrorAs(err, &refusal)
					must.Equal("employment_description_too_large", refusal.Code)
				} else {
					must.NoError(err)
					must.Contains(disclosure, description)
				}
			}
			end, err := b.OperationDisclosure(t.Context(), "end_employment", map[string]any{"employment_id": first.ID, "etag": read.ETag, "end_date": "2024"})
			must.NoError(err)
			must.NotContains(end, description)
			second, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser, Title: new("Other"), IsPrimary: new(false)})
			must.NoError(err)
			secondRead, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": second.ID})
			must.NoError(err)
			demotion, err := b.OperationDisclosure(t.Context(), "replace_employment", map[string]any{"employment_id": second.ID, "etag": secondRead.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": true}})
			must.NoError(err)
			must.NotContains(demotion, description)
		})
	}
}

func TestMCPEmploymentDescriptionChangedDuringApproval(t *testing.T) {
	must := require.New(t)
	b, st, person, org := employmentFixture(t)
	employment, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser, Description: new("Approved description")})
	must.NoError(err)
	read, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": employment.ID})
	must.NoError(err)
	session := legacyOperationSession(t, b, mcpserver.OperationFamilyEmployment, func(ctx context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assert.Contains(t, request.Params.Message, "Approved description")
		_, err := st.UpdateEmploymentContext(ctx, employment.ID, employment.Revision, store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser, Description: new("Changed description")})
		assert.NoError(t, err)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "delete_employment", Arguments: map[string]any{"employment_id": employment.ID, "etag": read.ETag}})
	must.NoError(err)
	must.Equal("operation_context_changed", loopToolFailure(t, result)["error"])
	current, err := st.GetEmploymentContext(t.Context(), employment.ID)
	must.NoError(err)
	must.Equal("Changed description", *current.Description)
}
