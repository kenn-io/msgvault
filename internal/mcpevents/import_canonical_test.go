package mcpevents

import (
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
)

func TestImportCanonicalArgumentsKeepArchiveAndExactSourceSeparate(t *testing.T) {
	const family = "msgvault.import_message_archived"
	ref := "mcp1." + strings.Repeat("A", 43)
	for _, test := range []struct {
		name      string
		input     map[string]any
		kind      string
		id        int64
		canonical string
	}{
		{"omitted archive", nil, "archive", 0, `{"include_from_me":false,"source_id":"*"}`},
		{"explicit archive", map[string]any{"source_id": "*", "include_from_me": true}, "archive", 0, `{"include_from_me":true,"source_id":"*"}`},
		{"exact source", map[string]any{"source_id": "42", "source_ref": ref}, "source", 42, `{"include_from_me":false,"source_id":"42","source_ref":"` + ref + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := Assert.New(t)

			got, err := canonicalArguments(family, test.input)
			Require.NoError(t, err)
			assert.Equal(test.kind, got.scopeKind)
			assert.Equal(test.id, got.scopeID)
			assert.Equal(test.canonical, string(got.bytes))
		})
	}
	for _, test := range []struct {
		name  string
		input map[string]any
	}{
		{"omitted archive with ref", map[string]any{"source_ref": ref}},
		{"explicit archive with ref", map[string]any{"source_id": "*", "source_ref": ref}},
		{"exact without ref", map[string]any{"source_id": "42"}},
		{"exact malformed ref", map[string]any{"source_id": "42", "source_ref": "untrusted"}},
		{"zero", map[string]any{"source_id": "0", "source_ref": ref}},
		{"leading zero", map[string]any{"source_id": "042", "source_ref": ref}},
		{"number", map[string]any{"source_id": 42, "source_ref": ref}},
		{"nil", map[string]any{"source_id": nil}},
		{"boolean string", map[string]any{"include_from_me": "false"}},
		{"reactions", map[string]any{"include_reactions": true}},
		{"owner", map[string]any{"principal": "owner:forged"}},
	} {
		t.Run(test.name, func(t *testing.T) { _, err := canonicalArguments(family, test.input); Require.Error(t, err) })
	}
}
