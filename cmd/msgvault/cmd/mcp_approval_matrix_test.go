package cmd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type approvalCase struct {
	backend *daemonMCPOperations
	family  mcpserver.OperationFamily
	name    string
	args    map[string]any
	change  func()
}

// The proxy leaves the real daemon routes in use and can make the disclosure
// read unavailable after the client has received its approval prompt.
func approvalReadProxy(t *testing.T, backend *daemonMCPOperations) (*atomic.Bool, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	target, err := url.Parse(backend.client.BaseURL())
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var unavailable, armed atomic.Bool
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() && r.Method == http.MethodGet {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if armed.Load() && r.Method != http.MethodGet {
			// Attribute disclosures post native dry runs; count only real writes.
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if !bytes.Contains(body, []byte(`"dry_run":true`)) {
				writes.Add(1)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	backend.client = client
	return &unavailable, &armed, &writes
}

func exerciseApprovalMatrix(t *testing.T, makeCase func(*testing.T) approvalCase) {
	t.Helper()
	for _, mode := range []string{"legacy", "resumed"} {
		for _, state := range []string{"stable", "changed", "unreadable"} {
			t.Run(mode+"/"+state, func(t *testing.T) {
				must := require.New(t)
				c := makeCase(t)
				unavailable, armed, writes := approvalReadProxy(t, c.backend)
				apply := func() {
					switch state {
					case "changed":
						c.change()
					case "unreadable":
						unavailable.Store(true)
					}
					armed.Store(true)
				}
				var session *sdkmcp.ClientSession
				if mode == "legacy" {
					session = legacyOperationSession(t, c.backend, c.family, func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
						apply()
						return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
					})
				} else {
					session = loopOperationMCPSession(t, c.backend, []mcpserver.OperationFamily{c.family}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
				}
				params := &sdkmcp.CallToolParams{Name: c.name, Arguments: c.args}
				result, err := session.CallTool(t.Context(), params)
				must.NoError(err)
				if mode == "resumed" {
					must.NotEmpty(result.RequestState)
					apply()
					params.RequestState = result.RequestState
					params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
					result, err = session.CallTool(t.Context(), params)
					must.NoError(err)
				}
				must.Equal(state != "stable", result.IsError, "%v", result.Content)
				if mode == "legacy" && state == "changed" {
					codes := map[mcpserver.OperationFamily]string{mcpserver.OperationFamilySources: "source_not_schedulable", mcpserver.OperationFamilyRecords: "person_revision_conflict", mcpserver.OperationFamilyCurated: "person_revision_conflict", mcpserver.OperationFamilyEmployment: "operation_context_changed", mcpserver.OperationFamilyOrganizations: "attribute_value_conflict"}
					must.Equal(codes[c.family], loopToolFailure(t, result)["error"])
				}
				if state == "stable" {
					must.Equal(int32(1), writes.Load())
				} else {
					must.Zero(writes.Load())
				}
			})
		}
	}
}

func TestApprovalMatrixSource(t *testing.T) {
	exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
		t.Helper()
		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("gmail", "sender@example.test")
		require.NoError(t, err)
		sched := scheduler.New(func(context.Context, string) error { return nil })
		t.Cleanup(func() { <-sched.Stop().Done() })
		require.NoError(t, sched.AddAccount("sender@example.test", "0 2 * * *"))
		b := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Scheduler: &schedulerAdapter{scheduler: sched}, Logger: slog.New(slog.DiscardHandler)}))
		return approvalCase{b, mcpserver.OperationFamilySources, "sync_source", map[string]any{"account": "sender@example.test", "source_type": "gmail"}, func() { _, err := st.StartSync(source.ID, "incremental"); require.NoError(t, err) }}
	})
}

func TestApprovalMatrixPersonName(t *testing.T) {
	exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
		t.Helper()
		st := testutil.NewTestStore(t)
		participant, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(t, err)
		b := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
		initial, err := b.ExecuteOperation(t.Context(), "get_person_edit_context", map[string]any{"person_id": person.ID})
		require.NoError(t, err)
		return approvalCase{b, mcpserver.OperationFamilyRecords, "set_person_display_name", map[string]any{"person_id": person.ID, "etag": initial.ETag, "display_name": "Approved"}, func() {
			result, err := b.ExecuteOperation(t.Context(), "set_person_display_name", map[string]any{"person_id": person.ID, "etag": initial.ETag, "display_name": "Changed"})
			require.NoError(t, err)
			require.False(t, result.IsError)
		}}
	})
}

func TestApprovalMatrixCuratedPerson(t *testing.T) {
	exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
		t.Helper()
		b, id := personRecordFixture(t)
		initial, err := b.ExecuteOperation(t.Context(), "get_person_record", map[string]any{"person_id": id})
		require.NoError(t, err)
		patch := func(name string) map[string]any {
			return map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "formatted", "formatted": name, "envelope": map[string]any{"source": "user"}}}}}
		}
		return approvalCase{b, mcpserver.OperationFamilyCurated, "patch_person_record", map[string]any{"person_id": id, "etag": initial.ETag, "patch": patch("Approved")}, func() {
			result, err := b.ExecuteOperation(t.Context(), "patch_person_record", map[string]any{"person_id": id, "etag": initial.ETag, "patch": patch("Changed")})
			require.NoError(t, err)
			require.False(t, result.IsError)
		}}
	})
}

func TestApprovalMatrixEmployment(t *testing.T) {
	exerciseApprovalMatrix(t, func(t *testing.T) approvalCase {
		t.Helper()
		must := require.New(t)
		b, st, person, org := employmentFixture(t)
		_, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Source: store.ProvenanceUser})
		must.NoError(err)
		target, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Target"), IsPrimary: new(false), Source: store.ProvenanceUser})
		must.NoError(err)
		other, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{PersonID: person, OrganizationID: org, Title: new("Other"), IsPrimary: new(false), Source: store.ProvenanceUser})
		must.NoError(err)
		initial, err := b.ExecuteOperation(t.Context(), "get_employment", map[string]any{"employment_id": target.ID})
		must.NoError(err)
		return approvalCase{b, mcpserver.OperationFamilyEmployment, "replace_employment", map[string]any{"employment_id": target.ID, "etag": initial.ETag, "employment": map[string]any{"person_id": person, "organization_id": org, "title": "Target", "source": "user", "is_primary": true}}, func() {
			_, err := st.SetPrimaryEmploymentContext(t.Context(), other.ID, other.Revision)
			must.NoError(err)
		}}
	})
}
