package mcpevents

import (
	"encoding/json"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"strings"
	"testing"
)

func TestOptionalResourceReferencesPreserveLegacyCanonicalIdentity(t *testing.T) {
	for _, tc := range []struct{ family, idKey, refKey, legacy string }{
		{messageFamily, "conversation_id", "conversation_ref", `{"conversation_id":"7","include_from_me":false}`},
		{draftFamily, "conversation_id", "conversation_ref", `{"conversation_id":"7","kinds":["*"]}`},
		{calendarFamily, "calendar_source_id", "source_ref", `{"calendar_source_id":"7"}`},
	} {
		t.Run(tc.family, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			input := map[string]any{tc.idKey: "7"}
			legacy, err := canonicalArguments(tc.family, input)
			require.NoError(err)
			assert.Equal(tc.legacy, string(legacy.bytes))
			reference := "mcp1." + strings.Repeat("A", 43)
			input[tc.refKey] = reference
			bound, err := canonicalArguments(tc.family, input)
			require.NoError(err)
			var object map[string]any
			require.NoError(json.Unmarshal(bound.bytes, &object))
			assert.Equal(reference, object[tc.refKey])
			delete(object, tc.refKey)
			raw, err := json.Marshal(object)
			require.NoError(err)
			assert.JSONEq(tc.legacy, string(raw))
			assert.NotEqual(subscriptionID("synthetic-owner", tc.family, legacy.bytes, "https://receiver.example.net/events"), subscriptionID("synthetic-owner", tc.family, bound.bytes, "https://receiver.example.net/events"))
			for _, bad := range []any{nil, 7, "", reference + "=", "mcp1." + strings.Repeat("A", 42) + "B", "mcp2." + strings.Repeat("A", 43)} {
				input[tc.refKey] = bad
				_, err := canonicalArguments(tc.family, input)
				require.Error(err)
			}
		})
	}
}
