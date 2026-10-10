package cmd

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func operationMCPSession(t *testing.T, backend *daemonMCPOperations, families []mcpserver.OperationFamily, clientOptions *sdkmcp.ClientOptions) *sdkmcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	directory := filepath.Join(t.TempDir(), "mcp")
	opts := daemonMCPServeOptions(ctx, backend.client, nil)
	opts.OperationWriteFamilies = families
	opts.AllowProfileWrites = slices.Contains(families, mcpserver.OperationFamilyRecords)
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeHTTPWithOptions(ctx, opts, mcpserver.HTTPOptions{Addr: "127.0.0.1:0", DiscoveryDirectory: directory, AllowWrites: true})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(10 * time.Second):
			assert.Fail(t, "MCP listener did not stop")
		}
	})
	var endpoints []mcpdiscovery.Endpoint
	require.Eventually(t, func() bool {
		var err error
		endpoints, err = mcpdiscovery.List(directory)
		return err == nil && len(endpoints) == 1
	}, 10*time.Second, 10*time.Millisecond)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, clientOptions)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: endpoints[0].URL}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func sourceOperationFixture(t *testing.T, fixture *api.Server) *daemonMCPOperations {
	t.Helper()
	server := httptest.NewServer(fixture.Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	capabilities, err := client.MCPCapabilities(t.Context())
	require.NoError(t, err)
	return newDaemonMCPOperations(client, capabilities)
}

func operationOutput[T any](t *testing.T, result *mcpserver.OperationResult) T {
	t.Helper()
	require.NotNil(t, result)
	require.False(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	var output T
	require.NoError(t, json.Unmarshal(data, &output))
	return output
}

func mcpSQLiteStoreWithPath(t *testing.T) (*store.Store, string) {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	var sequence int
	var name, path string
	requirements.NoError(st.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path))
	requirements.NotEmpty(path)
	return st, path
}

func TestMCPSourceStatusAndConfirmedIdentitiesUseRealStore(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "sender@example.com")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(source.ID, "alias@example.com", "synthetic-confirmation"))
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "list_source_status", map[string]any{"source_type": "gmail"})
	requirements.NoError(err)
	status := operationOutput[generated.SourceStatusResponse](t, result)
	requirements.Len(status.Sources, 1)
	assertions.Equal(source.ID, status.Sources[0].ID)
	result, err = backend.ExecuteOperation(t.Context(), "get_source_identities", map[string]any{"source_id": source.ID})
	requirements.NoError(err)
	identities := operationOutput[generated.SourceIdentitiesResponse](t, result)
	assertions.Equal(source.ID, identities.SourceID)
	requirements.Len(identities.Identities, 1)
	assertions.Equal("alias@example.com", identities.Identities[0].Identifier)
	assertions.Equal([]string{"synthetic-confirmation"}, identities.Identities[0].Signals)
}

func TestMCPNamedPromotionPreservesNativeRefusal(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRecords}, &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolPromotePerson, Arguments: map[string]any{"participant_id": 999999, "display_name": "Synthetic Name"}})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var refusal struct {
		Error                     string `json:"error"`
		OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
	}
	requirements.NoError(json.Unmarshal(data, &refusal))
	assertions.Equal("invalid_participant_id", refusal.Error)
	assertions.False(refusal.OperationMayHaveCompleted)
}

func TestMCPSlackPolicyPreservesFalseOmissionAndExactETag(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[slack]\ndms = true\ngroup_dms = true\nchannels = [\"C_EXAMPLE\"]\n"), 0o600))
	cfg, err := config.Load(path, "")
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	result, err := backend.ExecuteOperation(t.Context(), "get_slack_sync_policy", nil)
	requirements.NoError(err)
	policy := operationOutput[mcpserver.SlackSyncPolicy](t, result)
	requirements.NotEmpty(policy.ETag)
	assertions.Len(policy.Settings, 4)
	args := map[string]any{"etag": policy.ETag, "dms": false}
	disclosure, err := backend.OperationDisclosure(t.Context(), "update_slack_sync_policy", args)
	requirements.NoError(err)
	assertions.Contains(disclosure, `"dms":false`)
	result, err = backend.ExecuteOperation(t.Context(), "update_slack_sync_policy", args)
	requirements.NoError(err)
	updated := operationOutput[mcpserver.SlackSyncPolicy](t, result)
	assertions.NotEqual(policy.ETag, updated.ETag)
	assertions.True(updated.PendingRestart)
	reloaded, err := config.Load(path, "")
	requirements.NoError(err)
	assertions.False(reloaded.Slack.DMsEnabled())
	assertions.True(reloaded.Slack.GroupDMsEnabled())
	assertions.Equal([]string{"C_EXAMPLE"}, reloaded.Slack.Channels)
	result, err = backend.ExecuteOperation(t.Context(), "update_slack_sync_policy", map[string]any{"etag": policy.ETag, "group_dms": false})
	requirements.NoError(err)
	requirements.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(encoded), "settings_conflict")
	reloaded, err = config.Load(path, "")
	requirements.NoError(err)
	assertions.True(reloaded.Slack.GroupDMsEnabled())
}

func TestMCPSlackPolicySDKUsesRealDaemonAndApproval(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[slack]\ndms = true\n"), 0o600))
	cfg, err := config.Load(path, "")
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServer(cfg, nil, nil, slog.New(slog.DiscardHandler)))
	approved := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilySources}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, `"dms":false`)
		approved++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_slack_sync_policy"})
	requirements.NoError(err)
	requirements.False(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var policy mcpserver.SlackSyncPolicy
	requirements.NoError(json.Unmarshal(data, &policy))
	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_slack_sync_policy", Arguments: map[string]any{"etag": policy.ETag, "dms": false}})
	requirements.NoError(err)
	assertions.False(result.IsError)
	assertions.Equal(1, approved)
	reloaded, err := config.Load(path, "")
	requirements.NoError(err)
	assertions.False(reloaded.Slack.DMsEnabled())
}

func TestMCPPersonNameClearAndStaleRevisionUseOwnerCAS(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("contact@example.com", "Observed Example", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_person_edit_context", map[string]any{"person_id": person.ID})
	requirements.NoError(err)
	edit := operationOutput[mcpserver.PersonEditContext](t, result)
	assertions.Equal(person.Revision, edit.Person.Revision)
	requirements.NotEmpty(edit.ETag)
	result, err = backend.ExecuteOperation(t.Context(), "set_person_display_name", map[string]any{"person_id": person.ID, "etag": edit.ETag, "display_name": ""})
	requirements.NoError(err)
	updated := operationOutput[mcpserver.PersonEditContext](t, result)
	assertions.Nil(updated.Person.DisplayName)
	assertions.Greater(updated.Person.Revision, edit.Person.Revision)
	result, err = backend.ExecuteOperation(t.Context(), "set_person_display_name", map[string]any{"person_id": person.ID, "etag": edit.ETag, "display_name": "Stale Example"})
	requirements.NoError(err)
	assertions.True(result.IsError)
	current, err := st.GetPersonContext(context.Background(), person.ID)
	requirements.NoError(err)
	assertions.Nil(current.DisplayName)
}

type missingETagResponseWriter struct{ http.ResponseWriter }

func (w missingETagResponseWriter) WriteHeader(status int) {
	w.Header().Del("ETag")
	w.ResponseWriter.WriteHeader(status)
}
func (w missingETagResponseWriter) Write(data []byte) (int, error) {
	w.Header().Del("ETag")
	return w.ResponseWriter.Write(data)
}

func TestMCPPersonCommittedRenameWithMissingETagRemainsUncertain(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("contact@example.com", "Observed Example", "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	router := fixture.Router()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			router.ServeHTTP(missingETagResponseWriter{w}, r)
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	capabilities, err := client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	backend := newDaemonMCPOperations(client, capabilities)
	result, err := backend.ExecuteOperation(t.Context(), "get_person_edit_context", map[string]any{"person_id": person.ID})
	requirements.NoError(err)
	edit := operationOutput[mcpserver.PersonEditContext](t, result)
	result, err = backend.ExecuteOperation(t.Context(), "set_person_display_name", map[string]any{"person_id": person.ID, "etag": edit.ETag, "display_name": "Committed Example"})
	requirements.NoError(err)
	requirements.True(result.IsError)
	data, err := json.Marshal(result.Output)
	requirements.NoError(err)
	var receipt struct {
		Uncertain bool `json:"operation_may_have_completed"`
	}
	requirements.NoError(json.Unmarshal(data, &receipt))
	assertions.True(receipt.Uncertain)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(new("Committed Example"), current.DisplayName)
}

func TestMCPCacheBuildStatusKeepsUnknownJobRefusal(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	jobs := newCacheBuildJobs(t.Context(), nil, nil)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, CacheBuildStatusReader: jobs.status, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_cache_build_status", map[string]any{"job_id": "synthetic-missing-job"})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.IsError)
	encoded, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(encoded), "cache_build_not_found")
}

func TestMCPParticipantIdentityPreservesCacheUnavailableCode(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)}))
	result, err := backend.ExecuteOperation(t.Context(), "get_participant_identity", map[string]any{"participant_id": int64(1)})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.IsError)
	data, err := json.Marshal(result.Output)
	requirements.NoError(err)
	assertions.Contains(string(data), "analytical_cache_unavailable")
	assertions.NotContains(string(data), "Run msgvault")
}

func TestMCPPromotionSDKPreservesNameDefaultsAndIdempotence(t *testing.T) {
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRecords}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, "preserve an existing saved name")
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	for _, tc := range []struct {
		label, address string
		supplied       bool
		name           any
		want           *string
	}{
		{"omitted", "omitted@example.com", false, nil, new("Observed Example")},
		{"null", "null@example.com", true, nil, new("Observed Example")},
		{"explicit", "explicit@example.com", true, "Curated Example", new("Curated Example")},
		{"empty", "empty@example.com", true, "", nil},
	} {
		t.Run(tc.label, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			participant, err := st.EnsureParticipant(tc.address, "Observed Example", "example.com")
			requirements.NoError(err)
			args := map[string]any{"participant_id": participant}
			if tc.supplied {
				args["display_name"] = tc.name
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolPromotePerson, Arguments: args})
			requirements.NoError(err)
			requirements.False(result.IsError)
			data, err := json.Marshal(result.StructuredContent)
			requirements.NoError(err)
			var person generated.Person
			requirements.NoError(json.Unmarshal(data, &person))
			assertions.Equal(tc.want, person.DisplayName)
			result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcpserver.ToolPromotePerson, Arguments: map[string]any{"participant_id": participant, "display_name": "Replacement Example"}})
			requirements.NoError(err)
			assertions.False(result.IsError)
			current, err := st.GetPerson(person.ID)
			requirements.NoError(err)
			assertions.Equal(tc.want, current.DisplayName)
		})
	}
	assertions.Equal(7, approvals)
}

func TestMCPParticipantIdentityRetainsRealLinkedCluster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	directory := t.TempDir()
	analytics := filepath.Join(directory, "analytics")
	st, dbPath := mcpSQLiteStoreWithPath(t)
	primary, err := st.EnsureParticipantByIdentifier("email", "primary@example.com", "Primary Example")
	requirements.NoError(err)
	const secondaryKey = "beeper:example-account:whatsapp:example-service-account:@example:example.com"
	secondary, err := st.EnsureParticipantByIdentifier("beeper", secondaryKey, "Secondary Example")
	requirements.NoError(err)
	service, err := st.ResolveCommunicationServiceContext(t.Context(), "whatsapp")
	requirements.NoError(err)
	requirements.NoError(st.ClassifyParticipantIdentifierServiceContext(t.Context(), "beeper", secondaryKey, &service.ID, new("account"), new("example-whatsapp-account")))
	_, err = st.LinkParticipants(primary, secondary)
	requirements.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "sender@example.com")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic Thread")
	requirements.NoError(err)
	for index, participant := range []int64{primary, secondary} {
		_, err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: []string{"synthetic-primary", "synthetic-secondary"}[index], MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 9, index+1, 12, 0, 0, 0, time.UTC), Valid: true}, Subject: sql.NullString{String: "Synthetic subject", Valid: true}})
		requirements.NoError(err)
	}
	_, err = buildCache(dbPath, analytics, true)
	requirements.NoError(err)
	engine, err := query.NewDuckDBEngine(analytics, "", nil)
	requirements.NoError(err)
	t.Cleanup(func() {
		assertions.NoError(engine.Close())
	})
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st, analyticsDir: analytics}, Engine: engine, Logger: slog.New(slog.DiscardHandler)}))
	session := operationMCPSession(t, backend, nil, nil)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_participant_identity", Arguments: map[string]any{"participant_id": primary}})
	requirements.NoError(err)
	requirements.False(result.IsError)
	data, err := json.Marshal(result.StructuredContent)
	requirements.NoError(err)
	var identity generated.PersonSummary
	requirements.NoError(json.Unmarshal(data, &identity))
	requirements.NotNil(identity.Cluster)
	assertions.ElementsMatch([]int64{primary, secondary}, identity.Cluster.MemberIds)
	requirements.Len(identity.Cluster.Edges, 1)
	requirements.NotNil(identity.Cluster.Edges[0].LinkOrigin)
	assertions.Equal("manual", identity.Cluster.Edges[0].LinkOrigin.Kind)
	assertions.Len(identity.Identifiers, 2)
	assertions.Len(identity.Cluster.Members, 2)
	var secondaryIdentity *generated.PersonIdentifier
	for i := range identity.Identifiers {
		if identity.Identifiers[i].ParticipantID == secondary {
			secondaryIdentity = &identity.Identifiers[i]
		}
	}
	requirements.NotNil(secondaryIdentity)
	assertions.Equal(secondaryKey, secondaryIdentity.Value)
	assertions.Equal(new("whatsapp"), secondaryIdentity.ServiceSlug)
	assertions.Equal(new("account"), secondaryIdentity.ScopeKind)
	assertions.Equal(new("example-whatsapp-account"), secondaryIdentity.ScopeValue)
}

func TestMCPSourceToolsRequireActualRoutesAndRequestFields(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	backend := sourceOperationFixture(t, fixture)
	assertions.Subset(backend.capabilities(), []string{"list_source_status", "get_source_identities", "get_source_scheduler_status", "sync_source", "get_slack_sync_policy", "update_slack_sync_policy", "get_participant_identity", "get_cache_build_status", "get_person_edit_context", "set_person_display_name"})
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for i := range capabilities.Routes {
		if capabilities.Routes[i].OperationID == "patchSettings" || capabilities.Routes[i].OperationID == "patchPerson" || capabilities.Routes[i].OperationID == "createPerson" {
			capabilities.Routes[i].RequestProperties = nil
		}
	}
	missingFields := newDaemonMCPOperations(backend.client, capabilities)
	assertions.NotContains(missingFields.capabilities(), "update_slack_sync_policy")
	assertions.NotContains(missingFields.capabilities(), "set_person_display_name")
	assertions.False(supportsNamedPromotion(capabilities))
	assertions.Empty(newDaemonMCPOperations(backend.client, &apiprotocol.MCPCapabilities{Version: 1, Delegated: true}).capabilities())
}

func TestMCPSourceStatusRetainsRealSchedulerQueue(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "queued@example.com")
	requirements.NoError(err)
	gate := api.NewSerialOperationGate()
	release, ok := gate.BeginWork()
	requirements.True(ok)
	t.Cleanup(release)
	sched := scheduler.New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
	requirements.NoError(sched.AddAccount("queued@example.com", "0 2 * * *"))
	t.Cleanup(func() { release(); sched.Stop() })
	requirements.NoError(sched.TriggerSync("queued@example.com"))
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, Scheduler: &schedulerAdapter{scheduler: sched}, Logger: slog.New(slog.DiscardHandler)}))
	var status generated.SourceStatusResponse
	requirements.Eventually(func() bool {
		result, err := backend.ExecuteOperation(t.Context(), "list_source_status", nil)
		if err != nil || result == nil || result.IsError {
			return false
		}
		var ok bool
		status, ok = result.Output.(generated.SourceStatusResponse)
		requirements.True(ok)
		return len(status.Sources) == 1 && status.Sources[0].SchedulerQueued != nil && *status.Sources[0].SchedulerQueued
	}, 10*time.Second, 10*time.Millisecond)
	assertions.False(status.Sources[0].CanSync)
	assertions.Nil(status.Sources[0].SchedulerStartedAt)
}

func TestMCPCacheJobStatusRetainsRealLifecycle(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	directory := t.TempDir()
	_, dbPath := mcpSQLiteStoreWithPath(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	jobs := newCacheBuildJobs(ctx, nil, func(ctx context.Context, _ buildCacheMode) error {
		started <- struct{}{}
		select {
		case <-release:
			_, err := buildCache(dbPath, filepath.Join(directory, "analytics"), true)
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	jobs.logger = slog.New(slog.DiscardHandler)
	t.Cleanup(func() { cancel(); jobs.wg.Wait() })
	first, err := jobs.accept(buildCacheModeAuto)
	requirements.NoError(err)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		requirements.FailNow("cache job did not start")
	}
	pending, err := jobs.acceptAfterWrite(buildCacheModeAuto)
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, CacheBuildStatusReader: jobs.status, Logger: slog.New(slog.DiscardHandler)}))
	for _, tc := range []struct{ id, status string }{{first.JobID, api.CacheBuildRunning}, {pending.JobID, api.CacheBuildQueued}} {
		result, err := backend.ExecuteOperation(t.Context(), "get_cache_build_status", map[string]any{"job_id": tc.id})
		requirements.NoError(err)
		status := operationOutput[generated.CacheBuildStatus](t, result)
		assertions.Equal(tc.id, status.JobID)
		assertions.Equal(tc.status, status.Status)
		assertions.False(status.AcceptedAt.IsZero())
		assertions.Nil(status.FinishedAt)
	}
	close(release)
	// Two real cache publications run sequentially. Bound the observable
	// completion wait without making their throughput a correctness condition.
	requirements.Eventually(func() bool {
		status, ok := jobs.status(pending.JobID)
		return ok && status.Status == api.CacheBuildPublished
	}, 2*time.Minute, 10*time.Millisecond)
	for _, id := range []string{first.JobID, pending.JobID} {
		result, err := backend.ExecuteOperation(t.Context(), "get_cache_build_status", map[string]any{"job_id": id})
		requirements.NoError(err)
		status := operationOutput[generated.CacheBuildStatus](t, result)
		assertions.Equal(api.CacheBuildPublished, status.Status)
		assertions.NotNil(status.FinishedAt)
	}
	failedJobs := newCacheBuildJobs(t.Context(), nil, func(context.Context, buildCacheMode) error {
		_, err := buildCache(filepath.Join(directory, "missing", "archive.db"), filepath.Join(directory, "failed-analytics"), true)
		return err
	})
	failedJobs.logger = slog.New(slog.DiscardHandler)
	t.Cleanup(failedJobs.wg.Wait)
	failed, err := failedJobs.accept(buildCacheModeAuto)
	requirements.NoError(err)
	requirements.Eventually(func() bool {
		status, ok := failedJobs.status(failed.JobID)
		return ok && status.Status == api.CacheBuildFailed
	}, 10*time.Second, 10*time.Millisecond)
	failedBackend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, CacheBuildStatusReader: failedJobs.status, Logger: slog.New(slog.DiscardHandler)}))
	result, err := failedBackend.ExecuteOperation(t.Context(), "get_cache_build_status", map[string]any{"job_id": failed.JobID})
	requirements.NoError(err)
	status := operationOutput[generated.CacheBuildStatus](t, result)
	assertions.Equal(api.CacheBuildFailed, status.Status)
	requirements.NotNil(status.ErrorData)
	assertions.NotContains(*status.ErrorData, directory)
}
