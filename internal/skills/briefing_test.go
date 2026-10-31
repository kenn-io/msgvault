package skills

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readBriefingSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var settings map[string]any
	require.NoError(t, decoder.Decode(&settings))
	return settings
}

func TestInstallBriefingMergesSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	before := `{
		"model":"existing-model", "unknown":{"id":9007199254740993,"enabled":true},
		"hooks":{
			"SessionStart":[{"matcher":"startup","extra":"keep","hooks":[{"type":"command","command":"echo existing","timeout":9}]}],
			"Stop":[{"hooks":[{"type":"command","command":"echo stopped"}]}]
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	require.NoError(t, InstallBriefing(path))
	got := readBriefingSettings(t, path)
	assert.Equal(t, "existing-model", got["model"])
	assert.Equal(t, map[string]any{"id": json.Number("9007199254740993"), "enabled": true}, got["unknown"])
	hooks := got["hooks"].(map[string]any)
	assert.Equal(t, []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo stopped"}}}}, hooks["Stop"])
	groups := hooks["SessionStart"].([]any)
	require.Len(t, groups, 2)
	assert.Equal(t, map[string]any{"matcher": "startup", "extra": "keep", "hooks": []any{map[string]any{"type": "command", "command": "echo existing", "timeout": json.Number("9")}}}, groups[0])
	assert.Equal(t, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "msgvault timeline --brief"}}}, groups[1])
}

func TestInstallBriefingIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "hooks.json")
	require.NoError(t, InstallBriefing(path))
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, InstallBriefing(path))
	second, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
	groups := readBriefingSettings(t, path)["hooks"].(map[string]any)["SessionStart"].([]any)
	assert.Len(t, groups, 1)

	// An existing exact command retains its user-selected matcher and options.
	custom := `{"description":"keep","hooks":{"SessionStart":[{"matcher":"resume","hooks":[{"type":"command","command":"msgvault timeline --brief","timeout":17}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(custom), 0o600))
	require.NoError(t, InstallBriefing(path))
	second, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, custom, string(second), "an unchanged install must not rewrite settings")
}

func TestUninstallBriefingRemovesOnlyItsEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	before := `{"unknown":{"id":9007199254740993},"hooks":{
		"SessionStart":[
			{"matcher":"startup","extra":"keep","hooks":[{"type":"command","command":"msgvault timeline --brief"},{"type":"command","command":"echo existing","extra":[1,2]}]},
			{"hooks":[{"type":"command","command":"msgvault timeline --brief"}]},
			{"unknown":{"keep":true},"hooks":[{"type":"command","command":"msgvault timeline --brief"}]},
			{"hooks":[{"type":"command","command":"msgvault timeline --brief --json"},{"type":"command","command":"msgvault timeline --brief "}]}
		],
		"Stop":[{"hooks":[{"type":"command","command":"msgvault timeline --brief"}]}]
	}}`
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	require.NoError(t, UninstallBriefing(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"unknown":{"id":9007199254740993},"hooks":{
		"SessionStart":[
			{"matcher":"startup","extra":"keep","hooks":[{"type":"command","command":"echo existing","extra":[1,2]}]},
			{"unknown":{"keep":true},"hooks":[]},
			{"hooks":[{"type":"command","command":"msgvault timeline --brief --json"},{"type":"command","command":"msgvault timeline --brief "}]}
		],
		"Stop":[{"hooks":[{"type":"command","command":"msgvault timeline --brief"}]}]
	}}`, string(data))
	assert.Equal(t, json.Number("9007199254740993"), readBriefingSettings(t, path)["unknown"].(map[string]any)["id"])
	require.NoError(t, UninstallBriefing(path))
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(data), string(again))
}

func TestUninstallBriefingMissingOrUnrelatedIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "settings.json")
	require.NoError(t, UninstallBriefing(path))
	assert.NoDirExists(t, filepath.Dir(path))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	before := `{"hooks":{"Stop":[]},"unknown":true}`
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	require.NoError(t, UninstallBriefing(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, string(data))
}

func TestBriefingRejectsMalformedSettingsWithoutWriting(t *testing.T) {
	for _, before := range []string{
		`{`, `null`, `[]`, `{} {}`, `{"hooks":null}`, `{"hooks":[]}`,
		`{"hooks":{"SessionStart":null}}`, `{"hooks":{"SessionStart":{}}}`,
		`{"hooks":{"SessionStart":[null]}}`, `{"hooks":{"SessionStart":[{"hooks":"bad"}]}}`,
		`{"hooks":{"SessionStart":[{"hooks":[null]}]}}`,
	} {
		t.Run(before, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
			assert.Error(t, InstallBriefing(path))
			assert.Error(t, UninstallBriefing(path))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, string(data))
		})
	}
}

func TestInstallBriefingReplacesFileAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("open-file replacement semantics differ on Windows; SecureReplaceFile has platform coverage")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	before := `{"unknown":"keep"}`
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	old, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, old.Close()) })
	require.NoError(t, InstallBriefing(path))
	oldData, err := io.ReadAll(old)
	require.NoError(t, err)
	assert.Equal(t, before, string(oldData), "the old file stays intact while the path points at the replacement")
	assert.Contains(t, readBriefingSettings(t, path), "hooks")
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}
