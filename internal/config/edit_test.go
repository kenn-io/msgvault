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
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tt.before), 0o640))
			before, err := ReadConfigFile(path)
			require.NoError(err)

			after, err := EditConfigFile(path, before.ETag, tt.edits)
			require.NoError(err)
			got, err := os.ReadFile(path)
			require.NoError(err)
			assert.Equal(tt.want, string(got))
			assert.NotEqual(before.ETag, after.ETag)
			assert.Equal([]byte(tt.want), after.Content)
		})
	}
}

func TestEditConfigCreatesMissingParentDirectories(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "nested", "settings", "config.toml")
	before, err := ReadConfigFile(path)
	require.NoError(err)
	require.False(before.Exists)

	after, err := EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(err)
	require.True(after.Exists)
	assert.Contains(t, string(after.Content), `theme = "dark"`)
}

func TestEditConfigDoesNotCreateDirectoriesBeforeConcurrencyCheck(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	parent := filepath.Join(root, "nested", "settings")
	path := filepath.Join(parent, "config.toml")

	_, err := EditConfigFile(path, `"sha256-not-current"`, []Edit{{Key: "web.theme", Value: "dark"}})
	require.ErrorIs(err, ErrConfigConflict)
	_, statErr := os.Stat(parent)
	require.ErrorIs(statErr, fs.ErrNotExist)
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
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tt.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			require.NoError(err)
			assert.Equal(tt.want, string(after.Content))
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
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tt.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			require.NoError(err)
			assert.Equal(tt.want, string(after.Content))
		})
	}
}

func TestEditConfigSynthesizesOnlyRequestedTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before, err := ReadConfigFile(path)
	require.NoError(err)
	assert.False(before.Exists)

	after, err := EditConfigFile(path, before.ETag, []Edit{
		{Key: "web.theme", Value: "dark"},
		{Key: "integrations.tasks.enabled", Value: true},
	})
	require.NoError(err)
	assert.True(after.Exists)
	assert.Equal("[web]\ntheme = \"dark\"\n\n[integrations.tasks]\nenabled = true\n", string(after.Content))
	info, err := os.Stat(path)
	require.NoError(err)
	assert.Equal(os.FileMode(0o600), info.Mode().Perm())
}

func TestEditConfigTracksArrayTableBoundaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[web]\ntheme = \"system\"\n\n[[accounts]]\nemail = \"user-a@example.com\"\n"
	require.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)

	after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.density", Value: "compact"}})
	require.NoError(err)
	assert.Equal("[web]\ntheme = \"system\"\ndensity = \"compact\"\n\n[[accounts]]\nemail = \"user-a@example.com\"\n", string(after.Content))
	loaded, err := Load(path, "")
	require.NoError(err)
	assert.Equal("compact", loaded.Web.Density)
	assert.Equal("user-a@example.com", loaded.Accounts[0].Email)
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
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tt.before), 0o600))
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: tt.key, Value: tt.value}})
			require.NoError(err)
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
	assert.ErrorIs(t, err, ErrAmbiguousConfigTarget)
}

func TestEditConfigMissingKeyPreservesNoFinalNewline(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\""), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)

	after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.density", Value: "compact"}})
	require.NoError(err)
	assert.Equal(t, "[web]\ntheme = \"system\"\ndensity = \"compact\"", string(after.Content))
}

func TestEditConfigRejectsConcurrentAndAmbiguousWrites(t *testing.T) {
	t.Run("concurrent hand edit", func(t *testing.T) {
		require := require.New(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
		before, err := ReadConfigFile(path)
		require.NoError(err)
		require.NoError(os.WriteFile(path, []byte("# hand edit\n[web]\ntheme = \"light\"\n"), 0o600))

		_, err = EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
		require.ErrorIs(err, ErrConfigConflict)
		got, readErr := os.ReadFile(path)
		require.NoError(readErr)
		assert.Equal(t, "# hand edit\n[web]\ntheme = \"light\"\n", string(got))
	})

	t.Run("duplicate target", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		before := "[web]\ntheme = \"light\"\ntheme = \"dark\"\n"
		require.NoError(os.WriteFile(path, []byte(before), 0o600))
		snapshot, err := ReadConfigFile(path)
		require.NoError(err)

		_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "system"}})
		require.ErrorIs(err, ErrAmbiguousConfigTarget)
		got, readErr := os.ReadFile(path)
		require.NoError(readErr)
		assert.Equal(before, string(got))
	})

	t.Run("duplicate target across dotted and table forms", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		before := "web.theme = \"light\"\n[web]\ntheme = \"dark\"\n"
		require.NoError(t, os.WriteFile(path, []byte(before), 0o600))
		snapshot, err := ReadConfigFile(path)
		require.NoError(t, err)

		_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "system"}})
		assert.ErrorIs(t, err, ErrAmbiguousConfigTarget)
	})
}

func TestEditConfigDetectsRaceAtAtomicReplacement(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	handEdit := "# hand edit\n[web]\ntheme = \"light\"\n"
	require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	before, err := ReadConfigFile(path)
	require.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		return os.WriteFile(path, []byte(handEdit), 0o600)
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigConflict)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, handEdit, string(got))
}

func TestEditConfigDetectsSameContentInodeSubstitution(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	before, err := ReadConfigFile(path)
	require.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(beforeText), 0o600)
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigConflict)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, beforeText, string(got))
}

func TestEditConfigRetirementPreservesHardlinkedOriginal(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native config retirement")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	backup := filepath.Join(dir, "operator-backup.toml")
	beforeText := []byte("[web]\ntheme = \"system\"\n")
	require.NoError(os.WriteFile(path, beforeText, 0o600))
	require.NoError(os.Link(path, backup))
	before, err := ReadConfigFile(path)
	require.NoError(err)

	_, err = EditConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(err)
	assert.Equal(beforeText, mustReadFile(t, backup))
	recoveries, globErr := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	require.NoError(globErr)
	require.Len(recoveries, 1)
	assert.Equal(beforeText, mustReadFile(t, recoveries[0]))
}

func TestEditConfigCandidateAbortPreservesExternalHardlink(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native config retirement")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	external := filepath.Join(dir, "candidate-backup.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	before, err := ReadConfigFile(path)
	require.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		candidates, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*.toml.tmp"))
		require.NoError(globErr)
		require.Len(candidates, 1)
		require.NoError(os.Link(candidates[0], external))
		return errors.New("abort candidate publication")
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorContains(err, "abort candidate publication")
	wantCandidate := []byte("[web]\ntheme = \"dark\"\n")
	assert.Equal(wantCandidate, mustReadFile(t, external))
	recoveries, globErr := filepath.Glob(filepath.Join(dir, configRetiredPrefix+"*"))
	require.NoError(globErr)
	require.Len(recoveries, 1)
	assert.Equal(wantCandidate, mustReadFile(t, recoveries[0]))
}

func TestEditConfigClassifiesDeletionRaceAsConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	before, err := ReadConfigFile(path)
	require.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error { return os.Remove(path) }

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigConflict)
	_, statErr := os.Stat(path)
	assert.ErrorIs(statErr, fs.ErrNotExist)
}

func TestEditConfigMissingFileRejectsParentDirectorySwap(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("handle-relative missing-file publication is supported on Darwin and Linux")
	}
	require := require.New(t)
	assert := assert.New(t)
	root := t.TempDir()
	parent := filepath.Join(root, "active")
	displacedParent := filepath.Join(root, "original")
	require.NoError(os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	assert.False(snapshot.Exists)
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
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(unsafeContent, mustReadFile(t, path))
	assert.Equal(attackerCandidate, mustReadFile(t, substitutedCandidate))
	_, originalErr := os.Stat(filepath.Join(displacedParent, "config.toml"))
	require.ErrorIs(originalErr, fs.ErrNotExist)
	originalCandidates, globErr := filepath.Glob(filepath.Join(displacedParent, ".config-edit-*.toml.tmp"))
	require.NoError(globErr)
	assert.Empty(originalCandidates)
}

func TestEditConfigDetectsSymlinkRetargetRace(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink semantics differ on Windows")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	original := filepath.Join(dir, "original.toml")
	replacement := filepath.Join(dir, "replacement.toml")
	link := filepath.Join(dir, "config.toml")
	originalText := "[web]\ntheme = \"system\"\n"
	replacementText := "[web]\ntheme = \"light\"\n"
	require.NoError(os.WriteFile(original, []byte(originalText), 0o600))
	require.NoError(os.WriteFile(replacement, []byte(replacementText), 0o600))
	require.NoError(os.Symlink(filepath.Base(original), link))
	before, err := ReadConfigFile(link)
	require.NoError(err)
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink(filepath.Base(replacement), link)
	}

	_, err = editConfigFile(link, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigConflict)
	originalAfter, readErr := os.ReadFile(original)
	require.NoError(readErr)
	assert.Equal(originalText, string(originalAfter))
	replacementAfter, readErr := os.ReadFile(replacement)
	require.NoError(readErr)
	assert.Equal(replacementText, string(replacementAfter))
}

func TestEditConfigReleasesAuthorityAfterSuccessfulConflictRollback(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink semantics differ on Windows")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	original := filepath.Join(dir, "original.toml")
	replacementTarget := filepath.Join(dir, "replacement.toml")
	link := filepath.Join(dir, "config.toml")
	originalText := "[web]\ntheme = \"system\"\n"
	replacementText := "[web]\ntheme = \"light\"\n"
	require.NoError(os.WriteFile(original, []byte(originalText), 0o600))
	require.NoError(os.WriteFile(replacementTarget, []byte(replacementText), 0o600))
	require.NoError(os.Symlink(filepath.Base(original), link))
	snapshot, err := ReadConfigFile(link)
	require.NoError(err)
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
		require.NoError(os.Remove(link))
		require.NoError(os.Symlink(filepath.Base(replacementTarget), link))
		return replacement, nil
	}

	_, err = editConfigFile(link, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(1, releases)
	assert.Equal(originalText, string(mustReadFile(t, original)))
	assert.Equal(replacementText, string(mustReadFile(t, replacementTarget)))
	candidates, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*.toml.tmp"))
	require.NoError(globErr)
	assert.Empty(candidates)
}

func TestEditConfigFailsClosedWithoutConditionalReplacement(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	before, err := ReadConfigFile(path)
	require.NoError(err)
	ops := defaultConfigFileOps()
	ops.replace = func(_, _ string, _, _ ConfigFile) (configReplacement, error) {
		return configReplacement{}, ErrAtomicReplaceUnsupported
	}

	_, err = editConfigFile(path, before.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrAtomicReplaceUnsupported)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
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
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("durability unavailable")},
		&failingDirectoryHandle{},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedReplacementSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.Error(err)
	require.NotErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, beforeText, string(got))
}

func TestEditConfigReportsChangedWhenRollbackDirectorySyncFails(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("publication sync unavailable")},
		&failingDirectoryHandle{syncErr: errors.New("rollback sync unavailable")},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedReplacementSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, beforeText, string(got))
}

func TestEditConfigPreservesDisplacedArtifactWhenConflictRollbackFails(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	beforeText := "[web]\ntheme = \"system\"\n"
	require.NoError(os.WriteFile(path, []byte(beforeText), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	ops := defaultConfigFileOps()
	nativeReplace := ops.replace
	ops.replace = func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, replaceErr := nativeReplace(candidatePath, targetPath, candidateBefore, targetBefore)
		if replaceErr != nil {
			return configReplacement{}, replaceErr
		}
		// Change the displaced artifact after publication so verification must
		// enter rollback, then force rollback to retain every recovery file.
		require.NoError(os.Remove(replacement.displacedPath))
		require.NoError(os.WriteFile(replacement.displacedPath, []byte(beforeText), 0o600))
		replacement.rollbackPublished = func(ConfigFile) error { return errors.New("injected rollback failure") }
		return replacement, nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigChanged)
	artifacts, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*.toml.tmp"))
	require.NoError(globErr)
	require.Len(artifacts, 1, "the displaced operator file must remain recoverable")
	recovered, readErr := os.ReadFile(artifacts[0])
	require.NoError(readErr)
	assert.Equal(t, beforeText, string(recovered))
	assert.Contains(t, err.Error(), artifacts[0])
}

func TestConditionalReplaceRollbackPreservesLaterWriter(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	beforeText := []byte("[web]\ntheme = \"system\"\n")
	candidateText := []byte("[web]\ntheme = \"dark\"\n")
	laterText := []byte("# later writer\n[web]\ntheme = \"light\"\n")
	require.NoError(os.WriteFile(target, beforeText, 0o600))
	before, err := readConfigFileForEdit(target)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(before.retained.Close()) })
	candidate := filepath.Join(filepath.Dir(before.Path), "candidate.toml")
	require.NoError(os.WriteFile(candidate, candidateText, 0o600))

	replace := func(candidatePath, targetPath string, candidateBefore, targetBefore ConfigFile) (configReplacement, error) {
		replacement, replaceErr := beginConfigReplacement(candidatePath, targetPath, candidateBefore, targetBefore)
		if replaceErr != nil {
			return replacement, replaceErr
		}
		require.NoError(os.Remove(replacement.displacedPath))
		require.NoError(os.WriteFile(replacement.displacedPath,
			[]byte("# intervening writer\n[web]\ntheme = \"system\"\n"), 0o600))
		nativeRollback := replacement.rollbackPublished
		replacement.rollbackPublished = func(expected ConfigFile) error {
			// Install a later writer after conditionalReplace has verified the
			// published candidate and immediately before native rollback.
			require.NoError(os.Remove(target))
			require.NoError(os.WriteFile(target, laterText, 0o600))
			return nativeRollback(expected)
		}
		return replacement, nil
	}

	replacement, err := conditionalReplace(target, candidate, before, replace, ReadConfigFile)
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
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
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
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
	require.ErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
}

func TestEditConfigDeferredCandidateCleanupPreservesSubstitute(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conditional Unix cleanup")
	}
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	substitute := []byte("candidate substitute\n")
	var candidatePath string
	ops := defaultConfigFileOps()
	ops.beforeExchange = func() error {
		candidates, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-edit-*.toml.tmp"))
		require.NoError(globErr)
		require.Len(candidates, 1)
		candidatePath = candidates[0]
		require.NoError(os.Remove(candidatePath))
		require.NoError(os.WriteFile(candidatePath, substitute, 0o600))
		return errors.New("abort before publication")
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.Error(err)
	assert.Equal(t, substitute, mustReadFile(t, candidatePath))
	assert.Equal(t, "[web]\ntheme = \"system\"\n", string(mustReadFile(t, path)))
}

func TestEditConfigDisplacedCleanupPreservesSubstitute(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conditional Unix cleanup")
	}
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	substitute := []byte("displaced substitute\n")
	var displacedPath string
	ops := defaultConfigFileOps()
	ops.beforeExistingCleanup = func(replacement configReplacement) error {
		displacedPath = replacement.displacedPath
		require.NoError(os.Remove(displacedPath))
		require.NoError(os.WriteFile(displacedPath, substitute, 0o600))
		return nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(t, substitute, mustReadFile(t, displacedPath))
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(mustReadFile(t, path)))
}

func TestEditConfigMissingPublicationConsumesCandidate(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native no-replace publication")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	cleanupCalled := false
	ops := defaultConfigFileOps()
	ops.beforeMissingCleanup = func(configPublication) error {
		cleanupCalled = true
		return nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.NoError(err)
	assert.False(cleanupCalled)
	assert.Equal("[web]\ntheme = \"dark\"\n", string(mustReadFile(t, path)))
	candidates, globErr := filepath.Glob(filepath.Join(dir, ".config-edit-*"))
	require.NoError(globErr)
	assert.Empty(candidates)
}

func TestEditConfigCreationRollbackPreservesLaterWriter(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("conditional Unix cleanup")
	}
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
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
				require.NoError(nativeSync())
				require.NoError(os.Remove(path))
				require.NoError(os.WriteFile(path, later, 0o600))
				return errors.New("force creation rollback")
			}
			return nativeSync()
		}
		return publication, nil
	}

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigChanged)
	require.ErrorIs(err, ErrConfigConflict)
	assert.Equal(t, later, mustReadFile(t, path))
}

func TestEditConfigMissingPublicationHasNoCleanupStep(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
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
	require.NoError(err)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
}

func TestEditConfigRollsBackMissingFileWhenFirstDirectorySyncFails(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("durability unavailable")},
		&failingDirectoryHandle{},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedPublicationSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.Error(err)
	require.NotErrorIs(err, ErrConfigChanged)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestEditConfigReportsChangedWhenMissingRollbackDirectorySyncFails(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	opener := &sequenceDirectoryOpener{handles: []syncDirectoryHandle{
		&failingDirectoryHandle{syncErr: errors.New("publication sync unavailable")},
		&failingDirectoryHandle{syncErr: errors.New("rollback sync unavailable")},
	}}
	ops := defaultConfigFileOps()
	ops.openDirectory = opener.open
	overridePinnedPublicationSync(&ops, opener.open)

	_, err = editConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}}, ops)
	require.ErrorIs(err, ErrConfigChanged)
	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestEditConfigReportsChangedWhenFinalDirectorySyncFails(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if existing {
				require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
			}
			snapshot, err := ReadConfigFile(path)
			require.NoError(err)
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
			require.ErrorIs(err, ErrConfigChanged)
			got, readErr := os.ReadFile(path)
			require.NoError(readErr)
			assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
		})
	}
}

func TestEditConfigReportsChangedWhenFinalSnapshotReadFails(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
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
	require.ErrorIs(err, ErrConfigChanged)
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(got))
}

func TestResolveConfigTargetFailsClosedWhenOwnershipCannotBeVerified(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	link := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(target, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	require.NoError(os.Symlink(filepath.Base(target), link))

	resolved, mode, exists, err := resolveConfigTargetWithOwner(link, func(fs.FileInfo) (uint64, bool) {
		return 0, false
	})
	require.ErrorIs(err, ErrUnsafeConfigTarget)
	assert.Empty(resolved)
	assert.Zero(mode)
	assert.False(exists)
}

func TestResolveConfigTargetVerifiesParentDirectorySymlinkOwnership(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("Unix ownership semantics")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	require.NoError(os.Mkdir(realDir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(realDir, "config.toml"), []byte("[web]\n"), 0o600))
	parentLink := filepath.Join(dir, "linked")
	require.NoError(os.Symlink("real", parentLink))
	euid, supported := effectiveUserID()
	require.True(supported)

	resolved, mode, exists, err := resolveConfigTargetWithOwner(filepath.Join(parentLink, "config.toml"), func(info fs.FileInfo) (uint64, bool) {
		if info.Name() == "linked" {
			return euid + 1, true
		}
		return euid, true
	})
	require.ErrorIs(err, ErrUnsafeConfigTarget)
	assert.Empty(resolved)
	assert.Zero(mode)
	assert.False(exists)
}

func TestResolveConfigTargetAllowsVerifiedRootOwnedSystemHop(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("Unix ownership semantics")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	require.NoError(os.Mkdir(realDir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(realDir, "config.toml"), []byte("[web]\n"), 0o600))
	parentLink := filepath.Join(dir, "linked")
	require.NoError(os.Symlink("real", parentLink))
	euid, supported := effectiveUserID()
	require.True(supported)
	checked := false

	_, _, exists, err := resolveConfigTargetWithOwner(filepath.Join(parentLink, "config.toml"), func(info fs.FileInfo) (uint64, bool) {
		if info.Name() == "linked" {
			checked = true
			return 0, true
		}
		return euid, true
	})
	require.NoError(err)
	assert.True(checked)
	assert.True(exists)
}

func TestResolveConfigTargetRejectsIntermediateSymlinkSwapDuringInspection(t *testing.T) {
	if runtime.GOOS == windowsOS || runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("path-based fallback is not used on platforms with pinned resolvers")
	}
	require := require.New(t)
	dir := t.TempDir()
	firstDir := filepath.Join(dir, "first")
	secondDir := filepath.Join(dir, "second")
	require.NoError(os.Mkdir(firstDir, 0o700))
	require.NoError(os.Mkdir(secondDir, 0o700))
	first := filepath.Join(firstDir, "config.toml")
	second := filepath.Join(secondDir, "config.toml")
	require.NoError(os.WriteFile(first, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	require.NoError(os.WriteFile(second, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	link := filepath.Join(dir, "active")
	require.NoError(os.Symlink("first", link))
	euid, supported := effectiveUserID()
	require.True(supported)

	_, err := resolveOwnedSymlinksWithReadlink(filepath.Join(link, "config.toml"), func(fs.FileInfo) (uint64, bool) {
		return euid, true
	}, func(path string) (string, error) {
		if filepath.Base(path) == "active" {
			require.NoError(os.Remove(link))
			require.NoError(os.Symlink("second", link))
		}
		return os.Readlink(path)
	})
	require.ErrorIs(err, ErrUnsafeConfigTarget)
	assert.Equal(t, "[web]\ntheme = \"system\"\n", string(mustReadFile(t, first)))
	assert.Equal(t, "[web]\ntheme = \"light\"\n", string(mustReadFile(t, second)))
}

func TestFallbackResolverRejectsRemovedSymlinkAfterReadlink(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("Unix symlink semantics")
	}
	require := require.New(t)
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	require.NoError(os.Mkdir(targetDir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(targetDir, "config.toml"), []byte("[web]\n"), 0o600))
	link := filepath.Join(dir, "active")
	require.NoError(os.Symlink("target", link))
	euid, supported := effectiveUserID()
	require.True(supported)

	_, err := resolveOwnedSymlinksWithReadlink(filepath.Join(link, "config.toml"), func(fs.FileInfo) (uint64, bool) {
		return euid, true
	}, func(path string) (string, error) {
		target, err := os.Readlink(path)
		if filepath.Base(path) == "active" {
			require.NoError(os.Remove(link))
		}
		return target, err
	})
	require.ErrorIs(err, ErrUnsafeConfigTarget)
}

func TestPinnedResolverCannotBeRedirectedByIntermediateSymlinkSwap(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("pinned resolver is supported on Darwin and Linux")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	firstDir := filepath.Join(dir, "first")
	secondDir := filepath.Join(dir, "second")
	require.NoError(os.Mkdir(firstDir, 0o700))
	require.NoError(os.Mkdir(secondDir, 0o700))
	first := filepath.Join(firstDir, "config.toml")
	second := filepath.Join(secondDir, "config.toml")
	require.NoError(os.WriteFile(first, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	require.NoError(os.WriteFile(second, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	link := filepath.Join(dir, "active")
	require.NoError(os.Symlink("first", link))
	euid, supported := effectiveUserID()
	require.True(supported)
	swapped := false

	resolved, err := resolveOwnedSymlinksPinned(filepath.Join(link, "config.toml"), func(fs.FileInfo) (uint64, bool) {
		return euid, true
	}, func(file *os.File) {
		if filepath.Base(file.Name()) != "active" || swapped {
			return
		}
		swapped = true
		require.NoError(os.Remove(link))
		require.NoError(os.Symlink("second", link))
	})
	if err == nil {
		assert.Equal(first, resolved)
	}
	assert.True(swapped)
	assert.Equal("[web]\ntheme = \"system\"\n", string(mustReadFile(t, first)))
	assert.Equal("[web]\ntheme = \"light\"\n", string(mustReadFile(t, second)))
}

func TestEditConfigValidatesCompleteCandidateBeforeReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[analytics]\nengine = \"auto\"\n[server]\nbind_addr = \"127.0.0.1\"\n"
	require.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "analytics.engine", Value: "invalid"}})
	require.Error(err)
	assert.Contains(err.Error(), "invalid [analytics] engine")
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(before, string(got))
}

func TestEditConfigValidatesEnabledVectorCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	before := "[vector]\nenabled = false\n"
	require.NoError(os.WriteFile(path, []byte(before), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)

	_, err = EditConfigFile(path, snapshot.ETag, []Edit{{Key: "vector.enabled", Value: true}})
	require.Error(err)
	assert.Contains(err.Error(), "vector config")
	got, readErr := os.ReadFile(path)
	require.NoError(readErr)
	assert.Equal(before, string(got))
}

func TestEditConfigPreservesModeAndExistingSymlink(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink permission semantics differ on Windows")
	}
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "real-config.toml")
	link := filepath.Join(dir, "config.toml")
	intermediate := filepath.Join(dir, "config-link.toml")
	require.NoError(os.WriteFile(target, []byte("[web]\ndensity = \"compact\"\n"), 0o640))
	require.NoError(os.Chmod(target, 0o640))
	require.NoError(os.Symlink(filepath.Base(target), intermediate))
	require.NoError(os.Symlink(filepath.Base(intermediate), link))
	snapshot, err := ReadConfigFile(link)
	require.NoError(err)
	beforeInfo, err := os.Stat(target)
	require.NoError(err)
	beforeOwner, ok := fileOwner(beforeInfo)
	require.True(ok)

	_, err = EditConfigFile(link, snapshot.ETag, []Edit{{Key: "web.density", Value: "comfortable"}})
	require.NoError(err)
	linkInfo, err := os.Lstat(link)
	require.NoError(err)
	assert.NotZero(linkInfo.Mode() & os.ModeSymlink)
	targetInfo, err := os.Stat(target)
	require.NoError(err)
	assert.Equal(os.FileMode(0o640), targetInfo.Mode().Perm())
	afterOwner, ok := fileOwner(targetInfo)
	require.True(ok)
	assert.Equal(beforeOwner, afterOwner)
	got, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal("[web]\ndensity = \"comfortable\"\n", string(got))
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
		assert.ErrorIs(t, err, ErrUnsafeConfigTarget)
	})

	t.Run("non regular target", func(t *testing.T) {
		link := filepath.Join(dir, "directory.toml")
		require.NoError(t, os.Symlink(".", link))
		_, err := ReadConfigFile(link)
		require.ErrorIs(t, err, ErrUnsafeConfigTarget)
	})
}

func TestLoadConfigFileParsesSnapshotBytes(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	snapshot, err := ReadConfigFile(path)
	require.NoError(err)
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"dark\"\n"), 0o600))

	loaded, err := LoadConfigFile(snapshot, "")
	require.NoError(err)
	assert.Equal(t, "light", loaded.Web.Theme)
}

func TestLoadConfigFileUsesLogicalSymlinkPathForRelativeDefaults(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("symlink creation requires optional Windows privileges")
	}
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	logicalDir := filepath.Join(root, "logical")
	physicalDir := filepath.Join(root, "physical")
	require.NoError(os.Mkdir(logicalDir, 0o700))
	require.NoError(os.Mkdir(physicalDir, 0o700))
	target := filepath.Join(physicalDir, "config.toml")
	require.NoError(os.WriteFile(target, []byte("[web]\ntheme = \"dark\"\n"), 0o600))
	link := filepath.Join(logicalDir, "config.toml")
	require.NoError(os.Symlink(target, link))

	snapshot, err := ReadConfigFile(link)
	require.NoError(err)
	assert.Equal(link, snapshot.LogicalPath)
	resolvedTarget, err := filepath.EvalSymlinks(target)
	require.NoError(err)
	assert.Equal(resolvedTarget, snapshot.Path)
	fromSnapshot, err := LoadConfigFile(snapshot, "")
	require.NoError(err)
	fromDaemon, err := Load(link, "")
	require.NoError(err)
	assert.Equal(fromDaemon.HomeDir, fromSnapshot.HomeDir)
	assert.Equal(logicalDir, fromSnapshot.HomeDir)
}
