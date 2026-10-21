package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sequenceDirectoryOpener struct {
	handles []syncDirectoryHandle
	index   int
}

func (opener *sequenceDirectoryOpener) open(string) (syncDirectoryHandle, error) {
	if opener.index >= len(opener.handles) {
		return &failingDirectoryHandle{}, nil
	}
	handle := opener.handles[opener.index]
	opener.index++
	return handle, nil
}

type failingDirectoryHandle struct {
	syncErr  error
	closeErr error
}

func overridePinnedReplacementSync(ops *configFileOps, open func(string) (syncDirectoryHandle, error)) {
	nativeReplace := ops.replace
	ops.replace = func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, err := nativeReplace(candidatePath, targetPath, candidateBefore, targetBefore)
		if err == nil {
			replacement.syncDirectory = func() error {
				return syncConfigDirectory(filepath.Dir(targetPath), open)
			}
		}
		return replacement, err
	}
}

func overridePinnedPublicationSync(ops *configFileOps, open func(string) (syncDirectoryHandle, error)) {
	nativePublish := ops.publishNew
	ops.publishNew = func(candidatePath string, retained *os.File, before ConfigFile) (configPublication, error) {
		publication, err := nativePublish(candidatePath, retained, before)
		if err == nil {
			publication.syncDirectory = func() error {
				return syncConfigDirectory(filepath.Dir(before.Path), open)
			}
		}
		return publication, err
	}
}

func (handle *failingDirectoryHandle) Sync() error  { return handle.syncErr }
func (handle *failingDirectoryHandle) Close() error { return handle.closeErr }

func TestEditConfigPreservesUntargetedTOML(t *testing.T) {
	tests := []struct {
		name   string
		before string
		edits  []Edit
		want   string
	}{
		{
			name: "comments ordering quoted strings arrays and unknown sections",
			before: "# operator note\n[unknown]\nanswer = 42\n\n[server]\n" +
				"trusted_proxies = [\"127.0.0.1\", \"192.0.2.0/24\"] # keep placement\n" +
				"bind_addr = '127.0.0.1'\napi_port = 8080 # chosen port\n",
			edits: []Edit{
				{Key: "server.api_port", Value: int64(9090)},
				{Key: "server.trusted_proxies", Value: []string{"127.0.0.1", "2001:db8::1"}},
			},
			want: "# operator note\n[unknown]\nanswer = 42\n\n[server]\n" +
				"trusted_proxies = [\"127.0.0.1\", \"2001:db8::1\"] # keep placement\n" +
				"bind_addr = '127.0.0.1'\napi_port = 9090 # chosen port\n",
		},
		{
			name: "multiline array",
			before: "[server]\ntrusted_proxies = [\n" +
				"  \"127.0.0.1\", # loopback\n  \"192.0.2.0/24\",\n] # access list\nallow_insecure = false\n",
			edits: []Edit{{Key: "server.trusted_proxies", Value: []string{"127.0.0.1", "2001:db8::1"}}},
			want: "[server]\ntrusted_proxies = [\"127.0.0.1\", \"2001:db8::1\"] # access list\n" +
				"allow_insecure = false\n",
		},
		{
			name:   "CRLF",
			before: "# windows\r\n[web]\r\ntheme = \"system\"\r\n[unknown]\r\nkeep = true\r\n",
			edits:  []Edit{{Key: "web.theme", Value: "dark"}},
			want:   "# windows\r\n[web]\r\ntheme = \"dark\"\r\n[unknown]\r\nkeep = true\r\n",
		},
		{
			name:   "no final newline",
			before: "[analytics]\nengine = \"auto\"",
			edits:  []Edit{{Key: "analytics.engine", Value: "duckdb"}},
			want:   "[analytics]\nengine = \"duckdb\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tt.before), 0o640))
			before, err := ReadConfigFile(path)
			requirements.NoError(err)

			after, err := EditConfigFile(path, before.ETag, tt.edits)
			requirements.NoError(err)
			got, err := os.ReadFile(path)
			requirements.NoError(err)
			assertions.Equal(tt.want, string(got))
			assertions.NotEqual(before.ETag, after.ETag)
			assertions.Equal([]byte(tt.want), after.Content)
		})
	}
}

func TestEditConfigCreatesMissingParentDirectories(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "nested", "settings", "config.toml")
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	requirements.False(before.Exists)

	after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.NoError(err)
	requirements.True(after.Exists)
	assert.Contains(t, string(after.Content), `theme = "dark"`)
}

func TestRestoreConfigFileRestoresOriginallyMissingSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	requirements.False(before.Exists)
	after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.NoError(err)

	restored, err := RestoreConfigFile(path, after, before)
	requirements.NoError(err)
	assertions.False(restored.Exists)
	_, statErr := os.Stat(path)
	requirements.ErrorIs(statErr, fs.ErrNotExist)
	recoveries, err := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	requirements.NoError(err)
	requirements.Len(recoveries, 1)
	assertions.Equal(after.Content, mustReadFile(t, recoveries[0]))
}

func TestRestoreMissingConfigRefusesConcurrentReplacement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.NoError(err)
	operator := []byte("[web]\ntheme = \"light\"\n")
	requirements.NoError(os.WriteFile(path, operator, 0o600))

	_, err = RestoreConfigFile(path, after, before)
	requirements.ErrorIs(err, ErrConfigConflict)
	assertions.Equal(operator, mustReadFile(t, path))
}

func TestRestoreConfigPinsExpectedPublishedIdentityAtFinalBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		swap    func(t *testing.T, path string, content []byte)
		wantErr error
	}{
		{
			name: "different inode with identical bytes",
			swap: func(t *testing.T, path string, content []byte) {
				t.Helper()
				replacement := filepath.Join(filepath.Dir(path), "operator-replacement.toml")
				require.NoError(t, os.WriteFile(replacement, content, 0o600))
				require.NoError(t, os.Rename(replacement, path))
			},
			wantErr: ErrConfigConflict,
		},
		{
			name: "symlink to identical bytes",
			swap: func(t *testing.T, path string, content []byte) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "operator-target.toml")
				require.NoError(t, os.WriteFile(target, content, 0o600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(target, path))
			},
			// Windows rejects the reparse point outright instead of resolving
			// it; see expectedFinalBoundarySymlinkSwapError.
			wantErr: expectedFinalBoundarySymlinkSwapError(),
		},
		{
			name: "hardlink to identical bytes",
			swap: func(t *testing.T, path string, content []byte) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "operator-target.toml")
				require.NoError(t, os.WriteFile(target, content, 0o600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Link(target, path))
			},
			wantErr: ErrConfigConflict,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			original := []byte("[web]\ntheme = \"system\"\n")
			requirements.NoError(os.WriteFile(path, original, 0o600))
			before, err := ReadConfigFile(path)
			requirements.NoError(err)
			after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			requirements.NoError(err)

			ops := defaultConfigFileOps()
			ops.beforeExchange = func() error {
				test.swap(t, path, after.Content)
				return nil
			}
			_, err = restoreConfigFile(path, after, before, ops)
			requirements.Error(err)
			requirements.ErrorIs(err, test.wantErr)
			assertions.Equal(after.Content, mustReadFile(t, path))
		})
	}
}

func TestRestoreMissingConfigPinsExpectedPublishedIdentityAtFinalBoundary(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.NoError(err)
	operatorPath := filepath.Join(filepath.Dir(path), "operator.toml")
	requirements.NoError(os.WriteFile(operatorPath, after.Content, 0o600))
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		return os.Rename(operatorPath, path)
	}

	_, err = restoreConfigFile(path, after, before, ops)
	requirements.Error(err)
	requirements.ErrorIs(err, ErrConfigConflict)
	assertions.Equal(after.Content, mustReadFile(t, path))
	operator, err := ReadConfigFile(path)
	requirements.NoError(err)
	assertions.False(SameConfigFileVersion(after, operator))
}

func TestEditConfigDoesNotCreateDirectoriesBeforeConcurrencyCheck(t *testing.T) {
	requirements := require.New(t)
	root := t.TempDir()
	parent := filepath.Join(root, "nested", "settings")
	path := filepath.Join(parent, "config.toml")

	_, err := EditConfigFile(path, `"sha256-not-current"`, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.ErrorIs(err, ErrConfigConflict)
	_, statErr := os.Stat(parent)
	requirements.ErrorIs(statErr, fs.ErrNotExist)
}

func TestEditConfigIgnoresStructureInsideMultilineStrings(t *testing.T) {
	tests := []struct {
		name   string
		before string
		want   string
	}{
		{
			name: "basic string",
			before: "description = \"\"\"\n[web]\n" +
				"theme = \\\"content, not config\\\" # still content\n" +
				"escaped delimiter = \\\"\"\"\n\"\"\"\n\n" +
				"[web]\ntheme = \"system\"\n",
			want: "description = \"\"\"\n[web]\n" +
				"theme = \\\"content, not config\\\" # still content\n" +
				"escaped delimiter = \\\"\"\"\n\"\"\"\n\n" +
				"[web]\ntheme = \"dark\"\n",
		},
		{
			name: "literal string",
			before: "description = '''\n[web]\n" +
				"theme = 'content, not config'\n'''\n\n" +
				"[web]\ntheme = \"system\"\n",
			want: "description = '''\n[web]\n" +
				"theme = 'content, not config'\n'''\n\n" +
				"[web]\ntheme = \"dark\"\n",
		},
		{
			name:   "delimiter in comment",
			before: "# ignored delimiter: \"\"\"\n[web]\ntheme = \"system\"\n",
			want:   "# ignored delimiter: \"\"\"\n[web]\ntheme = \"dark\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tt.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			requirements.NoError(err)
			assertions.Equal(tt.want, string(after.Content))
		})
	}
}

func TestEditConfigReplacesCompleteMultilineStringAssignment(t *testing.T) {
	tests := []struct {
		name   string
		before string
		want   string
	}{
		{
			name: "basic",
			before: "[web]\n" +
				"theme = \"\"\"\nold theme\nwith \\\"quotes\\\"\n\"\"\" # operator note\n" +
				"density = \"compact\"\n",
			want: "[web]\n" +
				"theme = \"dark\" # operator note\n" +
				"density = \"compact\"\n",
		},
		{
			name: "literal",
			before: "[web]\n" +
				"theme = '''\nold theme\nwith 'quotes'\n''' # operator note\n" +
				"density = \"compact\"\n",
			want: "[web]\n" +
				"theme = \"dark\" # operator note\n" +
				"density = \"compact\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tt.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			requirements.NoError(err)
			assertions.Equal(tt.want, string(after.Content))
		})
	}
}

func TestEditConfigSynthesizesOnlyRequestedTables(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	assertions.False(before.Exists)

	after, err := EditConfigFile(path, before.ETag, []Edit{
		{Key: "web.theme", Value: "dark"},
		{Key: "integrations.tasks.enabled", Value: true},
	})
	requirements.NoError(err)
	assertions.True(after.Exists)
	assertions.Equal("[web]\ntheme = \"dark\"\n\n[integrations.tasks]\nenabled = true\n", string(after.Content))
	if runtime.GOOS != "windows" {
		// Unix permission enforcement. Windows security lives in the DACL,
		// which the Windows-specific tests verify via verifyConfigOwnerOnly;
		// Stat mode bits there are synthetic.
		info, err := os.Stat(path)
		requirements.NoError(err)
		assertions.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestEditConfigTracksArrayTableBoundaries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[web]\ntheme = \"system\"\n\n[[accounts]]\nemail = \"user-a@example.com\"\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.density", Value: "compact"}})
	requirements.NoError(err)
	assertions.Equal("[web]\ntheme = \"system\"\ndensity = \"compact\"\n\n[[accounts]]\nemail = \"user-a@example.com\"\n", string(after.Content))
	loaded, err := Load(path, "")
	requirements.NoError(err)
	assertions.Equal("compact", loaded.Web.Density)
	assertions.Equal("user-a@example.com", loaded.Accounts[0].Email)
}

func TestEditConfigRecognizesEquivalentTOMLPaths(t *testing.T) {
	tests := []struct {
		name   string
		before string
		key    string
		value  any
		want   string
	}{
		{
			name:   "quoted table and key",
			before: "[\"web\"]\n\"theme\" = \"system\"\n",
			key:    "web.theme",
			value:  "dark",
			want:   "[\"web\"]\n\"theme\" = \"dark\"\n",
		},
		{
			name:   "dotted quoted table",
			before: "[\"integrations\".tasks]\nenabled = false\n",
			key:    "integrations.tasks.enabled",
			value:  true,
			want:   "[\"integrations\".tasks]\nenabled = true\n",
		},
		{
			name:   "parent table and dotted key",
			before: "[integrations]\ntasks.enabled = false\n",
			key:    "integrations.tasks.enabled",
			value:  true,
			want:   "[integrations]\ntasks.enabled = true\n",
		},
		{
			name:   "parent table missing dotted key",
			before: "[integrations]\nother = true\n",
			key:    "integrations.tasks.enabled",
			value:  true,
			want:   "[integrations]\nother = true\ntasks.enabled = true\n",
		},
		{
			name:   "root dotted sibling missing key",
			before: "integrations.tasks.endpoint = \"https://tasks.example.com\"\n[web]\ntheme = \"system\"\n",
			key:    "integrations.tasks.enabled",
			value:  true,
			want:   "integrations.tasks.endpoint = \"https://tasks.example.com\"\nintegrations.tasks.enabled = true\n[web]\ntheme = \"system\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tt.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: tt.key, Value: tt.value}})
			requirements.NoError(err)
			assert.Equal(t, tt.want, string(after.Content))
		})
	}
}

func TestEditConfigRejectsSemanticDuplicateSpellings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "integrations.tasks.enabled = false\n[integrations]\ntasks.enabled = true\n"
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(t, err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "integrations.tasks.enabled", Value: true}})
	require.ErrorIs(t, err, ErrAmbiguousConfigTarget)
}

// TestConfigEditTablesPreservesUnrelatedBytesAndRemovesOneExactTable catches
// provider management rewriting comments/unknown tables or deleting a sibling
// profile while adding and removing a named provider table.
func TestConfigEditTablesPreservesUnrelatedBytesAndRemovesOneExactTable(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := `# operator comment
[people.sweep]
enabled = false
provider = "alpha" # selected profile

[people.sweep.providers.alpha]
protocol = "openai_chat"
endpoint = "https://alpha.example.test/v1"
model = "alpha-model"
auth = "bearer"
credential = "env"
credential_env = "ALPHA_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"

[people.sweep.providers.old]
protocol = "openai_chat"
endpoint = "https://old.example.test/v1"
model = "old-model"
auth = "none"
credential = "none"
output_mode = "prompt_json"
token_limit_parameter = "max_tokens"
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"

# comment owned by the following unknown table
[future.operator_extension] # unknown table must survive byte-for-byte
answer = 42
`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o640))
	// Set the mode being preserved explicitly; creation honors the caller's umask.
	requirements.NoError(os.Chmod(path, 0o640))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	added, err := EditConfigTables(path, snapshot.ETag, []TableEdit{{
		Path: []string{"people", "sweep", "providers", "beta"},
		Values: map[string]any{
			"protocol":              "openai_chat",
			"endpoint":              "https://beta.example.test/v1",
			"model":                 "beta-model",
			"auth":                  "bearer",
			"credential":            "stored",
			"output_mode":           "json_object",
			"token_limit_parameter": "max_tokens",
			"retention_posture":     "zero_retention",
			"training_posture":      "no_training",
			"allowed_sources":       []string{"conversation_text"},
			"source_since":          "2026-01-01",
		},
	}})
	requirements.NoError(err)
	assertions.Contains(string(added.Content), before)
	assertions.Contains(string(added.Content), "[people.sweep.providers.beta]\n")
	assertions.Contains(string(added.Content), "credential = \"stored\"\n")
	// Windows reports only the read-only attribute (0666 for writable files)
	// while Unix reports the exact requested mode, so the published mode is
	// asserted through the platform-aware sameConfigModePerm convention.
	assertions.True(sameConfigModePerm(fs.FileMode(0o640), added.Mode),
		"published config mode must stay 0640-equivalent, got %v", added.Mode)

	removed, err := EditConfigTables(path, added.ETag, []TableEdit{{
		Path:   []string{"people", "sweep", "providers", "old"},
		Remove: true,
	}})
	requirements.NoError(err)
	assertions.NotContains(string(removed.Content), "[people.sweep.providers.old]")
	assertions.Contains(string(removed.Content), "[people.sweep.providers.alpha]")
	assertions.Contains(string(removed.Content), "[people.sweep.providers.beta]")
	assertions.Contains(string(removed.Content), "# comment owned by the following unknown table\n[future.operator_extension] # unknown table must survive byte-for-byte\nanswer = 42\n")

	_, err = EditConfigTables(path, snapshot.ETag, []TableEdit{{
		Path: []string{"people", "sweep", "providers", "gamma"}, Values: map[string]any{"model": "gamma"},
	}})
	assertions.ErrorIs(err, ErrConfigConflict)
}

// TestConfigEditTablesRejectsAmbiguousProviderShapes catches a named profile
// edit guessing between duplicate table spellings.
func TestConfigEditTablesRejectsAmbiguousProviderShapes(t *testing.T) {
	tests := []struct {
		name   string
		before string
		want   error
	}{
		{
			name: "duplicate semantic table",
			before: `[people.sweep.providers.alpha]
model = "one"
[people.sweep.providers."alpha"]
model = "two"
`,
			want: ErrAmbiguousConfigTarget,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(test.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)

			_, err = EditConfigTables(path, snapshot.ETag, []TableEdit{{
				Path: []string{"people", "sweep", "providers", "alpha"},
				Values: map[string]any{
					"protocol": "openai_chat", "model": "new", "auth": "none",
					"credential": "none", "output_mode": "prompt_json",
					"token_limit_parameter": "max_tokens", "retention_posture": "local_only",
					"training_posture": "local_only", "allowed_sources": []string{"conversation_text"},
					"source_since": "2025-01-01",
				},
			}})
			requirements.ErrorIs(err, test.want)
			assertions.Equal(test.before, string(mustReadFile(t, path)))
		})
	}
}

// namedDefaultProviderValues and namedDefaultProviderTable describe one
// loadable named people provider profile in the byte order the editor
// appends, so table-edit candidates that publish the "default" selector
// stay valid configs.
var namedDefaultProviderValues = map[string]any{
	"protocol": "openai_chat", "endpoint": "https://default.example.test/v1",
	"model": "default-model", "auth": "bearer", "credential": "env",
	"credential_env": "DEFAULT_KEY", "output_mode": "native_json_schema",
	"token_limit_parameter": "max_completion_tokens", "retention_posture": "zero_retention",
	"training_posture": "no_training", "allowed_sources": []string{"conversation_text"},
	"source_since": "2025-01-01",
}

const namedDefaultProviderTable = `[people.sweep.providers.default]
allowed_sources = ["conversation_text"]
auth = "bearer"
credential = "env"
credential_env = "DEFAULT_KEY"
endpoint = "https://default.example.test/v1"
model = "default-model"
output_mode = "native_json_schema"
protocol = "openai_chat"
retention_posture = "zero_retention"
source_since = "2025-01-01"
token_limit_parameter = "max_completion_tokens"
training_posture = "no_training"
`

// TestConfigEditTablesSelectorJoinsImplicitParentTables reproduces the
// parentless legacy migration failure: when only sub-table headers such as
// [people.sweep.budgets] define people.sweep implicitly, the migrated
// selector used to be refused even though appending the explicit
// [people.sweep] header is valid TOML, because a super-table may be defined
// after its sub-tables.
func TestConfigEditTablesSelectorJoinsImplicitParentTables(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	before := `# retained operator comment
[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"

[people.sweep.provider]
kind = "openai_compatible"
endpoint = "https://legacy.example.test/v1"
model = "legacy-model"
api_key_env = "LEGACY_KEY"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"
request_timeout = "45s"

[people.sweep.budgets]
input_cost_microusd_per_million_tokens = 111 # operator price
output_cost_microusd_per_million_tokens = 222
`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	after, err := EditConfigTables(path, snapshot.ETag, []TableEdit{
		{Path: []string{"people", "sweep", "provider"}, Remove: true},
		{Path: []string{"people", "sweep"}, Values: map[string]any{"provider": "default"}},
		{Path: []string{"people", "sweep", "providers", "default"},
			Values: namedDefaultProviderValues, InsertOnly: true},
	})
	requirements.NoError(err)

	want := `# retained operator comment
[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"


[people.sweep.budgets]
input_cost_microusd_per_million_tokens = 111 # operator price
output_cost_microusd_per_million_tokens = 222

[people.sweep]
provider = "default"

` + namedDefaultProviderTable
	assertions.Equal(want, string(after.Content))

	loaded, err := LoadConfigFile(after, "")
	requirements.NoError(err)
	assertions.Equal("default", loaded.People.Sweep.Provider.Name)
	assertions.Equal(int64(111), loaded.People.Sweep.Budgets.InputCostMicroUSDPerMillionTokens)
}

// TestConfigEditTablesSelectorReplacesDottedFamilyValues reproduces the root
// dotted legacy encoding failure: `people.sweep.provider = { ... }` defines
// people.sweep through dotted keys, so the migrated selector may not append
// an explicit [people.sweep] header — it must replace the legacy value in
// place and join the dotted family for keys the file does not carry yet.
func TestConfigEditTablesSelectorReplacesDottedFamilyValues(t *testing.T) {
	t.Run("existing dotted keys are replaced in place", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		before := `# retained operator comment
people.sweep.enabled = false
people.sweep.provider = { kind = "openai_compatible", endpoint = "https://legacy.example.test/v1", model = "legacy-model", api_key_env = "LEGACY_KEY", retention_posture = "zero_retention", training_posture = "no_training", allowed_sources = ["conversation_text"], source_since = "2025-01-01", request_timeout = "45s" } # operator note

[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"
`
		requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
		snapshot, err := ReadConfigFile(path)
		requirements.NoError(err)

		after, err := EditConfigTables(path, snapshot.ETag, []TableEdit{
			{Path: []string{"people", "sweep"}, Values: map[string]any{"enabled": true, "provider": "default"}},
			{Path: []string{"people", "sweep", "providers", "default"},
				Values: namedDefaultProviderValues, InsertOnly: true},
		})
		requirements.NoError(err)

		want := `# retained operator comment
people.sweep.enabled = true
people.sweep.provider = "default" # operator note

[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"

` + namedDefaultProviderTable
		assertions.Equal(want, string(after.Content))
		loaded, err := LoadConfigFile(after, "")
		requirements.NoError(err)
		assertions.True(loaded.People.Sweep.Enabled)
		assertions.Equal("default", loaded.People.Sweep.Provider.Name)
	})

	t.Run("missing dotted keys join the family adjacently", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		before := `# retained operator comment
people.sweep.provider = { kind = "openai_compatible", endpoint = "https://legacy.example.test/v1", model = "legacy-model", api_key_env = "LEGACY_KEY", retention_posture = "zero_retention", training_posture = "no_training", allowed_sources = ["conversation_text"], source_since = "2025-01-01", request_timeout = "45s" }

[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"
`
		requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
		snapshot, err := ReadConfigFile(path)
		requirements.NoError(err)

		after, err := EditConfigTables(path, snapshot.ETag, []TableEdit{
			{Path: []string{"people", "sweep"}, Values: map[string]any{"enabled": true, "provider": "default"}},
			{Path: []string{"people", "sweep", "providers", "default"},
				Values: namedDefaultProviderValues, InsertOnly: true},
		})
		requirements.NoError(err)

		want := `# retained operator comment
people.sweep.provider = "default"
people.sweep.enabled = true

[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"

` + namedDefaultProviderTable
		assertions.Equal(want, string(after.Content))
		loaded, err := LoadConfigFile(after, "")
		requirements.NoError(err)
		assertions.True(loaded.People.Sweep.Enabled)
		assertions.Equal("default", loaded.People.Sweep.Provider.Name)
		assertions.Contains(loaded.People.Sweep.Providers, "default")
	})
}

// TestConfigEditTablesStillRefusesValueAssignedTable pins the refusal that
// stays: a table path that is itself assigned as a value (or nested below
// such an assignment) can never gain keys, because neither a header nor a
// dotted key may extend an inline value.
func TestConfigEditTablesStillRefusesValueAssignedTable(t *testing.T) {
	tests := []struct {
		name   string
		before string
	}{
		{
			name:   "target assigned as value",
			before: "people.sweep = { kind = \"openai_compatible\" }\n\n[data]\ndata_dir = \"x\"\n",
		},
		{
			name:   "ancestor assigned as value",
			before: "people = { sweep = { provider = \"default\" } }\n\n[data]\ndata_dir = \"x\"\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(test.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)

			_, err = EditConfigTables(path, snapshot.ETag, []TableEdit{{
				Path: []string{"people", "sweep"}, Values: map[string]any{"provider": "default"},
			}})
			requirements.ErrorIs(err, ErrAmbiguousConfigTarget)
			assertions.Equal(test.before, string(mustReadFile(t, path)))
		})
	}
}

func TestConfigEditTablesInsertOnlyRefusesExistingExactTable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[web]\ntheme = \"system\" # operator value\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigTables(path, snapshot.ETag, []TableEdit{{
		Path: []string{"web"}, Values: map[string]any{"theme": "dark"}, InsertOnly: true,
	}})
	requirements.ErrorIs(err, ErrAmbiguousConfigTarget)
	assertions.Equal(before, string(mustReadFile(t, path)))
}

// TestConfigEditTablesInsertOnlyRefusesExistingDottedFamily pins that
// InsertOnly refuses a preexisting table encoded as a dotted assignment
// family (for example people.sweep.providers.alpha.model = "..."), not only
// an explicit [table] header: named records must never be overwritten by a
// concurrent add regardless of how the file encodes them. Non-InsertOnly
// edits keep migrating dotted families in place.
func TestConfigEditTablesInsertOnlyRefusesExistingDottedFamily(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	before := `people.sweep.provider = "alpha"
people.sweep.providers.alpha.protocol = "openai_chat"
people.sweep.providers.alpha.endpoint = "https://alpha.example.test/v1"
people.sweep.providers.alpha.model = "alpha-model" # operator value
people.sweep.providers.alpha.auth = "none"
people.sweep.providers.alpha.credential = "none"
people.sweep.providers.alpha.output_mode = "prompt_json"
people.sweep.providers.alpha.token_limit_parameter = "max_tokens"
people.sweep.providers.alpha.retention_posture = "local_only"
people.sweep.providers.alpha.training_posture = "local_only"
people.sweep.providers.alpha.allowed_sources = ["conversation_text"]
people.sweep.providers.alpha.source_since = "2025-01-01"

[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"
`
	require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(t, err)

	t.Run("insert only refuses the dotted family", func(t *testing.T) {
		_, err := EditConfigTables(path, snapshot.ETag, []TableEdit{{
			Path:   []string{"people", "sweep", "providers", "alpha"},
			Values: namedDefaultProviderValues, InsertOnly: true,
		}})
		require.ErrorIs(t, err, ErrAmbiguousConfigTarget)
		assert.Equal(t, before, string(mustReadFile(t, path)))
	})

	t.Run("default edit migrates the dotted family in place", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		after, err := EditConfigTables(path, snapshot.ETag, []TableEdit{{
			Path:   []string{"people", "sweep", "providers", "alpha"},
			Values: namedDefaultProviderValues,
		}})
		requirements.NoError(err)

		want := `people.sweep.provider = "alpha"
people.sweep.providers.alpha.protocol = "openai_chat"
people.sweep.providers.alpha.endpoint = "https://default.example.test/v1"
people.sweep.providers.alpha.model = "default-model" # operator value
people.sweep.providers.alpha.auth = "bearer"
people.sweep.providers.alpha.credential = "env"
people.sweep.providers.alpha.output_mode = "native_json_schema"
people.sweep.providers.alpha.token_limit_parameter = "max_completion_tokens"
people.sweep.providers.alpha.retention_posture = "zero_retention"
people.sweep.providers.alpha.training_posture = "no_training"
people.sweep.providers.alpha.allowed_sources = ["conversation_text"]
people.sweep.providers.alpha.source_since = "2025-01-01"
people.sweep.providers.alpha.credential_env = "DEFAULT_KEY"

[data]
data_dir = "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"
`
		assertions.Equal(want, string(after.Content))
		loaded, err := LoadConfigFile(after, "")
		requirements.NoError(err)
		assertions.Equal("alpha", loaded.People.Sweep.Provider.Name)
		assertions.Contains(loaded.People.Sweep.Providers, "alpha")
	})
}

func TestConfigEditTablesRemovalRejectsDescendantExtensions(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := `[people.sweep]
enabled = false
provider = "old"

[people.sweep.providers.old]
protocol = "openai_chat"
endpoint = "https://old.example.test/v1"
model = "old-model"
auth = "none"
credential = "none"
output_mode = "prompt_json"
token_limit_parameter = "max_tokens"
retention_posture = "local_only"
training_posture = "local_only"
allowed_sources = ["conversation_text"]
source_since = "2025-01-01"

# operator-owned provider extension
[people.sweep.providers.old.future_extension]
answer = 42
`
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigTables(path, snapshot.ETag, []TableEdit{{
		Path: []string{"people", "sweep", "providers", "old"}, Remove: true,
	}})
	requirements.ErrorIs(err, ErrAmbiguousConfigTarget)
	assertions.Equal(before, string(mustReadFile(t, path)))
}

func TestEditConfigMissingKeyPreservesNoFinalNewline(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\""), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.density", Value: "compact"}})
	requirements.NoError(err)
	assert.Equal(t, "[web]\ntheme = \"system\"\ndensity = \"compact\"", string(after.Content))
}

func TestEditConfigRejectsConcurrentAndAmbiguousWrites(t *testing.T) {
	t.Run("concurrent hand edit", func(t *testing.T) {
		requirements := require.New(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
		before, err := ReadConfigFile(path)
		requirements.NoError(err)
		requirements.NoError(os.WriteFile(path, []byte("# hand edit\n[web]\ntheme = \"light\"\n"), 0o600))

		_, err = EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
		requirements.ErrorIs(err, ErrConfigConflict)
		got, readErr := os.ReadFile(path)
		requirements.NoError(readErr)
		assert.Equal(t, "# hand edit\n[web]\ntheme = \"light\"\n", string(got))
	})

	t.Run("duplicate target", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		before := "[web]\ntheme = \"light\"\ntheme = \"dark\"\n"
		requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
		snapshot, err := ReadConfigFile(path)
		requirements.NoError(err)

		_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "system"}})
		requirements.ErrorIs(err, ErrAmbiguousConfigTarget)
		got, readErr := os.ReadFile(path)
		requirements.NoError(readErr)
		assertions.Equal(before, string(got))
	})

	t.Run("duplicate target across dotted and table forms", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		before := "web.theme = \"light\"\n[web]\ntheme = \"dark\"\n"
		require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
		snapshot, err := ReadConfigFile(path)
		require.NoError(t, err)

		_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "system"}})
		require.ErrorIs(t, err, ErrAmbiguousConfigTarget)
	})
}

func TestEditConfigDetectsRaceAtAtomicReplacement(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	handEdit := "# hand edit\n[web]\ntheme = \"light\"\n"
	requirements.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		return os.WriteFile(path, []byte(handEdit), 0o600)
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigConflict)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, handEdit, string(got))
}

func TestEditConfigDetectsSameContentInodeSubstitution(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	requirements.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(beforeText), 0o600)
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigConflict)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, beforeText, string(got))
}

func TestEditConfigRetirementPreservesHardlinkedOriginal(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native config retirement")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	backup := filepath.Join(dir, "operator-backup.toml")
	beforeText := []byte("[web]\ntheme = \"system\"\n")
	requirements.NoError(os.WriteFile(path, beforeText, 0o600))
	requirements.NoError(os.Link(path, backup))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.NoError(err)
	assertions.Equal(beforeText, mustReadFile(t, backup))
	recoveries, globErr := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	requirements.NoError(globErr)
	requirements.Len(recoveries, 1)
	assertions.Equal(beforeText, mustReadFile(t, recoveries[0]))
}

func TestEditConfigCandidateAbortPreservesExternalHardlink(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native config retirement")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	external := filepath.Join(dir, "candidate-backup.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		candidates, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*.toml.tmp"))
		requirements.NoError(globErr)
		requirements.Len(candidates, 1)
		requirements.NoError(os.Link(candidates[0], external))
		return errors.New("abort candidate publication")
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorContains(err, "abort candidate publication")
	wantCandidate := []byte("[web]\ntheme = \"dark\"\n")
	assertions.Equal(wantCandidate, mustReadFile(t, external))
	recoveries, globErr := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	requirements.NoError(globErr)
	requirements.Len(recoveries, 1)
	assertions.Equal(wantCandidate, mustReadFile(t, recoveries[0]))
}

func TestEditConfigClassifiesDeletionRaceAsConflict(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error { return os.Remove(path) }

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigConflict)
	_, statErr := os.Stat(path)
	assertions.ErrorIs(statErr, fs.ErrNotExist)
}

func TestEditConfigMissingFileRejectsParentDirectorySwap(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("handle-relative missing-file publication is supported on Darwin and Linux")
	}
	requirements := require.New(t)
	assertions := assert.New(t)
	root := t.TempDir()
	parent := filepath.Join(root, "active")
	displacedParent := filepath.Join(root, "original")
	requirements.NoError(os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "config.toml")
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	assertions.False(snapshot.Exists)
	unsafeContent := []byte("# unrelated same-owner file\n")
	attackerCandidate := []byte("do not remove\n")
	var substitutedCandidate string
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		candidates, globErr := filepath.Glob(filepath.Join(parent, ".config-edit-*.toml.tmp"))
		if globErr != nil {
			return globErr
		}
		if len(candidates) != 1 {
			return fmt.Errorf("expected one candidate, got %d", len(candidates))
		}
		candidateName := filepath.Base(candidates[0])
		if renameErr := os.Rename(parent, displacedParent); renameErr != nil {
			return renameErr
		}
		if mkdirErr := os.Mkdir(parent, 0o700); mkdirErr != nil {
			return mkdirErr
		}
		substitutedCandidate = filepath.Join(parent, candidateName)
		if writeErr := os.WriteFile(substitutedCandidate, attackerCandidate, 0o600); writeErr != nil {
			return writeErr
		}
		return os.WriteFile(path, unsafeContent, 0o600)
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigConflict)
	assertions.Equal(unsafeContent, mustReadFile(t, path))
	assertions.Equal(attackerCandidate, mustReadFile(t, substitutedCandidate))
	_, originalErr := os.Stat(filepath.Join(displacedParent, "config.toml"))
	requirements.ErrorIs(originalErr, fs.ErrNotExist)
	originalCandidates, globErr := filepath.Glob(filepath.Join(displacedParent, ".config-edit-*.toml.tmp"))
	requirements.NoError(globErr)
	assertions.Empty(originalCandidates)
}

func TestEditConfigDetectsSymlinkRetargetRace(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink semantics differ on Windows")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	original := filepath.Join(dir, "original.toml")
	replacement := filepath.Join(dir, "replacement.toml")
	link := filepath.Join(dir, "config.toml")
	originalText := "[web]\ntheme = \"system\"\n"
	replacementText := "[web]\ntheme = \"light\"\n"
	requirements.NoError(os.WriteFile(original, []byte(originalText), 0o600))
	requirements.NoError(os.WriteFile(replacement, []byte(replacementText), 0o600))
	requirements.NoError(os.Symlink(filepath.Base(original), link))
	before, err := ReadConfigFile(link)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink(filepath.Base(replacement), link)
	}

	_, err = editConfigFile(link, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigConflict)
	originalAfter, readErr := os.ReadFile(original)
	requirements.NoError(readErr)
	assertions.Equal(originalText, string(originalAfter))
	replacementAfter, readErr := os.ReadFile(replacement)
	requirements.NoError(readErr)
	assertions.Equal(replacementText, string(replacementAfter))
}

func TestEditConfigReleasesAuthorityAfterSuccessfulConflictRollback(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink semantics differ on Windows")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	original := filepath.Join(dir, "original.toml")
	replacementTarget := filepath.Join(dir, "replacement.toml")
	link := filepath.Join(dir, "config.toml")
	originalText := "[web]\ntheme = \"system\"\n"
	replacementText := "[web]\ntheme = \"light\"\n"
	requirements.NoError(os.WriteFile(original, []byte(originalText), 0o600))
	requirements.NoError(os.WriteFile(replacementTarget, []byte(replacementText), 0o600))
	requirements.NoError(os.Symlink(filepath.Base(original), link))
	snapshot, err := ReadConfigFile(link)
	requirements.NoError(err)
	releases := 0
	ops := defaultConfigFileOps()
	nativeReplace := ops.replace
	ops.replace = func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, replaceErr := nativeReplace(candidatePath, targetPath, candidateBefore, targetBefore)
		if replaceErr != nil {
			return replacement, replaceErr
		}
		nativeRelease := replacement.release
		replacement.release = func() error {
			releases++
			return nativeRelease()
		}
		requirements.NoError(os.Remove(link))
		requirements.NoError(os.Symlink(filepath.Base(replacementTarget), link))
		return replacement, nil
	}

	_, err = editConfigFile(link, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigConflict)
	assertions.Equal(1, releases)
	assertions.Equal(originalText, string(mustReadFile(t, original)))
	assertions.Equal(replacementText, string(mustReadFile(t, replacementTarget)))
	candidates, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*.toml.tmp"))
	requirements.NoError(globErr)
	assertions.Empty(candidates)
}

func TestEditConfigFailsClosedWithoutConditionalReplacement(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	requirements.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	ops.replace = func(_, _ string, _, _ ConfigFile) (configReplacement, error) {
		return configReplacement{}, ErrAtomicReplaceUnsupported
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrAtomicReplaceUnsupported)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, beforeText, string(got))
}

func TestSyncConfigDirectoryPropagatesFailures(t *testing.T) {
	tests := []struct {
		name    string
		open    func(string) (syncDirectoryHandle, error)
		message string
	}{
		{
			name: "open",
			open: func(string) (syncDirectoryHandle, error) {
				return nil, errors.New("open failed")
			},
			message: "open config directory",
		},
		{
			name: "sync",
			open: func(string) (syncDirectoryHandle, error) {
				return &failingDirectoryHandle{syncErr: errors.New("sync failed")}, nil
			},
			message: "sync config directory",
		},
		{
			name: "close",
			open: func(string) (syncDirectoryHandle, error) {
				return &failingDirectoryHandle{closeErr: errors.New("close failed")}, nil
			},
			message: "close config directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := syncConfigDirectory(t.TempDir(), tt.open)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestEditConfigRollsBackExistingFileWhenFirstDirectorySyncFails(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	requirements.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("durability unavailable")},
		&failingDirectoryHandle{},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedReplacementSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.Error(err)
	requirements.NotErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, beforeText, string(got))
}

func TestEditConfigReportsChangedWhenRollbackDirectorySyncFails(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	requirements.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("publication sync unavailable")},
		&failingDirectoryHandle{syncErr: errors.New("rollback sync unavailable")},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedReplacementSync(&ops, opener.open)

	after, err := editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	assertions.Equal("[web]\ntheme = \"dark\"\n", string(after.Content))
	assertions.Equal(configETag(after.Content), after.ETag)
	assertions.True(after.Exists)
	assertions.NotEmpty(after.identity)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(beforeText, string(got))
}

func TestSameConfigModePermKeepsStrictUnixEquality(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows reports only the read-only attribute; see edit_mode_windows.go")
	}
	// Unix platforms observe real permission bits, so mode verification must
	// stay exact there: a published 0666 config must never verify against a
	// 0600 expectation. Windows reports only the read-only attribute and uses
	// the equivalence in edit_mode_windows.go instead.
	assert.False(t, sameConfigModePerm(fs.FileMode(0o600), fs.FileMode(0o666)))
	assert.True(t, sameConfigModePerm(fs.FileMode(0o600), fs.FileMode(0o600)))
	assert.False(t, sameConfigModePerm(fs.FileMode(0o600), fs.FileMode(0o444)))
}

func TestSameConfigFileVersionRejectsByteIdenticalReplacement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	content := []byte("[web]\ntheme = \"dark\"\n")
	requirements.NoError(os.WriteFile(path, content, 0o600))
	transaction, err := ReadConfigFile(path)
	requirements.NoError(err)
	assertions.True(SameConfigFileVersion(transaction, transaction))

	replacementPath := filepath.Join(filepath.Dir(path), "replacement.toml")
	requirements.NoError(os.WriteFile(replacementPath, content, 0o600))
	requirements.NoError(os.Rename(replacementPath, path))
	concurrent, err := ReadConfigFile(path)
	requirements.NoError(err)
	assertions.False(SameConfigFileVersion(transaction, concurrent))
}

func TestEditConfigReturnsPublishedIdentityWhenFinalReadSeesReplacement(t *testing.T) {
	newRequire := require.New
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	before, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	nativeRead := ops.read
	readCalls := 0
	ops.read = func(requested string) (ConfigFile, error) {
		require := newRequire(t)
		readCalls++
		if readCalls == 3 {
			content := mustReadFile(t, path)
			replacement := filepath.Join(filepath.Dir(path), "operator-replacement.toml")
			require.NoError(os.WriteFile(replacement, content, 0o600))
			replaceConfigTargetForFinalReadSwap(t, replacement, path)
		}
		return nativeRead(requested)
	}

	published, err := editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	current, readErr := ReadConfigFile(path)
	requirements.NoError(readErr)
	assertions.Equal(current.Content, published.Content)
	assertions.False(SameConfigFileVersion(published, current))
	assertions.NotEmpty(published.identity)
}

func TestEditConfigPreservesDisplacedArtifactWhenConflictRollbackFails(t *testing.T) {
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	requirements.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	nativeReplace := ops.replace
	ops.replace = func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, replaceErr := nativeReplace(candidatePath, targetPath, candidateBefore, targetBefore)
		if replaceErr != nil {
			return configReplacement{}, replaceErr
		}
		// Change the displaced artifact after publication so verification must
		// enter rollback, then force rollback to retain every recovery file.
		requirements.NoError(os.Remove(replacement.displacedPath))
		requirements.NoError(os.WriteFile(replacement.displacedPath, []byte(beforeText), 0o600))
		replacement.rollbackPublished = func(ConfigFile) error { return errors.New("injected rollback failure") }
		return replacement, nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	// The displaced artifact keeps the candidate name on darwin/linux (native
	// exchange) and gains a ".displaced" suffix on Windows (ReplaceFileW
	// backup), so match both spellings.
	artifacts, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*.toml.tmp*"))
	requirements.NoError(globErr)
	requirements.Len(artifacts, 1, "the displaced operator file must remain recoverable")
	recovered, readErr := os.ReadFile(artifacts[0])
	requirements.NoError(readErr)
	assert.Equal(t, beforeText, string(recovered))
	assert.Contains(t, err.Error(), artifacts[0])
}

func TestConditionalReplaceRollbackPreservesLaterWriter(t *testing.T) {
	requirements := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	beforeText := []byte("[web]\ntheme = \"system\"\n")
	candidateText := []byte("[web]\ntheme = \"dark\"\n")
	laterText := []byte("# later writer\n[web]\ntheme = \"light\"\n")
	requirements.NoError(os.WriteFile(target, beforeText, 0o600))
	before, err := readConfigFileForEdit(target)
	requirements.NoError(err)
	// Windows snapshots do not retain a descriptor; only Unix ones must be
	// closed here.
	if before.retained != nil {
		t.Cleanup(func() { requirements.NoError(before.retained.Close()) })
	}
	candidate := filepath.Join(filepath.Dir(before.Path), "candidate.toml")
	requirements.NoError(os.WriteFile(candidate, candidateText, 0o600))

	replace := func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, replaceErr := beginConfigReplacement(candidatePath, targetPath, candidateBefore, targetBefore)
		if replaceErr != nil {
			return replacement, replaceErr
		}
		requirements.NoError(os.Remove(replacement.displacedPath))
		requirements.NoError(os.WriteFile(replacement.displacedPath,
			[]byte("# intervening writer\n[web]\ntheme = \"system\"\n"), 0o600))
		nativeRollback := replacement.rollbackPublished
		replacement.rollbackPublished = func(expected ConfigFile) error {
			// Install a later writer after conditionalReplace has verified the
			// published candidate and immediately before native rollback.
			requirements.NoError(os.Remove(target))
			requirements.NoError(os.WriteFile(target, laterText, 0o600))
			return nativeRollback(expected)
		}
		return replacement, nil
	}

	replacement, err := conditionalReplace(target, candidate, before, replace, ReadConfigFile)
	// Production callers release through editConfigFile's defer; calling
	// conditionalReplace directly means releasing here, or the retained
	// directory pins block TempDir cleanup on Windows.
	if replacement.release != nil {
		t.Cleanup(func() { _ = replacement.release() })
	}
	requirements.ErrorIs(err, ErrConfigChanged)
	requirements.ErrorIs(err, ErrConfigConflict)
	assert.Equal(t, laterText, mustReadFile(t, target))
	assert.FileExists(t, replacement.displacedPath)
	assert.Contains(t, err.Error(), replacement.displacedPath)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

func mustConfigIdentity(t *testing.T, path string) string {
	t.Helper()
	snapshot, err := readPhysicalConfigSnapshot(path)
	require.NoError(t, err)
	return snapshot.identity
}

func TestEditConfigReportsChangedWhenExistingCleanupFails(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	nativeReplace := ops.replace
	ops.replace = func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, replaceErr := nativeReplace(candidatePath, targetPath, candidateBefore, targetBefore)
		if replaceErr == nil {
			replacement.cleanupDisplaced = func() error { return errors.New("cleanup unavailable") }
		}
		return replacement, replaceErr
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
}

func TestEditConfigDeferredCandidateCleanupPreservesSubstitute(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conditional Unix cleanup")
	}
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	substitute := []byte("candidate substitute\n")
	var candidatePath string
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		candidates, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-edit-*.toml.tmp"))
		requirements.NoError(globErr)
		requirements.Len(candidates, 1)
		candidatePath = candidates[0]
		requirements.NoError(os.Remove(candidatePath))
		requirements.NoError(os.WriteFile(candidatePath, substitute, 0o600))
		return errors.New("abort before publication")
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.Error(err)
	assert.Equal(t, substitute, mustReadFile(t, candidatePath))
	assert.Equal(t, "[web]\ntheme = \"system\"\n", string(mustReadFile(t, path)))
}

func TestEditConfigDisplacedCleanupPreservesSubstitute(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conditional Unix cleanup")
	}
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	substitute := []byte("displaced substitute\n")
	var displacedPath string
	ops := defaultConfigFileOps()
	ops.beforeExistingCleanup = func(replacement configReplacement) error {
		displacedPath = replacement.displacedPath
		requirements.NoError(os.Remove(displacedPath))
		requirements.NoError(os.WriteFile(displacedPath, substitute, 0o600))
		return nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	requirements.ErrorIs(err, ErrConfigConflict)
	assert.Equal(t, substitute, mustReadFile(t, displacedPath))
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(mustReadFile(t, path)))
}

func TestEditConfigMissingPublicationConsumesCandidate(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native no-replace publication")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	cleanupCalled := false
	ops := defaultConfigFileOps()
	ops.beforeMissingCleanup = func(configPublication) error {
		cleanupCalled = true
		return nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.NoError(err)
	assertions.False(cleanupCalled)
	assertions.Equal("[web]\ntheme = \"dark\"\n", string(mustReadFile(t, path)))
	candidates, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*"))
	requirements.NoError(globErr)
	assertions.Empty(candidates)
}

func TestEditConfigCreationRollbackPreservesLaterWriter(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conditional Unix cleanup")
	}
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	later := []byte("later creation writer\n")
	ops := defaultConfigFileOps()
	nativePublish := ops.publishNew
	ops.publishNew = func(candidatePath string, retained *os.File, before ConfigFile) (configPublication, error) {
		publication, publishErr := nativePublish(candidatePath, retained, before)
		if publishErr != nil {
			return publication, publishErr
		}
		nativeSync := publication.syncDirectory
		calls := 0
		publication.syncDirectory = func() error {
			calls++
			if calls == 1 {
				requirements.NoError(nativeSync())
				requirements.NoError(os.Remove(path))
				requirements.NoError(os.WriteFile(path, later, 0o600))
				return errors.New("force creation rollback")
			}
			return nativeSync()
		}
		return publication, nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	requirements.ErrorIs(err, ErrConfigConflict)
	assert.Equal(t, later, mustReadFile(t, path))
}

func TestEditConfigMissingPublicationHasNoCleanupStep(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	nativePublish := ops.publishNew
	ops.publishNew = func(candidatePath string, retained *os.File, before ConfigFile) (configPublication, error) {
		publication, publishErr := nativePublish(candidatePath, retained, before)
		if publishErr == nil {
			publication.cleanupCandidate = func() error { return errors.New("cleanup unavailable") }
		}
		return publication, publishErr
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.NoError(err)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
}

func TestEditConfigRollsBackMissingFileWhenFirstDirectorySyncFails(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("durability unavailable")},
		&failingDirectoryHandle{},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedPublicationSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.Error(err)
	requirements.NotErrorIs(err, ErrConfigChanged)
	_, statErr := os.Stat(path)
	requirements.ErrorIs(statErr, fs.ErrNotExist)
}

func TestEditConfigReportsChangedWhenMissingRollbackDirectorySyncFails(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("publication sync unavailable")},
		&failingDirectoryHandle{syncErr: errors.New("rollback sync unavailable")},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedPublicationSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	_, statErr := os.Stat(path)
	requirements.ErrorIs(statErr, fs.ErrNotExist)
}

func TestEditConfigReportsChangedWhenFinalDirectorySyncFails(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if existing {
				requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
			}
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)
			opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
				&failingDirectoryHandle{},
				&failingDirectoryHandle{syncErr: errors.New("final sync unavailable")},
			}}
			ops := defaultConfigFileOps()
			ops.openDirectory = opener.open
			if existing {
				overridePinnedReplacementSync(&ops, opener.open)
			} else {
				overridePinnedPublicationSync(&ops, opener.open)
			}

			_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
			requirements.ErrorIs(err, ErrConfigChanged)
			got, readErr := os.ReadFile(path)
			requirements.NoError(readErr)
			assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
		})
	}
}

func TestEditConfigReportsChangedWhenFinalSnapshotReadFails(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	ops := defaultConfigFileOps()
	reads := 0
	ops.read = func(readPath string) (ConfigFile, error) {
		reads++
		if reads >= 3 {
			return ConfigFile{}, errors.New("injected final read failure")
		}
		return ReadConfigFile(readPath)
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	requirements.ErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
}

func TestResolveConfigTargetFailsClosedWhenOwnershipCannotBeVerified(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	link := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(target, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	requirements.NoError(os.Symlink(filepath.Base(target), link))

	resolved, mode, exists, err := resolveConfigTargetWithOwner(link, func(fs.FileInfo) (uint64, bool) {
		return 0, false
	})
	requirements.ErrorIs(err, ErrUnsafeConfigTarget)
	assertions.Empty(resolved)
	assertions.Zero(mode)
	assertions.False(exists)
}

func TestResolveConfigTargetVerifiesParentDirectorySymlinkOwnership(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("Unix ownership semantics")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	requirements.NoError(os.Mkdir(realDir, 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(realDir, "config.toml"), []byte("[web]\n"), 0o600))
	parentLink := filepath.Join(dir, "linked")
	requirements.NoError(os.Symlink("real", parentLink))
	euid, supported := effectiveUserID()
	requirements.True(supported)

	resolved, mode, exists, err := resolveConfigTargetWithOwner(filepath.Join(parentLink, "config.toml"), func(info fs.FileInfo) (uint64, bool) {
		if info.Name() == "linked" {
			return euid + 1, true
		}
		return euid, true
	})
	requirements.ErrorIs(err, ErrUnsafeConfigTarget)
	assertions.Empty(resolved)
	assertions.Zero(mode)
	assertions.False(exists)
}

func TestResolveConfigTargetAllowsVerifiedRootOwnedSystemHop(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("Unix ownership semantics")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	requirements.NoError(os.Mkdir(realDir, 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(realDir, "config.toml"), []byte("[web]\n"), 0o600))
	parentLink := filepath.Join(dir, "linked")
	requirements.NoError(os.Symlink("real", parentLink))
	euid, supported := effectiveUserID()
	requirements.True(supported)
	checked := false

	_, _, exists, err := resolveConfigTargetWithOwner(filepath.Join(parentLink, "config.toml"), func(info fs.FileInfo) (uint64, bool) {
		if info.Name() == "linked" {
			checked = true
			return 0, true
		}
		return euid, true
	})
	requirements.NoError(err)
	assertions.True(checked)
	assertions.True(exists)
}

func TestResolveConfigTargetRejectsIntermediateSymlinkSwapDuringInspection(t *testing.T) {
	if runtime.GOOS == windowsOS || runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("path-based fallback is not used on platforms with pinned resolvers")
	}
	requirements := require.New(t)
	dir := t.TempDir()
	firstDir := filepath.Join(dir, "first")
	secondDir := filepath.Join(dir, "second")
	requirements.NoError(os.Mkdir(firstDir, 0o700))
	requirements.NoError(os.Mkdir(secondDir, 0o700))
	first := filepath.Join(firstDir, "config.toml")
	second := filepath.Join(secondDir, "config.toml")
	requirements.NoError(os.WriteFile(first, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	requirements.NoError(os.WriteFile(second, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	link := filepath.Join(dir, "active")
	requirements.NoError(os.Symlink("first", link))
	euid, supported := effectiveUserID()
	requirements.True(supported)

	_, err := resolveOwnedSymlinksWithReadlink(filepath.Join(link, "config.toml"), func(fs.FileInfo) (uint64, bool) {
		return euid, true
	}, func(path string) (string, error) {
		if filepath.Base(path) == "active" {
			requirements.NoError(os.Remove(link))
			requirements.NoError(os.Symlink("second", link))
		}
		return os.Readlink(path)
	})
	requirements.ErrorIs(err, ErrUnsafeConfigTarget)
	assert.Equal(t, "[web]\ntheme = \"system\"\n", string(mustReadFile(t, first)))
	assert.Equal(t, "[web]\ntheme = \"light\"\n", string(mustReadFile(t, second)))
}

func TestFallbackResolverRejectsRemovedSymlinkAfterReadlink(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("Unix symlink semantics")
	}
	requirements := require.New(t)
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	requirements.NoError(os.Mkdir(targetDir, 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(targetDir, "config.toml"), []byte("[web]\n"), 0o600))
	link := filepath.Join(dir, "active")
	requirements.NoError(os.Symlink("target", link))
	euid, supported := effectiveUserID()
	requirements.True(supported)

	_, err := resolveOwnedSymlinksWithReadlink(filepath.Join(link, "config.toml"), func(fs.FileInfo) (uint64, bool) {
		return euid, true
	}, func(path string) (string, error) {
		target, err := os.Readlink(path)
		if filepath.Base(path) == "active" {
			requirements.NoError(os.Remove(link))
		}
		return target, err
	})
	requirements.ErrorIs(err, ErrUnsafeConfigTarget)
}

func TestPinnedResolverCannotBeRedirectedByIntermediateSymlinkSwap(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("pinned resolver is supported on Darwin and Linux")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	firstDir := filepath.Join(dir, "first")
	secondDir := filepath.Join(dir, "second")
	requirements.NoError(os.Mkdir(firstDir, 0o700))
	requirements.NoError(os.Mkdir(secondDir, 0o700))
	first := filepath.Join(firstDir, "config.toml")
	second := filepath.Join(secondDir, "config.toml")
	requirements.NoError(os.WriteFile(first, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	requirements.NoError(os.WriteFile(second, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	link := filepath.Join(dir, "active")
	requirements.NoError(os.Symlink("first", link))
	euid, supported := effectiveUserID()
	requirements.True(supported)
	swapped := false

	resolved, err := resolveOwnedSymlinksPinned(filepath.Join(link, "config.toml"), func(fs.FileInfo) (uint64, bool) {
		return euid, true
	}, func(file *os.File) {
		if filepath.Base(file.Name()) != "active" || swapped {
			return
		}
		swapped = true
		requirements.NoError(os.Remove(link))
		requirements.NoError(os.Symlink("second", link))
	})
	if err == nil {
		assertions.Equal(first, resolved)
	}
	assertions.True(swapped)
	assertions.Equal("[web]\ntheme = \"system\"\n", string(mustReadFile(t, first)))
	assertions.Equal("[web]\ntheme = \"light\"\n", string(mustReadFile(t, second)))
}

func TestEditConfigValidatesCompleteCandidateBeforeReplacement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[analytics]\nengine = \"auto\"\n[server]\nbind_addr = \"127.0.0.1\"\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "analytics.engine", Value: "invalid"}})
	requirements.Error(err)
	assertions.Contains(err.Error(), "invalid [analytics] engine")
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(before, string(got))
}

func TestEditConfigValidatesEnabledVectorCandidate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[vector]\nenabled = false\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "vector.enabled", Value: true}})
	requirements.Error(err)
	assertions.Contains(err.Error(), "vector config")
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(before, string(got))
}

func TestEditConfigRejectsInvalidSlackSchedule(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[slack]\nenabled = true\nschedule = \"0 * * * *\"\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "slack.schedule", Value: "not a cron"}})
	requirements.ErrorIs(err, ErrInvalidConfigCandidate)
	assertions.Contains(err.Error(), "invalid slack.schedule")
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(before, string(got))
}

func TestEditConfigRejectsInvalidMatrixSchedule(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[matrix]\nenabled = true\nschedule = \"0 * * * *\"\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "matrix.schedule", Value: "not a cron"}})
	requirements.ErrorIs(err, ErrInvalidConfigCandidate)
	assertions.Contains(err.Error(), "invalid matrix.schedule")
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(before, string(got))
}

func TestEditConfigRejectsInvalidNotionMeetingsSchedule(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[[notion_meetings]]\n" +
		"identifier = \"notion-work\"\n" +
		"account_email = \"notion@example.com\"\n" +
		"token = \"ntn_test\"\n" +
		"schedule = \"15 */6 * * *\"\n" +
		"enabled = true\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "notion_meetings.schedule", Value: "not a cron"}})
	requirements.ErrorIs(err, ErrInvalidConfigCandidate)
	assertions.Contains(err.Error(), "invalid notion_meetings[0].schedule")
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(before, string(got))
}

func TestEditAllowsAutoMintForSecureNonLoopbackWithoutMintingKey(t *testing.T) { //nolint:paralleltest // process environment
	requirements := require.New(t)
	unsetServerKeyEnvironmentForTest(t)
	t.Setenv("MSGVAULT_BIND_ADDR", "0.0.0.0")
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	cfg, err := Load(path, "")
	requirements.NoError(err)
	keyPath := cfg.ServerKeyFilePath()
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	requirements.NoError(err, "config editing must allow the daemon's startup key mint policy")
	_, err = os.Stat(keyPath)
	requirements.ErrorIs(err, os.ErrNotExist, "editing config must not mint a server key")
	contents, err := os.ReadFile(path)
	requirements.NoError(err)
	assert.Contains(t, string(contents), `theme = "dark"`)
}

func TestEditDoesNotRequireLiveServerResources(t *testing.T) { //nolint:paralleltest // process environment
	t.Setenv("MSGVAULT_TEST_UNAVAILABLE_KEY", "")
	t.Setenv("MSGVAULT_API_PORT", "invalid-runtime-port")
	for _, server := range []string{
		"bind_addr = 'iface:msgvault-nonexistent-test-interface'",
		"api_key_env = 'MSGVAULT_TEST_UNAVAILABLE_KEY'",
		"api_key_file = 'missing-key'",
	} {
		t.Run(server, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte("[server]\n"+server+"\n[web]\ntheme = 'system'\n"), 0o600))
			before, err := ReadConfigFile(path)
			requirements.NoError(err)
			after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			requirements.NoError(err)
			cfg, err := LoadConfigFile(after, "")
			requirements.NoError(err)
			assertions.Equal("dark", cfg.Web.Theme)
			_, err = cfg.ResolveServerBindAddress()
			if err == nil {
				err = cfg.PrepareServerKey()
			}
			assertions.Error(err, "startup must still reject unavailable resources")
		})
	}
}

func unsetServerKeyEnvironmentForTest(t *testing.T) {
	t.Helper()
	for _, name := range []string{"MSGVAULT_API_KEY", "MSGVAULT_API_KEY_FILE", "MSGVAULT_API_KEY_ENV"} {
		envName := name
		original, _ := os.LookupEnv(envName)
		t.Setenv(envName, original)
		require.NoError(t, os.Unsetenv(envName))
	}
}

func TestEditConfigPreservesModeAndExistingSymlink(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink permission semantics differ on Windows")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "real-config.toml")
	link := filepath.Join(dir, "config.toml")
	intermediate := filepath.Join(dir, "config-link.toml")
	requirements.NoError(os.WriteFile(target, []byte("[web]\ndensity = \"compact\"\n"), 0o640))
	requirements.NoError(os.Chmod(target, 0o640))
	requirements.NoError(os.Symlink(filepath.Base(target), intermediate))
	requirements.NoError(os.Symlink(filepath.Base(intermediate), link))
	snapshot, err := ReadConfigFile(link)
	requirements.NoError(err)
	beforeInfo, err := os.Stat(target)
	requirements.NoError(err)
	beforeOwner, ok := fileOwner(beforeInfo)
	requirements.True(ok)

	_, err = EditConfigFile(link, snapshot.ETag, []Edit{{Key: "web.density", Value: "comfortable"}})
	requirements.NoError(err)
	linkInfo, err := os.Lstat(link)
	requirements.NoError(err)
	assertions.NotZero(linkInfo.Mode() & os.ModeSymlink)
	targetInfo, err := os.Stat(target)
	requirements.NoError(err)
	assertions.Equal(os.FileMode(0o640), targetInfo.Mode().Perm())
	afterOwner, ok := fileOwner(targetInfo)
	requirements.True(ok)
	assertions.Equal(beforeOwner, afterOwner)
	got, err := os.ReadFile(target)
	requirements.NoError(err)
	assertions.Equal("[web]\ndensity = \"comfortable\"\n", string(got))
}

func TestReadConfigFileRejectsUnsafeTargets(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink semantics differ on Windows")
	}
	dir := t.TempDir()

	t.Run("dangling symlink", func(t *testing.T) {
		link := filepath.Join(dir, "dangling.toml")
		require.NoError(t, os.Symlink("missing.toml", link))
		_, err := ReadConfigFile(link)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrUnsafeConfigTarget)
	})

	t.Run("non regular target", func(t *testing.T) {
		link := filepath.Join(dir, "directory.toml")
		require.NoError(t, os.Symlink(".", link))
		_, err := ReadConfigFile(link)
		require.ErrorIs(t, err, ErrUnsafeConfigTarget)
	})
}

func TestLoadConfigFileParsesSnapshotBytes(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"dark\"\n"), 0o600))

	loaded, err := LoadConfigFile(snapshot, "")
	requirements.NoError(err)
	assert.Equal(t, "light", loaded.Web.Theme)
}

func TestLoadConfigFileUsesLogicalSymlinkPathForRelativeDefaults(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink creation requires optional Windows privileges")
	}
	assertions := assert.New(t)
	requirements := require.New(t)
	root := t.TempDir()
	logicalDir := filepath.Join(root, "logical")
	physicalDir := filepath.Join(root, "physical")
	requirements.NoError(os.Mkdir(logicalDir, 0o700))
	requirements.NoError(os.Mkdir(physicalDir, 0o700))
	target := filepath.Join(physicalDir, "config.toml")
	requirements.NoError(os.WriteFile(target, []byte("[web]\ntheme = \"dark\"\n"), 0o600))
	link := filepath.Join(logicalDir, "config.toml")
	requirements.NoError(os.Symlink(target, link))

	snapshot, err := ReadConfigFile(link)
	requirements.NoError(err)
	assertions.Equal(link, snapshot.LogicalPath)
	resolvedTarget, err := filepath.EvalSymlinks(target)
	requirements.NoError(err)
	assertions.Equal(resolvedTarget, snapshot.Path)
	fromSnapshot, err := LoadConfigFile(snapshot, "")
	requirements.NoError(err)
	fromDaemon, err := Load(link, "")
	requirements.NoError(err)
	assertions.Equal(fromDaemon.HomeDir, fromSnapshot.HomeDir)
	assertions.Equal(logicalDir, fromSnapshot.HomeDir)
}

func TestEditConfigRejectsInvalidMuesliSchedule(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[[muesli]]\n" +
		"identifier = \"mac\"\n" +
		"account_email = \"you@example.com\"\n" +
		"schedule = \"*/30 * * * *\"\n" +
		"enabled = true\n"
	requirements.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	requirements.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "muesli.schedule", Value: "not a cron"}})
	requirements.ErrorIs(err, ErrInvalidConfigCandidate)
	assertions.Contains(err.Error(), "invalid muesli[0].schedule")
	got, readErr := os.ReadFile(path)
	requirements.NoError(readErr)
	assertions.Equal(before, string(got))
}

func TestEditConfigRejectsInvalidPlaudSchedule(t *testing.T) {
	for kind, before := range map[string]string{
		"plaud":  "[[plaud]]\nidentifier = \"work\"\naccount_email = \"owner@example.com\"\nschedule = \"15 */6 * * *\"\nenabled = true\n",
		"twilio": "[[twilio]]\naccount_email='user@example.com'\naccount_sid='AC00000000000000000000000000000001'\nauth_token='synthetic'\nschedule='15 */6 * * *'\n",
	} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(before), 0600))
			snapshot, err := ReadConfigFile(path)
			requirements.NoError(err)
			_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: kind + ".schedule", Value: "not a cron"}})
			requirements.ErrorIs(err, ErrInvalidConfigCandidate)
			assertions.Contains(err.Error(), "invalid "+kind+"[0].schedule")
			got, err := os.ReadFile(path)
			requirements.NoError(err)
			assertions.Equal(before, string(got))
		})
	}
}

func TestEditConfigFilePreservesConversationPolicy(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	for _, body := range []string{"", "[beeper]\nmax_participants=0\n[slack]\nmax_participants=17\n[discord]\nmax_participants=25\n[teams]\nmax_participants=31\n[conversation_policy]\nactivity_window_days=13\n[[conversation_policy.overrides]]\nsource_type='slack'\nsource_identifier='T_EXAMPLE:U_EXAMPLE'\nconversation_id='room'\ntier='cold'\n"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		requirements.NoError(os.WriteFile(path, []byte(body), 0600))
		before, err := Load(path, "")
		requirements.NoError(err)
		snapshot, err := ReadConfigFile(path)
		requirements.NoError(err)
		_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "beeper.media_max_participants", Value: int64(12)}, {Key: "web.theme", Value: "dark"}})
		requirements.NoError(err)
		after, err := Load(path, "")
		requirements.NoError(err)
		assertions.Equal(before.ConversationPolicySettings(), after.ConversationPolicySettings())
		if body == "" {
			assertions.Equal(20, after.Beeper.MaxParticipants)
			assertions.Equal(180, after.ConversationPolicy.ActivityWindowDays)
		}
	}
}
