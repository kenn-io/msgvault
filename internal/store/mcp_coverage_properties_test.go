package store

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func FuzzMCPScopeCoverageProjectionNormalization(f *testing.F) {
	f.Add(uint8(0), true, false, []byte{0})
	f.Add(uint8(1), false, true, []byte{1, 2, 3, 4, 5})
	f.Add(uint8(255), true, true, []byte{255, 17, 33})
	f.Fuzz(func(t *testing.T, source uint8, enabled, reactions bool, choices []byte) {
		sources := []string{"gmail", "imap", "beeper", "slack", "teams", "discord", "msmail", "matrix"}
		firstSource, secondSource := sources[int(source)%len(sources)], sources[(int(source)+1)%len(sources)]
		kinds := []string{"message"}
		if reactions {
			kinds = append(kinds, "reaction")
		}
		base := MCPEventsConfig{Enabled: enabled, Principal: "synthetic-owner", Capabilities: []MCPEventCapability{
			{Family: "msgvault.message_archived", SourceType: firstSource, Kinds: slices.Clone(kinds)},
			{Family: "msgvault.message_archived", SourceType: secondSource, Kinds: []string{"message"}},
			{Family: "msgvault.draft_changed", SourceType: firstSource, Kinds: []string{"created", "deleted"}},
		}}
		_, legacyFingerprint := mcpNewRuntime(base)
		full := base
		full.Capabilities = append(slices.Clone(base.Capabilities),
			MCPEventCapability{Family: "msgvault.source_message_archived", SourceType: firstSource, Kinds: slices.Clone(kinds)},
			MCPEventCapability{Family: "msgvault.inbox_message_archived", SourceType: secondSource, Kinds: []string{"message"}})
		_, fullFingerprint := mcpNewRuntime(full)
		// Reorder whole capabilities, duplicate entries and reverse/duplicate kind
		// lists. These transformations preserve sets but change wire ordering.
		for i, choice := range choices[:min(len(choices), 64)] {
			n := len(full.Capabilities)
			a, b := i%n, int(choice)%n
			full.Capabilities[a], full.Capabilities[b] = full.Capabilities[b], full.Capabilities[a]
			capability := full.Capabilities[b]
			capability.Kinds = slices.Clone(capability.Kinds)
			slices.Reverse(capability.Kinds)
			capability.Kinds = append(capability.Kinds, capability.Kinds[0])
			full.Capabilities = append(full.Capabilities, capability)
		}
		runtime, gotFull := mcpNewRuntime(full)
		gotLegacy, projected := mcpLegacyScopeCoverageFingerprint(runtime)
		assert.True(t, projected)
		assert.Equal(t, legacyFingerprint, gotLegacy)
		assert.Equal(t, fullFingerprint, gotFull)
	})
}

func TestMCPScopeCoverageProjectionRejectsUnbackedCapabilities(t *testing.T) {
	base := MCPEventsConfig{Enabled: true, Capabilities: []MCPEventCapability{
		{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message", "reaction"}},
		{Family: "msgvault.message_archived", SourceType: "imap", Kinds: []string{"message"}},
	}}
	_, legacy := mcpNewRuntime(base)
	for _, extra := range []MCPEventCapability{
		{Family: "msgvault.inbox_message_archived", SourceType: "imap", Kinds: []string{"reaction"}},
		{Family: "msgvault.source_message_archived", SourceType: "missing", Kinds: []string{"message"}},
		{Family: "example.unknown", SourceType: "gmail", Kinds: []string{"message"}},
	} {
		t.Run(extra.Family+"/"+extra.SourceType, func(t *testing.T) {
			changed := base
			changed.Capabilities = append(slices.Clone(base.Capabilities), extra)
			runtime, _ := mcpNewRuntime(changed)
			fingerprint, projected := mcpLegacyScopeCoverageFingerprint(runtime)
			assert.False(t, projected)
			assert.NotEqual(t, legacy, fingerprint)
		})
	}
}
