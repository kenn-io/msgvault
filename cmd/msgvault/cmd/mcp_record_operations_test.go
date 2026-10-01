package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPRecordDiscoveryUsesActualOwningRoutes(t *testing.T) {
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStore(t)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	for _, name := range []string{"get_person_record", "list_person_record_history", "update_person_record", "list_person_attributes", "set_person_attribute", "remove_person_attribute", "list_attribute_definitions", "get_attribute_definition", "create_attribute_definition", "update_attribute_definition", "remove_attribute_definition", "get_person_record_media"} {
		assertions.Contains(backend.capabilities(), name)
	}
}

func TestMCPRecordPatchUsesNativeAtomicRevisionAndCallerETag(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStore(t)
	participant, err := st.EnsureParticipant("record@example.test", "Synthetic Record Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	approvals := 0
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRecords}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_record", Arguments: map[string]any{"person_id": person.ID}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var read struct {
		ETag string `json:"etag"`
	}
	requirements.NoError(json.Unmarshal(data, &read))
	requirements.NotEmpty(read.ETag)
	args := map[string]any{"person_id": person.ID, "etag": read.ETag, "patch": map[string]any{
		"names":      map[string]any{"add": []any{map[string]any{"name_kind": "nickname", "formatted": "Synthetic Alias", "envelope": map[string]any{"source": "user"}}}},
		"categories": map[string]any{"add": []any{map[string]any{"original_value": "synthetic-category", "envelope": map[string]any{"source": "user"}}}},
	}}
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_person_record", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError, "%s", settingsMCPDiagnostic(called))
	profile, err := st.GetPersonProfileContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(person.Revision+1, profile.Person.Revision, "two actions commit as one native revision")
	requirements.Len(profile.Names, 1)
	assertions.Equal("Synthetic Alias", *profile.Names[0].Formatted)
	requirements.Len(profile.Categories, 1)
	assertions.Equal("synthetic-category", profile.Categories[0].OriginalValue)
	assertions.Equal(1, approvals)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_person_record", Arguments: args})
	requirements.NoError(err)
	assertions.True(called.IsError, "the original ETag must remain stale")
	assertions.Equal(1, approvals, "stale observation must refuse before approval")
	unchanged, err := st.GetPersonProfileContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(profile.Person.Revision, unchanged.Person.Revision)
}

func recordSDKFixture(t *testing.T) (*store.Store, *store.Person, *daemonMCPOperations, *sdkmcp.ClientSession, *int) {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewSQLiteTestStore(t)
	participant, err := st.EnsureParticipant("record@example.test", "Synthetic Record Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	approvals := new(int)
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyRecords}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, _ *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		*approvals++
		return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}})
	return st, person, backend, session, approvals
}

func callRecordTool[T any](t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, called.IsError, "%s", settingsMCPDiagnostic(called))
	data, err := json.Marshal(called.StructuredContent)
	require.NoError(t, err)
	var value T
	require.NoError(t, json.Unmarshal(data, &value))
	return value
}

func TestMCPRecordDefinitionNullablePatchAndSeededGuards(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, backend, session, approvals := recordSDKFixture(t)
	created := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": "synthetic_toggle", "label": "Synthetic toggle", "description": "Synthetic definition", "value_type": "boolean", "field_type": "checkbox", "is_sensitive": true, "is_audited": true, "display_order": 0}})
	assertions.Equal("user", created.Definition.Ownership)
	assertions.True(created.Definition.IsSensitive)
	requirements.NotEmpty(created.Definition.UniversalID)
	requirements.NotEmpty(created.ETag)
	updated := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "update_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": created.ETag, "changes": map[string]any{"description": nil, "is_sensitive": false, "display_order": 0}})
	assertions.Nil(updated.Definition.Description)
	assertions.False(updated.Definition.IsSensitive)
	assertions.NotEqual(created.ETag, updated.ETag)
	native, err := st.GetAttributeDefinitionContext(t.Context(), created.Definition.ID)
	requirements.NoError(err)
	assertions.Nil(native.Description)
	assertions.Equal(created.Definition.Revision+1, native.Revision)
	count := *approvals
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_attribute_definition", Arguments: map[string]any{"definition_id": created.Definition.ID, "etag": created.ETag, "changes": map[string]any{"label": "Stale rename"}}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(count, *approvals)
	for _, changes := range []map[string]any{{"label": nil}, {"is_sensitive": nil}, {"is_active": nil}, {"value_type": "text"}, {}} {
		result, err := backend.ExecuteOperation(t.Context(), "update_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": updated.ETag, "changes": changes})
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	removed := callRecordTool[struct {
		Removed bool `json:"removed"`
	}](t, session, "remove_attribute_definition", map[string]any{"definition_id": created.Definition.ID, "etag": updated.ETag})
	assertions.True(removed.Removed)
	_, err = st.GetAttributeDefinitionContext(t.Context(), created.Definition.ID)
	requirements.ErrorIs(err, store.ErrAttributeDefinitionNotFound)
	notes, err := st.GetAttributeDefinitionBySlugContext(t.Context(), store.AttributeObjectPerson, store.AttributeSlugNotes)
	requirements.NoError(err)
	seeded := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "get_attribute_definition", map[string]any{"definition_id": notes.ID})
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_attribute_definition", Arguments: map[string]any{"definition_id": notes.ID, "etag": seeded.ETag}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Contains(settingsMCPDiagnostic(called), "attribute_definition_not_deletable")
}

func TestMCPRecordAttributeFalseCASHistoryAndSensitiveSelection(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, _ := recordSDKFixture(t)
	definition := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": "synthetic_toggle", "label": "Synthetic toggle", "value_type": "boolean", "field_type": "checkbox", "is_sensitive": true}})
	args := map[string]any{"person_id": person.ID, "slug": definition.Definition.Slug, "value": map[string]any{"value": map[string]any{"type": "boolean", "boolean": false}, "source": "user", "ordinal": 0}, "dry_run": true}
	preview := callRecordTool[generated.PersonAttributeWrite](t, session, "set_person_attribute", args)
	assertions.True(preview.DryRun)
	native, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Definition.Slug})
	requirements.NoError(err)
	assertions.Empty(native)
	args["dry_run"] = false
	first := callRecordTool[generated.PersonAttributeWrite](t, session, "set_person_attribute", args)
	requirements.NotNil(first.Value)
	requirements.NotNil(first.Value.Value.Boolean)
	assertions.False(*first.Value.Value.Boolean)
	_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugNotes, Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Private synthetic Notes")}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	for _, readArgs := range []map[string]any{{"person_id": person.ID}, {"person_id": person.ID, "include_sensitive": true}, {"person_id": person.ID, "fields": []any{definition.Definition.Slug, "notes"}}} {
		read := callRecordTool[generated.PersonAttributesResponse](t, session, "list_person_attributes", readArgs)
		for _, group := range read.Attributes {
			assertions.NotEqual("notes", group.Definition.Slug)
			assertions.NotEqual(definition.Definition.Slug, group.Definition.Slug)
		}
	}
	selected := callRecordTool[generated.PersonAttributesResponse](t, session, "list_person_attributes", map[string]any{"person_id": person.ID, "fields": []any{definition.Definition.Slug, "notes"}, "include_sensitive": true, "history": true})
	requirements.Len(selected.Attributes, 2)
	value := map[string]any{"value": map[string]any{"type": "boolean", "boolean": true}, "source": "user", "expected_value_id": first.Value.ID}
	second := callRecordTool[generated.PersonAttributeWrite](t, session, "set_person_attribute", map[string]any{"person_id": person.ID, "slug": definition.Definition.Slug, "value": value})
	requirements.NotNil(second.Value)
	requirements.NotNil(second.Superseded)
	assertions.Equal(first.Value.ID, second.Superseded.ID)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person.ID, "slug": definition.Definition.Slug, "value": value}})
	requirements.NoError(err)
	requirements.True(called.IsError, "%s", settingsMCPDiagnostic(called))
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var conflict mcpserver.AttributeConflict
	requirements.NoError(json.Unmarshal(data, &conflict))
	assertions.Equal("attribute_value_conflict", conflict.Error)
	requirements.NotNil(conflict.CurrentValueID)
	assertions.Equal(second.Value.ID, *conflict.CurrentValueID)
	requirements.NotNil(conflict.CurrentValue)
	assertions.Equal(second.Value.ID, conflict.CurrentValue.ID)
	assertions.True(*conflict.CurrentValue.Value.Boolean)
	removed := callRecordTool[generated.PersonAttributeWrite](t, session, "remove_person_attribute", map[string]any{"person_id": person.ID, "slug": definition.Definition.Slug, "expected_value_id": second.Value.ID, "ordinal": 0})
	requirements.NotNil(removed.Superseded)
	assertions.Equal(second.Value.ID, removed.Superseded.ID)
	history := callRecordTool[generated.PersonAttributesResponse](t, session, "list_person_attributes", map[string]any{"person_id": person.ID, "fields": []any{definition.Definition.Slug}, "include_sensitive": true, "history": true})
	requirements.Len(history.Attributes, 1)
	assertions.Empty(history.Attributes[0].Current)
	requirements.Len(history.Attributes[0].History, 2)
	record := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "get_attribute_definition", map[string]any{"definition_id": definition.Definition.ID})
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "remove_attribute_definition", Arguments: map[string]any{"definition_id": definition.Definition.ID, "etag": record.ETag}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Contains(settingsMCPDiagnostic(called), "attribute_definition_has_values")
}

func TestMCPRecordStoredMediaScopeBoundsAndNoURLDereference(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, backend, session, _ := recordSDKFixture(t)
	requests := atomic.Int64{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusTeapot) }))
	defer remote.Close()
	payload := []byte("synthetic inline profile media")
	inline, err := st.AddPersonMediaContext(t.Context(), person.ID, store.PersonMediaInput{MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"), Data: payload, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	uri, err := st.AddPersonMediaContext(t.Context(), person.ID, store.PersonMediaInput{MediaKind: store.PersonMediaPhoto, URI: new(remote.URL + "/private-photo"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	large, err := st.AddPersonMediaContext(t.Context(), person.ID, store.PersonMediaInput{MediaKind: store.PersonMediaPhoto, Data: bytes.Repeat([]byte("x"), mcpRecordMediaLimit+1), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	participant, err := st.EnsureParticipant("other-record@example.test", "Synthetic Other Person", "example.test")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	read := callRecordTool[mcpserver.PersonRecordMedia](t, session, "get_person_record_media", map[string]any{"person_id": person.ID, "media_id": inline.Envelope.ID})
	assertions.Equal(payload, read.Data)
	assertions.Equal(int64(len(payload)), read.ByteSize)
	assertions.Equal("image/png", read.MediaType)
	current := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": person.ID})
	assertions.Nil(current.Media)
	selected := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": person.ID, "fields": []any{"media"}})
	requirements.NotNil(selected.Media)
	requirements.Len(*selected.Media, 3)
	for _, test := range []struct {
		person, media int64
		code          string
	}{{other.ID, inline.Envelope.ID, "profile_media_not_found"}, {person.ID, uri.Envelope.ID, "profile_media_content_unavailable"}, {person.ID, large.Envelope.ID, "profile_media_limit_exceeded"}} {
		called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_record_media", Arguments: map[string]any{"person_id": test.person, "media_id": test.media}})
		requirements.NoError(err)
		assertions.True(called.IsError)
		assertions.Contains(settingsMCPDiagnostic(called), test.code)
	}
	for _, args := range []map[string]any{{"person_id": person.ID, "fields": []any{"observations"}}, {"person_id": person.ID, "fields": []any{"observations"}, "include_sensitive": false}, {"person_id": person.ID, "fields": []any{"unknown"}}, {"person_id": person.ID, "fields": nil}} {
		result, err := backend.ExecuteOperation(t.Context(), "list_person_record_history", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
	history := callRecordTool[mcpserver.PersonRecord](t, session, "list_person_record_history", map[string]any{"person_id": person.ID, "fields": []any{"observations"}, "include_sensitive": true})
	requirements.NotNil(history.Observations)
	assertions.Nil(history.Media)
	assertions.Nil(history.Names)
	participantID := person.ParticipantIDs[0]
	observed, err := st.RecordContactObservationContext(t.Context(), participantID, store.ParticipantContactObservationInput{AddressKind: store.ContactAddressEmail, OriginalValue: "observed-record@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	history = callRecordTool[mcpserver.PersonRecord](t, session, "list_person_record_history", map[string]any{"person_id": person.ID, "fields": []any{"observations"}, "include_sensitive": true})
	requirements.NotNil(history.Observations)
	requirements.Len(*history.Observations, 1)
	assertions.Equal(observed.Observation.Envelope.ID, (*history.Observations)[0].Envelope.ID)
	assertions.Equal("observed-record@example.test", (*history.Observations)[0].OriginalValue)
	assertions.Zero(requests.Load(), "no owning operation dereferences profile URIs")
}

func TestMCPRecordDiscoveryDropsMissingOwningContract(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: testutil.NewSQLiteTestStore(t), config: cfg}, Logger: slog.New(slog.DiscardHandler)}))
	capabilities, err := backend.client.MCPCapabilities(t.Context())
	requirements.NoError(err)
	for _, route := range mcpRecordRoutes {
		changed := *capabilities
		changed.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(descriptor apiprotocol.MCPRouteDescriptor) bool { return descriptor.OperationID == route.id })
		assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
		for _, property := range route.properties {
			changed.Routes = slices.Clone(capabilities.Routes)
			for i := range changed.Routes {
				if changed.Routes[i].OperationID == route.id {
					changed.Routes[i].RequestProperties = slices.DeleteFunc(slices.Clone(changed.Routes[i].RequestProperties), func(value string) bool { return value == property })
				}
			}
			assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
		}
		if route.contextID != "" {
			changed.Routes = slices.DeleteFunc(slices.Clone(capabilities.Routes), func(descriptor apiprotocol.MCPRouteDescriptor) bool { return descriptor.OperationID == route.contextID })
			assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
			for _, query := range route.contextQuery {
				changed.Routes = slices.Clone(capabilities.Routes)
				for i := range changed.Routes {
					if changed.Routes[i].OperationID == route.contextID {
						changed.Routes[i].QueryParameters = slices.DeleteFunc(slices.Clone(changed.Routes[i].QueryParameters), func(value string) bool { return value == query })
					}
				}
				assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
			}
		}
		for _, query := range route.query {
			changed.Routes = slices.Clone(capabilities.Routes)
			for i := range changed.Routes {
				if changed.Routes[i].OperationID == route.id {
					changed.Routes[i].QueryParameters = slices.DeleteFunc(slices.Clone(changed.Routes[i].QueryParameters), func(value string) bool { return value == query })
				}
			}
			assertions.NotContains(newDaemonMCPOperations(backend.client, &changed).capabilities(), route.name)
		}
	}
	delegated := *capabilities
	delegated.Delegated = true
	delegatedBackend := newDaemonMCPOperations(backend.client, &delegated)
	for _, route := range mcpRecordRoutes {
		assertions.NotContains(delegatedBackend.capabilities(), route.name)
	}
}

func TestMCPRecordAtomicRollbackBoundedActionsAndSupersededHistory(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, backend, session, approvals := recordSDKFixture(t)
	participant, err := st.EnsureParticipant("other-atomic@example.test", "Synthetic Other Atomic Person", "example.test")
	requirements.NoError(err)
	other, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	foreign, err := st.AddPersonNameContext(t.Context(), other.ID, store.PersonNameInput{NameKind: store.PersonNameNickname, Formatted: new("Foreign synthetic name"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	current := callRecordTool[mcpserver.PersonRecord](t, session, "get_person_record", map[string]any{"person_id": person.ID})
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_person_record", Arguments: map[string]any{"person_id": person.ID, "etag": current.ETag, "patch": map[string]any{"names": map[string]any{"add": []any{map[string]any{"name_kind": "nickname", "formatted": "Must roll back", "envelope": map[string]any{"source": "user"}}}, "supersede": []any{foreign.Envelope.ID}}}}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Contains(settingsMCPDiagnostic(called), "profile_value_not_found")
	unchanged, err := st.GetPersonProfileContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(person.Revision, unchanged.Person.Revision)
	assertions.Empty(unchanged.Names)
	foreignAfter, err := st.GetPersonProfileContext(t.Context(), other.ID)
	requirements.NoError(err)
	requirements.Len(foreignAfter.Names, 1)
	actions := make([]any, 201)
	for i := range actions {
		actions[i] = map[string]any{"name_kind": "nickname", "formatted": "Synthetic bounded name", "envelope": map[string]any{"source": "user"}}
	}
	count := *approvals
	result, err := backend.ExecuteOperation(t.Context(), "update_person_record", map[string]any{"person_id": person.ID, "etag": current.ETag, "patch": map[string]any{"names": map[string]any{"add": actions}}})
	requirements.NoError(err)
	assertions.True(result.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "update_person_record", Arguments: map[string]any{"person_id": person.ID, "etag": current.ETag, "patch": map[string]any{"names": map[string]any{"add": actions}}}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(count, *approvals, "too many actions refuse before approval")
	first := callRecordTool[mcpserver.PersonRecord](t, session, "update_person_record", map[string]any{"person_id": person.ID, "etag": current.ETag, "patch": map[string]any{"names": map[string]any{"add": actions[:1]}}})
	requirements.NotNil(first.Names)
	requirements.Len(*first.Names, 1)
	second := callRecordTool[mcpserver.PersonRecord](t, session, "update_person_record", map[string]any{"person_id": person.ID, "etag": first.ETag, "patch": map[string]any{"names": map[string]any{"supersede": []any{(*first.Names)[0].Envelope.ID}, "add": []any{map[string]any{"name_kind": "nickname", "formatted": "Replacement synthetic name", "envelope": map[string]any{"source": "user", "ordinal": 0}}}}}})
	requirements.NotNil(second.Names)
	requirements.Len(*second.Names, 1)
	assertions.Equal(person.Revision+2, second.Person.Revision)
	history := callRecordTool[mcpserver.PersonRecord](t, session, "list_person_record_history", map[string]any{"person_id": person.ID, "fields": []any{"names"}})
	requirements.NotNil(history.Names)
	requirements.Len(*history.Names, 2)
	superseded := 0
	for _, name := range *history.Names {
		if name.Envelope.SupersededAt != nil {
			superseded++
		}
	}
	assertions.Equal(1, superseded)
}

func TestMCPRecordTypedValuesAndNativeTextCap(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	_, person, backend, session, _ := recordSDKFixture(t)
	for _, test := range []struct {
		slug, kind, field string
		value             any
	}{{"synthetic_count", "integer", "duration", int64(0)}, {"synthetic_real", "real", "text", 1e20}, {"synthetic_json", "json", "json", map[string]any{"nested": []any{false, 0, "synthetic JSON"}}}} {
		created := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": test.slug, "label": "Synthetic typed value", "value_type": test.kind, "field_type": test.field}})
		written := callRecordTool[generated.PersonAttributeWrite](t, session, "set_person_attribute", map[string]any{"person_id": person.ID, "slug": created.Definition.Slug, "value": map[string]any{"value": map[string]any{"type": test.kind, test.kind: test.value}, "source": "user"}})
		requirements.NotNil(written.Value)
		assertions.Equal(test.kind, written.Value.Value.Type)
		switch test.kind {
		case "integer":
			requirements.NotNil(written.Value.Value.Integer)
			assertions.Equal(int64(0), *written.Value.Value.Integer)
		case "real":
			requirements.NotNil(written.Value.Value.Real)
			assertions.Equal(math.Float64bits(1e20), math.Float64bits(*written.Value.Value.Real), "the native float value round-trips exactly")
		case "json":
			assertions.JSONEq(`{"nested":[false,0,"synthetic JSON"]}`, string(written.Value.Value.JSON))
		}
	}
	written := callRecordTool[generated.PersonAttributeWrite](t, session, "set_person_attribute", map[string]any{"person_id": person.ID, "slug": "how_we_met", "value": map[string]any{"value": map[string]any{"type": "text", "text": strings.Repeat("x", 280)}, "source": "user"}})
	requirements.NotNil(written.Value)
	requirements.NotNil(written.Value.Value.Text)
	assertions.Len(*written.Value.Value.Text, 280)
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person.ID, "slug": "how_we_met", "value": map[string]any{"value": map[string]any{"type": "text", "text": strings.Repeat("x", 281)}, "source": "user"}}})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Contains(settingsMCPDiagnostic(called), "attribute_value_invalid")
	for _, args := range []map[string]any{{"person_id": person.ID, "slug": "synthetic_count", "value": map[string]any{"value": map[string]any{"type": "integer", "integer": float64(9007199254740992)}}}, {"person_id": person.ID, "slug": "synthetic_count", "value": map[string]any{"value": map[string]any{"type": "integer", "integer": 0}, "source": nil}}, {"person_id": person.ID, "slug": "synthetic_count", "value": map[string]any{"value": map[string]any{"type": "integer", "integer": 0}, "env": "invented"}}} {
		result, err := backend.ExecuteOperation(t.Context(), "set_person_attribute", args)
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
}

func TestMCPRecordOpaqueJSONRejectsUnsafeIntegerBeforeApproval(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, person, _, session, approvals := recordSDKFixture(t)
	created := callRecordTool[mcpserver.AttributeDefinitionRecord](t, session, "create_attribute_definition", map[string]any{"definition": map[string]any{"object_type": "person", "slug": "synthetic_json", "label": "Synthetic JSON", "value_type": "json", "field_type": "json"}})
	count := *approvals
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: map[string]any{"person_id": person.ID, "slug": created.Definition.Slug, "value": map[string]any{"value": map[string]any{"type": "json", "json": map[string]any{"nested": []any{nil, map[string]any{"opaque_integer": int64(9007199254740993)}}}}, "source": "user"}}})
	requirements.NoError(err)
	assertions.True(called.IsError, "an SDK-unsafe JSON integer must never be persisted after rounding")
	assertions.Equal(count, *approvals, "refuse before approval")
	values, err := st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: created.Definition.Slug})
	requirements.NoError(err)
	assertions.Empty(values)
	safe := map[string]any{"ordinal": 0.5, "source": nil, "nested": []any{nil, true, 0.125, int64(9007199254740991)}}
	written := callRecordTool[generated.PersonAttributeWrite](t, session, "set_person_attribute", map[string]any{"person_id": person.ID, "slug": created.Definition.Slug, "value": map[string]any{"value": map[string]any{"type": "json", "json": safe}, "source": "user"}})
	requirements.NotNil(written.Value)
	assertions.JSONEq(`{"ordinal":0.5,"source":null,"nested":[null,true,0.125,9007199254740991]}`, string(written.Value.Value.JSON))
	values, err = st.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: created.Definition.Slug})
	requirements.NoError(err)
	requirements.Len(values, 1)
	assertions.JSONEq(`{"ordinal":0.5,"source":null,"nested":[null,true,0.125,9007199254740991]}`, string(values[0].Value.JSON))
}
