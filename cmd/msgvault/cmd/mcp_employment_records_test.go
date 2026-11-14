package cmd

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
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

func TestMCPEmploymentDisclosure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, _, person, org := employmentFixture(t)
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
					if capabilities.Routes[i].OperationID == "patchEmployment" || capabilities.Routes[i].OperationID == "createEmployment" {
						capabilities.Routes[i].RequestProperties = slices.DeleteFunc(capabilities.Routes[i].RequestProperties, func(property string) bool { return property == missing })
					}
				}
			}
			assert.NotContains(t, newDaemonMCPOperations(backend.client, capabilities).capabilities(), "replace_employment")
			assert.NotContains(t, newDaemonMCPOperations(backend.client, capabilities).capabilities(), "create_employment")
		})
	}
}

func TestMCPRecordWriteFlagsAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family mcpserver.OperationFamily
	}{
		{"employment", mcpserver.OperationFamilyEmployment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			addMCPWriteFlags(cmd, "mcp-")
			var opts mcpserver.ServeOptions
			applyMCPWriteFlags(cmd, "mcp-", &opts)
			assert.NotContains(t, opts.OperationWriteFamilies, tc.family)
			require.NoError(t, cmd.Flags().Set("mcp-allow-"+tc.name+"-writes", "true"))
			opts = mcpserver.ServeOptions{}
			applyMCPWriteFlags(cmd, "mcp-", &opts)
			assert.Contains(t, opts.OperationWriteFamilies, tc.family)
			assert.NotContains(t, opts.OperationWriteFamilies, mcpserver.OperationFamilyRecords)
		})
	}
}

func TestMCPEmploymentApprovedPrimaryIsGuarded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b, st, person, org := employmentFixture(t)
	first, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("First"), Description: new("Private primary description"), Source: store.ProvenanceUser})
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
	assert.NotContains(disclosure, "Private primary description")
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
	b, _, _, org := employmentFixture(t)
	for _, tc := range []struct {
		name, path, code string
		options          runtime.RequestOptions
	}{
		{"employment", "/api/v1/employments/{id}", "invalid_employment_id", &generated.GetEmploymentRequestOptions{PathParams: &generated.GetEmploymentPath{ID: -1}}},
		{"organization", "/api/v1/organizations/{id}", "invalid_organization_id", &generated.GetOrganizationRequestOptions{PathParams: &generated.GetOrganizationPath{ID: -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := readMCPOperationJSON[map[string]any](t.Context(), b.client, http.MethodGet, tc.path, tc.options, http.StatusOK)
			require.NoError(t, err)
			assertLoopRefusal(t, result, tc.code, false)
		})
	}
	result, err := readMCPOperationJSON[map[string]any](t.Context(), b.client, http.MethodPost, "/api/v1/organizations/{id}/attributes", &generated.SetOrganizationAttributeRequestOptions{PathParams: &generated.SetOrganizationAttributePath{ID: org}, Body: &generated.SetOrganizationAttributeBody{DefinitionSlug: "example", Source: "user", Value: generated.AttributeValue{Type: "text", Text: new("value")}, ExpectedValueID: new(int64(-1))}}, http.StatusCreated)
	require.NoError(t, err)
	assertLoopRefusal(t, result, "invalid_expected_value_id", false)
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

func TestMCPEmploymentCreateRefusesDisappearedPrimary(t *testing.T) {
	must := require.New(t)
	b, st, person, org := employmentFixture(t)
	primary, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Primary"), Source: store.ProvenanceUser, IsPrimary: new(true)})
	must.NoError(err)
	session := legacyOperationSession(t, b, mcpserver.OperationFamilyEmployment, func(ctx context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		_, err := st.EndEmploymentContext(ctx, primary.ID, primary.Revision, store.PartialDate{Year: new(2024)})
		assert.NoError(t, err)
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_employment", Arguments: map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "title": "New", "source": "user", "is_primary": true}}})
	must.NoError(err)
	must.True(result.IsError)
	must.Equal("operation_context_changed", loopToolFailure(t, result)["error"])
}

func TestMCPEmploymentLargeDescriptionApproval(t *testing.T) {
	for _, name := range []string{"replace_employment", "delete_employment"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			b, st, person, org := employmentFixture(t)
			description := strings.Repeat("x", 8193)
			record, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser, Description: &description})
			must.NoError(err)
			read, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": record.ID})
			must.NoError(err)
			calls := 0
			session := legacyOperationSession(t, b, mcpserver.OperationFamilyEmployment, func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
				calls++
				assert.Contains(t, request.Params.Message, description)
				return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
			})
			args := map[string]any{"employment_id": record.ID, "etag": read.ETag}
			if name == "replace_employment" {
				args["employment"] = map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
			must.NoError(err)
			must.False(result.IsError)
			must.Equal(1, calls)
		})
	}
	t.Run("read limit", func(t *testing.T) {
		must := require.New(t)
		b, st, person, org := employmentFixture(t)
		description := strings.Repeat("x", 1<<20)
		record, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser, Description: &description})
		must.NoError(err)
		_, err = b.OperationDisclosure(t.Context(), "delete_employment", map[string]any{"employment_id": record.ID, "etag": "any"})
		var refusal *mcpserver.OperationRefusalError
		must.ErrorAs(err, &refusal)
		must.Equal("operation_response_limit_exceeded", refusal.Code)
	})
}
