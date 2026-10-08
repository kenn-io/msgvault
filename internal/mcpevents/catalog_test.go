package mcpevents

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCatalogReflectsOnlyEnabledReadyFamilies(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	s, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{"gmail", "gcal", "caldav", "google_calendar"}, KeyPath: t.TempDir() + "/key", OwnerKey: "synthetic-owner"})
	require.NoError(err)
	definitions := s.Catalog().Events
	require.Len(definitions, 3)
	require.Equal(messageFamily, definitions[0].Name)
	messageInputs, ok := definitions[0].InputSchema["properties"].(map[string]any)
	require.True(ok)
	assert.NotContains(messageInputs, "include_reactions")
	for _, definition := range definitions {
		assert.Equal([]string{"webhook"}, definition.Delivery)
		assert.Equal(false, definition.InputSchema["additionalProperties"])
		assert.Equal("object", definition.PayloadSchema["type"])
		assert.Contains(definition.Description, "get_mcp_event")
		assert.Contains(definition.Description, "get_message")
	}
	assert.Len(s.Capabilities(), 3)
	definitions[0].InputSchema["type"] = "corrupted"
	assert.Equal("object", s.Catalog().Events[0].InputSchema["type"], "catalog caller cannot change service schema")
	assert.Empty(capabilities(false, []string{"gmail"}))
}

func TestCanonicalArgumentsNormalizeDefaultsAndSets(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	a, err := canonicalArguments(messageFamily, map[string]any{"conversation_id": "7"})
	require.NoError(err)
	assert.JSONEq(`{"conversation_id":"7","include_from_me":false}`, string(a.bytes))
	b, err := canonicalArguments(messageFamily, map[string]any{"conversation_id": "7", "include_from_me": false})
	require.NoError(err)
	assert.Equal(a.bytes, b.bytes)
	_, err = canonicalArguments(messageFamily, map[string]any{"conversation_id": "7", "include_reactions": true})
	require.Error(err)
	a, err = canonicalArguments(draftFamily, map[string]any{"conversation_id": "7", "kinds": []any{"updated", "created", "updated"}})
	require.NoError(err)
	assert.Equal(`{"conversation_id":"7","kinds":["created","updated"]}`, string(a.bytes)) //nolint:testifylint // Canonical argument ordering defines the subscription identity.
	b, err = canonicalArguments(draftFamily, map[string]any{"conversation_id": "7"})
	require.NoError(err)
	assert.Equal([]string{"*"}, b.kinds)
	for _, bad := range []any{nil, []any{}, []any{"*"}, []any{"sent"}, []any{true}, "created"} {
		_, err := canonicalArguments(draftFamily, map[string]any{"conversation_id": "7", "kinds": bad})
		require.Error(err)
	}
}

func FuzzCanonicalArgumentsSetPermutation(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2})
	f.Fuzz(func(t *testing.T, order []byte) {
		if len(order) == 0 {
			return
		}
		kinds := make([]any, 0, len(order))
		seen := map[string]bool{}
		for _, i := range order {
			kind := []string{"created", "updated", "deleted"}[int(i)%3]
			kinds = append(kinds, kind)
			seen[kind] = true
		}
		a, err := canonicalArguments(draftFamily, map[string]any{"conversation_id": "9223372036854775807", "kinds": kinds})
		Require.NoError(t, err)
		var got struct {
			Kinds []string `json:"kinds"`
		}
		Require.NoError(t, json.Unmarshal(a.bytes, &got))
		Assert.Len(t, got.Kinds, len(seen))
		for _, kind := range got.Kinds {
			Assert.True(t, seen[kind])
		}
		for i := 1; i < len(got.Kinds); i++ {
			Assert.Less(t, got.Kinds[i-1], got.Kinds[i])
		}
	})
}

func TestManagedDraftCatalogOwnerReadGate(t *testing.T) {
	for _, tc := range []struct{ source, draftKind string }{
		{"beeper", "beeper"}, {"slack", "chat"}, {"slackdump", "chat"}, {"teams", "chat"}, {"discord", "chat"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			s, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{tc.source}, KeyPath: t.TempDir() + "/key", OwnerKey: "synthetic-owner"})
			require.NoError(err)
			want := []store.MCPEventCapability{{Family: draftFamily, SourceType: tc.source, Kinds: []string{"created", "updated", "deleted"}}}
			if tc.source != "slackdump" {
				want = append([]store.MCPEventCapability{{Family: messageFamily, SourceType: tc.source, Kinds: []string{"message"}}}, want...)
			}
			require.Equal(want, s.Capabilities(), "only implemented kinds are ready on this explicitly enabled source")
			caps := s.Capabilities()
			caps[0].Kinds[0] = "corrupted"
			assert.Equal(want, s.Capabilities(), "capability callers cannot change enabled kinds")
			catalog := s.Catalog().Events
			require.Len(catalog, len(want))
			var draft Definition
			for _, definition := range catalog {
				if definition.Name == draftFamily {
					draft = definition
				}
			}
			require.Equal(draftFamily, draft.Name)
			assert.Contains(draft.Description, "draft_get")
			assert.NotContains(draft.Description, "get_message", "non-email drafts have no archived message read recipe")
			assert.Contains(draft.Description, "get_mcp_event")
			encoded, err := json.Marshal(draft.PayloadSchema)
			require.NoError(err)
			var schema jsonschema.Schema
			require.NoError(json.Unmarshal(encoded, &schema))
			resolved, err := schema.Resolve(nil)
			require.NoError(err)
			payload := map[string]any{"kind": "created", "draft_id": "synthetic-draft", "revision": float64(1), "draft_kind": tc.draftKind, "conversation_id": strconv.FormatInt(f.ConvID, 10), "source_id": strconv.FormatInt(f.Source.ID, 10), "changed_at": "2026-10-06T00:00:00Z", "created_by": "owner"}
			require.NoError(resolved.Validate(payload), "non-email draft occurrence has no archived message row")
			payload["message_id"] = "1"
			require.Error(resolved.Validate(payload), "non-email draft must omit message_id")
			payload["draft_kind"] = "gmail"
			require.Error(resolved.Validate(payload), "schema advertises only enabled draft kinds")
		})
	}
}

func TestCatalogDefaultCapabilitiesAndSourceIsolation(t *testing.T) {
	want := []store.MCPEventCapability{
		{Family: messageFamily, SourceType: "gmail", Kinds: []string{"message"}},
		{Family: draftFamily, SourceType: "gmail", Kinds: []string{"created", "updated", "deleted"}},
		{Family: messageFamily, SourceType: "imap", Kinds: []string{"message"}},
		{Family: draftFamily, SourceType: "imap", Kinds: []string{"created", "updated", "deleted"}},
		{Family: calendarFamily, SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}},
	}
	Assert.Equal(t, want, capabilities(true, []string{"gmail", "imap", "gcal"}))
	for _, source := range []string{"beeper", "slack", "slackdump", "teams", "discord"} {
		Assert.Empty(t, capabilities(false, []string{source}))
	}
	Assert.Empty(t, capabilities(true, []string{"Beeper", "slack_dump", "local_chat", "caldav", "google_calendar"}))
}

func TestManagedDraftMixedPayloadSchema(t *testing.T) {
	require := Require.New(t)
	s := &Service{caps: capabilities(true, []string{"gmail", "imap", "beeper", "slack"})}
	var definition Definition
	for _, item := range s.Catalog().Events {
		if item.Name == draftFamily {
			definition = item
		}
	}
	require.NotEmpty(definition.Name)
	Assert.Contains(t, definition.Description, "draft_get")
	encoded, err := json.Marshal(definition.PayloadSchema)
	require.NoError(err)
	var schema jsonschema.Schema
	require.NoError(json.Unmarshal(encoded, &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(err)
	for _, draftKind := range []string{"gmail", "imap", "beeper", "chat"} {
		t.Run(draftKind, func(t *testing.T) {
			require := Require.New(t)
			payload := map[string]any{"kind": "deleted", "draft_id": "synthetic-deleted-draft", "revision": float64(2), "draft_kind": draftKind, "conversation_id": "1", "source_id": "1", "changed_at": "2026-10-06T00:00:00Z", "created_by": "owner"}
			if draftKind == "gmail" || draftKind == "imap" {
				require.Error(resolved.Validate(payload), "email draft requires its archived message row")
				payload["message_id"] = "1"
				require.NoError(resolved.Validate(payload))
			} else {
				require.NoError(resolved.Validate(payload))
				payload["message_id"] = "1"
				require.Error(resolved.Validate(payload))
			}
		})
	}
}

func TestCatalogMatrixMessageOnly(t *testing.T) {
	Assert.Equal(t, []store.MCPEventCapability{{Family: messageFamily, SourceType: "matrix", Kinds: []string{"message"}}}, capabilities(true, []string{"matrix"}))
	Assert.Empty(t, capabilities(false, []string{"matrix"}))
}

func TestCatalogMSMailMessageOnly(t *testing.T) {
	Assert.Equal(t, []store.MCPEventCapability{{Family: messageFamily, SourceType: "msmail", Kinds: []string{"message"}}}, capabilities(true, []string{"msmail"}))
	Assert.Empty(t, capabilities(false, []string{"msmail"}))
}

func TestCatalogCombinedNativeProviders(t *testing.T) {
	got := make(map[string][]string)
	for _, capability := range capabilities(true, []string{"beeper", "slack", "teams", "discord", "msmail", "matrix", "slackdump"}) {
		got[capability.SourceType] = append(got[capability.SourceType], capability.Family)
	}
	Require.Equal(t, map[string][]string{
		"beeper":    {messageFamily, draftFamily},
		"slack":     {messageFamily, draftFamily},
		"teams":     {messageFamily, draftFamily},
		"discord":   {messageFamily, draftFamily},
		"msmail":    {messageFamily},
		"matrix":    {messageFamily},
		"slackdump": {draftFamily},
	}, got)
}

func TestMessageCatalogAttachmentReadScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sources     []string
		attachments bool
		mixedMatrix bool
	}{
		{name: "matrix", sources: []string{"matrix"}},
		{name: "mail", sources: []string{"gmail"}, attachments: true},
		{name: "mixed", sources: []string{"gmail", "matrix"}, attachments: true, mixedMatrix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			s := Service{caps: capabilities(true, tc.sources)}
			var message Definition
			for _, definition := range s.Catalog().Events {
				if definition.Name == messageFamily {
					message = definition
				}
			}
			Require.Equal(t, messageFamily, message.Name)
			assert.Contains(message.Description, "get_message")
			assert.Contains(message.Description, "list_thread")
			assert.Contains(message.Description, "get_mcp_event")
			if tc.attachments {
				assert.Contains(message.Description, "get_attachment")
			} else {
				assert.NotContains(message.Description, "get_attachment")
			}
			if tc.mixedMatrix {
				assert.Contains(message.Description, "not available for Matrix")
			}
		})
	}
}
