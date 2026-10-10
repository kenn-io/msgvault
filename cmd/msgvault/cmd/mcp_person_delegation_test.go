package cmd

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Exercise production delegated stdio startup and the official SDK catalog.
func TestMCPDelegatedPersonProductionDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name               string
		permissions        []string
		optIn, read, write bool
	}{
		{"reader", []string{"person.read"}, true, true, false},
		{"editor", []string{"person.read", "person.edit"}, true, true, true},
		{"writes disabled", []string{"person.read", "person.edit"}, false, true, false},
		{"missing read", []string{"person.edit"}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			previous := mcpAllowProfileWrites
			mcpAllowProfileWrites = tc.optIn
			t.Cleanup(func() { mcpAllowProfileWrites = previous })
			fixture := newDraftReplyFixture(t)
			participant, err := fixture.store.EnsureParticipant("delegated-person@example.test", "Delegated Example", "example.test")
			requirements.NoError(err)
			person, _, err := fixture.store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			var previousNameID int64
			if tc.write {
				previousName, err := fixture.store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameFormatted, Formatted: new("Previous Structured Example"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				requirements.NoError(err)
				previousNameID = previousName.Envelope.ID
				person, err = fixture.store.GetPerson(person.ID)
				requirements.NoError(err)
			}
			definition, err := fixture.store.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: "synthetic-page-id-mcp", ObjectType: store.AttributeObjectPerson, Slug: "synthetic_page_id", Label: "Synthetic page ID", ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText, APIMutable: true, IsDeletable: true})
			requirements.NoError(err)
			for _, typed := range []struct {
				slug      string
				valueType store.AttributeValueType
				fieldType store.AttributeFieldType
			}{{"synthetic_integer", store.AttributeValueInteger, store.AttributeFieldDuration}, {"synthetic_json", store.AttributeValueJSON, store.AttributeFieldJSON}} {
				_, err := fixture.store.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{UniversalID: typed.slug, ObjectType: store.AttributeObjectPerson, Slug: typed.slug, Label: typed.slug, ValueType: typed.valueType, FieldType: typed.fieldType, APIMutable: true, IsDeletable: true})
				requirements.NoError(err)
			}
			// Rate limiting has its own native tests. Give these sequential contract
			// requests distinct synthetic peers so runner speed cannot affect them.
			var requestNumber atomic.Uint64
			server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.RemoteAddr = fmt.Sprintf("192.0.2.%d:12345", requestNumber.Add(1))
					next.ServeHTTP(w, r)
				})
			})
			owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = owner.Close() })
			response, err := owner.DoGeneratedRequestWithContext(t.Context(), http.MethodPost, "/api/v1/agent-tokens", &generated.IssueAgentTokenRequestOptions{Body: &generated.IssueAgentTokenBody{Label: "Synthetic person MCP", Permissions: tc.permissions, PersonIds: []int64{person.ID}}})
			requirements.NoError(err)
			defer func() { assertions.NoError(response.Body.Close()) }()
			requirements.Equal(http.StatusCreated, response.StatusCode)
			var grant generated.AgentTokenIssueResponse
			requirements.NoError(json.UnmarshalRead(response.Body, &grant))
			tokenFile := filepath.Join(t.TempDir(), "agent.token")
			requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = t.TempDir()
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
			session := mcpDraftTestSession(ctx, t)
			names := mcpDraftToolNames(t, session)
			for _, entry := range []struct {
				name string
				want bool
			}{{"get_person_edit_context", tc.read}, {"set_person_display_name", tc.write}, {"get_person_structured_profile", tc.read}, {"patch_person_profile", tc.write}, {"get_person_attributes", tc.read}, {"set_person_attribute", tc.write}, {"clear_person_attribute", tc.write}} {
				if entry.want {
					assertions.Contains(names, entry.name)
				} else {
					assertions.NotContains(names, entry.name)
				}
			}
			if tc.read {
				read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_edit_context", Arguments: map[string]any{"person_id": person.ID}})
				requirements.NoError(err)
				requirements.False(read.IsError, mcpDraftResultText(t, read))
				after, err := fixture.store.GetPerson(person.ID)
				requirements.NoError(err)
				assertions.Equal(person, after, "MCP read must preserve native person")
				outside, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_edit_context", Arguments: map[string]any{"person_id": int64(999999)}})
				requirements.NoError(err)
				assertions.True(outside.IsError)
				structuredRead, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_structured_profile", Arguments: map[string]any{"person_id": person.ID}})
				requirements.NoError(err)
				requirements.False(structuredRead.IsError, mcpDraftResultText(t, structuredRead))
				structuredOutside, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_structured_profile", Arguments: map[string]any{"person_id": int64(999999)}})
				requirements.NoError(err)
				assertions.True(structuredOutside.IsError)
				attributeRead, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_attributes", Arguments: map[string]any{"person_id": person.ID}})
				requirements.NoError(err)
				requirements.False(attributeRead.IsError, mcpDraftResultText(t, attributeRead))
				var attributeContext struct {
					ETag       string                             `json:"etag"`
					Attributes generated.PersonAttributesResponse `json:"attributes"`
				}
				attributeJSON, err := json.Marshal(attributeRead.StructuredContent)
				requirements.NoError(err)
				requirements.NoError(json.Unmarshal(attributeJSON, &attributeContext))
				assertions.NotEmpty(attributeContext.ETag)
				assertions.Equal(person.ID, attributeContext.Attributes.PersonID)
				attributeOutside, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_attributes", Arguments: map[string]any{"person_id": int64(999999)}})
				requirements.NoError(err)
				assertions.True(attributeOutside.IsError)
				if tc.write {
					attributeArgs := map[string]any{"person_id": person.ID, "etag": attributeContext.ETag, "attribute_slug": definition.Slug, "expected_value_id": int64(0), "value": map[string]any{"type": "text", "text": "Synthetic external page 42"}}
					for _, field := range []string{"person_id", "expected_value_id", "ordinal"} {
						unsafeArgs := maps.Clone(attributeArgs)
						unsafeArgs[field] = int64(9_007_199_254_740_992)
						unsafe, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: unsafeArgs})
						requirements.NoError(err)
						assertions.True(unsafe.IsError)
						assertions.False(unsafe.NeedsInput(), "unsafe JSON IDs must not reach confirmation")
					}
					outsideAttributeArgs := maps.Clone(attributeArgs)
					outsideAttributeArgs["person_id"] = int64(999999)
					outsideAttributeSet, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: outsideAttributeArgs})
					requirements.NoError(err)
					assertions.True(outsideAttributeSet.IsError)
					attributePending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: attributeArgs})
					requirements.NoError(err)
					requirements.True(attributePending.NeedsInput())
					attributeValues, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug, IncludeHistory: true})
					requirements.NoError(err)
					assertions.Empty(attributeValues, "confirmation must precede custom field writes")
					attributeApplied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: attributeArgs, RequestState: attributePending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					requirements.False(attributeApplied.IsError, mcpDraftResultText(t, attributeApplied))
					attributeValues, err = fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
					requirements.NoError(err)
					requirements.Len(attributeValues, 1)
					assertions.Equal(new("Synthetic external page 42"), attributeValues[0].Value.Text)
					assertions.Equal(store.ProvenanceUser, attributeValues[0].Source)
					assertions.Equal(new("agent:"+grant.ID), attributeValues[0].Actor)
					duplicate, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: attributeArgs})
					requirements.NoError(err)
					assertions.True(duplicate.IsError, "absent-value CAS must reject replay")
					clearArgs := map[string]any{"person_id": person.ID, "etag": attributeContext.ETag, "attribute_slug": definition.Slug, "expected_value_id": attributeValues[0].ID}
					clearPending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "clear_person_attribute", Arguments: clearArgs})
					requirements.NoError(err)
					requirements.True(clearPending.NeedsInput())
					beforeClear, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_attributes", Arguments: map[string]any{"person_id": person.ID}})
					requirements.NoError(err)
					requirements.False(beforeClear.IsError, mcpDraftResultText(t, beforeClear))
					cleared, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "clear_person_attribute", Arguments: clearArgs, RequestState: clearPending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					requirements.False(cleared.IsError, mcpDraftResultText(t, cleared))
					attributeValues, err = fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
					requirements.NoError(err)
					assertions.Empty(attributeValues)

					for _, unsafeValue := range []struct {
						slug  string
						value map[string]any
					}{{"synthetic_integer", map[string]any{"type": "integer", "integer": int64(9007199254740993)}}, {"synthetic_integer", map[string]any{"type": "integer", "integer": int64(-9007199254740993)}}, {"synthetic_json", map[string]any{"type": "json", "json": map[string]any{"nested": []any{int64(9007199254740993)}}}}} {
						unsafeArgs := map[string]any{"person_id": person.ID, "etag": attributeContext.ETag, "attribute_slug": unsafeValue.slug, "expected_value_id": int64(0), "value": unsafeValue.value}
						refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: unsafeArgs})
						requirements.NoError(err)
						assertions.True(refused.IsError, "unsafe numeric value must be refused")
						assertions.False(refused.NeedsInput(), "unsafe numeric value must not reach confirmation")
					}
					attributeCapabilities, err := owner.MCPCapabilities(t.Context())
					requirements.NoError(err)
					attributeBackend := newDaemonMCPOperations(owner, attributeCapabilities)
					var previousDisclosure string
					for _, rawJSON := range []string{`{"label":"Synthetic JSON","nested":{"alpha":true,"beta":42}}`, `{"nested":{"beta":42,"alpha":true},"label":"Synthetic JSON"}`} {
						previewArgs := map[string]any{"person_id": person.ID, "etag": attributeContext.ETag, "attribute_slug": "synthetic_json", "expected_value_id": int64(0), "value": map[string]any{"type": "json", "json": jsontext.Value(rawJSON)}}
						disclosure, err := attributeBackend.OperationDisclosure(t.Context(), "set_person_attribute", previewArgs)
						requirements.NoError(err)
						requirements.NotEmpty(disclosure)
						if previousDisclosure != "" {
							assertions.Equal(sha256.Sum256([]byte(previousDisclosure)), sha256.Sum256([]byte(disclosure)), "equivalent raw JSON order must preserve confirmation")
						}
						previousDisclosure = disclosure
					}
					previewValues, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: "synthetic_json", IncludeHistory: true})
					requirements.NoError(err)
					assertions.Empty(previewValues)
					jsonArgs := map[string]any{"person_id": person.ID, "etag": attributeContext.ETag, "attribute_slug": "synthetic_json", "expected_value_id": int64(0), "value": map[string]any{"type": "json", "json": map[string]any{"label": "Synthetic JSON", "nested": []any{true, nil, int64(-9007199254740991)}}}}
					jsonPending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: jsonArgs})
					requirements.NoError(err)
					requirements.True(jsonPending.NeedsInput(), "JSON object values must reach native confirmation")
					jsonApplied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: jsonArgs, RequestState: jsonPending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					requirements.False(jsonApplied.IsError, mcpDraftResultText(t, jsonApplied))
					jsonValues, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: "synthetic_json"})
					requirements.NoError(err)
					requirements.Len(jsonValues, 1)
					assertions.JSONEq(`{"label":"Synthetic JSON","nested":[true,null,-9007199254740991]}`, string(jsonValues[0].Value.JSON))
					jsonRead, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_attributes", Arguments: map[string]any{"person_id": person.ID}})
					requirements.NoError(err)
					requirements.False(jsonRead.IsError, mcpDraftResultText(t, jsonRead))

					for _, nativeValue := range []struct {
						slug  string
						value store.AttributeValue
					}{{"synthetic_integer", store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(9007199254740993))}}, {"synthetic_json", store.AttributeValue{Type: store.AttributeValueJSON, JSON: []byte(`{"nested":[9007199254740993]}`)}}} {
						seeded, err := fixture.store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: nativeValue.slug, Value: nativeValue.value, Source: store.ProvenanceUser})
						requirements.NoError(err)
						requirements.NotNil(seeded.Value)
						before, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: nativeValue.slug, IncludeHistory: true})
						requirements.NoError(err)
						for _, operation := range []string{"clear_person_attribute", "set_person_attribute"} {
							unsafeArgs := map[string]any{"person_id": person.ID, "etag": attributeContext.ETag, "attribute_slug": nativeValue.slug, "expected_value_id": seeded.Value.ID}
							if operation == "set_person_attribute" {
								if nativeValue.slug == "synthetic_integer" {
									unsafeArgs["value"] = map[string]any{"type": "integer", "integer": int64(42)}
								} else {
									unsafeArgs["value"] = map[string]any{"type": "json", "json": "Synthetic replacement"}
								}
							}
							refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: operation, Arguments: unsafeArgs})
							requirements.NoError(err)
							assertions.True(refused.IsError, "stored unsafe values must refuse before confirmation")
							assertions.False(refused.NeedsInput())
						}
						after, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: nativeValue.slug, IncludeHistory: true})
						requirements.NoError(err)
						assertions.Equal(before, after)
						_, err = fixture.store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: nativeValue.slug, ExpectedValueID: &seeded.Value.ID})
						requirements.NoError(err)
					}
					data, err := json.Marshal(read.StructuredContent)
					requirements.NoError(err)
					var edit mcpserver.PersonEditContext
					requirements.NoError(json.Unmarshal(data, &edit))
					args := map[string]any{"person_id": person.ID, "etag": edit.ETag, "display_name": "MCP Changed Example"}
					pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_display_name", Arguments: args})
					requirements.NoError(err)
					requirements.True(pending.NeedsInput())
					unchanged, err := fixture.store.GetPerson(person.ID)
					requirements.NoError(err)
					assertions.Equal(person, unchanged, "approval must precede mutation")
					applied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_display_name", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					requirements.False(applied.IsError, mcpDraftResultText(t, applied))
					after, err = fixture.store.GetPerson(person.ID)
					requirements.NoError(err)
					assertions.Equal(new("MCP Changed Example"), after.DisplayName)
					assertions.Equal(person.Revision+1, after.Revision)
					structured, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_structured_profile", Arguments: map[string]any{"person_id": person.ID}})
					requirements.NoError(err)
					requirements.False(structured.IsError, mcpDraftResultText(t, structured))
					var profile struct {
						ETag    string                            `json:"etag"`
						Profile generated.StructuredPersonProfile `json:"profile"`
					}
					data, err = json.Marshal(structured.StructuredContent)
					requirements.NoError(err)
					requirements.NoError(json.Unmarshal(data, &profile))
					ownerCapabilities, err := owner.MCPCapabilities(t.Context())
					requirements.NoError(err)
					backend := newDaemonMCPOperations(owner, ownerCapabilities)
					for _, section := range []string{"names", "contact_points", "addresses", "dates", "categories", "media"} {
						unsafeArgs := map[string]any{"person_id": person.ID, "etag": profile.ETag, "patch": map[string]any{section: map[string]any{"supersede": []int64{9007199254740993}}}}
						refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_profile", Arguments: unsafeArgs})
						assertions.True(err != nil || refused != nil && refused.IsError, "unsafe %s value ID must be refused", section)
						if err == nil {
							requirements.NotNil(refused)
							assertions.False(refused.NeedsInput(), "unsafe %s value ID must be refused before confirmation", section)
						}
						disclosure, err := backend.OperationDisclosure(t.Context(), "patch_person_profile", unsafeArgs)
						var refusal *mcpserver.OperationRefusalError
						requirements.ErrorAs(err, &refusal)
						assertions.Equal("invalid_arguments", refusal.Code)
						assertions.Empty(disclosure)
						direct, err := backend.ExecuteOperation(t.Context(), "patch_person_profile", unsafeArgs)
						requirements.NoError(err)
						requirements.NotNil(direct)
						assertions.True(direct.IsError)
						data, err := json.Marshal(direct.Output)
						requirements.NoError(err)
						var failure struct {
							Error string `json:"error"`
						}
						requirements.NoError(json.Unmarshal(data, &failure))
						assertions.Equal("invalid_arguments", failure.Error)
					}
					var patchBody map[string]any
					requirements.NoError(json.Unmarshal([]byte(`{
						"names":{"add":[{"name_kind":"formatted","formatted":"Structured MCP Example","envelope":{"source":"user"}}]},
						"contact_points":{"add":[{"address_kind":"email","original_value":"structured-mcp@example.test","envelope":{"source":"user"}}]},
						"addresses":{"add":[{"address_kind":"postal","free_text":"Exampleville","envelope":{"source":"user"}}]},
						"dates":{"add":[{"date_kind":"custom","date_text":"Spring 2020","envelope":{"source":"user"}}]},
						"categories":{"add":[{"original_value":"Synthetic Friends","envelope":{"source":"user"}}]},
						"media":{"add":[{"media_kind":"photo","uri":"https://media.example.test/photo.jpg","envelope":{"source":"user"}}]}
					}`), &patchBody))
					names, ok := patchBody["names"].(map[string]any)
					requirements.True(ok)
					names["supersede"] = []int64{previousNameID}
					patchArgs := map[string]any{"person_id": person.ID, "etag": profile.ETag, "patch": patchBody}
					profilePending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_profile", Arguments: patchArgs})
					requirements.NoError(err)
					requirements.True(profilePending.NeedsInput())
					profileApplied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_profile", Arguments: patchArgs, RequestState: profilePending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					requirements.False(profileApplied.IsError, mcpDraftResultText(t, profileApplied))
					nativeProfile, err := fixture.store.GetPersonProfileContext(t.Context(), person.ID)
					requirements.NoError(err)
					requirements.Len(nativeProfile.Names, 1)
					assertions.Len(nativeProfile.ContactPoints, 1)
					assertions.Len(nativeProfile.Addresses, 1)
					assertions.Len(nativeProfile.Dates, 1)
					assertions.Len(nativeProfile.Categories, 1)
					assertions.Len(nativeProfile.Media, 1)
					assertions.Equal(new("Structured MCP Example"), nativeProfile.Names[len(nativeProfile.Names)-1].Formatted)
					assertions.Equal(new("MCP Changed Example"), nativeProfile.Person.DisplayName)
					stale, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "patch_person_profile", Arguments: patchArgs})
					requirements.NoError(err)
					assertions.True(stale.IsError)
					unchangedProfile, err := fixture.store.GetPersonProfileContext(t.Context(), person.ID)
					requirements.NoError(err)
					assertions.Equal(nativeProfile, unchangedProfile)
					after = &nativeProfile.Person
					read, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_edit_context", Arguments: map[string]any{"person_id": person.ID}})
					requirements.NoError(err)
					requirements.False(read.IsError, mcpDraftResultText(t, read))
					data, err = json.Marshal(read.StructuredContent)
					requirements.NoError(err)
					requirements.NoError(json.Unmarshal(data, &edit))
					args["etag"] = edit.ETag
					args["display_name"] = "Revoked Example"
					pending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_display_name", Arguments: args})
					requirements.NoError(err)
					requirements.True(pending.NeedsInput())
					attributeArgs["etag"] = edit.ETag
					attributePending, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: attributeArgs})
					requirements.NoError(err)
					requirements.True(attributePending.NeedsInput())
					requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
					denied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_display_name", Arguments: args, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					assertions.True(denied.IsError)
					unchanged, err = fixture.store.GetPerson(person.ID)
					requirements.NoError(err)
					assertions.Equal(after, unchanged, "revoked approval must preserve native person")
					attributeDenied, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "set_person_attribute", Arguments: attributeArgs, RequestState: attributePending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
					requirements.NoError(err)
					assertions.True(attributeDenied.IsError)
					attributeValues, err = fixture.store.ListPersonAttributeValuesContext(t.Context(), person.ID, store.PersonAttributeQuery{DefinitionSlug: definition.Slug})
					requirements.NoError(err)
					assertions.Empty(attributeValues, "revoked custom-field approval must not recreate the cleared slot")
					attributeRevokedRead, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_person_attributes", Arguments: map[string]any{"person_id": person.ID}})
					requirements.NoError(err)
					assertions.True(attributeRevokedRead.IsError)
				}
			}
			for _, name := range []string{"list_source_status", "sync_source", "get_participant_identity", "get_slack_sync_policy", "promote_person", "search_metadata"} {
				assertions.NotContains(names, name)
			}
		})
	}
}

// The daemon adapter also rejects unsafe values when called without SDK validation.
func TestMCPPersonAttributeArgumentsNumericBoundary(t *testing.T) {
	for _, value := range []generated.AttributeValue{
		{Type: "integer", Integer: new(int64(9007199254740993))},
		{Type: "integer", Integer: new(int64(-9007199254740993))},
		{Type: "json", JSON: []byte(`{"nested":[9007199254740993]}`)},
	} {
		assert.False(t, validPersonAttributeArguments(personAttributeArguments{PersonID: 1, ETag: "etag", AttributeSlug: "synthetic", ExpectedValueID: new(int64(0)), Value: &value}, false))
	}
}
