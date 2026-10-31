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
	requirements := require.New(t)

	assertions := assert.New(t)

	path := filepath.Join(t.TempDir(), "settings.json")
	before := `{
		"model":"existing-model", "unknown":{"id":9007199254740993,"enabled":true},
		"hooks":{
			"SessionStart":[{"matcher":"startup","extra":"keep","hooks":[{"type":"command","command":"echo existing","timeout":9}]}],
			"Stop":[{"hooks":[{"type":"command","command":"echo stopped"}]}]
		}
	}`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	requirements.NoError(InstallBriefing(path))
	got := readBriefingSettings(t, path)
	assertions.Equal("existing-model", got["model"])
	assertions.Equal(map[string]any{"id": json.Number("9007199254740993"), "enabled": true}, got["unknown"])
	hooks := got["hooks"].(map[string]any)
	assertions.Equal([]any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo stopped"}}}}, hooks["Stop"])
	groups := hooks["SessionStart"].([]any)
	requirements.Len(groups, 2)
	assertions.Equal(map[string]any{"matcher": "startup", "extra": "keep", "hooks": []any{map[string]any{"type": "command", "command": "echo existing", "timeout": json.Number("9")}}}, groups[0])
	assertions.Equal(map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "msgvault timeline --brief"}}}, groups[1])
}

func TestInstallBriefingIsIdempotent(t *testing.T) {
	requirements := require.New(t)

	assertions := assert.New(t)

	path := filepath.Join(t.TempDir(), ".codex", "hooks.json")
	requirements.NoError(InstallBriefing(path))
	first, err := os.ReadFile(path)
	requirements.NoError(err)
	requirements.NoError(InstallBriefing(path))
	second, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(string(first), string(second))
	groups := readBriefingSettings(t, path)["hooks"].(map[string]any)["SessionStart"].([]any)
	assertions.Len(groups, 1)

	// An existing exact command retains its user-selected matcher and options.
	custom := `{"description":"keep","hooks":{"SessionStart":[{"matcher":"resume","hooks":[{"type":"command","command":"msgvault timeline --brief","timeout":17}]}]}}`
	requirements.NoError(os.WriteFile(path, []byte(custom), 0o600))
	requirements.NoError(InstallBriefing(path))
	second, err = os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(custom, string(second), "an unchanged install must not rewrite settings")
}

func TestUninstallBriefingRemovesOnlyItsEntry(t *testing.T) {
	requirements := require.New(t)

	assertions := assert.New(t)

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
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	requirements.NoError(UninstallBriefing(path))
	data, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.JSONEq(`{"unknown":{"id":9007199254740993},"hooks":{
		"SessionStart":[
			{"matcher":"startup","extra":"keep","hooks":[{"type":"command","command":"echo existing","extra":[1,2]}]},
			{"unknown":{"keep":true},"hooks":[]},
			{"hooks":[{"type":"command","command":"msgvault timeline --brief --json"},{"type":"command","command":"msgvault timeline --brief "}]}
		],
		"Stop":[{"hooks":[{"type":"command","command":"msgvault timeline --brief"}]}]
	}}`, string(data))
	assertions.Equal(json.Number("9007199254740993"), readBriefingSettings(t, path)["unknown"].(map[string]any)["id"])
	requirements.NoError(UninstallBriefing(path))
	again, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(string(data), string(again))
}

func TestUninstallBriefingMissingOrUnrelatedIsNoOp(t *testing.T) {
	requirements := require.New(t)

	assertions := assert.New(t)

	path := filepath.Join(t.TempDir(), "absent", "settings.json")
	requirements.NoError(UninstallBriefing(path))
	assertions.NoDirExists(filepath.Dir(path))
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	before := `{"hooks":{"Stop":[]},"unknown":true}`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	requirements.NoError(UninstallBriefing(path))
	data, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(before, string(data))
}

func TestBriefingRejectsMalformedSettingsWithoutWriting(t *testing.T) {
	for _, before := range []string{
		`{`, `null`, `[]`, `{} {}`, `{"hooks":null}`, `{"hooks":[]}`,
		`{"hooks":{"SessionStart":null}}`, `{"hooks":{"SessionStart":{}}}`,
		`{"hooks":{"SessionStart":[null]}}`, `{"hooks":{"SessionStart":[{"hooks":"bad"}]}}`,
		`{"hooks":{"SessionStart":[{"hooks":[null]}]}}`,
	} {
		t.Run(before, func(t *testing.T) {
			requirements := require.New(t)

			assertions := assert.New(t)

			path := filepath.Join(t.TempDir(), "settings.json")
			requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
			assertions.Error(InstallBriefing(path))
			assertions.Error(UninstallBriefing(path))
			data, err := os.ReadFile(path)
			requirements.NoError(err)
			assertions.Equal(before, string(data))
		})
	}
}

func TestInstallBriefingReplacesFileAtomically(t *testing.T) {
	requirements := require.New(t)

	assertions := assert.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("open-file replacement semantics differ on Windows; SecureReplaceFile has platform coverage")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	before := `{"unknown":"keep"}`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	old, err := os.Open(path)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, old.Close()) })
	requirements.NoError(InstallBriefing(path))
	oldData, err := io.ReadAll(old)
	requirements.NoError(err)
	assertions.Equal(before, string(oldData), "the old file stays intact while the path points at the replacement")
	assertions.Contains(readBriefingSettings(t, path), "hooks")
	entries, err := os.ReadDir(filepath.Dir(path))
	requirements.NoError(err)
	assertions.Len(entries, 1, "no temporary file is left behind")
}
