package cmd

import (
	"context"
	"log/slog"
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
	body["title"] = "Engineer"
	replaced := operationOutput[mcpserver.EmploymentRecord](t, call("replace_employment", map[string]any{"employment_id": got.Employment.ID, "etag": got.ETag, "employment": body}))
	require.NotEqual(got.ETag, replaced.ETag)
	require.NotNil(replaced.Employment.Title)
	assert.Equal("Engineer", *replaced.Employment.Title)
	assertDefiniteRefusal(t, call("end_employment", map[string]any{"employment_id": got.Employment.ID, "etag": got.ETag, "end_date": "2024"}), "employment_revision_conflict")
	body["is_primary"] = true
	primary := operationOutput[mcpserver.EmploymentRecord](t, call("replace_employment", map[string]any{"employment_id": got.Employment.ID, "etag": replaced.ETag, "employment": body}))
	assert.True(primary.Employment.IsPrimary)
	ended := operationOutput[mcpserver.EmploymentRecord](t, call("end_employment", map[string]any{"employment_id": got.Employment.ID, "etag": primary.ETag, "end_date": "2024-05"}))
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
	_, err := st.GetEmploymentContext(t.Context(), got.Employment.ID)
	assert.ErrorIs(err, store.ErrEmploymentNotFound)
}

func TestMCPEmploymentDisclosure(t *testing.T) {
	assert := assert.New(t)
	requirements := require.New(t)
	b, _, person, org := employmentFixture(t)
	first, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": true, "description": "Synthetic employment note"}})
	requirements.NoError(err)
	record := operationOutput[mcpserver.EmploymentRecord](t, first)
	end, err := b.OperationDisclosure(t.Context(), "end_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "end_date": "2024-05"})
	requirements.NoError(err)
	assert.NotContains(end, "Synthetic employment note")
	assert.Contains(end, `"operation":"end_employment"`)
	assert.Contains(end, `"end_date":"2024-05"`)
	deletion, err := b.OperationDisclosure(t.Context(), "delete_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag})
	requirements.NoError(err)
	assert.Contains(deletion, `"operation":"delete_employment"`)
	assert.Contains(deletion, "Synthetic employment note")
	replacement, err := b.OperationDisclosure(t.Context(), "replace_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}})
	requirements.NoError(err)
	assert.Contains(replacement, "Synthetic employment note")
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

func TestMCPEmploymentCapabilities(t *testing.T) {
	all := []string{"get_employment", "list_person_employments", "list_organization_employments", "create_employment", "replace_employment", "end_employment", "delete_employment"}
	for _, tc := range []struct {
		missing, property string
		want              []string
	}{
		{"", "", all},
		{"getEmployment", "", []string{"list_person_employments", "list_organization_employments", "create_employment"}},
		{"listPersonEmployments", "", []string{"get_employment", "list_organization_employments", "end_employment", "delete_employment"}},
		{"", "expected_primary", []string{"get_employment", "list_person_employments", "list_organization_employments", "end_employment", "delete_employment"}},
	} {
		t.Run(tc.missing+tc.property, func(t *testing.T) {
			backend, _, _, _ := employmentFixture(t)
			capabilities, err := backend.client.MCPCapabilities(t.Context())
			require.NoError(t, err)
			capabilities.Routes = slices.DeleteFunc(capabilities.Routes, func(route apiprotocol.MCPRouteDescriptor) bool { return route.OperationID == tc.missing })
			for i := range capabilities.Routes {
				if capabilities.Routes[i].OperationID == "patchEmployment" || capabilities.Routes[i].OperationID == "createEmployment" {
					capabilities.Routes[i].RequestProperties = slices.DeleteFunc(capabilities.Routes[i].RequestProperties, func(property string) bool { return property == tc.property })
				}
			}
			got := newDaemonMCPOperations(backend.client, capabilities).capabilities()
			for _, name := range all {
				assert.Equal(t, slices.Contains(tc.want, name), slices.Contains(got, name), name)
			}
		})
	}
}

func TestMCPEmploymentWriteFlag(t *testing.T) {
	checks := assert.New(t)
	cmd := &cobra.Command{Use: "test"}
	addMCPWriteFlags(cmd, "mcp-")
	var opts mcpserver.ServeOptions
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	checks.NotContains(opts.OperationWriteFamilies, mcpserver.OperationFamilyEmployment)
	require.NoError(t, cmd.Flags().Set("mcp-allow-employment-writes", "true"))
	opts = mcpserver.ServeOptions{}
	applyMCPWriteFlags(cmd, "mcp-", &opts)
	checks.Contains(opts.OperationWriteFamilies, mcpserver.OperationFamilyEmployment)
	checks.NotContains(opts.OperationWriteFamilies, mcpserver.OperationFamilyRecords)
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
	assertDefiniteRefusal(t, result, "employment_primary_conflict")
	current, err := st.GetEmploymentContext(t.Context(), third.ID)
	require.NoError(err)
	assert.True(current.IsPrimary)
	current, err = st.GetEmploymentContext(t.Context(), first.ID)
	require.NoError(err)
	assert.False(current.IsPrimary)
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

func TestMCPEmploymentCreatePrimaryRefusesBeforeApproval(t *testing.T) {
	must := require.New(t)
	b, st, person, org := employmentFixture(t)
	_, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Primary"), Source: store.ProvenanceUser, IsPrimary: new(true)})
	must.NoError(err)
	approvals := 0
	session := legacyOperationSession(t, b, mcpserver.OperationFamilyEmployment, func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "create_employment", Arguments: map[string]any{"employment": map[string]any{"person_id": person, "organization_id": org, "title": "New", "source": "user", "is_primary": true}}})
	must.NoError(err)
	must.True(result.IsError)
	must.Equal("employment_primary_conflict", loopToolFailure(t, result)["error"])
	must.Zero(approvals)
}

func TestMCPEmploymentRejectsOversizedDescription(t *testing.T) {
	b, st, person, org := employmentFixture(t)
	body := map[string]any{"person_id": person, "organization_id": org, "source": "user", "is_primary": false}
	created, err := b.ExecuteOperation(t.Context(), "create_employment", map[string]any{"employment": body})
	require.NoError(t, err)
	record := operationOutput[mcpserver.EmploymentRecord](t, created)
	body["description"] = strings.Repeat("x", 131073)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"create_employment", map[string]any{"employment": body}},
		{"replace_employment", map[string]any{"employment_id": record.Employment.ID, "etag": record.ETag, "employment": body}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := b.ExecuteOperation(t.Context(), tc.name, tc.args)
			require.NoError(t, err)
			assertDefiniteRefusal(t, result, "invalid_arguments")
		})
	}
	got, err := st.GetEmploymentContext(t.Context(), record.Employment.ID)
	require.NoError(t, err)
	assert.Equal(t, record.Employment.Revision, got.Revision)
}
